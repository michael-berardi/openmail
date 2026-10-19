package imapx

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestSelectableSkipsContainers(t *testing.T) {
	cases := []struct {
		name  string
		attrs []imap.MailboxAttr
		want  bool
	}{
		{"plain", nil, true},
		{"gmail container", []imap.MailboxAttr{imap.MailboxAttrNoSelect, imap.MailboxAttrHasChildren}, false},
		{"list-extended nonexistent", []imap.MailboxAttr{imap.MailboxAttrNonExistent, imap.MailboxAttrHasChildren}, false},
		{"sent", []imap.MailboxAttr{imap.MailboxAttrSent}, true},
	}
	for _, c := range cases {
		if got := Selectable(&imap.ListData{Mailbox: "x", Attrs: c.attrs}); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if Role(&imap.ListData{Mailbox: "[Gmail]/Sent Mail", Attrs: []imap.MailboxAttr{imap.MailboxAttrSent}}) != "sent" {
		t.Error("special-use sent not detected")
	}
	if Role(&imap.ListData{Mailbox: "Sent Items"}) != "sent" || Role(&imap.ListData{Mailbox: "INBOX"}) != "inbox" {
		t.Error("name-based roles")
	}
}
