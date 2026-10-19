package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/model"
)

func cmdList(ctx context.Context, args []string) error {
	fs := newFlags("ls")
	limit := fs.Int("limit", 25, "number of results")
	offset := fs.Int("offset", 0, "skip results")
	threads := fs.Bool("threads", false, "group into conversations")
	asJSON := fs.Bool("json", false, "JSON output")
	junk := fs.Bool("junk", false, "include Junk and Trash")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	q := strings.Join(pos, " ")
	if q == "" {
		q = "in:inbox"
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	lo := core.ListOptions{Limit: *limit, Offset: *offset, IncludeJunk: *junk}
	if *threads {
		ts, err := app.Threads(q, lo)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(ts)
		}
		for _, t := range ts {
			mark := " "
			if t.Unread > 0 {
				mark = "●"
			}
			cnt := ""
			if t.Count > 1 {
				cnt = fmt.Sprintf(" (%d)", t.Count)
			}
			fmt.Printf("%s %-9s %-6s %-22s %s%s\n", mark, t.ThreadID, when(t.Latest.Date), clip(strings.Join(t.Senders, ", "), 22), clip(t.Latest.Subject, 60), cnt)
		}
		return nil
	}
	ms, err := app.Search(q, lo)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(ms)
	}
	if len(ms) == 0 {
		fmt.Println("No messages.")
		return nil
	}
	for _, m := range ms {
		mark := " "
		switch {
		case !m.Seen:
			mark = "●"
		case m.Flagged:
			mark = "⚑"
		}
		att := " "
		if m.HasAttach {
			att = "📎"
		}
		fmt.Printf("%s %-7d %-6s %-22s %s %s\n", mark, m.ID, when(m.Date), clip(fromLabel(m.From), 22), att, clip(m.Subject, 60))
	}
	return nil
}

func cmdRead(ctx context.Context, args []string) error {
	fs := newFlags("read")
	raw := fs.Bool("raw", false, "print the raw message source")
	asJSON := fs.Bool("json", false, "JSON output")
	markRead := fs.Bool("mark-read", true, "mark the message read")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: openmail read <id> [--raw] [--json]")
	}
	n, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not a message id", pos[0])
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	if *raw {
		b, err := app.Store.RawMessage(n)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	}
	m, err := app.GetMessage(n)
	if err != nil {
		return err
	}
	if *asJSON {
		m.HTML = ""
		return printJSON(m)
	}
	printMessage(m)
	if *markRead && !m.Seen {
		if err := app.Act(ctx, actor(), []int64{n}, core.ActRead, ""); err != nil {
			fmt.Fprintln(os.Stderr, "could not mark read:", err)
		}
	}
	return nil
}

func printMessage(m model.Message) {
	fmt.Printf("From:    %s\n", addrLine([]model.Address{m.From}))
	fmt.Printf("To:      %s\n", addrLine(m.To))
	if len(m.Cc) > 0 {
		fmt.Printf("Cc:      %s\n", addrLine(m.Cc))
	}
	fmt.Printf("Date:    %s\n", m.Time().Format(time.RFC1123))
	fmt.Printf("Subject: %s\n", m.Subject)
	fmt.Printf("Id:      %d   Thread: %s   %s/%s\n", m.ID, m.ThreadID, m.AccountID, m.Mailbox)
	for _, a := range m.Attachments {
		if a.Inline && a.Filename == "" {
			continue
		}
		fmt.Printf("Attach:  [%d] %s (%s, %d bytes)\n", a.Index, a.Filename, a.ContentType, a.Size)
	}
	fmt.Println()
	fmt.Println(m.Text)
}

func addrLine(l []model.Address) string {
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

func cmdThread(ctx context.Context, args []string) error {
	pos, err := parse(newFlags("thread"), args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: openmail thread <thread-id>")
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	ms, err := app.GetThread(pos[0])
	if err != nil {
		return err
	}
	for i, m := range ms {
		if i > 0 {
			fmt.Println("\n" + strings.Repeat("─", 72))
		}
		printMessage(m)
	}
	return nil
}

func readBody(body, file string) (string, error) {
	switch {
	case file == "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	case file != "":
		b, err := os.ReadFile(file)
		return string(b), err
	case body != "":
		return body, nil
	case !isTTY():
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	return "", errors.New("no body: use --body, --body-file or pipe text on stdin")
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func cmdSend(ctx context.Context, args []string) error {
	fs := newFlags("send")
	to := fs.String("to", "", "recipients, comma separated")
	cc := fs.String("cc", "", "cc")
	bcc := fs.String("bcc", "", "bcc")
	subject := fs.String("subject", "", "subject")
	body := fs.String("body", "", "message text")
	bodyFile := fs.String("body-file", "", "read the body from a file ('-' for stdin)")
	account := fs.String("from", "", "sending account (default: first account)")
	var attach listFlag
	fs.Var(&attach, "attach", "attach a file (repeatable)")
	now := fs.Bool("now", false, "send immediately, skipping the undo delay")
	draftOnly := fs.Bool("draft", false, "save as a draft instead of sending")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *to == "" || *subject == "" {
		return errors.New("usage: openmail send --to a@x.com --subject S (--body T | --body-file F) [--attach f] [--from account] [--now|--draft]")
	}
	text, err := readBody(*body, *bodyFile)
	if err != nil {
		return err
	}
	d := model.Draft{AccountID: strings.ToLower(*account), Subject: *subject, Body: text}
	if d.To, err = parseAddrList(*to); err != nil {
		return err
	}
	if d.Cc, err = parseAddrList(*cc); err != nil {
		return err
	}
	if d.Bcc, err = parseAddrList(*bcc); err != nil {
		return err
	}
	for _, f := range attach {
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("attachment: %w", err)
		}
		d.Attachments = append(d.Attachments, model.DraftAttachment{Filename: filepath.Base(f), ContentType: mime.TypeByExtension(filepath.Ext(f)), Size: len(b), Data: b})
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	return saveAndSend(ctx, app, d, *draftOnly, *now)
}

func saveAndSend(ctx context.Context, app *core.App, d model.Draft, draftOnly, now bool) error {
	d, err := app.SaveDraft(actor(), d)
	if err != nil {
		return err
	}
	if draftOnly {
		fmt.Printf("saved draft %d\n", d.ID)
		return nil
	}
	if now {
		if err := app.SendNow(ctx, actor(), d.ID); err != nil {
			return fmt.Errorf("draft %d kept; fix the problem and run `openmail drafts send %d`: %w", d.ID, d.ID, err)
		}
		fmt.Println("sent")
		return nil
	}
	// The undo window only works while a process is running to deliver the message.
	d2, err := app.SendDraft(actor(), d.ID)
	if err != nil {
		return err
	}
	wait := time.Until(time.Unix(d2.SendAt, 0))
	if wait > 0 {
		time.Sleep(wait + 100*time.Millisecond)
	}
	if err := app.SendNow(ctx, actor(), d.ID); err != nil {
		return fmt.Errorf("draft %d kept: %w", d.ID, err)
	}
	fmt.Println("sent")
	return nil
}

func cmdReply(ctx context.Context, args []string) error {
	fs := newFlags("reply")
	all := fs.Bool("all", false, "reply to everyone")
	body := fs.String("body", "", "your reply text")
	bodyFile := fs.String("body-file", "", "read the reply from a file ('-' for stdin)")
	send := fs.Bool("send", false, "send instead of saving a draft")
	now := fs.Bool("now", false, "send immediately")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: openmail reply <id> [--all] --body TEXT [--send]")
	}
	n, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not a message id", pos[0])
	}
	text, err := readBody(*body, *bodyFile)
	if err != nil {
		return err
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	d, err := app.Reply(actor(), n, *all, text)
	if err != nil {
		return err
	}
	if !*send && !*now {
		fmt.Printf("saved draft %d to %s: %s\n", d.ID, addrLine(d.To), d.Subject)
		return nil
	}
	return saveAndSend(ctx, app, d, false, *now)
}

func cmdForward(ctx context.Context, args []string) error {
	fs := newFlags("forward")
	to := fs.String("to", "", "recipients")
	body := fs.String("body", "", "text above the forwarded message")
	send := fs.Bool("send", false, "send instead of saving a draft")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 || *to == "" {
		return errors.New("usage: openmail forward <id> --to a@x.com [--body TEXT] [--send]")
	}
	n, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not a message id", pos[0])
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	d, err := app.Forward(actor(), n, *body)
	if err != nil {
		return err
	}
	if d.To, err = parseAddrList(*to); err != nil {
		return err
	}
	if d, err = app.SaveDraft(actor(), d); err != nil {
		return err
	}
	if !*send {
		fmt.Printf("saved draft %d\n", d.ID)
		return nil
	}
	return saveAndSend(ctx, app, d, false, false)
}

func cmdDrafts(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	one := func() (int64, error) {
		pos, err := parse(newFlags("drafts "+sub), rest)
		if err != nil || len(pos) != 1 {
			return 0, fmt.Errorf("usage: openmail drafts %s <id>", sub)
		}
		return strconv.ParseInt(pos[0], 10, 64)
	}
	switch sub {
	case "list", "ls":
		ds, err := app.Store.ListDrafts("")
		if err != nil {
			return err
		}
		if len(ds) == 0 {
			fmt.Println("No drafts.")
		}
		for _, d := range ds {
			fmt.Printf("%-5d %-17s %-14s %-28s %s\n", d.ID, d.State, d.Author, clip(addrLine(d.To), 28), clip(d.Subject, 50))
		}
		return nil
	case "show":
		id, err := one()
		if err != nil {
			return err
		}
		d, err := app.Store.GetDraft(id)
		if err != nil {
			return err
		}
		fmt.Printf("State: %s   Author: %s   Account: %s\nTo: %s\nSubject: %s\n\n%s\n", d.State, d.Author, d.AccountID, addrLine(d.To), d.Subject, d.Body)
		return nil
	case "send":
		id, err := one()
		if err != nil {
			return err
		}
		return app.SendNow(ctx, actor(), id)
	case "approve":
		id, err := one()
		if err != nil {
			return err
		}
		d, err := app.ApproveDraft(actor(), id)
		if err != nil {
			return err
		}
		fmt.Printf("approved; sends at %s (run `openmail serve` to deliver, or `openmail drafts send %d`)\n", time.Unix(d.SendAt, 0).Format("15:04:05"), id)
		return nil
	case "cancel":
		id, err := one()
		if err != nil {
			return err
		}
		_, err = app.CancelSend(actor(), id)
		return err
	case "delete", "rm":
		id, err := one()
		if err != nil {
			return err
		}
		return app.DeleteDraft(actor(), id)
	}
	return fmt.Errorf("unknown drafts command %q (list, show, send, approve, cancel, delete)", sub)
}

func cmdAct(kind string) cmdFunc {
	return func(ctx context.Context, args []string) error {
		pos, err := parse(newFlags(kind), args)
		if err != nil {
			return err
		}
		list, err := ids(pos)
		if err != nil {
			return err
		}
		app, err := openApp()
		if err != nil {
			return err
		}
		defer app.Close()
		if err := app.Act(ctx, actor(), list, kind, ""); err != nil {
			return err
		}
		fmt.Printf("%s: %d message(s)\n", kind, len(list))
		return nil
	}
}

func cmdMove(ctx context.Context, args []string) error {
	fs := newFlags("move")
	to := fs.String("to", "", "destination folder name or role (archive, trash, junk, inbox)")
	pos, err := parse(fs, args)
	if err != nil || *to == "" {
		return errors.New("usage: openmail move <ids...> --to <folder>")
	}
	list, err := ids(pos)
	if err != nil {
		return err
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	return app.Act(ctx, actor(), list, core.ActMove, *to)
}

func cmdSnooze(ctx context.Context, args []string) error {
	fs := newFlags("snooze")
	until := fs.String("until", "", "3h | tonight | tomorrow | weekend | next week | RFC3339 time")
	pos, err := parse(fs, args)
	if err != nil || *until == "" {
		return errors.New("usage: openmail snooze <ids...> --until tomorrow")
	}
	list, err := ids(pos)
	if err != nil {
		return err
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	return app.Act(ctx, actor(), list, core.ActSnooze, *until)
}

func cmdSync(ctx context.Context, args []string) error {
	fs := newFlags("sync")
	full := fs.Bool("full", false, "re-check every folder")
	older := fs.Int("older", 0, "also fetch this many older messages for the inbox")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	acct := ""
	if len(pos) > 0 {
		acct = pos[0]
	}
	start := time.Now()
	if err := app.SyncNow(ctx, acct, *full); err != nil {
		return err
	}
	if *older > 0 {
		accts, _ := app.Store.ListAccounts()
		for _, a := range accts {
			if acct != "" && !strings.EqualFold(acct, a.ID) {
				continue
			}
			n, err := app.LoadOlder(ctx, a.ID, "inbox", *older)
			if err != nil {
				return err
			}
			fmt.Printf("%s: loaded %d older message(s)\n", a.ID, n)
		}
	}
	ov, err := app.Overview()
	if err != nil {
		return err
	}
	for _, a := range ov.Accounts {
		unread, total := 0, 0
		for _, m := range a.Mailboxes {
			total += m.Total
			if m.Role == model.RoleInbox {
				unread = m.Unread
			}
		}
		fmt.Printf("%-32s %5d unread in inbox, %d messages stored\n", a.Email, unread, total)
	}
	fmt.Printf("synced in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdUnsub(ctx context.Context, args []string) error {
	pos, err := parse(newFlags("unsubscribe"), args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: openmail unsubscribe <id>")
	}
	n, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not a message id", pos[0])
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	res, err := app.Unsubscribe(ctx, actor(), n)
	if err != nil {
		return err
	}
	if res.Done {
		fmt.Printf("unsubscribed (%s)\n", res.Method)
	} else {
		fmt.Printf("open this link to unsubscribe: %s\n", res.URL)
	}
	return nil
}
