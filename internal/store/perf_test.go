package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/query"
)

func TestScale(t *testing.T) {
	if os.Getenv("OPENMAIL_SCALE") == "" {
		t.Skip("set OPENMAIL_SCALE=1 to run the 30k-message scale test")
	}
	s, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.PutAccount(model.Account{Email: "a@b.c", Enabled: true})
	mb := &model.Mailbox{AccountID: "a@b.c", Name: "INBOX", Role: "inbox", Subscribed: true}
	_ = s.UpsertMailbox(mb)
	start := time.Now()
	const n = 30000
	for i := 0; i < n; i++ {
		m := model.Message{AccountID: "a@b.c", MailboxID: mb.ID, UID: uint32(i + 1), MessageID: fmt.Sprint("m", i), ThreadID: fmt.Sprint("t", i/3), Subject: fmt.Sprintf("Subject number %d about budget", i),
			From: model.Address{Name: "Sender", Addr: fmt.Sprintf("s%d@x.com", i%500)}, Date: time.Now().Add(-time.Duration(i) * time.Minute).Unix(), Seen: i%4 != 0,
			Text: "Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. unique" + fmt.Sprint(i)}
		if _, err := s.SaveMessage(&m, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("insert %d: %s", n, time.Since(start))
	_ = s.RefreshCounts(mb.ID)
	tm := func(name string, f func()) { st := time.Now(); f(); t.Logf("%-28s %s", name, time.Since(st)) }
	q, _ := query.Parse("in:inbox", time.Now())
	tm("threads inbox (50)", func() {
		r, _ := s.Threads(q, FindOptions{Limit: 50})
		if len(r) != 50 {
			t.Fatal(len(r))
		}
	})
	tm("threads page 2", func() { s.Threads(q, FindOptions{Limit: 50, Offset: 50}) })
	qu, _ := query.Parse("is:unread in:inbox", time.Now())
	tm("threads unread", func() { s.Threads(qu, FindOptions{Limit: 50}) })
	qs, _ := query.Parse("budget unique777", time.Now())
	tm("fts search", func() {
		r, _ := s.Find(qs, FindOptions{Limit: 50})
		if len(r) < 1 {
			t.Fatal(len(r))
		}
	})
	qf, _ := query.Parse("from:s42@x.com", time.Now())
	tm("from filter", func() { s.Find(qf, FindOptions{Limit: 50}) })
	tm("overview mailbox counts", func() { s.ListMailboxes("") })
}
