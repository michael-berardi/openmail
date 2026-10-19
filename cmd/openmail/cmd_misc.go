package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/secrets"
)

func cmdVersion(ctx context.Context, args []string) error {
	dir, _ := core.DataDir()
	fmt.Printf("openmail %s (%s/%s)\ndata: %s\n", core.Version, runtime.GOOS, runtime.GOARCH, dir)
	return nil
}

func cmdConfig(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	switch args[0] {
	case "list", "ls":
		all := app.Settings()
		var keys []string
		for k := range all {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%-28s %s\n", k, all[k])
		}
		return nil
	case "get":
		if len(args) != 2 {
			return errors.New("usage: openmail config get <key>")
		}
		v, ok := app.Settings()[args[1]]
		if !ok {
			return fmt.Errorf("unknown setting %q (see `openmail config list`)", args[1])
		}
		fmt.Println(v)
		return nil
	case "set":
		if len(args) < 3 {
			return errors.New("usage: openmail config set <key> <value>")
		}
		return app.SetSetting(actor(), args[1], strings.Join(args[2:], " "))
	}
	return errors.New("usage: openmail config <list|get|set>")
}

func cmdTokens(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	switch args[0] {
	case "list", "ls":
		ts, err := app.Store.ListTokens()
		if err != nil {
			return err
		}
		for _, t := range ts {
			last := "never"
			if t.LastUse > 0 {
				last = time.Unix(t.LastUse, 0).Format("2006-01-02 15:04")
			}
			fmt.Printf("%-20s %-6s created %s, last used %s\n", t.Name, t.Scope, time.Unix(t.Created, 0).Format("2006-01-02"), last)
		}
		return nil
	case "create":
		fs := newFlags("tokens create")
		scope := fs.String("scope", "write", "read | write | send | admin")
		pos, err := parse(fs, args[1:])
		if err != nil || len(pos) != 1 {
			return errors.New("usage: openmail tokens create <name> [--scope read|write|send|admin]")
		}
		tok, err := app.CreateToken(actor(), pos[0], *scope)
		if err != nil {
			return err
		}
		fmt.Printf("Token for %q (%s). It is shown once:\n\n  %s\n\nMCP over HTTP: Authorization: Bearer <token> to http://127.0.0.1:7777/mcp\n", pos[0], *scope, tok)
		return nil
	case "revoke", "rm":
		if len(args) != 2 {
			return errors.New("usage: openmail tokens revoke <name>")
		}
		return app.RevokeToken(actor(), args[1])
	}
	return errors.New("usage: openmail tokens <list|create|revoke>")
}

func cmdRules(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	switch args[0] {
	case "list", "ls":
		rs, err := app.Store.ListRules()
		if err != nil {
			return err
		}
		if len(rs) == 0 {
			fmt.Println("No rules.")
		}
		for _, r := range rs {
			on := "on "
			if !r.Enabled {
				on = "off"
			}
			var cs, as []string
			for _, c := range r.Conditions {
				cs = append(cs, fmt.Sprintf("%s %s %q", c.Field, c.Op, c.Value))
			}
			for _, a := range r.Actions {
				as = append(as, strings.TrimSpace(a.Kind+" "+a.Value))
			}
			join := " and "
			if !r.MatchAll {
				join = " or "
			}
			fmt.Printf("%-4d %s %-24s when %s → %s  [%s]\n", r.ID, on, r.Name, strings.Join(cs, join), strings.Join(as, ", "), r.Source)
		}
		return nil
	case "add":
		fs := newFlags("rules add")
		name := fs.String("name", "", "rule name")
		from := fs.String("from", "", "sender contains")
		subject := fs.String("subject", "", "subject contains")
		to := fs.String("to", "", "recipient contains")
		list := fs.String("list-id", "", "List-Id contains")
		any := fs.Bool("any", false, "match any condition instead of all")
		move := fs.String("move", "", "move to folder")
		archive := fs.Bool("archive", false, "archive")
		read := fs.Bool("mark-read", false, "mark read")
		flag := fs.Bool("flag", false, "flag")
		keyword := fs.String("keyword", "", "add keyword")
		if _, err := parse(fs, args[1:]); err != nil {
			return err
		}
		r := model.Rule{Name: *name, Enabled: true, MatchAll: !*any, Source: "manual"}
		add := func(f, v string) {
			if v != "" {
				r.Conditions = append(r.Conditions, model.Condition{Field: f, Op: "contains", Value: v})
			}
		}
		add("from", *from)
		add("subject", *subject)
		add("to", *to)
		add("list_id", *list)
		if *move != "" {
			r.Actions = append(r.Actions, model.Action{Kind: "move", Value: *move})
		}
		if *archive {
			r.Actions = append(r.Actions, model.Action{Kind: "archive"})
		}
		if *read {
			r.Actions = append(r.Actions, model.Action{Kind: "mark_read"})
		}
		if *flag {
			r.Actions = append(r.Actions, model.Action{Kind: "flag"})
		}
		if *keyword != "" {
			r.Actions = append(r.Actions, model.Action{Kind: "keyword", Value: *keyword})
		}
		if r.Name == "" {
			r.Name = "Rule " + time.Now().Format("2006-01-02 15:04")
		}
		saved, err := app.SaveRule(actor(), r)
		if err != nil {
			return err
		}
		fmt.Printf("rule %d saved. Test it on existing mail: openmail rules run --query in:inbox --dry-run\n", saved.ID)
		return nil
	case "rm", "remove":
		if len(args) != 2 {
			return errors.New("usage: openmail rules rm <id>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return err
		}
		return app.DeleteRule(actor(), id)
	case "run":
		fs := newFlags("rules run")
		q := fs.String("query", "in:inbox", "messages to run the rules on")
		dry := fs.Bool("dry-run", false, "only count matches")
		if _, err := parse(fs, args[1:]); err != nil {
			return err
		}
		n, err := app.RunRules(ctx, actor(), *q, *dry)
		if err != nil {
			return err
		}
		verb := "applied to"
		if *dry {
			verb = "would apply to"
		}
		fmt.Printf("rules %s %d message(s)\n", verb, n)
		return nil
	}
	return errors.New("usage: openmail rules <list|add|rm|run>")
}

func cmdAudit(ctx context.Context, args []string) error {
	fs := newFlags("audit")
	limit := fs.Int("limit", 50, "entries")
	who := fs.String("actor", "", "only this actor prefix (cli, web, mcp:, agent:, rules)")
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	es, err := app.Store.ListAudit(*limit, *who)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(es)
	}
	for _, e := range es {
		ok := "ok "
		if !e.OK {
			ok = "ERR"
		}
		fmt.Printf("%s %s %-16s %-24s %s %s\n", time.Unix(e.TS, 0).Format("01-02 15:04:05"), ok, e.Actor, e.Action, clip(e.Args, 70), e.Error)
	}
	return nil
}

func cmdDoctor(ctx context.Context, args []string) error {
	bad := 0
	check := func(name string, err error, fix string) {
		if err != nil {
			bad++
			fmt.Printf("FAIL  %s: %v\n      → %s\n", name, err, fix)
			return
		}
		fmt.Printf("ok    %s\n", name)
	}
	dir, err := core.DataDir()
	check("data directory", err, "set OPENMAIL_HOME to a writable folder")
	app, err := openApp()
	check("database ("+dir+")", err, "check permissions on the data directory")
	if err != nil {
		return errors.New("cannot continue without a database")
	}
	defer app.Close()
	var ic string
	check("database integrity", app.Store.DB().QueryRow(`PRAGMA quick_check`).Scan(&ic), "restore from a backup of openmail.db")
	if ic != "ok" && ic != "" {
		check("database integrity", errors.New(ic), "restore from a backup of openmail.db")
	}
	probe := "doctor-probe"
	kerr := secrets.Set("password", probe, "x")
	if kerr == nil {
		_, kerr = secrets.Get("password", probe)
		_ = secrets.Delete("password", probe)
	}
	check("keychain", kerr, "unlock the keychain, or export "+secrets.EnvName("password", "you@example.com")+" per account")
	accts, _ := app.Store.ListAccounts()
	if len(accts) == 0 {
		fmt.Println("none  no accounts: run `openmail accounts add you@example.com` or `openmail import apple`")
	}
	for _, a := range accts {
		c, cancel := context.WithTimeout(ctx, 40*time.Second)
		err := app.CheckAccount(c, a.ID)
		cancel()
		check("account "+a.ID, err, "run `openmail accounts password "+a.ID+"` (app password) or `openmail accounts login "+a.ID+"`")
	}
	if n, _ := app.Store.CountPendingOps(); n > 0 {
		fmt.Printf("note  %d change(s) waiting to reach the server (they send on the next sync)\n", n)
	}
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		_, err := os.ReadDir(home + "/Library/Mail")
		if err != nil {
			fmt.Println("note  Apple Mail's folder is not readable here; `openmail import apple` needs Full Disk Access for this terminal")
		} else {
			fmt.Println("ok    Apple Mail data readable (import apple will work)")
		}
		if _, err := exec.LookPath("security"); err != nil {
			bad++
		}
	}
	for _, p := range []string{"google", "microsoft"} {
		if app.Setting("oauth."+p+".client_id") == "" {
			fmt.Printf("note  %s OAuth sign-in is not configured (app passwords work; see docs/oauth.md)\n", p)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d problem(s) found", bad)
	}
	fmt.Println("\nAll checks passed.")
	return nil
}

func cmdFolders(ctx context.Context, args []string) error {
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	if len(args) > 0 && args[0] == "sync" {
		if len(args) != 4 || (args[3] != "on" && args[3] != "off") {
			return errors.New("usage: openmail folders sync <account> <folder> on|off")
		}
		if err := app.SetMailboxSync(actor(), args[1], args[2], args[3] == "on"); err != nil {
			return err
		}
		fmt.Printf("%s/%s: sync %s (takes effect on the next sync)\n", args[1], args[2], args[3])
		return nil
	}
	acct := ""
	if len(args) > 0 {
		acct = args[0]
	}
	mbs, err := app.Store.ListMailboxes(acct)
	if err != nil {
		return err
	}
	for _, m := range mbs {
		state := "sync"
		if !m.Subscribed {
			state = "off "
		}
		fmt.Printf("%-28s %s  %-8s %6d messages %5d unread  %s\n", m.AccountID, state, m.Role, m.Total, m.Unread, m.Name)
	}
	return nil
}
