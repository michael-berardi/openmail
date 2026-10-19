package query

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	q, err := Parse(`from:alice subject:"q3 plan" is:unread has:attachment after:7d in:inbox budget "exact phrase"`, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.From) != 1 || q.From[0] != "alice" || q.Subject[0] != "q3 plan" || q.Unread == nil || !*q.Unread || q.HasAttachment == nil || q.Mailbox != "inbox" {
		t.Fatalf("parsed wrong: %+v", q)
	}
	if want := now.AddDate(0, 0, -7).Unix(); q.After != want {
		t.Fatalf("after=%d want %d", q.After, want)
	}
	if len(q.Text) != 2 || q.Text[1] != "exact phrase" {
		t.Fatalf("text: %v", q.Text)
	}
	if _, err := Parse("is:nonsense", now); err == nil {
		t.Fatal("unknown is: filter must fail with a message")
	}
	if _, err := Parse("after:someday", now); err == nil {
		t.Fatal("bad date must fail")
	}
}

func TestFTSNeverInjectsOperators(t *testing.T) {
	got := FTS([]string{`foo" OR "bar`, "a b", "plain"})
	want := `"foo  OR  bar" AND "a b" AND "plain"*` // injected quotes become part of one safe phrase
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
