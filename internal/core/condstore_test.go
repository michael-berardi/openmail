package core

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/michael-berardi/openmail/internal/model"
)

// scriptedIMAP speaks just enough IMAP (with CONDSTORE) to prove the client sends the right
// commands and understands HIGHESTMODSEQ and MODSEQ. The memory server we use elsewhere has no
// CONDSTORE, so this is the only end-to-end check of the incremental path.
type scriptedIMAP struct {
	mu   sync.Mutex
	cmds []string
}

func (s *scriptedIMAP) serve(t *testing.T, ln net.Listener, body string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			r := bufio.NewReader(c)
			fmt.Fprint(c, "* OK ready\r\n")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				sp := strings.SplitN(line, " ", 2)
				if len(sp) < 2 {
					continue
				}
				tag, cmd := sp[0], sp[1]
				s.mu.Lock()
				s.cmds = append(s.cmds, cmd)
				s.mu.Unlock()
				up := strings.ToUpper(cmd)
				switch {
				case strings.HasPrefix(up, "CAPABILITY"):
					fmt.Fprintf(c, "* CAPABILITY IMAP4rev1 CONDSTORE UIDPLUS IDLE\r\n%s OK done\r\n", tag)
				case strings.HasPrefix(up, "LOGIN"):
					fmt.Fprintf(c, "%s OK [CAPABILITY IMAP4rev1 CONDSTORE UIDPLUS IDLE] logged in\r\n", tag)
				case strings.HasPrefix(up, "SELECT"), strings.HasPrefix(up, "EXAMINE"):
					fmt.Fprintf(c, "* 3 EXISTS\r\n* OK [UIDVALIDITY 7] x\r\n* OK [UIDNEXT 102] x\r\n* OK [HIGHESTMODSEQ 510] x\r\n* FLAGS (\\Seen \\Flagged)\r\n%s OK [READ-ONLY] selected\r\n", tag)
				case strings.HasPrefix(up, "UID FETCH") && strings.Contains(up, "CHANGEDSINCE"):
					fmt.Fprintf(c, "* 2 FETCH (UID 2 FLAGS (\\Seen \\Flagged) MODSEQ (505))\r\n%s OK done\r\n", tag)
				case strings.HasPrefix(up, "UID FETCH 101:*"):
					fmt.Fprintf(c, "* 3 FETCH (UID 101 RFC822.SIZE %d)\r\n%s OK done\r\n", len(body), tag)
				case strings.HasPrefix(up, "UID FETCH 101 "), strings.HasPrefix(up, "UID FETCH 101("):
					fmt.Fprintf(c, "* 3 FETCH (UID 101 FLAGS () INTERNALDATE \"19-Oct-2026 10:00:00 +0000\" RFC822.SIZE %d BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(body), len(body), body, tag)
				case strings.HasPrefix(up, "LOGOUT"):
					fmt.Fprintf(c, "* BYE\r\n%s OK bye\r\n", tag)
					return
				default:
					fmt.Fprintf(c, "%s BAD unexpected %s\r\n", tag, cmd)
				}
			}
		}(c)
	}
}

func TestIncrementalSyncSpeaksCondstore(t *testing.T) {
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv := &scriptedIMAP{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	body := "From: n@x.com\r\nTo: me@x.com\r\nSubject: new arrival\r\nMessage-ID: <n1@x>\r\nDate: Mon, 19 Oct 2026 10:00:00 +0000\r\n\r\nhello\r\n"
	go srv.serve(t, ln, body)

	app, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	acct := model.Account{ID: "me@x.com", Email: "me@x.com", Username: "me@x.com", IMAPHost: "127.0.0.1", IMAPPort: port, IMAPSecurity: "none", Auth: "password", Enabled: true}
	if err := app.Store.PutAccount(acct); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENMAIL_PASSWORD_ME_X_COM", "pw")
	mb := model.Mailbox{AccountID: acct.ID, Name: "INBOX", Role: "inbox", Subscribed: true}
	if err := app.Store.UpsertMailbox(&mb); err != nil {
		t.Fatal(err)
	}
	// two messages known locally from the previous sync, then one arrival and a flag change on the server
	for u := uint32(1); u <= 2; u++ {
		m := model.Message{AccountID: acct.ID, MailboxID: mb.ID, UID: u, MessageID: fmt.Sprint("old", u), ThreadID: fmt.Sprint("t", u), Subject: "old", Date: 1}
		if _, err := app.Store.SaveMessage(&m, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Store.SetMailboxSync(mb.ID, 7, 101, 100, 2, 500); err != nil {
		t.Fatal(err)
	}
	mb, _ = app.Store.GetMailbox(mb.ID)

	c, err := app.Dial(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := app.syncMailbox(ctx, acct, c, mb, false); err != nil {
		t.Fatalf("incremental sync: %v", err)
	}

	srv.mu.Lock()
	cmds := strings.Join(srv.cmds, "\n")
	srv.mu.Unlock()
	for _, want := range []string{"(CONDSTORE)", "CHANGEDSINCE 500", "UID FETCH 101:*"} {
		if !strings.Contains(strings.ToUpper(cmds), strings.ToUpper(want)) {
			t.Fatalf("client never sent %q. commands:\n%s", want, cmds)
		}
	}
	if strings.Contains(strings.ToUpper(cmds), "UID FETCH 1:* (UID FLAGS RFC822SIZE)") {
		t.Fatalf("a pure arrival must not fetch every flag again:\n%s", cmds)
	}
	got, _ := app.Search("subject:arrival", ListOptions{})
	if len(got) != 1 || got[0].UID != 101 {
		t.Fatalf("arrival not stored: %+v", got)
	}
	m2, _ := app.Store.FindMailbox(acct.ID, "INBOX")
	if m2.ModSeq != 510 || m2.UIDNext != 102 || m2.Exists != 3 || m2.LastUID != 101 {
		t.Fatalf("sync state not advanced: %+v", m2)
	}
	flagged, _ := app.Search("is:flagged", ListOptions{})
	if len(flagged) != 1 || flagged[0].UID != 2 {
		t.Fatalf("changed flags not applied: %+v", flagged)
	}
	// and a second pass with nothing new is a pure no-op beyond SELECT
	srv.mu.Lock()
	srv.cmds = nil
	srv.mu.Unlock()
	if err := app.syncMailbox(ctx, acct, c, m2, false); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	again := strings.Join(srv.cmds, "\n")
	srv.mu.Unlock()
	if strings.Contains(strings.ToUpper(again), "FETCH") {
		t.Fatalf("unchanged mailbox must not fetch anything:\n%s", again)
	}
}
