package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/michael-berardi/openmail/internal/compose"
	"github.com/michael-berardi/openmail/internal/imapx"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/textutil"
)

// IsAgent reports whether the actor is an automated client (MCP or other agent).
func IsAgent(actor string) bool {
	return strings.HasPrefix(actor, "mcp:") || strings.HasPrefix(actor, "agent:")
}

const (
	StateDraft   = "draft"
	StatePending = "pending_approval" // agent asked to send; a human must approve
	StateQueued  = "queued"
	StateSending = "sending"
	StateFailed  = "failed"
)

func parseAddrs(list []model.Address) error {
	for _, a := range list {
		if _, err := mail.ParseAddress(a.Addr); err != nil {
			return fmt.Errorf("invalid address %q", a.Addr)
		}
	}
	return nil
}

// SaveDraft creates or updates a draft.
func (a *App) SaveDraft(actor string, d model.Draft) (out model.Draft, err error) {
	defer func() {
		a.Store.Audit(actor, "draft.save", map[string]any{"id": out.ID, "to": d.To, "subject": d.Subject}, err)
	}()
	if d.AccountID == "" {
		d.AccountID = a.Setting("send.default_account")
		if d.AccountID == "" {
			accts, lerr := a.Store.ListAccounts()
			if lerr != nil || len(accts) == 0 {
				return d, errors.New("no accounts configured: add one with `openmail accounts add`")
			}
			d.AccountID = accts[0].ID
		}
	}
	if _, err = a.Store.GetAccount(d.AccountID); err != nil {
		return d, err
	}
	for _, l := range [][]model.Address{d.To, d.Cc, d.Bcc} {
		if err = parseAddrs(l); err != nil {
			return d, err
		}
	}
	// Callers (the REST API, MCP) may only supply content. Workflow fields are decided here: a
	// client can never create a draft that is already queued, approved or owned by someone else.
	author := "user"
	if IsAgent(actor) {
		author = actor
	}
	if d.ID != 0 {
		old, gerr := a.Store.GetDraft(d.ID)
		if gerr != nil {
			err = gerr
			return d, err
		}
		if old.State == StateSending || old.State == "sent" {
			err = fmt.Errorf("draft %d is already %s", d.ID, old.State)
			return d, err
		}
		// any edit invalidates a pending approval or queued send, atomically with respect to the outbox
		ok, terr := a.Store.TransitionDraft(d.ID, []string{StateDraft, StatePending, StateQueued, StateFailed}, StateDraft, 0, false, "")
		if terr != nil || !ok {
			if terr == nil {
				terr = fmt.Errorf("draft %d is already being sent", d.ID)
			}
			err = terr
			return d, err
		}
		author = old.Author
		if d.Attachments == nil {
			d.Attachments = old.Attachments
		}
		d.ServerUID, d.ServerBox, d.DraftKey = old.ServerUID, old.ServerBox, old.DraftKey
	} else {
		d.ServerUID, d.ServerBox, d.DraftKey = 0, "", ""
	}
	d.Author, d.State, d.Approved, d.SendAt, d.QueuedBy, d.Error = author, StateDraft, false, 0, "", ""
	if strings.TrimSpace(d.HTMLBody) != "" {
		// formatted drafts: keep only safe markup, and derive the plain-text alternative from it
		clean, _ := textutil.SanitizeHTML(d.HTMLBody, false)
		d.HTMLBody = clean
		d.Body = textutil.HTMLToText(clean)
	}
	if d.DraftKey == "" {
		d.DraftKey = newDraftKey()
	}
	if err = a.Store.SaveDraft(&d); err != nil {
		return d, err
	}
	if d.State == StateDraft || d.State == StatePending {
		a.scheduleDraftPush(d.ID, draftPushDelay)
	}
	a.emit(Event{Type: "outbox", Account: d.AccountID})
	return d, nil
}

func (a *App) DeleteDraft(actor string, id int64) (err error) {
	defer func() { a.Store.Audit(actor, "draft.delete", map[string]any{"id": id}, err) }()
	if old, gerr := a.Store.GetDraft(id); gerr == nil {
		a.deleteServerDraft(context.Background(), old)
	}
	err = a.Store.DeleteDraft(id)
	if err == nil {
		a.emit(Event{Type: "outbox"})
	}
	return err
}

// Reply builds a saved reply draft for a message.
func (a *App) Reply(actor string, msgID int64, replyAll bool, body string) (model.Draft, error) {
	return a.buildResponse(actor, msgID, replyAll, body, false)
}

// Forward builds a saved forward draft including attachments.
func (a *App) Forward(actor string, msgID int64, body string) (model.Draft, error) {
	return a.buildResponse(actor, msgID, false, body, true)
}

func prefix(subject, p string) string {
	s := strings.TrimSpace(subject)
	l := strings.ToLower(s)
	if strings.HasPrefix(l, strings.ToLower(p)) {
		return s
	}
	return p + " " + s
}

func (a *App) buildResponse(actor string, msgID int64, replyAll bool, body string, forward bool) (d model.Draft, err error) {
	defer func() {
		a.Store.Audit(actor, "draft.response", map[string]any{"message": msgID, "reply_all": replyAll, "forward": forward}, err)
	}()
	m, err := a.Store.GetMessage(msgID)
	if err != nil {
		return d, err
	}
	acct, err := a.Store.GetAccount(m.AccountID)
	if err != nil {
		return d, err
	}
	d = model.Draft{AccountID: acct.ID}
	who := m.From.Name
	if who == "" {
		who = m.From.Addr
	}
	quote := textutil.QuoteReply(who+" <"+m.From.Addr+">", m.Time().Format("Mon, 2 Jan 2006 at 15:04"), m.Text)
	hideBody := IsAgent(actor) && !a.SettingBool("agent.expose_bodies")
	if hideBody {
		quote = "" // an agent that may not read bodies must not receive them through a quoted draft either
	}
	sig := ""
	if strings.TrimSpace(acct.Signature) != "" {
		sig = "\n-- \n" + strings.TrimSpace(acct.Signature) + "\n"
	}
	if forward {
		d.Subject = prefix(m.Subject, "Fwd:")
		header := fmt.Sprintf("---------- Forwarded message ----------\nFrom: %s <%s>\nDate: %s\nSubject: %s\nTo: %s\n\n", m.From.Name, m.From.Addr, m.Time().Format(time.RFC1123Z), m.Subject, joinAddrs(m.To))
		d.Body = strings.TrimSpace(body) + "\n" + sig + "\n" + header + m.Text
		if hideBody {
			d.Body = strings.TrimSpace(body) + "\n" + sig
		}
		if raw, rerr := a.Store.RawMessage(m.ID); rerr == nil {
			for _, at := range m.Attachments {
				if at.Inline && at.Filename == "" {
					continue
				}
				if meta, data, aerr := compose.Attachment(raw, at.Index); aerr == nil {
					d.Attachments = append(d.Attachments, model.DraftAttachment{Filename: meta.Filename, ContentType: meta.ContentType, Size: len(data), Data: data})
				}
			}
		}
	} else {
		d.Subject = prefix(m.Subject, "Re:")
		d.InReplyTo = m.MessageID
		d.References = append(append([]string{}, m.References...), m.MessageID)
		d.ReplyToMsg = m.ID
		to := m.From
		if len(m.ReplyTo) > 0 {
			to = m.ReplyTo[0]
		}
		d.To = []model.Address{to}
		if replyAll {
			self := strings.ToLower(acct.Email)
			seen := map[string]bool{strings.ToLower(to.Addr): true, self: true}
			for _, x := range m.To {
				if !seen[strings.ToLower(x.Addr)] {
					seen[strings.ToLower(x.Addr)] = true
					d.To = append(d.To, x)
				}
			}
			for _, x := range m.Cc {
				if !seen[strings.ToLower(x.Addr)] {
					seen[strings.ToLower(x.Addr)] = true
					d.Cc = append(d.Cc, x)
				}
			}
		}
		d.Body = strings.TrimSpace(body) + "\n" + sig + "\n" + quote
		if strings.TrimSpace(body) == "" {
			d.Body = "\n" + sig + "\n" + quote
		}
	}
	d.Author = "user"
	if IsAgent(actor) {
		d.Author = actor
	}
	if d.DraftKey == "" {
		d.DraftKey = newDraftKey()
	}
	if err = a.Store.SaveDraft(&d); err != nil {
		return d, err
	}
	a.scheduleDraftPush(d.ID, draftPushDelay)
	a.emit(Event{Type: "outbox", Account: d.AccountID})
	return d, nil
}

func joinAddrs(l []model.Address) string {
	var p []string
	for _, a := range l {
		if a.Name != "" {
			p = append(p, a.Name+" <"+a.Addr+">")
		} else {
			p = append(p, a.Addr)
		}
	}
	return strings.Join(p, ", ")
}

// SendDraft queues a draft for delivery after the undo window. Agents are subject to the
// agent.send_policy setting; humans (CLI, web) are not.
func (a *App) SendDraft(actor string, id int64) (d model.Draft, err error) {
	defer func() { a.Store.Audit(actor, "draft.send", map[string]any{"id": id, "state": d.State}, err) }()
	d, err = a.Store.GetDraft(id)
	if err != nil {
		return d, err
	}
	if d.State == StateSending || d.State == "sent" {
		return d, fmt.Errorf("draft %d is already %s", id, d.State)
	}
	if len(d.To)+len(d.Cc)+len(d.Bcc) == 0 {
		return d, errors.New("draft has no recipients")
	}
	if IsAgent(actor) {
		switch a.Setting("agent.send_policy") {
		case "draft_only":
			return d, errors.New("sending by agents is disabled (agent.send_policy=draft_only). The draft is saved; ask the user to review and send it")
		case "approve":
			if !d.Approved {
				ok, terr := a.Store.TransitionDraft(id, []string{StateDraft, StatePending, StateFailed, StateQueued}, StatePending, 0, false, actor)
				if terr != nil || !ok {
					if terr == nil {
						terr = fmt.Errorf("draft %d is already being sent", id)
					}
					return d, terr
				}
				d.State, d.QueuedBy = StatePending, actor
				a.emit(Event{Type: "outbox", Account: d.AccountID, Detail: "approval"})
				return d, nil
			}
		}
		if err = a.checkAgentLimits(actor, d); err != nil {
			return d, err
		}
	} else {
		d.Approved = true
	}
	return a.queue(d, actor)
}

func (a *App) queue(d model.Draft, by string) (model.Draft, error) {
	delay := time.Duration(a.SettingInt("send.undo_seconds")) * time.Second
	sendAt := now().Add(delay).Unix()
	ok, err := a.Store.TransitionDraft(d.ID, []string{StateDraft, StatePending, StateFailed, StateQueued}, StateQueued, sendAt, d.Approved, by)
	if err != nil {
		return d, err
	}
	if !ok {
		return d, fmt.Errorf("draft %d is already being sent", d.ID)
	}
	d.State, d.SendAt, d.Error, d.QueuedBy = StateQueued, sendAt, "", by
	a.emit(Event{Type: "outbox", Account: d.AccountID, Detail: "queued"})
	return d, nil
}

func (a *App) checkAgentLimits(actor string, d model.Draft) error {
	if max := a.SettingInt("agent.max_sends_hour"); max > 0 {
		if n := a.Store.CountAudit("draft.sent", "mcp:", now().Add(-time.Hour).Unix()) + a.Store.CountAudit("draft.sent", "agent:", now().Add(-time.Hour).Unix()); n >= max {
			return fmt.Errorf("agent send limit reached (%d per hour, agent.max_sends_hour)", max)
		}
	}
	if allowed := strings.TrimSpace(a.Setting("agent.allowed_domains")); allowed != "" {
		ok := map[string]bool{}
		for _, dom := range strings.Split(allowed, ",") {
			ok[strings.ToLower(strings.TrimSpace(dom))] = true
		}
		for _, l := range [][]model.Address{d.To, d.Cc, d.Bcc} {
			for _, r := range l {
				at := strings.LastIndex(r.Addr, "@")
				if at < 0 || !ok[strings.ToLower(r.Addr[at+1:])] {
					return fmt.Errorf("recipient %s is outside agent.allowed_domains (%s)", r.Addr, allowed)
				}
			}
		}
	}
	return nil
}

// ApproveDraft is the human decision on an agent-requested send. Humans only.
func (a *App) ApproveDraft(actor string, id int64) (d model.Draft, err error) {
	defer func() { a.Store.Audit(actor, "draft.approve", map[string]any{"id": id}, err) }()
	if IsAgent(actor) {
		return d, errors.New("only a person can approve a send")
	}
	d, err = a.Store.GetDraft(id)
	if err != nil {
		return d, err
	}
	if d.State == StateSending || d.State == "sent" {
		return d, fmt.Errorf("draft %d is already %s", id, d.State)
	}
	d.Approved = true
	return a.queue(d, actor)
}

// RejectDraft returns a pending or queued draft to draft state.
func (a *App) CancelSend(actor string, id int64) (d model.Draft, err error) {
	defer func() { a.Store.Audit(actor, "draft.cancel", map[string]any{"id": id}, err) }()
	d, err = a.Store.GetDraft(id)
	if err != nil {
		return d, err
	}
	if d.State == StateSending || d.State == "sent" {
		return d, fmt.Errorf("draft %d is already %s and cannot be recalled", id, d.State)
	}
	ok, terr := a.Store.TransitionDraft(id, []string{StateDraft, StatePending, StateQueued, StateFailed}, StateDraft, 0, false, "")
	if terr != nil {
		return d, terr
	}
	if !ok {
		return d, fmt.Errorf("draft %d is already being sent and cannot be recalled", id)
	}
	if d, err = a.Store.GetDraft(id); err != nil {
		return d, err
	}
	a.scheduleDraftPush(d.ID, draftPushDelay)
	a.emit(Event{Type: "outbox", Account: d.AccountID})
	return d, nil
}

// SendNow queues a draft with no undo delay (used by CLI --now).
func (a *App) SendNow(ctx context.Context, actor string, id int64) error {
	d, err := a.SendDraft(actor, id)
	if err != nil {
		return err
	}
	if d.State != StateQueued {
		return fmt.Errorf("draft %d is %s, not queued: %s", id, d.State, d.Error)
	}
	return a.sendOne(ctx, id)
}

func (a *App) processOutbox(ctx context.Context) {
	ids, err := a.Store.DueDrafts(now().Unix())
	if err != nil {
		a.Store.Audit("system", "outbox.scan", nil, err)
		return
	}
	for _, id := range ids {
		if err := a.sendOne(ctx, id); err != nil {
			a.emit(Event{Type: "error", Detail: err.Error()})
		}
	}
}

// sendOne delivers a queued draft and files a copy in Sent.
func (a *App) sendOne(ctx context.Context, id int64) (err error) {
	claimed, err := a.Store.ClaimDraft(id)
	if err != nil || !claimed {
		return err
	}
	d, err := a.Store.GetDraft(id)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			d.State, d.Error = StateFailed, err.Error()
			_ = a.Store.SaveDraft(&d)
			a.Store.Audit(d.Author, "draft.failed", map[string]any{"id": id}, err)
			a.emit(Event{Type: "outbox", Account: d.AccountID, Detail: "failed"})
		}
	}()
	acct, err := a.Store.GetAccount(d.AccountID)
	if err != nil {
		return err
	}
	out := compose.Outgoing{
		From: model.Address{Name: acct.DisplayName, Addr: acct.Email},
		To:   d.To, Cc: d.Cc, Bcc: d.Bcc, Subject: d.Subject, Text: d.Body, HTML: d.HTMLBody,
		InReplyTo: d.InReplyTo, References: d.References, Attachments: d.Attachments,
	}
	raw, _, err := compose.Build(out)
	if err != nil {
		return err
	}
	cr, err := a.Creds(ctx, acct)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err = imapx.Send(sctx, acct, cr, acct.Email, out.Recipients(), raw); err != nil {
		return err
	}
	// The message is out. Anything below must not mark the send as failed.
	actor := d.QueuedBy // whoever put it in the outbox is accountable for the send and counts toward limits
	if !IsAgent(actor) {
		actor = "user"
	}
	a.Store.Audit(actor, "draft.sent", map[string]any{"id": id, "to": d.To, "subject": d.Subject}, nil)
	for _, r := range append(append([]model.Address{}, d.To...), d.Cc...) {
		_ = a.Store.TouchContact(r, now().Unix())
	}
	if !acct.SavesSent {
		if sent, merr := a.Store.FindMailbox(acct.ID, model.RoleSent); merr == nil {
			q := queuedOp{Dest: sent.Name, Raw: raw, Flags: []string{`\Seen`}}
			aerr := a.withConn(ctx, acct, func(c *imapx.Conn) error { return a.applyServerOp(c, "append", q, nil) })
			if aerr != nil {
				if qerr := a.Store.EnqueueOp(acct.ID, "append", q); qerr != nil {
					a.Store.Audit("system", "sent.copy", map[string]any{"id": id}, qerr)
				}
			}
		}
	}
	if d.ReplyToMsg != 0 {
		_ = a.markAnswered(ctx, d.ReplyToMsg)
	}
	a.deleteServerDraft(ctx, d)
	if derr := a.Store.DeleteDraft(id); derr != nil {
		a.Store.Audit("system", "draft.cleanup", map[string]any{"id": id}, derr)
	}
	a.emit(Event{Type: "outbox", Account: d.AccountID, Detail: "sent"})
	a.Refresh(acct.ID)
	return nil
}

func (a *App) markAnswered(ctx context.Context, id int64) error {
	m, err := a.Store.GetMessage(id)
	if err != nil {
		return err
	}
	acct, err := a.Store.GetAccount(m.AccountID)
	if err != nil {
		return err
	}
	mb, err := a.Store.GetMailbox(m.MailboxID)
	if err != nil {
		return err
	}
	q := queuedOp{Mailbox: mb.Name, UIDs: []uint32{m.UID}, Add: []string{`\Answered`}}
	if err := a.serverOrQueue(ctx, acct, "flags", q, nil); err != nil {
		return err
	}
	return a.Store.SetFlags(m.ID, m.Seen, m.Flagged, true, m.Draft, m.Deleted, m.Keywords)
}

// ---- unsubscribe ----

// UnsubscribeResult reports what was done.
type UnsubscribeResult struct {
	Method string `json:"method"` // one-click | mailto | link
	URL    string `json:"url,omitempty"`
	Done   bool   `json:"done"`
	Detail string `json:"detail,omitempty"`
}

// Unsubscribe uses the sender's List-Unsubscribe header: RFC 8058 one-click when offered,
// otherwise a mailto request, otherwise it returns the link for a person to open.
func (a *App) Unsubscribe(ctx context.Context, actor string, msgID int64) (res UnsubscribeResult, err error) {
	defer func() {
		a.Store.Audit(actor, "message.unsubscribe", map[string]any{"id": msgID, "method": res.Method}, err)
	}()
	m, err := a.Store.GetMessage(msgID)
	if err != nil {
		return res, err
	}
	if m.ListUnsub == "" {
		return res, errors.New("this message has no List-Unsubscribe header")
	}
	var https, mailto string
	for _, part := range strings.Split(m.ListUnsub, ",") {
		p := strings.Trim(strings.TrimSpace(part), "<>")
		switch {
		case strings.HasPrefix(p, "https://") && https == "":
			https = p
		case strings.HasPrefix(p, "mailto:") && mailto == "":
			mailto = p
		}
	}
	// The target comes from the sender, so an agent only acts on it when the user allows agents to
	// send; otherwise it gets the link and tells the user.
	if IsAgent(actor) && a.Setting("agent.send_policy") != "allow" {
		method, target := "link", https
		if https == "" {
			method, target = "mailto", mailto
		}
		return UnsubscribeResult{Method: method, URL: target, Done: false, Detail: "agents may not unsubscribe on their own under the current send policy; ask the user"}, nil
	}
	if https != "" && m.ListUnsubPost {
		req, rerr := http.NewRequestWithContext(ctx, "POST", https, strings.NewReader("List-Unsubscribe=One-Click"))
		if rerr != nil {
			return res, rerr
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, derr := publicHTTPClient().Do(req)
		if derr != nil {
			return res, fmt.Errorf("one-click unsubscribe request: %w", derr)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return res, fmt.Errorf("one-click unsubscribe refused: HTTP %d", resp.StatusCode)
		}
		return UnsubscribeResult{Method: "one-click", URL: https, Done: true}, nil
	}
	if mailto != "" {
		u, perr := url.Parse(mailto)
		if perr != nil {
			return res, fmt.Errorf("bad mailto link: %w", perr)
		}
		to := []model.Address{{Addr: strings.ToLower(strings.TrimSpace(u.Opaque))}}
		if err = parseAddrs(to); err != nil || strings.ContainsAny(to[0].Addr, ",;") {
			return res, fmt.Errorf("unsubscribe address %q is not a single valid address", u.Opaque)
		}
		subj := u.Query().Get("subject")
		if subj == "" {
			subj = "unsubscribe"
		}
		d := model.Draft{AccountID: m.AccountID, To: to, Subject: subj, Body: u.Query().Get("body")}
		if d.Body == "" {
			d.Body = "unsubscribe"
		}
		saved, serr := a.SaveDraft(actor, d)
		if serr != nil {
			return res, serr
		}
		// goes through the normal send path, so policy, limits and the undo window all apply
		q, qerr := a.SendDraft(actor, saved.ID)
		if qerr != nil {
			return res, qerr
		}
		detail := "unsubscribe email queued"
		if q.State == StatePending {
			detail = "unsubscribe email waiting for approval"
		}
		return UnsubscribeResult{Method: "mailto", URL: mailto, Done: q.State == StateQueued, Detail: detail}, nil
	}
	return UnsubscribeResult{Method: "link", URL: https, Done: false, Detail: "open this link to unsubscribe"}, nil
}

// publicHTTPClient makes requests only to public addresses, so a link inside an email cannot be
// used to reach services on this machine or its network. The check runs at connect time, which
// also covers DNS rebinding and redirects.
func publicHTTPClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("refusing to connect to non-public address %s", host)
		}
		return nil
	}}
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: d.DialContext, Proxy: nil},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Scheme != "https" {
				return errors.New("unsubscribe redirects must stay on https and are limited to 3")
			}
			return nil
		}}
}
