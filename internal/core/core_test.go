package core

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/zalando/go-keyring"

	"github.com/michael-berardi/openmail/internal/model"
)

type lit struct {
	*strings.Reader
}

func (l lit) Size() int64 { return int64(l.Len()) }

func msg(id, inReplyTo, from, subject, body string, date time.Time) string {
	h := fmt.Sprintf("From: %s\r\nTo: me@test.local\r\nSubject: %s\r\nMessage-ID: <%s>\r\nDate: %s\r\n", from, subject, id, date.Format(time.RFC1123Z))
	if inReplyTo != "" {
		h += "In-Reply-To: <" + inReplyTo + ">\r\nReferences: <" + inReplyTo + ">\r\n"
	}
	return h + "Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
}

type smtpBackend struct {
	mu   sync.Mutex
	msgs []string
	rcpt [][]string
}

type smtpSession struct {
	b    *smtpBackend
	to   []string
	auth bool
}

func (b *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) { return &smtpSession{b: b}, nil }
func (s *smtpSession) AuthMechanisms() []string                      { return []string{"PLAIN"} }
func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if username != "me@test.local" || password != "secret" {
			return fmt.Errorf("bad credentials")
		}
		s.auth = true
		return nil
	}), nil
}
func (s *smtpSession) Reset()                                      { s.to = nil }
func (s *smtpSession) Logout() error                               { return nil }
func (s *smtpSession) Mail(from string, o *smtp.MailOptions) error { return nil }
func (s *smtpSession) Rcpt(to string, o *smtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}
func (s *smtpSession) Data(r io.Reader) error {
	b, _ := io.ReadAll(r)
	s.b.mu.Lock()
	s.b.msgs = append(s.b.msgs, string(b))
	s.b.rcpt = append(s.b.rcpt, s.to)
	s.b.mu.Unlock()
	return nil
}

func startServers(t *testing.T) (imapPort, smtpPort int, sb *smtpBackend, user *imapmemserver.User) {
	t.Helper()
	ms := imapmemserver.New()
	user = imapmemserver.NewUser("me@test.local", "secret")
	for _, n := range []string{"INBOX", "Sent", "Trash", "Drafts", "Archive"} {
		if err := user.Create(n, nil); err != nil && n != "INBOX" {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-48 * time.Hour)
	for i, m := range []string{
		msg("a1@x", "", "Alice <alice@example.com>", "Quarterly plan", "Here is the Q3 plan with budget numbers.", base),
		msg("a2@x", "a1@x", "Bob <bob@example.com>", "Re: Quarterly plan", "Looks good, ship it.", base.Add(time.Hour)),
		msg("a3@x", "", "News <news@list.example>", "Weekly digest", "Unrelated newsletter text.", base.Add(2*time.Hour)),
	} {
		_ = i
		if _, err := user.Append("INBOX", lit{strings.NewReader(m)}, &imap.AppendOptions{Time: base}); err != nil {
			t.Fatal(err)
		}
	}
	ms.AddUser(user)
	is := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return ms.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}, imap.CapMove: {}, imap.CapIdle: {}, imap.CapNamespace: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go is.Serve(ln)
	t.Cleanup(func() { is.Close() })
	imapPort = ln.Addr().(*net.TCPAddr).Port

	sb = &smtpBackend{}
	ss := smtp.NewServer(sb)
	ss.Domain, ss.AllowInsecureAuth = "test.local", true
	sl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go ss.Serve(sl)
	t.Cleanup(func() { ss.Close() })
	return imapPort, sl.Addr().(*net.TCPAddr).Port, sb, user
}

func TestEndToEnd(t *testing.T) {
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ip, sp, sb, _ := startServers(t)

	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.SetSetting("test", "send.undo_seconds", "0"); err != nil {
		t.Fatal(err)
	}

	res, err := app.AddAccount(ctx, "test", AddRequest{
		Email: "me@test.local", Password: "secret", DisplayName: "Me",
		IMAPHost: "127.0.0.1", IMAPPort: ip, IMAPSecurity: "none",
		SMTPHost: "127.0.0.1", SMTPPort: sp, SMTPSecurity: "none",
	})
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if res.Account.ID != "me@test.local" {
		t.Fatalf("account id %q", res.Account.ID)
	}
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	msgs, err := app.Search("in:inbox", ListOptions{})
	if err != nil || len(msgs) != 3 {
		t.Fatalf("inbox: %v n=%d", err, len(msgs))
	}
	threads, err := app.Threads("in:inbox", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 {
		t.Fatalf("want 2 threads (reply grouped), got %d", len(threads))
	}
	found, _ := app.Search("budget", ListOptions{})
	if len(found) != 1 || found[0].From.Addr != "alice@example.com" {
		t.Fatalf("full-text search: %+v", found)
	}
	if found, _ := app.Search("from:bob is:unread", ListOptions{}); len(found) != 1 {
		t.Fatalf("structured search got %d", len(found))
	}

	// mark read goes to the server and survives a re-sync
	id := found[0].ID
	if err := app.Act(ctx, "test", []int64{id}, ActRead, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	m, _ := app.GetMessage(id)
	if !m.Seen {
		t.Fatal("seen flag lost after sync")
	}

	// move to trash then archive another
	if err := app.Act(ctx, "test", []int64{id}, ActTrash, ""); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if inbox, _ := app.Search("in:inbox", ListOptions{}); len(inbox) != 2 {
		t.Fatalf("inbox after trash = %d", len(inbox))
	}
	if tr, _ := app.Search("in:trash", ListOptions{}); len(tr) != 1 {
		t.Fatalf("trash = %d", len(tr))
	}

	// reply -> send as human
	all, _ := app.Search("subject:Weekly", ListOptions{})
	d, err := app.Reply("test", all[0].ID, false, "Thanks, unsubscribing.")
	if err != nil {
		t.Fatal(err)
	}
	if d.To[0].Addr != "news@list.example" || !strings.HasPrefix(d.Subject, "Re:") || d.InReplyTo != "a3@x" {
		t.Fatalf("reply draft wrong: %+v", d)
	}
	if err := app.SendNow(ctx, "test", d.ID); err != nil {
		t.Fatalf("send: %v", err)
	}
	sb.mu.Lock()
	if len(sb.msgs) != 1 || !strings.Contains(sb.msgs[0], "In-Reply-To: <a3@x>") || !strings.Contains(sb.msgs[0], "unsubscribing") {
		t.Fatalf("smtp capture: %q", sb.msgs)
	}
	sb.mu.Unlock()
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if sent, _ := app.Search("in:sent", ListOptions{}); len(sent) != 1 {
		t.Fatalf("sent copy missing: %d", len(sent))
	}
	if after, _ := app.GetMessage(all[0].ID); !after.Answered {
		t.Fatal("original not marked answered")
	}

	// agent send policy: approve (default)
	d2, err := app.SaveDraft("mcp:claude", model.Draft{AccountID: "me@test.local", To: []model.Address{{Addr: "x@example.com"}}, Subject: "hi", Body: "from agent"})
	if err != nil {
		t.Fatal(err)
	}
	d2, err = app.SendDraft("mcp:claude", d2.ID)
	if err != nil || d2.State != StatePending {
		t.Fatalf("agent send should wait for approval: %v %+v", err, d2)
	}
	if _, err := app.ApproveDraft("mcp:claude", d2.ID); err == nil {
		t.Fatal("agent must not approve its own send")
	}
	if d2, err = app.ApproveDraft("user", d2.ID); err != nil || d2.State != StateQueued {
		t.Fatalf("approve: %v %+v", err, d2)
	}
	app.SetSetting("test", "agent.send_policy", "draft_only")
	d3, _ := app.SaveDraft("mcp:claude", model.Draft{AccountID: "me@test.local", To: []model.Address{{Addr: "x@example.com"}}, Subject: "again", Body: "b"})
	if _, err := app.SendDraft("mcp:claude", d3.ID); err == nil {
		t.Fatal("draft_only must refuse agent send")
	}

	// remove account wipes local data
	if err := app.RemoveAccount("test", "me@test.local"); err != nil {
		t.Fatal(err)
	}
	if left, _ := app.Search("", ListOptions{IncludeJunk: true}); len(left) != 0 {
		t.Fatalf("messages left after remove: %d", len(left))
	}
}

func TestOfflineChangesAreQueuedAndReplayed(t *testing.T) {
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ip, sp, _, _ := startServers(t)
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.AddAccount(ctx, "test", AddRequest{Email: "me@test.local", Password: "secret", IMAPHost: "127.0.0.1", IMAPPort: ip, IMAPSecurity: "none", SMTPHost: "127.0.0.1", SMTPPort: sp, SMTPSecurity: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	msgs, _ := app.Search("is:unread subject:Weekly", ListOptions{})
	if len(msgs) != 1 {
		t.Fatalf("seed: %d", len(msgs))
	}
	acct, _ := app.Store.GetAccount("me@test.local")
	good := acct
	// point at a dead port: the change must still succeed locally and be queued
	acct.IMAPPort = 1
	if err := app.Store.PutAccount(acct); err != nil {
		t.Fatal(err)
	}
	app.dropOpsConn(acct.ID)
	if err := app.Act(ctx, "test", []int64{msgs[0].ID}, ActRead, ""); err != nil {
		t.Fatalf("offline act must not fail: %v", err)
	}
	if n, _ := app.Store.CountPendingOps(); n != 1 {
		t.Fatalf("pending ops = %d, want 1", n)
	}
	if m, _ := app.GetMessage(msgs[0].ID); !m.Seen {
		t.Fatal("local state must reflect the change immediately")
	}
	// back online: the queue drains and the server agrees
	if err := app.Store.PutAccount(good); err != nil {
		t.Fatal(err)
	}
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if n, _ := app.Store.CountPendingOps(); n != 0 {
		t.Fatalf("queue not drained: %d", n)
	}
	if m, _ := app.GetMessage(msgs[0].ID); !m.Seen {
		t.Fatal("server state lost the queued change")
	}
}

func TestEnginePushesNewMail(t *testing.T) {
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ip, sp, _, user := startServers(t)
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.AddAccount(ctx, "test", AddRequest{Email: "me@test.local", Password: "secret", IMAPHost: "127.0.0.1", IMAPPort: ip, IMAPSecurity: "none", SMTPHost: "127.0.0.1", SMTPPort: sp, SMTPSecurity: "none"}); err != nil {
		t.Fatal(err)
	}
	run, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- app.Run(run) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m, _ := app.Search("in:inbox", ListOptions{}); len(m) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial sync never finished")
		}
		time.Sleep(100 * time.Millisecond)
	}
	ch, unsub := app.Subscribe()
	defer unsub()
	m := msg("push1@x", "", "Carol <carol@example.com>", "Pushed message", "arrived while idle", time.Now())
	if _, err := user.Append("INBOX", lit{strings.NewReader(m)}, &imap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		if got, _ := app.Search("subject:Pushed", ListOptions{}); len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("IDLE did not deliver the new message")
		}
		select {
		case <-ch:
		case <-time.After(200 * time.Millisecond):
		}
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("engine: %v", err)
	}
}

func TestLargeMessagesAreStubbedAndDownloadable(t *testing.T) {
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ip, sp, _, user := startServers(t)
	big := msg("big1@x", "", "Video <video@example.com>", "Holiday video", strings.Repeat("A", 2<<20), time.Now())
	if _, err := user.Append("INBOX", lit{strings.NewReader(big)}, &imap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.SetSetting("test", "sync.max_message_mb", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.AddAccount(ctx, "test", AddRequest{Email: "me@test.local", Password: "secret", IMAPHost: "127.0.0.1", IMAPPort: ip, IMAPSecurity: "none", SMTPHost: "127.0.0.1", SMTPPort: sp, SMTPSecurity: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	got, _ := app.Search("subject:Holiday", ListOptions{})
	if len(got) != 1 || !got[0].Partial || got[0].From.Addr != "video@example.com" {
		t.Fatalf("large message should be a stub with headers: %+v", got)
	}
	if _, err := app.Store.RawMessage(got[0].ID); err == nil {
		t.Fatal("stub must not hold a raw body")
	}
	if err := app.DownloadMessage(ctx, "test", got[0].ID); err != nil {
		t.Fatalf("download: %v", err)
	}
	full, _ := app.GetMessage(got[0].ID)
	if full.Partial || !strings.HasPrefix(full.Text, "AAAA") {
		t.Fatalf("download did not fill the message: partial=%v text=%.20q", full.Partial, full.Text)
	}
	raw, err := app.Store.RawMessage(got[0].ID)
	if err != nil || len(raw) < 2<<20 {
		t.Fatalf("raw after download: %v len=%d", err, len(raw))
	}
	var stored int
	_ = app.Store.DB().QueryRow(`SELECT length(data) FROM raw WHERE message_pk=?`, got[0].ID).Scan(&stored)
	if stored > len(raw)/10 {
		t.Fatalf("raw should be stored compressed: %d of %d bytes", stored, len(raw))
	}
}

func TestThreadingIsOrderIndependent(t *testing.T) {
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	mb := model.Mailbox{AccountID: "a@x.com", Name: "INBOX"}
	uid := uint32(0)
	mk := func(id, irt string, refs ...string) string {
		m := &model.Message{MessageID: id, InReplyTo: irt, References: refs}
		tid := app.threadFor("a@x.com", m, mb)
		uid++
		m.ThreadID, m.AccountID, m.MailboxID, m.UID = tid, "a@x.com", 1, uid
		if _, err := app.Store.SaveMessage(m, nil); err != nil {
			t.Fatal(err)
		}
		return tid
	}
	// deepest reply arrives first, then the middle, then the root
	t3 := mk("c@x", "b@x", "a@x", "b@x")
	t2 := mk("b@x", "a@x", "a@x")
	t1 := mk("a@x", "")
	if t1 != t2 || t2 != t3 {
		t.Fatalf("one conversation expected, got %s %s %s", t1, t2, t3)
	}
	if other := mk("z@x", ""); other == t1 {
		t.Fatal("unrelated message must not join the thread")
	}
}

func draftsOnServer(t *testing.T, app *App, ctx context.Context) int {
	t.Helper()
	acct, _ := app.Store.GetAccount("me@test.local")
	c, err := app.Dial(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sel, err := c.Select("Drafts", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return int(sel.NumMessages)
}

func TestDraftsMirrorToServerAndBack(t *testing.T) {
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ip, sp, _, user := startServers(t)
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.AddAccount(ctx, "test", AddRequest{Email: "me@test.local", Password: "secret", IMAPHost: "127.0.0.1", IMAPPort: ip, IMAPSecurity: "none", SMTPHost: "127.0.0.1", SMTPPort: sp, SMTPSecurity: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	// a draft with no recipient yet is mirrored too
	d, err := app.SaveDraft("user", model.Draft{AccountID: "me@test.local", Subject: "Plan", Body: "first version"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pushDraft(ctx, d.ID); err != nil {
		t.Fatalf("push: %v", err)
	}
	if n := draftsOnServer(t, app, ctx); n != 1 {
		t.Fatalf("server drafts = %d, want 1", n)
	}
	// editing replaces the server copy instead of piling up versions
	d.Body = "second version"
	d, _ = app.SaveDraft("user", d)
	if err := app.pushDraft(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if n := draftsOnServer(t, app, ctx); n != 1 {
		t.Fatalf("after edit server drafts = %d, want 1", n)
	}
	// syncing must not show our own draft back to us as a message
	if err := app.SyncNow(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := app.Search("in:drafts", ListOptions{IncludeJunk: true}); len(got) != 0 {
		t.Fatalf("own draft echoed into the mailbox view: %d", len(got))
	}
	// a draft written elsewhere appears as a message and can be taken over
	other := msg("other@x", "", "Me <me@test.local>", "Started on my phone", "half a sentence", time.Now())
	if _, err := user.Append("Drafts", lit{strings.NewReader(other)}, &imap.AppendOptions{Flags: []imap.Flag{imap.FlagDraft}, Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := app.SyncNow(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	found, _ := app.Search("in:drafts subject:phone", ListOptions{IncludeJunk: true})
	if len(found) != 1 {
		t.Fatalf("draft from another client not visible: %d", len(found))
	}
	ed, err := app.EditServerDraft(ctx, "test", found[0].ID)
	if err != nil || ed.Subject != "Started on my phone" || ed.ServerUID == 0 {
		t.Fatalf("edit server draft: %v %+v", err, ed)
	}
	ed.Body = "finished sentence"
	ed, _ = app.SaveDraft("user", ed)
	if err := app.pushDraft(ctx, ed.ID); err != nil {
		t.Fatal(err)
	}
	if n := draftsOnServer(t, app, ctx); n != 2 { // Plan + the taken-over one (old copy replaced)
		t.Fatalf("server drafts = %d, want 2", n)
	}
	// deleting locally deletes remotely
	if err := app.DeleteDraft("user", d.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteDraft("user", ed.ID); err != nil {
		t.Fatal(err)
	}
	if n := draftsOnServer(t, app, ctx); n != 0 {
		t.Fatalf("server drafts after delete = %d, want 0", n)
	}
}

func TestFormattedDraftsAreSanitisedAndGetPlainAlternative(t *testing.T) {
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	_ = app.Store.PutAccount(model.Account{Email: "me@x.com", Enabled: true, Source: "demo"})
	d, err := app.SaveDraft("user", model.Draft{AccountID: "me@x.com", To: []model.Address{{Addr: "a@b.com"}}, Subject: "s",
		HTMLBody: `<p>Hello <b>there</b></p><script>steal()</script><img src=x onerror=alert(1)><a href="javascript:evil()">x</a>`})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := app.Store.GetDraft(d.ID)
	for _, bad := range []string{"<script", "onerror", "javascript:"} {
		if strings.Contains(got.HTMLBody, bad) {
			t.Fatalf("%q survived: %s", bad, got.HTMLBody)
		}
	}
	if !strings.Contains(got.HTMLBody, "<b>there</b>") || !strings.Contains(got.Body, "Hello there") {
		t.Fatalf("formatting/plain alt wrong: html=%q body=%q", got.HTMLBody, got.Body)
	}
}

func TestOneFolderPerRole(t *testing.T) {
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ip, sp, _, user := startServers(t)
	for _, n := range []string{"Sent Messages", "Deleted Messages", "Spam"} {
		if err := user.Create(n, nil); err != nil {
			t.Fatal(err)
		}
	}
	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.AddAccount(ctx, "test", AddRequest{Email: "me@test.local", Password: "secret", IMAPHost: "127.0.0.1", IMAPPort: ip, IMAPSecurity: "none", SMTPHost: "127.0.0.1", SMTPPort: sp, SMTPSecurity: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := app.SyncNow(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	mbs, _ := app.Store.ListMailboxes("me@test.local")
	count := map[string]int{}
	roleOf := map[string]string{}
	for _, m := range mbs {
		if m.Role != "" {
			count[m.Role]++
		}
		roleOf[m.Name] = m.Role
	}
	for role, n := range count {
		if n != 1 {
			t.Errorf("role %s is held by %d folders", role, n)
		}
	}
	if roleOf["Sent"] != "sent" || roleOf["Sent Messages"] != "" || roleOf["Trash"] != "trash" || roleOf["Deleted Messages"] != "" {
		t.Errorf("roles: %v", roleOf)
	}
}
