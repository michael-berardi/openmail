package core

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/michael-berardi/openmail/internal/compose"
	"github.com/michael-berardi/openmail/internal/imapx"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/rules"
	"github.com/michael-berardi/openmail/internal/store"
)

// ErrEngineBusy means another OpenMail process already runs the sync engine on this data.
var ErrEngineBusy = errors.New("another OpenMail process is already syncing this mailbox")

// Run starts the sync engine: one goroutine per enabled account (IMAP IDLE for push),
// plus a ticker for snoozes and the outbox. It blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	release, ok, err := a.acquireEngineLock()
	if err != nil {
		return fmt.Errorf("engine lock: %w", err)
	}
	if !ok {
		return ErrEngineBusy
	}
	defer release()
	a.engineMu.Lock()
	if a.engineCtx != nil {
		a.engineMu.Unlock()
		return errors.New("engine already running")
	}
	a.engineCtx = ctx
	a.engineMu.Unlock()
	accts, err := a.Store.ListAccounts()
	if err != nil {
		return err
	}
	if n, ferr := a.Store.FailStuckDrafts(); ferr == nil && n > 0 {
		a.Store.Audit("system", "draft.recovered", map[string]int{"drafts": n}, nil)
	}
	for _, acct := range accts {
		if acct.Enabled {
			a.startAccount(acct)
		}
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.engineMu.Lock()
			a.engineCtx = nil
			for _, c := range a.running {
				c()
			}
			a.engineMu.Unlock()
			return nil
		case <-t.C:
			a.tick(ctx)
		}
	}
}

func (a *App) startAccount(acct model.Account) {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	if a.engineCtx == nil || !acct.Enabled {
		return
	}
	if c, ok := a.running[acct.ID]; ok {
		c()
	}
	ctx, cancel := context.WithCancel(a.engineCtx)
	a.running[acct.ID] = cancel
	go a.accountLoop(ctx, acct.ID)
}

func (a *App) stopAccount(id string) {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	if c, ok := a.running[id]; ok {
		c()
		delete(a.running, id)
	}
	a.mu.Lock()
	delete(a.status, id)
	a.mu.Unlock()
}

func (a *App) accountLoop(ctx context.Context, id string) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		acct, err := a.Store.GetAccount(id)
		if err != nil {
			return
		}
		start := time.Now()
		err = a.session(ctx, acct)
		if ctx.Err() != nil {
			return
		}
		msg := "session ended"
		if err != nil {
			msg = err.Error()
		}
		a.setStatus(id, func(s *AccountStatus) { s.State, s.Error = "error", msg })
		a.emit(Event{Type: "error", Account: id, Detail: msg})
		if time.Since(start) > 2*time.Minute {
			backoff = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
}

// session holds one IMAP connection: full sync, then IDLE until something happens.
func (a *App) session(ctx context.Context, acct model.Account) error {
	a.setStatus(acct.ID, func(s *AccountStatus) { s.State, s.Error = "connecting", "" })
	c, err := a.Dial(ctx, acct)
	if err != nil {
		return err
	}
	defer c.Close()
	lastFull := time.Time{}
	poll := time.Duration(a.SettingInt("sync.poll_seconds")) * time.Second
	if poll < 30*time.Second {
		poll = 30 * time.Second
	}
	canIdle := c.Caps().Has(imap.CapIdle)
	for ctx.Err() == nil {
		a.setStatus(acct.ID, func(s *AccountStatus) { s.State = "syncing" })
		if err := a.drainOps(ctx, acct, c); err != nil {
			return err
		}
		if time.Since(lastFull) >= poll {
			if err := a.syncAccountConn(ctx, acct, c, false); err != nil {
				return err
			}
			lastFull = time.Now()
		} else if err := a.syncInbox(ctx, acct, c); err != nil {
			return err
		}
		a.setStatus(acct.ID, func(s *AccountStatus) { s.State, s.LastSync, s.Error, s.Progress = "idle", time.Now().Unix(), "", "" })
		wait := poll
		if canIdle {
			if wait > 9*time.Minute {
				wait = 9 * time.Minute
			}
			if err := a.idleInbox(ctx, c, wait, a.kickChan(acct.ID)); err != nil {
				return err
			}
		} else {
			if wait > 60*time.Second {
				wait = 60 * time.Second
			}
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			case <-a.kickChan(acct.ID):
			}
		}
	}
	return nil
}

func (a *App) idleInbox(ctx context.Context, c *imapx.Conn, d time.Duration, kick <-chan struct{}) error {
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	idle, err := c.Idle()
	if err != nil {
		return fmt.Errorf("start IDLE: %w", err)
	}
	// drain stale signals from the sync we just did
	select {
	case <-c.Changes:
	default:
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	case <-c.Changes:
	case <-kick:
	case <-c.Closed():
	}
	if err := idle.Close(); err != nil {
		return fmt.Errorf("stop IDLE: %w", err)
	}
	return idle.Wait()
}

func (a *App) kickChan(id string) <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kicks == nil {
		a.kicks = map[string]chan struct{}{}
	}
	ch, ok := a.kicks[id]
	if !ok {
		ch = make(chan struct{}, 1)
		a.kicks[id] = ch
	}
	return ch
}

// SyncNow runs a one-shot full sync for one account (or all when id is empty) and returns
// when finished. Used by `openmail sync` and the refresh button.
func (a *App) SyncNow(ctx context.Context, id string, full bool) error {
	accts, err := a.Store.ListAccounts()
	if err != nil {
		return err
	}
	var errs []string
	for _, acct := range accts {
		if (id != "" && !strings.EqualFold(acct.ID, id)) || !acct.Enabled {
			continue
		}
		c, err := a.Dial(ctx, acct)
		if err != nil {
			errs = append(errs, acct.ID+": "+err.Error())
			a.setStatus(acct.ID, func(s *AccountStatus) { s.State, s.Error = "error", err.Error() })
			continue
		}
		err = a.drainOps(ctx, acct, c)
		if err == nil {
			err = a.syncAccountConn(ctx, acct, c, full)
		}
		c.Close()
		if err != nil {
			errs = append(errs, acct.ID+": "+err.Error())
			a.setStatus(acct.ID, func(s *AccountStatus) { s.State, s.Error = "error", err.Error() })
			continue
		}
		a.setStatus(acct.ID, func(s *AccountStatus) { s.State, s.LastSync, s.Error, s.Progress = "idle", time.Now().Unix(), "", "" })
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Refresh asks the running engine to sync now (no-op outside serve mode).
func (a *App) Refresh(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, ch := range a.kicks {
		if id == "" || k == id {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

func (a *App) syncWindow(acct model.Account) int {
	if acct.SyncDays > 0 {
		return acct.SyncDays
	}
	return a.SettingInt("sync.days")
}

// syncAccountConn lists mailboxes and syncs each one that is enabled for sync.
func (a *App) syncAccountConn(ctx context.Context, acct model.Account, c *imapx.Conn, full bool) error {
	opts := &imap.ListOptions{}
	if c.Caps().Has(imap.CapSpecialUse) {
		opts.ReturnSpecialUse = true
	}
	list, err := c.List("", "*", opts).Collect()
	if err != nil {
		return fmt.Errorf("list mailboxes: %w", err)
	}
	seen := map[string]bool{}
	type cand struct {
		d        *imap.ListData
		role     string
		explicit bool
	}
	var cands []cand
	for _, d := range list {
		if !imapx.Selectable(d) {
			continue
		}
		cands = append(cands, cand{d: d, role: imapx.Role(d), explicit: imapx.RoleAttr(d) != ""})
	}
	// A role belongs to one folder: servers like Zoho carry both "Sent" and "Sent Messages". The one the
	// server declared wins; otherwise the shortest name. The others stay plain folders.
	best := map[string]int{}
	for i, c := range cands {
		if c.role == "" || c.role == model.RoleInbox {
			continue
		}
		j, ok := best[c.role]
		if !ok || (c.explicit && !cands[j].explicit) || (c.explicit == cands[j].explicit && len(c.d.Mailbox) < len(cands[j].d.Mailbox)) {
			best[c.role] = i
		}
	}
	var mbs []model.Mailbox
	for i, c := range cands {
		d, role := c.d, c.role
		if role != "" && role != model.RoleInbox && best[role] != i {
			role = ""
		}
		mb := model.Mailbox{AccountID: acct.ID, Name: d.Mailbox, Delim: string(d.Delim), Role: role, Subscribed: true}
		for _, at := range d.Attrs {
			if at == imap.MailboxAttrFlagged || at == imap.MailboxAttrImportant {
				mb.Subscribed = false
			}
		}
		if err := a.Store.UpsertMailbox(&mb); err != nil {
			return err
		}
		seen[d.Mailbox] = true
		mbs = append(mbs, mb)
	}
	local, _ := a.Store.ListMailboxes(acct.ID)
	for _, l := range local {
		if !seen[l.Name] {
			_ = a.Store.DeleteMailbox(l.ID)
		}
	}
	rank := func(m model.Mailbox) int {
		switch m.Role {
		case model.RoleInbox:
			return 0
		case model.RoleSent:
			return 1
		case model.RoleDrafts:
			return 2
		case model.RoleJunk, model.RoleTrash:
			return 8
		}
		return 5
	}
	sort.SliceStable(mbs, func(i, j int) bool { return rank(mbs[i]) < rank(mbs[j]) })
	var failed []string
	for _, mb := range mbs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cur, err := a.Store.GetMailbox(mb.ID)
		if err != nil || !cur.Subscribed {
			continue
		}
		a.setStatus(acct.ID, func(s *AccountStatus) { s.State, s.Progress = "syncing", mb.Name })
		if err := a.syncMailbox(ctx, acct, c, cur, full); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// One broken folder must not stop the rest; report it at the end.
			failed = append(failed, fmt.Sprintf("%s: %v", mb.Name, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("sync: %s", strings.Join(failed, "; "))
	}
	return nil
}

func (a *App) syncInbox(ctx context.Context, acct model.Account, c *imapx.Conn) error {
	mb, err := a.Store.FindMailbox(acct.ID, model.RoleInbox)
	if err != nil {
		return a.syncAccountConn(ctx, acct, c, false)
	}
	return a.syncMailbox(ctx, acct, c, mb, false)
}

func toFlags(fl []imap.Flag) (seen, flagged, answered, draft, deleted bool, kw []string) {
	for _, f := range fl {
		switch f {
		case imap.FlagSeen:
			seen = true
		case imap.FlagFlagged:
			flagged = true
		case imap.FlagAnswered:
			answered = true
		case imap.FlagDraft:
			draft = true
		case imap.FlagDeleted:
			deleted = true
		default:
			if !strings.HasPrefix(string(f), "\\") {
				kw = append(kw, string(f))
			}
		}
	}
	return
}

// syncMailbox brings the local copy of one mailbox in line with the server.
//
// With CONDSTORE it is incremental: nothing changed means no traffic beyond SELECT; pure
// arrivals fetch only the new UIDs and the flags that changed since the last modseq.
// Otherwise (or on servers without CONDSTORE) it compares every UID and flag.
func (a *App) syncMailbox(ctx context.Context, acct model.Account, c *imapx.Conn, mb model.Mailbox, full bool) error {
	cond := c.Caps().Has(imap.CapCondStore)
	sel, err := c.Select(mb.Name, &imap.SelectOptions{ReadOnly: true, CondStore: cond}).Wait()
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	cond = cond && sel.HighestModSeq > 0 // a server that advertises CONDSTORE but reports no modseq gets the safe path
	exists, uidNext := int(sel.NumMessages), uint32(sel.UIDNext)
	if mb.UIDValidity != 0 && mb.UIDValidity != sel.UIDValidity {
		if err := a.Store.ClearMailbox(mb.ID); err != nil {
			return err
		}
		mb.LastUID, mb.UIDNext, mb.Exists, mb.ModSeq = 0, 0, 0, 0
	}
	done := func(lastUID uint32) error {
		if err := a.Store.SetMailboxSync(mb.ID, sel.UIDValidity, uidNext, lastUID, exists, sel.HighestModSeq); err != nil {
			return err
		}
		return a.Store.RefreshCounts(mb.ID)
	}
	if exists == 0 {
		if err := a.Store.ClearMailbox(mb.ID); err != nil {
			return err
		}
		return done(0)
	}
	// locally assigned UIDs (after an offline move) are only replaced by a full comparison
	plan := planSync(mb, sel.UIDValidity, exists, uidNext, sel.HighestModSeq, cond, full || a.Store.HasPlaceholderUID(mb.ID))
	if plan == planSkip {
		return nil // nothing changed
	}
	arrivals := exists - mb.Exists
	appendsOnly := plan == planAppends

	var fresh []uint32
	sizes := map[uint32]int64{}
	changed := false
	maxUID := mb.LastUID
	if appendsOnly {
		if sel.HighestModSeq != mb.ModSeq {
			var all imap.UIDSet
			all.AddRange(1, 0)
			buf, err := c.Fetch(all, &imap.FetchOptions{UID: true, Flags: true, ModSeq: true, ChangedSince: mb.ModSeq}).Collect()
			if err != nil {
				return fmt.Errorf("fetch changed flags: %w", err)
			}
			for _, m := range buf {
				seen, flagged, answered, draft, deleted, kw := toFlags(m.Flags)
				if err := a.Store.SetFlagsByUID(mb.ID, uint32(m.UID), seen, flagged, answered, draft, deleted, kw); err != nil {
					return err
				}
				changed = true
			}
		}
		if arrivals > 0 {
			var set imap.UIDSet
			set.AddRange(imap.UID(mb.LastUID+1), 0)
			buf, err := c.Fetch(set, &imap.FetchOptions{UID: true, RFC822Size: true}).Collect()
			if err != nil {
				return fmt.Errorf("fetch new uids: %w", err)
			}
			for _, m := range buf {
				u := uint32(m.UID)
				if u > mb.LastUID && !a.Store.HasUID(mb.ID, u) {
					fresh = append(fresh, u)
					sizes[u] = m.RFC822Size
				}
				if u > maxUID {
					maxUID = u
				}
			}
		}
	} else {
		// full comparison: every server UID with its flags and size
		var all imap.UIDSet
		all.AddRange(1, 0)
		buf, err := c.Fetch(all, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true}).Collect()
		if err != nil {
			return fmt.Errorf("fetch flags: %w", err)
		}
		server := make(map[uint32][]imap.Flag, len(buf))
		for _, m := range buf {
			server[uint32(m.UID)] = m.Flags
			sizes[uint32(m.UID)] = m.RFC822Size
			if uint32(m.UID) > maxUID {
				maxUID = uint32(m.UID)
			}
		}
		local, err := a.Store.UIDsInMailbox(mb.ID)
		if err != nil {
			return err
		}
		for uid, id := range local {
			fl, ok := server[uid]
			if !ok {
				if err := a.Store.DeleteMessage(id); err != nil {
					return err
				}
				changed = true
				continue
			}
			seen, flagged, answered, draft, deleted, kw := toFlags(fl)
			if err := a.Store.SetFlags(id, seen, flagged, answered, draft, deleted, kw); err != nil {
				return err
			}
		}
		var want []uint32
		if mb.LastUID == 0 {
			if window := a.syncWindow(acct); window > 0 {
				crit := &imap.SearchCriteria{Since: time.Now().AddDate(0, 0, -window)}
				sd, err := c.UIDSearch(crit, nil).Wait()
				if err != nil {
					return fmt.Errorf("search recent messages: %w", err)
				}
				for _, u := range sd.AllUIDs() {
					want = append(want, uint32(u))
				}
			} else {
				for u := range server {
					want = append(want, u)
				}
			}
		} else {
			for u := range server {
				if u > mb.LastUID {
					want = append(want, u)
				}
			}
		}
		for _, u := range want {
			if _, have := local[u]; !have {
				fresh = append(fresh, u)
			}
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i] > fresh[j] }) // newest first
	if max := a.SettingInt("sync.max_per_mailbox"); max > 0 && len(fresh) > max {
		fresh = fresh[:max]
	}
	newCount, err := a.fetchAndStore(ctx, acct, c, mb, fresh, sizes, true)
	if err != nil {
		return err
	}
	if err := done(maxUID); err != nil {
		return err
	}
	if newCount > 0 || changed {
		a.emit(Event{Type: "message", Account: acct.ID, Detail: mb.Name})
	}
	return nil
}

// fetchAndStore downloads messages in batches and ingests them. applyRules is true for
// freshly arrived mail, false when loading history. Messages above sync.max_message_mb are
// stored as headers only and can be downloaded on demand.
func (a *App) fetchAndStore(ctx context.Context, acct model.Account, c *imapx.Conn, mb model.Mailbox, uids []uint32, sizes map[uint32]int64, applyRules bool) (int, error) {
	const batch = 25
	limit := int64(a.SettingInt("sync.max_message_mb")) << 20
	var small, large []uint32
	for _, u := range uids {
		if limit > 0 && sizes[u] > limit {
			large = append(large, u)
		} else {
			small = append(small, u)
		}
	}
	ruleSet, _ := a.Store.ListRules()
	stored := 0
	section := &imap.FetchItemBodySection{Peek: true}
	for i := 0; i < len(small); i += batch {
		if ctx.Err() != nil {
			return stored, ctx.Err()
		}
		end := i + batch
		if end > len(small) {
			end = len(small)
		}
		var set imap.UIDSet
		for _, u := range small[i:end] {
			set.AddNum(imap.UID(u))
		}
		msgs, err := c.Fetch(set, &imap.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
		if err != nil {
			return stored, fmt.Errorf("fetch messages: %w", err)
		}
		for _, fm := range msgs {
			raw := fm.FindBodySection(section)
			if mb.Role == model.RoleDrafts {
				if _, ours := a.Store.DraftByKey(draftKeyFromRaw(raw)); ours {
					continue // the server copy of a draft this app owns
				}
			}
			m := a.ingest(acct, mb, fm, raw)
			ins, err := a.Store.SaveMessage(&m, raw)
			if err != nil {
				return stored, err
			}
			if !ins {
				continue
			}
			stored++
			a.noteContacts(m, mb)
			if applyRules && mb.Role == model.RoleInbox && !m.Seen && time.Since(m.Time()) < 72*time.Hour {
				a.runRules(ctx, ruleSet, m, raw)
			}
		}
		a.setStatus(acct.ID, func(s *AccountStatus) {
			s.Progress = fmt.Sprintf("%s: %d/%d", mb.Name, end, len(uids))
		})
		if stored > 0 {
			a.emit(Event{Type: "message", Account: acct.ID, Detail: mb.Name})
		}
	}
	for i := 0; i < len(large); i += batch {
		end := i + batch
		if end > len(large) {
			end = len(large)
		}
		var set imap.UIDSet
		for _, u := range large[i:end] {
			set.AddNum(imap.UID(u))
		}
		msgs, err := c.Fetch(set, &imap.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true, Envelope: true}).Collect()
		if err != nil {
			return stored, fmt.Errorf("fetch large message headers: %w", err)
		}
		for _, fm := range msgs {
			m := a.stub(acct, mb, fm)
			if ins, err := a.Store.SaveMessage(&m, nil); err != nil {
				return stored, err
			} else if ins {
				stored++
				a.noteContacts(m, mb)
			}
		}
	}
	return stored, nil
}

func envAddrs(l []imap.Address) []model.Address {
	var out []model.Address
	for _, x := range l {
		if x.Mailbox == "" {
			continue
		}
		out = append(out, model.Address{Name: x.Name, Addr: strings.ToLower(x.Addr())})
	}
	return out
}

// stub builds a headers-only row for a message too large to download during sync.
func (a *App) stub(acct model.Account, mb model.Mailbox, fm *imapclient.FetchMessageBuffer) model.Message {
	m := model.Message{AccountID: acct.ID, MailboxID: mb.ID, UID: uint32(fm.UID), Size: fm.RFC822Size, Partial: true, HasAttach: true}
	if e := fm.Envelope; e != nil {
		m.Subject, m.MessageID = e.Subject, e.MessageID
		if !e.Date.IsZero() {
			m.Date = e.Date.Unix()
		}
		if f := envAddrs(e.From); len(f) > 0 {
			m.From = f[0]
		}
		m.To, m.Cc, m.ReplyTo = envAddrs(e.To), envAddrs(e.Cc), envAddrs(e.ReplyTo)
		if len(e.InReplyTo) > 0 {
			m.InReplyTo = e.InReplyTo[0]
		}
	}
	if m.Date == 0 {
		m.Date = fm.InternalDate.Unix()
	}
	m.Seen, m.Flagged, m.Answered, m.Draft, m.Deleted, m.Keywords = toFlags(fm.Flags)
	m.Text = fmt.Sprintf("Large message (%.1f MB), not downloaded.", float64(fm.RFC822Size)/1048576)
	m.Snippet = fmt.Sprintf("Large message (%.1f MB), not downloaded", float64(fm.RFC822Size)/1048576)
	m.ThreadID = a.threadFor(acct.ID, &m, mb)
	return m
}

// DownloadMessage fetches a large message that sync skipped.
func (a *App) DownloadMessage(ctx context.Context, actor string, id int64) (err error) {
	defer func() { a.Store.Audit(actor, "message.download", map[string]any{"id": id}, err) }()
	m, err := a.Store.GetMessage(id)
	if err != nil {
		return err
	}
	if !m.Partial {
		return nil
	}
	acct, err := a.Store.GetAccount(m.AccountID)
	if err != nil {
		return err
	}
	mb, err := a.Store.GetMailbox(m.MailboxID)
	if err != nil {
		return err
	}
	var raw []byte
	section := &imap.FetchItemBodySection{Peek: true}
	err = a.withConn(ctx, acct, func(c *imapx.Conn) error {
		if _, err := c.Select(mb.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return fmt.Errorf("select %s: %w", mb.Name, err)
		}
		msgs, err := c.Fetch(imap.UIDSetNum(imap.UID(m.UID)), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
		if err != nil {
			return fmt.Errorf("download message: %w", err)
		}
		if len(msgs) == 0 {
			return errors.New("the server no longer has this message")
		}
		raw = msgs[0].FindBodySection(section)
		return nil
	})
	if err != nil {
		return err
	}
	parsed, err := compose.Parse(raw)
	if err != nil {
		return err
	}
	parsed.Subject, parsed.From, parsed.To, parsed.Cc = m.Subject, m.From, m.To, m.Cc
	if err = a.Store.UpdateFullMessage(id, &parsed, raw); err != nil {
		return err
	}
	a.emit(Event{Type: "message", Account: m.AccountID})
	return nil
}

func (a *App) noteContacts(m model.Message, mb model.Mailbox) {
	when := m.Date
	if mb.Role == model.RoleSent {
		for _, r := range append(append([]model.Address{}, m.To...), m.Cc...) {
			_ = a.Store.TouchContact(r, when)
		}
		return
	}
	_ = a.Store.TouchContact(m.From, when)
}

func (a *App) runRules(ctx context.Context, rs []model.Rule, m model.Message, raw []byte) {
	if len(rs) == 0 {
		return
	}
	in := &rules.Input{Msg: m, Raw: raw}
	acts := rules.Evaluate(rs, in)
	if len(acts) == 0 {
		return
	}
	if err := a.applyRuleActions(ctx, m, acts); err != nil {
		a.Store.Audit("rules", "rule.apply", map[string]any{"message": m.ID, "subject": m.Subject}, err)
	}
}

// ingest turns a fetched message into a stored Message.
func (a *App) ingest(acct model.Account, mb model.Mailbox, fm *imapclient.FetchMessageBuffer, raw []byte) model.Message {
	m, err := compose.Parse(raw)
	if err != nil {
		m = model.Message{Subject: "(unreadable message)", Snippet: err.Error()}
		if fm.Envelope != nil {
			m.Subject = fm.Envelope.Subject
		}
	}
	m.AccountID, m.MailboxID, m.UID = acct.ID, mb.ID, uint32(fm.UID)
	if m.Date == 0 {
		m.Date = fm.InternalDate.Unix()
	}
	if m.Size == 0 {
		m.Size = fm.RFC822Size
	}
	m.Seen, m.Flagged, m.Answered, m.Draft, m.Deleted, m.Keywords = toFlags(fm.Flags)
	m.ThreadID = a.threadFor(acct.ID, &m, mb)
	return m
}

func (a *App) threadFor(accountID string, m *model.Message, mb model.Mailbox) string {
	refs := append([]string{}, m.References...)
	if m.InReplyTo != "" {
		refs = append([]string{m.InReplyTo}, refs...)
	}
	// nearest ancestors first
	for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
		refs[i], refs[j] = refs[j], refs[i]
	}
	if t := a.Store.FindThreadByMessageIDs(accountID, refs); t != "" {
		return t
	}
	if t := a.Store.FindThreadByChildren(accountID, m.MessageID); t != "" {
		return t
	}
	// Seed from the thread root so replies and the original agree even when they arrive in any order.
	seed := m.MessageID
	if len(m.References) > 0 {
		seed = m.References[0]
	} else if m.InReplyTo != "" {
		seed = m.InReplyTo
	}
	if seed == "" {
		seed = fmt.Sprintf("%s/%d/%d", accountID, mb.ID, m.UID)
	}
	h := sha1.Sum([]byte(accountID + "|" + seed))
	return "t_" + hex.EncodeToString(h[:6])
}

// LoadOlder fetches up to n older messages for a mailbox (below the oldest one held locally).
func (a *App) LoadOlder(ctx context.Context, accountID, mailbox string, n int) (int, error) {
	acct, err := a.Store.GetAccount(accountID)
	if err != nil {
		return 0, err
	}
	mb, err := a.Store.FindMailbox(accountID, mailbox)
	if err != nil {
		return 0, err
	}
	c, err := a.Dial(ctx, acct)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if _, err := c.Select(mb.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return 0, err
	}
	var all imap.UIDSet
	all.AddRange(1, 0)
	buf, err := c.Fetch(all, &imap.FetchOptions{UID: true, RFC822Size: true}).Collect()
	if err != nil {
		return 0, err
	}
	sizes := map[uint32]int64{}
	for _, m := range buf {
		sizes[uint32(m.UID)] = m.RFC822Size
	}
	local, err := a.Store.UIDsInMailbox(mb.ID)
	if err != nil {
		return 0, err
	}
	var missing []uint32
	for _, m := range buf {
		if _, ok := local[uint32(m.UID)]; !ok {
			missing = append(missing, uint32(m.UID))
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] > missing[j] })
	if n <= 0 {
		n = 200
	}
	if len(missing) > n {
		missing = missing[:n]
	}
	got, err := a.fetchAndStore(ctx, acct, c, mb, missing, sizes, false)
	if err != nil {
		return got, fmt.Errorf("load older mail in %s/%s: %w", accountID, mb.Name, err)
	}
	_ = a.Store.RefreshCounts(mb.ID)
	return got, nil
}

// SetMailboxSync turns background sync on or off for a mailbox.
func (a *App) SetMailboxSync(actor, accountID, mailbox string, on bool) (err error) {
	defer func() {
		a.Store.Audit(actor, "mailbox.sync", map[string]any{"account": accountID, "mailbox": mailbox, "on": on}, err)
	}()
	mb, err := a.Store.FindMailbox(accountID, mailbox)
	if err != nil {
		return err
	}
	if err = a.Store.SetMailboxSubscribed(mb.ID, on); err != nil {
		return err
	}
	a.Refresh(accountID)
	return nil
}

// tick runs periodic local work: wake snoozed mail and send queued mail.
func (a *App) tick(ctx context.Context) {
	if ids, err := a.Store.DueSnoozed(time.Now().Unix()); err == nil {
		for _, id := range ids {
			if err := a.wakeSnoozed(ctx, id); err != nil {
				a.Store.Audit("system", "snooze.wake", map[string]any{"message": id}, err)
			}
		}
	}
	a.processOutbox(ctx)
}

var _ = store.ErrNotFound

type syncPlan int

const (
	planFull    syncPlan = iota // compare every UID and flag
	planAppends                 // only new arrivals and changed flags
	planSkip                    // nothing changed since the last sync
)

// planSync decides how much work a mailbox needs. mb holds what the last sync recorded; the
// other arguments come from SELECT. Anything that cannot be proven to be a pure arrival or no
// change at all falls back to the full comparison, which is always correct.
func planSync(mb model.Mailbox, validity uint32, exists int, uidNext uint32, modSeq uint64, cond, full bool) syncPlan {
	synced := mb.LastUID > 0 && mb.UIDValidity == validity
	if !cond || !synced || full || mb.ModSeq == 0 {
		return planFull
	}
	if uidNext == mb.UIDNext && exists == mb.Exists && modSeq == mb.ModSeq {
		return planSkip
	}
	arrivals := exists - mb.Exists
	if arrivals >= 0 && arrivals == int(uidNext)-int(mb.UIDNext) && modSeq >= mb.ModSeq {
		return planAppends
	}
	return planFull
}
