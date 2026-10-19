package core

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/michael-berardi/openmail/internal/model"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	if err := app.Store.PutAccount(model.Account{Email: "me@x.com", Enabled: true, Source: "demo"}); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestAdminTokenNeverReachesSettingsClients(t *testing.T) {
	app := newTestApp(t)
	tok, err := app.EnsureLocalToken()
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range app.Settings() {
		if strings.HasPrefix(k, "_") || strings.Contains(v, tok) {
			t.Fatalf("settings expose %s", k)
		}
	}
	all, _ := app.Store.AllSettings()
	for k, v := range all {
		if strings.Contains(v, tok) {
			t.Fatalf("plaintext admin token stored in setting %s", k)
		}
	}
	// a restart issues a new token and the old one stops working
	tok2, _ := app.EnsureLocalToken()
	if tok2 == tok {
		t.Fatal("token must change on every start")
	}
	if _, err := app.Authenticate(tok); err == nil {
		t.Fatal("previous admin token still valid")
	}
	if _, err := app.Authenticate(tok2); err != nil {
		t.Fatal(err)
	}
}

func TestClientCannotForgeWorkflowFields(t *testing.T) {
	app := newTestApp(t)
	d, err := app.SaveDraft("agent:evil", model.Draft{AccountID: "me@x.com", To: []model.Address{{Addr: "v@x.com"}}, Subject: "s", Body: "b",
		State: StateQueued, Approved: true, SendAt: 1, Author: "user", QueuedBy: "user", ServerUID: 5, ServerBox: "Drafts"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := app.Store.GetDraft(d.ID)
	if got.State != StateDraft || got.Approved || got.SendAt != 0 || got.Author != "agent:evil" || got.QueuedBy != "" || got.ServerUID != 0 {
		t.Fatalf("workflow fields were taken from the client: %+v", got)
	}
	if due, _ := app.Store.DueDrafts(1 << 40); len(due) != 0 {
		t.Fatal("a forged draft was queued for sending")
	}
	// editing someone else's draft does not change its owner
	own, _ := app.SaveDraft("user", model.Draft{AccountID: "me@x.com", Subject: "mine"})
	edited, _ := app.SaveDraft("agent:evil", model.Draft{ID: own.ID, AccountID: "me@x.com", Subject: "changed", Author: "agent:evil"})
	if e, _ := app.Store.GetDraft(edited.ID); e.Author != "user" {
		t.Fatalf("author changed to %s", e.Author)
	}
}

func TestAgentLimitsAreEnforcedInCore(t *testing.T) {
	app := newTestApp(t)
	m := model.Message{AccountID: "me@x.com", MailboxID: 1, UID: 1, MessageID: "a@b", ThreadID: "t", Subject: "x"}
	_ = app.Store.UpsertMailbox(&model.Mailbox{AccountID: "me@x.com", Name: "INBOX", Role: "inbox"})
	m.MailboxID = 1
	if _, err := app.Store.SaveMessage(&m, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := app.Act(ctx, "agent:x", []int64{m.ID}, ActDelete, ""); err == nil {
		t.Fatal("agents must not permanently delete")
	}
	for _, k := range []string{"agent.send_policy", "oauth.google.client_id", "send.undo_seconds"} {
		if err := app.SetSetting("agent:x", k, "allow"); err == nil {
			t.Fatalf("agent changed %s", k)
		}
	}
	if err := app.SetSetting("mcp:x", "ui.theme", "dark"); err != nil {
		t.Fatalf("agents may change appearance: %v", err)
	}
}

func TestUnsubscribeIsGatedForAgents(t *testing.T) {
	app := newTestApp(t)
	_ = app.Store.UpsertMailbox(&model.Mailbox{AccountID: "me@x.com", Name: "INBOX", Role: "inbox"})
	m := model.Message{AccountID: "me@x.com", MailboxID: 1, UID: 1, MessageID: "n@l", ThreadID: "t", Subject: "news",
		ListUnsub: "<https://127.0.0.1:9/u>, <mailto:unsub@list.example?subject=remove>"}
	if _, err := app.Store.SaveMessage(&m, nil); err != nil {
		t.Fatal(err)
	}
	res, err := app.Unsubscribe(context.Background(), "mcp:x", m.ID)
	if err != nil || res.Done {
		t.Fatalf("agent under default policy must only get the link: %+v %v", res, err)
	}
	if ds, _ := app.Store.ListDrafts(""); len(ds) != 0 {
		t.Fatalf("agent created a send: %+v", ds)
	}
	// even a person cannot be tricked into hitting internal addresses with one-click
	m2 := model.Message{AccountID: "me@x.com", MailboxID: 1, UID: 2, MessageID: "n2@l", ThreadID: "t2", Subject: "news2", ListUnsub: "<https://127.0.0.1:9/u>", ListUnsubPost: true}
	_, _ = app.Store.SaveMessage(&m2, nil)
	if _, err := app.Unsubscribe(context.Background(), "user", m2.ID); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("one-click to loopback must be refused: %v", err)
	}
}

func TestPairMovedUIDsUsesServerOrder(t *testing.T) {
	var src, dst imap.UIDSet
	src.AddNum(10, 11, 12)
	dst.AddNum(500, 501, 502)
	got := pairUIDs(src, dst)
	if got[10] != 500 || got[11] != 501 || got[12] != 502 || len(got) != 3 {
		t.Fatalf("%v", got)
	}
	// our own order is irrelevant: lookups are by the source UID
	if pairUIDs(src, imap.UIDSet{}) != nil {
		t.Fatal("mismatched sets must not guess")
	}
}

func TestDraftCannotBeRecalledOnceSending(t *testing.T) {
	app := newTestApp(t)
	d, _ := app.SaveDraft("user", model.Draft{AccountID: "me@x.com", To: []model.Address{{Addr: "a@b.com"}}, Subject: "s", Body: "b"})
	q, err := app.SendDraft("user", d.ID)
	if err != nil || q.State != StateQueued || q.QueuedBy != "user" {
		t.Fatalf("%v %+v", err, q)
	}
	if ok, _ := app.Store.ClaimDraft(d.ID); !ok {
		t.Fatal("claim failed")
	}
	if _, err := app.CancelSend("user", d.ID); err == nil {
		t.Fatal("undo must fail once the message is being sent")
	}
	if _, err := app.ApproveDraft("user", d.ID); err == nil {
		t.Fatal("approve must not requeue a draft that is sending")
	}
	if _, err := app.SaveDraft("user", model.Draft{ID: d.ID, AccountID: "me@x.com", Subject: "edit"}); err == nil {
		t.Fatal("editing a sending draft must fail")
	}
	if n, _ := app.Store.FailStuckDrafts(); n != 1 {
		t.Fatalf("stuck draft not recovered: %d", n)
	}
	if got, _ := app.Store.GetDraft(d.ID); got.State != StateFailed || !strings.Contains(got.Error, "check Sent") {
		t.Fatalf("%+v", got)
	}
}

func TestAgentCannotEmptyTrashByTrashingTwice(t *testing.T) {
	app := newTestApp(t)
	trash := model.Mailbox{AccountID: "me@x.com", Name: "Trash", Role: "trash"}
	_ = app.Store.UpsertMailbox(&trash)
	m := model.Message{AccountID: "me@x.com", MailboxID: trash.ID, UID: 1, MessageID: "t@x", ThreadID: "t", Subject: "x"}
	if _, err := app.Store.SaveMessage(&m, nil); err != nil {
		t.Fatal(err)
	}
	if err := app.Act(context.Background(), "mcp:x", []int64{m.ID}, ActTrash, ""); err == nil {
		t.Fatal("trash from Trash is a permanent delete and must be refused for agents")
	}
	if _, err := app.Store.GetMessage(m.ID); err != nil {
		t.Fatalf("message was deleted: %v", err)
	}
}

func TestQueuedAgentSendsCountTowardTheHourlyLimit(t *testing.T) {
	app := newTestApp(t)
	if err := app.SetSetting("user", "agent.send_policy", "allow"); err != nil {
		t.Fatal(err)
	}
	if err := app.SetSetting("user", "agent.max_sends_hour", "2"); err != nil {
		t.Fatal(err)
	}
	var last error
	for i := 0; i < 4; i++ {
		d, err := app.SaveDraft("mcp:x", model.Draft{AccountID: "me@x.com", To: []model.Address{{Addr: "a@b.com"}}, Subject: "s", Body: "b"})
		if err != nil {
			t.Fatal(err)
		}
		_, last = app.SendDraft("mcp:x", d.ID)
		if i < 2 && last != nil {
			t.Fatalf("send %d should pass: %v", i, last)
		}
	}
	if last == nil || !strings.Contains(last.Error(), "limit") {
		t.Fatalf("drafts queued inside the undo window must count: %v", last)
	}
}

func TestPublicIPRanges(t *testing.T) {
	for ip, want := range map[string]bool{"8.8.8.8": true, "93.184.216.34": true, "127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.100.1.1": false, "198.18.0.5": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fd00::1": false, "64:ff9b::7f00:1": false, "2606:4700::1": true} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
}

func TestOverviewListsAreNeverNull(t *testing.T) {
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ov, _ := app.Overview()
	b, _ := json.Marshal(ov)
	if strings.Contains(string(b), `"accounts":null`) {
		t.Fatalf("accounts must be []: %s", b)
	}
	_ = app.Store.PutAccount(model.Account{Email: "n@x.com", Enabled: true})
	ov, _ = app.Overview()
	b, _ = json.Marshal(ov)
	if strings.Contains(string(b), `"mailboxes":null`) {
		t.Fatalf("mailboxes must be []: %s", b)
	}
}
