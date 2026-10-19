// Package compose parses RFC 5322 messages into OpenMail's model and builds outgoing mail.
package compose

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"mime"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	gomail "github.com/emersion/go-message/mail"

	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/textutil"
)

func cleanID(s string) string {
	return strings.Trim(strings.TrimSpace(s), "<>")
}

func addrs(h gomail.Header, key string) []model.Address {
	l, err := h.AddressList(key)
	if err != nil && len(l) == 0 {
		return nil
	}
	out := make([]model.Address, 0, len(l))
	for _, a := range l {
		out = append(out, model.Address{Name: a.Name, Addr: strings.ToLower(a.Address)})
	}
	return out
}

var refRe = regexp.MustCompile(`<([^<>\s]+)>`)

// Parsed is the result of parsing a raw message.
type Parsed struct {
	Msg model.Message
}

// Parse turns raw RFC 822 bytes into a Message (without account/mailbox/uid fields).
func Parse(raw []byte) (model.Message, error) {
	var m model.Message
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return m, fmt.Errorf("parse message: %w", err)
	}
	h := mr.Header
	m.Subject, _ = h.Subject()
	m.MessageID = cleanID(h.Get("Message-Id"))
	m.InReplyTo = cleanID(h.Get("In-Reply-To"))
	if i := strings.Index(m.InReplyTo, ">"); i > 0 {
		m.InReplyTo = m.InReplyTo[:i]
	}
	for _, r := range refRe.FindAllStringSubmatch(h.Get("References"), -1) {
		m.References = append(m.References, r[1])
	}
	if m.InReplyTo == "" {
		if r := refRe.FindStringSubmatch(h.Get("In-Reply-To")); r != nil {
			m.InReplyTo = r[1]
		}
	}
	if f := addrs(h, "From"); len(f) > 0 {
		m.From = f[0]
	}
	m.To, m.Cc, m.Bcc, m.ReplyTo = addrs(h, "To"), addrs(h, "Cc"), addrs(h, "Bcc"), addrs(h, "Reply-To")
	if d, err := h.Date(); err == nil {
		m.Date = d.Unix()
	}
	m.ListUnsub = strings.TrimSpace(h.Get("List-Unsubscribe"))
	m.ListUnsubPost = strings.Contains(strings.ToLower(h.Get("List-Unsubscribe-Post")), "one-click")
	m.AuthResults = strings.TrimSpace(h.Get("Authentication-Results"))

	var text, htm strings.Builder
	idx := 0
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) || message.IsUnknownEncoding(err) {
				continue
			}
			break
		}
		switch ph := p.Header.(type) {
		case *gomail.InlineHeader:
			ct, params, _ := ph.ContentType()
			body, _ := io.ReadAll(io.LimitReader(p.Body, 25<<20))
			switch {
			case ct == "text/plain":
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.Write(body)
			case ct == "text/html":
				if htm.Len() > 0 {
					htm.WriteString("\n")
				}
				htm.Write(body)
			default:
				name := params["name"]
				m.Attachments = append(m.Attachments, model.Attachment{Index: idx, Filename: name, ContentType: ct, Size: len(body), Inline: true, ContentID: cleanID(ph.Get("Content-Id"))})
				idx++
			}
		case *gomail.AttachmentHeader:
			ct, _, _ := ph.ContentType()
			name, _ := ph.Filename()
			body, _ := io.Copy(io.Discard, p.Body)
			m.Attachments = append(m.Attachments, model.Attachment{Index: idx, Filename: name, ContentType: ct, Size: int(body), ContentID: cleanID(ph.Get("Content-Id"))})
			idx++
		}
	}
	m.HTML = htm.String()
	m.Text = strings.ReplaceAll(text.String(), "\r\n", "\n")
	if strings.TrimSpace(m.Text) == "" && m.HTML != "" {
		m.Text = textutil.HTMLToText(m.HTML)
	}
	m.Text = textutil.StripInvisible(m.Text)
	m.Snippet = textutil.Snippet(m.Text, 180)
	for _, a := range m.Attachments {
		if !a.Inline || a.Filename != "" {
			m.HasAttach = true
		}
	}
	m.Size = int64(len(raw))
	return m, nil
}

// Attachment extracts attachment number idx (as numbered by Parse) from raw.
func Attachment(raw []byte, idx int) (model.Attachment, []byte, error) {
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return model.Attachment{}, nil, err
	}
	i := 0
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) || message.IsUnknownEncoding(err) {
				continue
			}
			return model.Attachment{}, nil, err
		}
		var a model.Attachment
		isAtt := false
		switch ph := p.Header.(type) {
		case *gomail.InlineHeader:
			ct, params, _ := ph.ContentType()
			if ct == "text/plain" || ct == "text/html" {
				continue
			}
			a = model.Attachment{ContentType: ct, Filename: params["name"], Inline: true}
			isAtt = true
		case *gomail.AttachmentHeader:
			ct, _, _ := ph.ContentType()
			name, _ := ph.Filename()
			a = model.Attachment{ContentType: ct, Filename: name}
			isAtt = true
		}
		if !isAtt {
			continue
		}
		if i == idx {
			data, err := io.ReadAll(p.Body)
			a.Index, a.Size = idx, len(data)
			return a, data, err
		}
		i++
	}
	return model.Attachment{}, nil, fmt.Errorf("attachment %d not found", idx)
}

// ---- building ----

type Outgoing struct {
	From        model.Address
	To, Cc, Bcc []model.Address
	Subject     string
	Text        string
	HTML        string
	InReplyTo   string
	References  []string
	Attachments []model.DraftAttachment
	Date        time.Time
	Headers     map[string]string
	Draft       bool // allow no recipients (work in progress)
}

func newID(domain string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	if domain == "" {
		domain = "openmail.local"
	}
	return hex.EncodeToString(b) + "@" + domain
}

func toGo(a []model.Address) []*gomail.Address {
	out := make([]*gomail.Address, 0, len(a))
	for _, x := range a {
		out = append(out, &gomail.Address{Name: x.Name, Address: x.Addr})
	}
	return out
}

// Recipients lists every envelope recipient (To, Cc, Bcc).
func (o Outgoing) Recipients() []string {
	var r []string
	for _, l := range [][]model.Address{o.To, o.Cc, o.Bcc} {
		for _, a := range l {
			r = append(r, a.Addr)
		}
	}
	return r
}

// Build renders the message to raw RFC 5322 bytes and returns its Message-ID.
func Build(o Outgoing) ([]byte, string, error) {
	if o.From.Addr == "" {
		return nil, "", fmt.Errorf("message has no From address")
	}
	if len(o.Recipients()) == 0 && !o.Draft {
		return nil, "", fmt.Errorf("message has no recipients")
	}
	for _, r := range o.Recipients() {
		if _, err := mail.ParseAddress(r); err != nil && !o.Draft {
			return nil, "", fmt.Errorf("invalid recipient address %q: %w", r, err)
		}
	}
	at := strings.LastIndex(o.From.Addr, "@")
	id := newID(o.From.Addr[at+1:])
	var h gomail.Header
	if o.Date.IsZero() {
		o.Date = time.Now()
	}
	h.SetDate(o.Date)
	h.SetSubject(o.Subject)
	h.SetAddressList("From", toGo([]model.Address{o.From}))
	if len(o.To) > 0 {
		h.SetAddressList("To", toGo(o.To))
	}
	if len(o.Cc) > 0 {
		h.SetAddressList("Cc", toGo(o.Cc))
	}
	h.Set("Message-Id", "<"+id+">")
	h.Set("X-Mailer", "OpenMail")
	if o.InReplyTo != "" {
		h.Set("In-Reply-To", "<"+cleanID(o.InReplyTo)+">")
		refs := append([]string{}, o.References...)
		if len(refs) == 0 || refs[len(refs)-1] != cleanID(o.InReplyTo) {
			refs = append(refs, cleanID(o.InReplyTo))
		}
		var rs []string
		for _, r := range refs {
			rs = append(rs, "<"+cleanID(r)+">")
		}
		h.Set("References", strings.Join(rs, " "))
	}
	for k, v := range o.Headers {
		h.Set(k, v)
	}
	htmlBody := o.HTML
	if htmlBody == "" {
		htmlBody = TextToHTML(o.Text)
	}
	var buf bytes.Buffer
	if len(o.Attachments) == 0 {
		w, err := gomail.CreateWriter(&buf, h)
		if err != nil {
			return nil, "", err
		}
		if err := writeAlt(w, o.Text, htmlBody); err != nil {
			return nil, "", err
		}
		if err := w.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), id, nil
	}
	w, err := gomail.CreateWriter(&buf, h)
	if err != nil {
		return nil, "", err
	}
	if err := writeAlt(w, o.Text, htmlBody); err != nil {
		return nil, "", err
	}
	for _, a := range o.Attachments {
		var ah gomail.AttachmentHeader
		ct := a.ContentType
		if ct == "" {
			ct = mime.TypeByExtension(strings.ToLower(extOf(a.Filename)))
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		ah.Set("Content-Type", ct)
		ah.SetFilename(a.Filename)
		aw, err := w.CreateAttachment(ah)
		if err != nil {
			return nil, "", err
		}
		if _, err := aw.Write(a.Data); err != nil {
			return nil, "", err
		}
		if err := aw.Close(); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), id, nil
}

func extOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i:]
	}
	return ""
}

func writeAlt(w *gomail.Writer, text, htm string) error {
	iw, err := w.CreateInline()
	if err != nil {
		return err
	}
	var th gomail.InlineHeader
	th.Set("Content-Type", "text/plain; charset=utf-8")
	tw, err := iw.CreatePart(th)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(tw, text); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	var hh gomail.InlineHeader
	hh.Set("Content-Type", "text/html; charset=utf-8")
	hw, err := iw.CreatePart(hh)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(hw, htm); err != nil {
		return err
	}
	if err := hw.Close(); err != nil {
		return err
	}
	return iw.Close()
}

var linkRe = regexp.MustCompile(`https?://[^\s<>"]+`)

// TextToHTML renders plain text as simple HTML: links, quotes and line breaks.
func TextToHTML(t string) string {
	var b strings.Builder
	b.WriteString(`<div style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;font-size:14px;line-height:1.5">`)
	inQuote := false
	for _, line := range strings.Split(strings.ReplaceAll(t, "\r\n", "\n"), "\n") {
		quoted := strings.HasPrefix(line, ">")
		if quoted && !inQuote {
			b.WriteString(`<blockquote style="margin:0 0 0 .5em;padding-left:1em;border-left:2px solid #bbb;color:#555">`)
			inQuote = true
		} else if !quoted && inQuote {
			b.WriteString("</blockquote>")
			inQuote = false
		}
		line = strings.TrimPrefix(strings.TrimPrefix(line, ">"), " ")
		esc := html.EscapeString(line)
		esc = linkRe.ReplaceAllStringFunc(esc, func(u string) string { return `<a href="` + u + `">` + u + `</a>` })
		b.WriteString(esc + "<br>\n")
	}
	if inQuote {
		b.WriteString("</blockquote>")
	}
	b.WriteString("</div>")
	return b.String()
}
