package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/query"
)

func TestPrivateFilesAndBoundedAuditLog(t *testing.T) {
	dir := t.TempDir()
	// An existing, loosely permissioned data directory must be tightened too.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "openmail.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir permissions: %v %v", fi, err)
	}
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		fi, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions: %v %v", p, fi, err)
		}
	}
	for i := 0; i < auditRetention+200; i++ {
		s.Audit("cli", "test", map[string]string{"note": "private"}, nil)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > auditRetention {
		t.Fatalf("audit log grew to %d rows; cap is %d", n, auditRetention)
	}
	entries, err := s.ListAudit(10, "")
	if err != nil || len(entries) != 10 {
		t.Fatalf("recent audit entries missing: %d %v", len(entries), err)
	}
}

func TestContactSearchPrefersPrefixAndVIP(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, a := range []model.Address{{Name: "Alex Rivera", Addr: "alex@atlas.example"}, {Name: "Your Bank", Addr: "security@bank-alerts.example"}, {Name: "Calvin", Addr: "calvin@x.example"}} {
		if err := s.TouchContact(a, 1); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		_ = s.TouchContact(model.Address{Name: "Your Bank", Addr: "security@bank-alerts.example"}, 2)
	}
	got, err := s.SearchContacts("al", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) < 2 || got[0].Addr != "alex@atlas.example" {
		t.Fatalf("prefix match should come first despite lower use count: %+v", got)
	}
	_ = s.SetVIP("calvin@x.example", true)
	if got, _ := s.SearchContacts("al", 10); got[0].Addr != "calvin@x.example" {
		t.Fatalf("VIPs lead: %+v", got)
	}
	if _, err := s.SearchContacts(`50%_off\`, 5); err != nil {
		t.Fatalf("wildcards must be escaped: %v", err)
	}
}

func TestRawCompressionRoundTrip(t *testing.T) {
	for _, in := range [][]byte{[]byte("short"), make([]byte, 100000), []byte("\x1f\x8bnot really gzip but looks like it")} {
		out, err := unpackRaw(packRaw(in))
		if err != nil || string(out) != string(in) {
			t.Fatalf("round trip failed for %d bytes: %v", len(in), err)
		}
	}
}

func TestAllMailDuplicatesAreHidden(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.PutAccount(model.Account{Email: "g@x.com", Enabled: true})
	inbox := &model.Mailbox{AccountID: "g@x.com", Name: "INBOX", Role: "inbox", Subscribed: true}
	all := &model.Mailbox{AccountID: "g@x.com", Name: "[Gmail]/All Mail", Role: "all", Subscribed: true}
	_ = s.UpsertMailbox(inbox)
	_ = s.UpsertMailbox(all)
	save := func(mb *model.Mailbox, uid uint32, id string) {
		m := model.Message{AccountID: "g@x.com", MailboxID: mb.ID, UID: uid, MessageID: id, ThreadID: "t" + id, Subject: "s " + id, Date: int64(uid)}
		if _, err := s.SaveMessage(&m, nil); err != nil {
			t.Fatal(err)
		}
	}
	save(inbox, 1, "a")  // in both
	save(all, 10, "a")   // the duplicate
	save(all, 11, "old") // archived only: must stay visible
	got, err := s.Find(query.Query{}, FindOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want inbox copy + archived-only message, got %d: %+v", len(got), got)
	}
	for _, m := range got {
		if m.MessageID == "a" && m.Mailbox != "INBOX" {
			t.Fatalf("duplicate shown from All Mail: %+v", m)
		}
	}
}
