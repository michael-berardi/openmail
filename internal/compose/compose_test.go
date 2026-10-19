package compose

import (
	"strings"
	"testing"

	"github.com/michael-berardi/openmail/internal/model"
)

func TestBuildParseRoundTrip(t *testing.T) {
	raw, id, err := Build(Outgoing{
		From: model.Address{Name: "Zoë Müller", Addr: "zoe@example.com"}, To: []model.Address{{Name: "Bob", Addr: "bob@example.org"}},
		Cc: []model.Address{{Addr: "cc@example.org"}}, Subject: "Café ☕ plan", Text: "Héllo\n> quoted\nhttps://x.example/a?b=1&c=2",
		InReplyTo: "parent@x", References: []string{"root@x"},
		Attachments: []model.DraftAttachment{{Filename: "résumé.txt", ContentType: "text/plain", Data: []byte("data")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Café ☕ plan" || m.From.Name != "Zoë Müller" || m.MessageID != id || m.InReplyTo != "parent@x" {
		t.Fatalf("headers lost: %+v", m)
	}
	if len(m.References) != 2 || m.References[0] != "root@x" || m.References[1] != "parent@x" {
		t.Fatalf("references: %v", m.References)
	}
	if !strings.Contains(m.Text, "Héllo") || !m.HasAttach || len(m.Attachments) != 1 || m.Attachments[0].Filename != "résumé.txt" {
		t.Fatalf("body/attachments lost: %+v", m)
	}
	att, data, err := Attachment(raw, 0)
	if err != nil || string(data) != "data" || att.Filename != "résumé.txt" {
		t.Fatalf("attachment: %v %v %q", att, err, data)
	}
	if !strings.Contains(string(raw), "text/html") {
		t.Fatal("multipart/alternative with an HTML part expected")
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	if _, _, err := Build(Outgoing{From: model.Address{Addr: "a@b.com"}}); err == nil {
		t.Fatal("no recipients must fail")
	}
	if _, _, err := Build(Outgoing{From: model.Address{Addr: "a@b.com"}, To: []model.Address{{Addr: "not an address"}}}); err == nil {
		t.Fatal("bad recipient must fail")
	}
}

func TestParseLatin1AndHTMLOnly(t *testing.T) {
	raw := "From: a@x.com\r\nTo: b@x.com\r\nSubject: =?ISO-8859-1?Q?caf=E9?=\r\nContent-Type: text/html; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>caf=E9 <b>bold</b></p><div style=\"display:none\">hidden</div>"
	m, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "café" || !strings.Contains(m.Text, "café bold") || strings.Contains(m.Text, "hidden") {
		t.Fatalf("got subject=%q text=%q", m.Subject, m.Text)
	}
}
