package discover

import (
	"context"
	"testing"
)

func TestPresets(t *testing.T) {
	cases := map[string]struct{ imap, smtp, auth string }{
		"me@icloud.com":  {"imap.mail.me.com", "smtp.mail.me.com", "password"},
		"x@gmail.com":    {"imap.gmail.com", "smtp.gmail.com", "oauth2"},
		"x@outlook.com":  {"outlook.office365.com", "smtp.office365.com", "oauth2"},
		"x@fastmail.com": {"imap.fastmail.com", "smtp.fastmail.com", "password"},
		"x@zoho.com":     {"imap.zoho.com", "smtp.zoho.com", "password"},
		"x@yahoo.com":    {"imap.mail.yahoo.com", "smtp.mail.yahoo.com", "password"},
	}
	for email, want := range cases {
		s, err := Discover(context.Background(), email)
		if err != nil {
			t.Fatalf("%s: %v", email, err)
		}
		if s.IMAPHost != want.imap || s.SMTPHost != want.smtp || s.Auth != want.auth || s.Username != email {
			t.Errorf("%s: %+v", email, s)
		}
	}
	for _, bad := range []string{"", "nope", "a@", "@b.com", "a b@c.com"} {
		if _, err := Discover(context.Background(), bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
