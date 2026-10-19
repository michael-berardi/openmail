package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/michael-berardi/openmail/internal/compose"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/query"
	"github.com/michael-berardi/openmail/internal/rules"
	"github.com/michael-berardi/openmail/internal/store"
	"github.com/michael-berardi/openmail/internal/textutil"
)

// ListOptions control paging and scope for list and search calls.
type ListOptions struct {
	Limit       int
	Offset      int
	IncludeJunk bool
	Oldest      bool
}

func (o ListOptions) store() store.FindOptions {
	return store.FindOptions{Limit: o.Limit, Offset: o.Offset, IncludeJunk: o.IncludeJunk, Oldest: o.Oldest, ExcludeSnooze: true}
}

// ParseQuery parses search text. Empty text means "everything in the inbox".
func ParseQuery(s string) (query.Query, error) {
	return query.Parse(s, now())
}

// Search returns messages matching the query string.
func (a *App) Search(qs string, o ListOptions) ([]model.Message, error) {
	q, err := ParseQuery(qs)
	if err != nil {
		return nil, err
	}
	fo := o.store()
	if q.Snoozed {
		fo.ExcludeSnooze = false
	}
	msgs, err := a.Store.Find(q, fo)
	if err != nil {
		return nil, err
	}
	a.markVIP(msgs)
	return msgs, nil
}

// Threads returns conversations matching the query string.
func (a *App) Threads(qs string, o ListOptions) ([]store.ThreadSummary, error) {
	q, err := ParseQuery(qs)
	if err != nil {
		return nil, err
	}
	fo := o.store()
	if q.Snoozed {
		fo.ExcludeSnooze = false
	}
	ts, err := a.Store.Threads(q, fo)
	if err != nil {
		return nil, err
	}
	for i := range ts {
		a.markVIP([]model.Message{ts[i].Latest})
	}
	return ts, nil
}

func (a *App) markVIP(msgs []model.Message) {
	vips, err := a.Store.VIPs()
	if err != nil || len(vips) == 0 {
		return
	}
	set := map[string]bool{}
	for _, v := range vips {
		set[v.Addr] = true
	}
	for i := range msgs {
		msgs[i].VIP = set[strings.ToLower(msgs[i].From.Addr)]
	}
}

func (a *App) GetMessage(id int64) (model.Message, error) {
	m, err := a.Store.GetMessage(id)
	if err != nil {
		return m, err
	}
	msgs := []model.Message{m}
	a.markVIP(msgs)
	return msgs[0], nil
}

func (a *App) GetThread(threadID string) ([]model.Message, error) {
	msgs, err := a.Store.ThreadMessages(threadID)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("thread %s: %w", threadID, store.ErrNotFound)
	}
	a.markVIP(msgs)
	return msgs, nil
}

var cidRe = regexp.MustCompile(`(?i)cid:([^"'\s)>]+)`)

// RenderHTML returns safe HTML for display. Remote images are blocked unless allowed by
// the privacy setting, a per-sender allowance, or allowRemote. Inline cid: images are
// embedded. hadRemote tells the UI whether to offer "load remote content".
func (a *App) RenderHTML(id int64, allowRemote bool) (htmlOut string, hadRemote, blocked bool, err error) {
	m, err := a.Store.GetMessage(id)
	if err != nil {
		return "", false, false, err
	}
	src := m.HTML
	if strings.TrimSpace(src) == "" {
		src = compose.TextToHTML(m.Text)
	}
	allow := allowRemote || a.Setting("privacy.remote_content") == "allow" || a.Store.RemoteAllowed(m.From.Addr)
	out, had := textutil.SanitizeHTML(src, !allow)
	if cidRe.MatchString(out) {
		if raw, rerr := a.Store.RawMessage(id); rerr == nil {
			for _, at := range m.Attachments {
				if at.ContentID == "" || !strings.HasPrefix(at.ContentType, "image/") || at.Size > 8<<20 {
					continue
				}
				if _, data, aerr := compose.Attachment(raw, at.Index); aerr == nil {
					uri := "data:" + at.ContentType + ";base64," + base64.StdEncoding.EncodeToString(data)
					out = strings.ReplaceAll(out, "cid:"+at.ContentID, uri)
				}
			}
		}
	}
	return out, had, had && !allow, nil
}

// Attachment returns attachment idx of a message.
func (a *App) Attachment(msgID int64, idx int) (model.Attachment, []byte, error) {
	raw, err := a.Store.RawMessage(msgID)
	if err != nil {
		return model.Attachment{}, nil, err
	}
	return compose.Attachment(raw, idx)
}

// Overview summarises accounts and mailboxes for sidebars and agents.
type Overview struct {
	Accounts []AccountView   `json:"accounts"`
	Statuses []AccountStatus `json:"status"`
	Pending  int             `json:"pending_ops"`
	Outbox   int             `json:"outbox"`
	NeedsOK  int             `json:"needs_approval"`
}

type AccountView struct {
	model.Account
	Mailboxes []model.Mailbox `json:"mailboxes"`
	HasSecret bool            `json:"-"`
}

func (a *App) Overview() (Overview, error) {
	ov := Overview{Accounts: []AccountView{}}
	accts, err := a.Store.ListAccounts()
	if err != nil {
		return ov, err
	}
	for _, acct := range accts {
		mbs, err := a.Store.ListMailboxes(acct.ID)
		if err != nil {
			return ov, err
		}
		if mbs == nil {
			mbs = []model.Mailbox{}
		}
		ov.Accounts = append(ov.Accounts, AccountView{Account: acct, Mailboxes: mbs})
	}
	ov.Statuses = a.Statuses()
	ov.Pending, _ = a.Store.CountPendingOps()
	if ds, err := a.Store.ListDrafts(""); err == nil {
		for _, d := range ds {
			switch d.State {
			case StateQueued, StateSending, StateFailed:
				ov.Outbox++
			case StatePending:
				ov.NeedsOK++
			}
		}
	}
	return ov, nil
}

// ---- rules ----

func (a *App) SaveRule(actor string, r model.Rule) (out model.Rule, err error) {
	defer func() { a.Store.Audit(actor, "rule.save", r, err) }()
	if err = rules.Validate(r); err != nil {
		return r, err
	}
	if IsAgent(actor) && r.Source == "" {
		r.Source = "agent"
	}
	for _, act := range r.Actions {
		if act.Kind == "delete" && IsAgent(actor) {
			err = errors.New("agents cannot create rules that delete mail; use archive or move to Trash for review")
			return r, err
		}
	}
	err = a.Store.SaveRule(&r)
	return r, err
}

func (a *App) DeleteRule(actor string, id int64) (err error) {
	defer func() { a.Store.Audit(actor, "rule.delete", map[string]any{"id": id}, err) }()
	return a.Store.DeleteRule(id)
}

// RunRules applies the enabled rules to existing mail matching a query; dryRun only counts.
func (a *App) RunRules(ctx context.Context, actor, qs string, dryRun bool) (matched int, err error) {
	defer func() {
		a.Store.Audit(actor, "rule.run", map[string]any{"query": qs, "dry_run": dryRun, "matched": matched}, err)
	}()
	q, err := ParseQuery(qs)
	if err != nil {
		return 0, err
	}
	rs, err := a.Store.ListRules()
	if err != nil {
		return 0, err
	}
	msgs, err := a.Store.Find(q, store.FindOptions{Limit: 500, IncludeJunk: false})
	if err != nil {
		return 0, err
	}
	for _, m := range msgs {
		raw, _ := a.Store.RawMessage(m.ID)
		full, gerr := a.Store.GetMessage(m.ID)
		if gerr != nil {
			continue
		}
		acts := rules.Evaluate(rs, &rules.Input{Msg: full, Raw: raw})
		if len(acts) == 0 {
			continue
		}
		matched++
		if !dryRun {
			if err = a.applyRuleActions(ctx, full, acts); err != nil {
				return matched, err
			}
		}
	}
	return matched, nil
}

// ---- tokens ----

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// CreateToken makes a bearer token for the web API and MCP over HTTP. The plaintext is
// returned once; only its hash is stored.
func (a *App) CreateToken(actor, name, scope string) (string, error) {
	if !model.ScopeAllows(scope, "read") {
		return "", fmt.Errorf("scope must be one of read, write, send, admin")
	}
	if strings.TrimSpace(name) == "" {
		return "", errors.New("token needs a name")
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := "om_" + hex.EncodeToString(b)
	err := a.Store.CreateToken(name, hashToken(tok), scope)
	a.Store.Audit(actor, "token.create", map[string]string{"name": name, "scope": scope}, err)
	if err != nil {
		return "", err
	}
	return tok, nil
}

func (a *App) RevokeToken(actor, name string) error {
	err := a.Store.DeleteToken(name)
	a.Store.Audit(actor, "token.revoke", map[string]string{"name": name}, err)
	return err
}

// Authenticate resolves a bearer token.
func (a *App) Authenticate(token string) (model.Token, error) {
	if !strings.HasPrefix(token, "om_") {
		return model.Token{}, store.ErrNotFound
	}
	return a.Store.TokenByHash(hashToken(token))
}

// EnsureLocalToken issues a fresh admin token for this run of the app and invalidates the previous
// one. Only its hash is stored; the plaintext exists in this process and in the URL it opens.
func (a *App) EnsureLocalToken() (string, error) {
	_ = a.Store.DeleteSetting("_web_token") // earlier versions kept the plaintext in the database
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := "om_" + hex.EncodeToString(b)
	if err := a.Store.ReplaceToken("web-ui", hashToken(tok), "admin"); err != nil {
		return "", err
	}
	return tok, nil
}

var _ = time.Now
