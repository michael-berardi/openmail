package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/michael-berardi/openmail/internal/imapx"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/store"
)

// Action kinds accepted by Act.
const (
	ActRead     = "read"
	ActUnread   = "unread"
	ActFlag     = "flag"
	ActUnflag   = "unflag"
	ActArchive  = "archive"
	ActTrash    = "trash"
	ActJunk     = "junk"
	ActNotJunk  = "not_junk"
	ActMove     = "move"    // Value = mailbox name or role
	ActCopy     = "copy"    // Value = mailbox name or role; the original stays
	ActKeyword  = "keyword" // Value = keyword to add; prefix "-" removes
	ActDelete   = "delete"  // permanent
	ActSnooze   = "snooze"  // Value = RFC3339 time or duration like 3h
	ActUnsnooze = "unsnooze"
)

var errOffline = errors.New("offline")

// ---- ops connection ----

func (a *App) dropOpsConn(id string) {
	a.opsMu.Lock()
	defer a.opsMu.Unlock()
	if c, ok := a.opsConn[id]; ok {
		c.Close()
		delete(a.opsConn, id)
	}
}

// withConn runs fn on the pooled connection for acct, reconnecting once on a dead link.
// Returns an error wrapping errOffline when the server cannot be reached.
func (a *App) withConn(ctx context.Context, acct model.Account, fn func(*imapx.Conn) error) error {
	a.opsMu.Lock()
	defer a.opsMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		c := a.opsConn[acct.ID]
		if c != nil {
			select {
			case <-c.Closed():
				c = nil
				delete(a.opsConn, acct.ID)
			default:
			}
		}
		if c == nil {
			var err error
			c, err = a.Dial(ctx, acct)
			if err != nil {
				var ce *imapx.ConnectError
				if errors.As(err, &ce) {
					return fmt.Errorf("%w: %v", errOffline, err)
				}
				return err
			}
			a.opsConn[acct.ID] = c
		}
		err := fn(c)
		if err == nil {
			return nil
		}
		if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "use of closed") || strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "connection reset") {
			c.Close()
			delete(a.opsConn, acct.ID)
			continue
		}
		return err
	}
	return fmt.Errorf("%w: connection to %s keeps dropping", errOffline, acct.IMAPHost)
}

// ---- queued ops (offline) ----

type queuedOp struct {
	Mailbox string   `json:"mailbox"`
	UIDs    []uint32 `json:"uids"`
	Add     []string `json:"add,omitempty"`
	Remove  []string `json:"remove,omitempty"`
	Dest    string   `json:"dest,omitempty"`
	Raw     []byte   `json:"raw,omitempty"` // for append
	Flags   []string `json:"flags,omitempty"`
}

func (a *App) drainOps(ctx context.Context, acct model.Account, c *imapx.Conn) error {
	ops, err := a.Store.PendingOps(acct.ID, 200)
	if err != nil || len(ops) == 0 {
		return err
	}
	for _, op := range ops {
		var q queuedOp
		if err := json.Unmarshal([]byte(op.Payload), &q); err != nil {
			_ = a.Store.FinishOp(op.ID)
			continue
		}
		err := a.applyServerOp(c, op.Kind, q, nil)
		if err != nil {
			_ = a.Store.FailOp(op.ID, err.Error())
			if op.Attempts >= 5 {
				a.Store.Audit("system", "op.abandoned", map[string]any{"kind": op.Kind, "mailbox": q.Mailbox}, err)
				_ = a.Store.FinishOp(op.ID)
			}
			continue
		}
		_ = a.Store.FinishOp(op.ID)
	}
	return nil
}

// applyServerOp performs a flag change, move, expunge or append on an open connection.
// moved receives the new UIDs for a move when the server reports them.
func (a *App) applyServerOp(c *imapx.Conn, kind string, q queuedOp, moved *map[uint32]uint32) error {
	if kind == "append" {
		return appendRaw(c, q.Dest, q.Raw, q.Flags)
	}
	if _, err := c.Select(q.Mailbox, nil).Wait(); err != nil {
		return fmt.Errorf("select %s: %w", q.Mailbox, err)
	}
	var set imap.UIDSet
	for _, u := range q.UIDs {
		set.AddNum(imap.UID(u))
	}
	switch kind {
	case "flags":
		toFlags := func(s []string) []imap.Flag {
			var f []imap.Flag
			for _, x := range s {
				f = append(f, imap.Flag(x))
			}
			return f
		}
		if len(q.Add) > 0 {
			if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: toFlags(q.Add)}, nil).Close(); err != nil {
				return fmt.Errorf("set flags: %w", err)
			}
		}
		if len(q.Remove) > 0 {
			if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: toFlags(q.Remove)}, nil).Close(); err != nil {
				return fmt.Errorf("clear flags: %w", err)
			}
		}
	case "move":
		md, err := c.Move(set, q.Dest).Wait()
		if err != nil {
			return fmt.Errorf("move to %s: %w", q.Dest, err)
		}
		if moved != nil && md != nil && md.SourceUIDs != nil && md.DestUIDs != nil {
			src, ok1 := md.SourceUIDs.(imap.UIDSet)
			dst, ok2 := md.DestUIDs.(imap.UIDSet)
			if ok1 && ok2 {
				*moved = pairUIDs(src, dst)
			}
		}
	case "copy":
		if _, err := c.Copy(set, q.Dest).Wait(); err != nil {
			return fmt.Errorf("copy to %s: %w", q.Dest, err)
		}
	case "expunge":
		if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			return fmt.Errorf("mark deleted: %w", err)
		}
		if c.Caps().Has(imap.CapUIDPlus) {
			if err := c.UIDExpunge(set).Close(); err != nil {
				return fmt.Errorf("expunge: %w", err)
			}
		}
		// Without UIDPLUS a plain EXPUNGE would also remove other messages marked \Deleted by
		// other clients, so the message stays hidden (flagged \Deleted) until the server expunges.
	default:
		return fmt.Errorf("unknown op %q", kind)
	}
	return nil
}

func appendRaw(c *imapx.Conn, mailbox string, raw []byte, flags []string) error {
	_, err := appendRawUID(c, mailbox, raw, flags)
	return err
}

// appendRawUID appends a message and returns its new UID when the server reports it (UIDPLUS).
func appendRawUID(c *imapx.Conn, mailbox string, raw []byte, flags []string) (uint32, error) {
	var fl []imap.Flag
	for _, f := range flags {
		fl = append(fl, imap.Flag(f))
	}
	cmd := c.Append(mailbox, int64(len(raw)), &imap.AppendOptions{Flags: fl, Time: time.Now()})
	if _, err := cmd.Write(raw); err != nil {
		return 0, fmt.Errorf("append to %s: %w", mailbox, err)
	}
	if err := cmd.Close(); err != nil {
		return 0, fmt.Errorf("append to %s: %w", mailbox, err)
	}
	data, err := cmd.Wait()
	if err != nil {
		return 0, fmt.Errorf("append to %s: %w", mailbox, err)
	}
	if data != nil {
		return uint32(data.UID), nil
	}
	return 0, nil
}

// ---- Act ----

// Act applies an action to messages. The server is changed first; if it is unreachable the
// change is applied locally and queued so it is never lost.
func (a *App) Act(ctx context.Context, actor string, ids []int64, kind, value string) (err error) {
	defer func() {
		a.Store.Audit(actor, "message."+kind, map[string]any{"ids": ids, "value": value}, err)
	}()
	if len(ids) == 0 {
		return errors.New("no messages given")
	}
	if kind == ActDelete && IsAgent(actor) {
		return errors.New("agents cannot permanently delete mail; use trash and let the user empty it")
	}
	if len(ids) > 500 {
		return errors.New("at most 500 messages per action")
	}
	byGroup := map[string][]model.Message{}
	var order []string
	for _, id := range ids {
		m, gerr := a.Store.GetMessage(id)
		if gerr != nil {
			return gerr
		}
		key := fmt.Sprintf("%s\x00%d", m.AccountID, m.MailboxID)
		if _, ok := byGroup[key]; !ok {
			order = append(order, key)
		}
		byGroup[key] = append(byGroup[key], m)
	}
	for _, key := range order {
		msgs := byGroup[key]
		if err = a.actGroup(ctx, msgs, kind, value); err != nil {
			return err
		}
	}
	a.emit(Event{Type: "message"})
	return nil
}

func (a *App) actGroup(ctx context.Context, msgs []model.Message, kind, value string) error {
	first := msgs[0]
	acct, err := a.Store.GetAccount(first.AccountID)
	if err != nil {
		return err
	}
	mb, err := a.Store.GetMailbox(first.MailboxID)
	if err != nil {
		return err
	}
	uids := make([]uint32, 0, len(msgs))
	for _, m := range msgs {
		uids = append(uids, m.UID)
	}
	switch kind {
	case ActRead, ActUnread, ActFlag, ActUnflag, ActKeyword:
		var add, rem []string
		switch kind {
		case ActRead:
			add = []string{string(imap.FlagSeen)}
		case ActUnread:
			rem = []string{string(imap.FlagSeen)}
		case ActFlag:
			add = []string{string(imap.FlagFlagged)}
		case ActUnflag:
			rem = []string{string(imap.FlagFlagged)}
		case ActKeyword:
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, " (){%*\"\\") {
				return fmt.Errorf("invalid keyword %q (no spaces or special characters)", value)
			}
			if strings.HasPrefix(value, "-") {
				rem = []string{value[1:]}
			} else {
				add = []string{value}
			}
		}
		q := queuedOp{Mailbox: mb.Name, UIDs: uids, Add: add, Remove: rem}
		if err := a.serverOrQueue(ctx, acct, "flags", q, nil); err != nil {
			return err
		}
		for _, m := range msgs {
			seen, flagged, answered, draft, deleted, kw := m.Seen, m.Flagged, m.Answered, m.Draft, m.Deleted, m.Keywords
			for _, f := range add {
				switch f {
				case string(imap.FlagSeen):
					seen = true
				case string(imap.FlagFlagged):
					flagged = true
				default:
					kw = appendUnique(kw, f)
				}
			}
			for _, f := range rem {
				switch f {
				case string(imap.FlagSeen):
					seen = false
				case string(imap.FlagFlagged):
					flagged = false
				default:
					kw = removeStr(kw, f)
				}
			}
			if err := a.Store.SetFlags(m.ID, seen, flagged, answered, draft, deleted, kw); err != nil {
				return err
			}
		}
		return a.Store.RefreshCounts(mb.ID)
	case ActMove, ActArchive, ActTrash, ActJunk, ActNotJunk:
		dest := value
		switch kind {
		case ActArchive:
			dest = model.RoleArchive
		case ActTrash:
			dest = model.RoleTrash
			if mb.Role == model.RoleTrash {
				return a.actGroup(ctx, msgs, ActDelete, "")
			}
		case ActJunk:
			dest = model.RoleJunk
		case ActNotJunk:
			dest = model.RoleInbox
		}
		return a.moveMessages(ctx, acct, mb, msgs, dest)
	case ActCopy:
		to, err := a.resolveMailbox(ctx, acct, value)
		if err != nil {
			return err
		}
		return a.serverOrQueue(ctx, acct, "copy", queuedOp{Mailbox: mb.Name, UIDs: uids, Dest: to.Name}, nil)
	case ActDelete:
		q := queuedOp{Mailbox: mb.Name, UIDs: uids}
		if err := a.serverOrQueue(ctx, acct, "expunge", q, nil); err != nil {
			return err
		}
		for _, m := range msgs {
			if err := a.Store.DeleteMessage(m.ID); err != nil {
				return err
			}
		}
		return a.Store.RefreshCounts(mb.ID)
	case ActSnooze:
		until, err := parseUntil(value, now())
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if err := a.Store.Snooze(m.ID, until.Unix()); err != nil {
				return err
			}
		}
		return nil
	case ActUnsnooze:
		for _, m := range msgs {
			if err := a.Store.Unsnooze(m.ID); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown action %q", kind)
}

func (a *App) serverOrQueue(ctx context.Context, acct model.Account, kind string, q queuedOp, moved *map[uint32]uint32) error {
	if acct.Source == "demo" {
		return nil // the demo mailbox is local only
	}
	err := a.withConn(ctx, acct, func(c *imapx.Conn) error { return a.applyServerOp(c, kind, q, moved) })
	if err == nil {
		return nil
	}
	if errors.Is(err, errOffline) {
		if qerr := a.Store.EnqueueOp(acct.ID, kind, q); qerr != nil {
			return qerr
		}
		return nil
	}
	return err
}

// resolveMailbox finds a destination by role or name, creating an Archive folder if needed.
func (a *App) resolveMailbox(ctx context.Context, acct model.Account, roleOrName string) (model.Mailbox, error) {
	mb, err := a.Store.FindMailbox(acct.ID, roleOrName)
	if err == nil {
		return mb, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return mb, err
	}
	switch strings.ToLower(roleOrName) {
	case model.RoleArchive:
		if mb, err := a.Store.FindMailbox(acct.ID, model.RoleAll); err == nil {
			return mb, nil
		}
		roleOrName = "Archive"
	case model.RoleTrash, model.RoleJunk, model.RoleInbox, model.RoleSent, model.RoleDrafts:
		return mb, fmt.Errorf("account %s has no %s folder", acct.ID, roleOrName)
	}
	// create a new folder with that name
	nm := model.Mailbox{AccountID: acct.ID, Name: roleOrName, Delim: "/", Subscribed: true}
	if strings.EqualFold(roleOrName, "Archive") {
		nm.Role = model.RoleArchive
	}
	if err := a.withConn(ctx, acct, func(c *imapx.Conn) error {
		if err := c.Create(roleOrName, nil).Wait(); err != nil {
			return fmt.Errorf("create folder %q: %w", roleOrName, err)
		}
		_ = c.Subscribe(roleOrName).Wait()
		return nil
	}); err != nil {
		return mb, err
	}
	if err := a.Store.UpsertMailbox(&nm); err != nil {
		return mb, err
	}
	return nm, nil
}

func (a *App) moveMessages(ctx context.Context, acct model.Account, from model.Mailbox, msgs []model.Message, dest string) error {
	to, err := a.resolveMailbox(ctx, acct, dest)
	if err != nil {
		return err
	}
	if to.ID == from.ID {
		return nil
	}
	uids := make([]uint32, 0, len(msgs))
	for _, m := range msgs {
		uids = append(uids, m.UID)
	}
	var newUIDs map[uint32]uint32
	q := queuedOp{Mailbox: from.Name, UIDs: uids, Dest: to.Name}
	if err := a.serverOrQueue(ctx, acct, "move", q, &newUIDs); err != nil {
		return err
	}
	for _, m := range msgs {
		uid := newUIDs[m.UID] // 0 when the server did not report it (offline, or no UIDPLUS)
		if uid == 0 {
			// the next sync of the destination replaces this placeholder with the real row
			uid = 0x80000000 + uint32(m.ID&0x3fffffff)
		}
		if err := a.Store.UpdateMessageLocation(m.ID, to.ID, uid); err != nil {
			return err
		}
	}
	_ = a.Store.RefreshCounts(from.ID)
	return a.Store.RefreshCounts(to.ID)
}

func (a *App) applyRuleActions(ctx context.Context, m model.Message, acts []model.Action) error {
	for _, act := range acts {
		var err error
		switch act.Kind {
		case "copy":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActCopy, act.Value)
		case "move":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActMove, act.Value)
		case "mark_read":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActRead, "")
		case "mark_unread":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActUnread, "")
		case "flag":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActFlag, "")
		case "unflag":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActUnflag, "")
		case "keyword":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActKeyword, act.Value)
		case "archive":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActArchive, "")
		case "delete":
			err = a.Act(ctx, "rules", []int64{m.ID}, ActTrash, "")
		}
		if err != nil {
			return fmt.Errorf("rule action %s: %w", act.Kind, err)
		}
		if act.Kind == "move" || act.Kind == "archive" || act.Kind == "delete" {
			return nil // message left the inbox; later actions would target a stale location
		}
	}
	return nil
}

// ---- snooze ----

func parseUntil(v string, from time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		if t.Before(from) {
			return t, errors.New("snooze time is in the past")
		}
		return t, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return from.Add(d), nil
	}
	switch strings.ToLower(v) {
	case "tomorrow":
		t := from.AddDate(0, 0, 1)
		return time.Date(t.Year(), t.Month(), t.Day(), 8, 0, 0, 0, from.Location()), nil
	case "tonight":
		t := time.Date(from.Year(), from.Month(), from.Day(), 19, 0, 0, 0, from.Location())
		if t.Before(from) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	case "next week", "next-week", "monday":
		t := from.AddDate(0, 0, 1)
		for t.Weekday() != time.Monday {
			t = t.AddDate(0, 0, 1)
		}
		return time.Date(t.Year(), t.Month(), t.Day(), 8, 0, 0, 0, from.Location()), nil
	case "weekend", "saturday":
		t := from.AddDate(0, 0, 1)
		for t.Weekday() != time.Saturday {
			t = t.AddDate(0, 0, 1)
		}
		return time.Date(t.Year(), t.Month(), t.Day(), 9, 0, 0, 0, from.Location()), nil
	}
	return time.Time{}, fmt.Errorf("cannot read snooze time %q (use an RFC3339 time, a duration like 3h, or tomorrow, tonight, weekend, next week)", v)
}

// wakeSnoozed returns a snoozed message to the top of the inbox as unread.
func (a *App) wakeSnoozed(ctx context.Context, id int64) error {
	m, err := a.Store.GetMessage(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return a.Store.Unsnooze(id)
		}
		return err
	}
	if err := a.Store.Unsnooze(id); err != nil {
		return err
	}
	if err := a.Act(ctx, "system", []int64{id}, ActUnread, ""); err != nil {
		return err
	}
	if m.Mailbox != "" {
		if mb, err := a.Store.GetMailbox(m.MailboxID); err == nil && mb.Role != model.RoleInbox {
			_ = a.Act(ctx, "system", []int64{id}, ActMove, model.RoleInbox)
		}
	}
	a.emit(Event{Type: "message", Account: m.AccountID, Detail: "snooze"})
	return nil
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func removeStr(s []string, v string) []string {
	out := s[:0:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// pairUIDs maps each source UID to its new UID using the server's own ordering of both lists.
// It returns nil unless both lists are fully enumerable and the same length.
func pairUIDs(src, dst imap.UIDSet) map[uint32]uint32 {
	sn, s1 := src.Nums()
	dn, d1 := dst.Nums()
	if !s1 || !d1 || len(sn) == 0 || len(sn) != len(dn) {
		return nil
	}
	m := make(map[uint32]uint32, len(sn))
	for i := range sn {
		m[uint32(sn[i])] = uint32(dn[i])
	}
	return m
}
