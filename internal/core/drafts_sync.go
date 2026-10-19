package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/michael-berardi/openmail/internal/compose"
	"github.com/michael-berardi/openmail/internal/imapx"
	"github.com/michael-berardi/openmail/internal/model"
)

const draftHeader = "X-OpenMail-Draft"

// draftPushDelay coalesces autosaves so the server sees one copy per pause, not one per keystroke.
const draftPushDelay = 15 * time.Second

func newDraftKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// draftKeyFromRaw reads our draft key from the header block of a raw message.
func draftKeyFromRaw(raw []byte) string {
	head := raw
	if len(head) > 8192 {
		head = head[:8192]
	}
	if end := bytes.Index(head, []byte("\r\n\r\n")); end >= 0 {
		head = head[:end]
	} else if end := bytes.Index(head, []byte("\n\n")); end >= 0 {
		head = head[:end]
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		if len(line) > len(draftHeader)+1 && bytes.EqualFold(line[:len(draftHeader)+1], []byte(draftHeader+":")) {
			return string(bytes.TrimSpace(line[len(draftHeader)+1:]))
		}
	}
	return ""
}

func (a *App) syncsDrafts(acct model.Account) bool {
	return acct.Enabled && acct.Source != "demo" && acct.Source != "local"
}

// scheduleDraftPush mirrors a draft to the server's Drafts folder after a quiet period.
func (a *App) scheduleDraftPush(id int64, delay time.Duration) {
	a.draftMu.Lock()
	defer a.draftMu.Unlock()
	if t, ok := a.draftTimers[id]; ok {
		t.Stop()
	}
	a.draftTimers[id] = time.AfterFunc(delay, func() {
		a.draftMu.Lock()
		delete(a.draftTimers, id)
		a.draftMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := a.pushDraft(ctx, id); err != nil {
			a.Store.Audit("system", "draft.server_sync", map[string]any{"id": id}, err)
		}
	})
}

// FlushDrafts pushes every draft still waiting for its quiet period. Called on exit.
func (a *App) FlushDrafts(ctx context.Context) {
	a.draftMu.Lock()
	ids := make([]int64, 0, len(a.draftTimers))
	for id, t := range a.draftTimers {
		t.Stop()
		ids = append(ids, id)
	}
	a.draftTimers = map[int64]*time.Timer{}
	a.draftMu.Unlock()
	for _, id := range ids {
		if err := a.pushDraft(ctx, id); err != nil {
			a.Store.Audit("system", "draft.server_sync", map[string]any{"id": id}, err)
		}
	}
}

// pushDraft appends the current draft to the server Drafts folder and removes the previous copy.
func (a *App) pushDraft(ctx context.Context, id int64) error {
	a.pushMu.Lock()
	defer a.pushMu.Unlock()
	d, err := a.Store.GetDraft(id)
	if err != nil {
		return nil // deleted meanwhile
	}
	if d.State != StateDraft && d.State != StatePending {
		return nil
	}
	acct, err := a.Store.GetAccount(d.AccountID)
	if err != nil || !a.syncsDrafts(acct) {
		return nil
	}
	box, err := a.Store.FindMailbox(acct.ID, model.RoleDrafts)
	if err != nil {
		return nil // server has no Drafts folder; the draft stays local
	}
	if d.DraftKey == "" {
		d.DraftKey = newDraftKey()
	}
	raw, msgID, err := compose.Build(compose.Outgoing{
		From: model.Address{Name: acct.DisplayName, Addr: acct.Email}, To: d.To, Cc: d.Cc, Bcc: d.Bcc,
		Subject: d.Subject, Text: d.Body, HTML: d.HTMLBody, InReplyTo: d.InReplyTo, References: d.References,
		Attachments: d.Attachments, Draft: true, Headers: map[string]string{draftHeader: d.DraftKey},
	})
	if err != nil {
		return err
	}
	var uid uint32
	err = a.withConn(ctx, acct, func(c *imapx.Conn) error {
		u, err := appendRawUID(c, box.Name, raw, []string{`\Draft`, `\Seen`})
		if err != nil {
			return err
		}
		if u == 0 { // server without UIDPLUS: find what we just added
			if _, err := c.Select(box.Name, nil).Wait(); err != nil {
				return err
			}
			sd, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: msgID}}}, nil).Wait()
			if err == nil {
				if all := sd.AllUIDs(); len(all) > 0 {
					u = uint32(all[len(all)-1])
				}
			}
		}
		uid = u
		if d.ServerUID != 0 && d.ServerBox != "" {
			if err := a.applyServerOp(c, "expunge", queuedOp{Mailbox: d.ServerBox, UIDs: []uint32{d.ServerUID}}, nil); err != nil {
				a.Store.Audit("system", "draft.server_replace", map[string]any{"id": id}, err)
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errOffline) {
			return nil // the next save retries
		}
		return err
	}
	d.ServerUID, d.ServerBox = uid, box.Name
	cur, gerr := a.Store.GetDraft(id)
	if gerr != nil {
		return nil
	}
	cur.ServerUID, cur.ServerBox, cur.DraftKey = d.ServerUID, d.ServerBox, d.DraftKey
	return a.Store.SaveDraftMeta(&cur)
}

// deleteServerDraft removes the server copy of a draft (queued if the server is unreachable).
func (a *App) deleteServerDraft(ctx context.Context, d model.Draft) {
	a.pushMu.Lock()
	defer a.pushMu.Unlock()
	if cur, err := a.Store.GetDraft(d.ID); err == nil {
		d.ServerUID, d.ServerBox = cur.ServerUID, cur.ServerBox // a push may have replaced the copy since d was read
	}
	if d.ServerUID == 0 || d.ServerBox == "" {
		return
	}
	acct, err := a.Store.GetAccount(d.AccountID)
	if err != nil || !a.syncsDrafts(acct) {
		return
	}
	a.draftMu.Lock()
	if t, ok := a.draftTimers[d.ID]; ok {
		t.Stop()
		delete(a.draftTimers, d.ID)
	}
	a.draftMu.Unlock()
	if err := a.serverOrQueue(ctx, acct, "expunge", queuedOp{Mailbox: d.ServerBox, UIDs: []uint32{d.ServerUID}}, nil); err != nil {
		a.Store.Audit("system", "draft.server_delete", map[string]any{"id": d.ID}, err)
	}
}

// EditServerDraft turns a message from the server Drafts folder (for example one started in
// Apple Mail on another device) into an editable local draft. Saving replaces the server copy.
func (a *App) EditServerDraft(ctx context.Context, actor string, msgID int64) (d model.Draft, err error) {
	defer func() { a.Store.Audit(actor, "draft.edit_server", map[string]any{"message": msgID}, err) }()
	m, err := a.Store.GetMessage(msgID)
	if err != nil {
		return d, err
	}
	if m.MailboxRole != model.RoleDrafts {
		return d, fmt.Errorf("message %d is not in a Drafts folder", msgID)
	}
	if m.Partial {
		if err = a.DownloadMessage(ctx, actor, msgID); err != nil {
			return d, err
		}
		if m, err = a.Store.GetMessage(msgID); err != nil {
			return d, err
		}
	}
	d = model.Draft{AccountID: m.AccountID, To: m.To, Cc: m.Cc, Bcc: m.Bcc, Subject: m.Subject, Body: m.Text, InReplyTo: m.InReplyTo, References: m.References,
		Author: "user", ServerUID: m.UID, ServerBox: m.Mailbox, DraftKey: newDraftKey()}
	if raw, rerr := a.Store.RawMessage(msgID); rerr == nil {
		if key := draftKeyFromRaw(raw); key != "" {
			d.DraftKey = key
		}
		for _, at := range m.Attachments {
			if at.Inline && at.Filename == "" {
				continue
			}
			if meta, data, aerr := compose.Attachment(raw, at.Index); aerr == nil {
				d.Attachments = append(d.Attachments, model.DraftAttachment{Filename: meta.Filename, ContentType: meta.ContentType, Size: len(data), Data: data})
			}
		}
	}
	if err = a.Store.SaveDraft(&d); err != nil {
		return d, err
	}
	a.emit(Event{Type: "outbox", Account: d.AccountID})
	return d, nil
}
