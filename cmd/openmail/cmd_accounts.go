package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/michael-berardi/openmail/internal/apple"
	"github.com/michael-berardi/openmail/internal/core"
)

func cmdAccounts(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	switch sub {
	case "list", "ls":
		fs := newFlags("accounts list")
		asJSON := fs.Bool("json", false, "JSON output")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		accts, err := app.Store.ListAccounts()
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(accts)
		}
		if len(accts) == 0 {
			fmt.Println("No accounts yet. Add one with `openmail accounts add you@example.com` or `openmail import apple`.")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ACCOUNT\tIMAP\tSMTP\tAUTH\tSOURCE\tSTATE")
		for _, a := range accts {
			st := "on"
			if !a.Enabled {
				st = "off"
			}
			fmt.Fprintf(tw, "%s\t%s:%d\t%s:%d\t%s\t%s\t%s\n", a.Email, a.IMAPHost, a.IMAPPort, a.SMTPHost, a.SMTPPort, a.Auth, a.Source, st)
		}
		return tw.Flush()
	case "add":
		return accountsAdd(ctx, app, rest)
	case "remove", "rm":
		pos, err := parse(newFlags("accounts remove"), rest)
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("usage: openmail accounts remove <email>")
		}
		if isTTY() && !confirm("Remove "+pos[0]+" and its local copy of mail? (the server is not touched)") {
			return errors.New("cancelled")
		}
		if err := app.RemoveAccount(actor(), pos[0]); err != nil {
			return err
		}
		fmt.Println("removed", pos[0])
		return nil
	case "test":
		pos, err := parse(newFlags("accounts test"), rest)
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("usage: openmail accounts test <email>")
		}
		if err := app.CheckAccount(ctx, pos[0]); err != nil {
			return err
		}
		fmt.Println("ok: signed in to IMAP and SMTP")
		return nil
	case "login":
		pos, err := parse(newFlags("accounts login"), rest)
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("usage: openmail accounts login <email>")
		}
		return app.LoginOAuth(ctx, actor(), pos[0], func(u string) error {
			fmt.Println("Opening your browser to sign in. If nothing opens, visit:\n" + u)
			_ = openBrowser(u)
			return nil
		})
	case "password":
		pos, err := parse(newFlags("accounts password"), rest)
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("usage: openmail accounts password <email>   (reads the new password from the terminal or stdin)")
		}
		pw, err := readSecret("New password for " + pos[0] + ": ")
		if err != nil {
			return err
		}
		if err := app.SetPassword(ctx, actor(), pos[0], pw); err != nil {
			return err
		}
		fmt.Println("password updated and verified")
		return nil
	case "update":
		fs := newFlags("accounts update")
		name := fs.String("name", "", "display name")
		sig := fs.String("signature", "", "signature text")
		color := fs.String("color", "", "accent colour")
		enabled := fs.String("enabled", "", "true or false")
		days := fs.Int("sync-days", -1, "how many days of mail to keep (0 = everything)")
		pos, err := parse(fs, rest)
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("usage: openmail accounts update <email> [--name ..] [--signature ..] [--color ..] [--enabled true|false] [--sync-days N]")
		}
		var p core.AccountPatch
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "name":
				p.DisplayName = name
			case "signature":
				p.Signature = sig
			case "color":
				p.Color = color
			case "enabled":
				b := *enabled == "true"
				p.Enabled = &b
			case "sync-days":
				p.SyncDays = days
			}
		})
		a, err := app.UpdateAccount(actor(), pos[0], p)
		if err != nil {
			return err
		}
		return printJSON(a)
	}
	return fmt.Errorf("unknown accounts command %q (list, add, remove, test, login, password, update)", sub)
}

func accountsAdd(ctx context.Context, app *core.App, args []string) error {
	fs := newFlags("accounts add")
	pwEnv := fs.String("password-env", "", "read the password from this environment variable")
	pwStdin := fs.Bool("password-stdin", false, "read the password from stdin")
	name := fs.String("name", "", "display name")
	user := fs.String("username", "", "login name if it differs from the address")
	imap := fs.String("imap", "", "IMAP server host[:port] (skips discovery)")
	smtp := fs.String("smtp", "", "SMTP server host[:port]")
	imapSec := fs.String("imap-security", "", "tls | starttls")
	smtpSec := fs.String("smtp-security", "", "tls | starttls")
	noTest := fs.Bool("no-test", false, "save without signing in first")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return fmt.Errorf("usage: openmail accounts add <email> [--password-env VAR | --password-stdin] [--imap host:port --smtp host:port]")
	}
	email := strings.ToLower(pos[0])
	req := core.AddRequest{Email: email, DisplayName: *name, Username: *user, SkipTest: *noTest}
	if *imap != "" || *smtp != "" {
		if *imap == "" || *smtp == "" {
			return errors.New("give both --imap and --smtp, or neither to discover them")
		}
		req.IMAPHost, req.IMAPPort = splitHostPort(*imap, 993)
		req.SMTPHost, req.SMTPPort = splitHostPort(*smtp, 465)
		req.IMAPSecurity, req.SMTPSecurity = *imapSec, *smtpSec
		if req.IMAPSecurity == "" {
			req.IMAPSecurity = secFor(req.IMAPPort, 993)
		}
		if req.SMTPSecurity == "" {
			req.SMTPSecurity = secFor(req.SMTPPort, 465)
		}
	}
	// Discover first so the user sees what was found and which kind of password is needed.
	if req.IMAPHost == "" {
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s, derr := app.Discover(dctx, email)
		cancel()
		if derr != nil {
			return derr
		}
		fmt.Fprintf(os.Stderr, "Found %s: IMAP %s:%d, SMTP %s:%d (%s)\n", orStr(s.Provider, "your mail host"), s.IMAPHost, s.IMAPPort, s.SMTPHost, s.SMTPPort, s.Source)
		if s.Help != "" {
			fmt.Fprintln(os.Stderr, "  "+s.Help)
		}
		if s.Auth == "oauth2" {
			res, err := app.AddAccount(ctx, actor(), req)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Signing in with %s…\n", res.Account.OAuthProvider)
			if err := app.LoginOAuth(ctx, actor(), email, func(u string) error {
				fmt.Println("Opening your browser. If nothing opens, visit:\n" + u)
				_ = openBrowser(u)
				return nil
			}); err != nil {
				return err
			}
			fmt.Println("added", email)
			return nil
		}
	}
	switch {
	case *pwEnv != "":
		req.Password = os.Getenv(*pwEnv)
		if req.Password == "" {
			return fmt.Errorf("environment variable %s is empty", *pwEnv)
		}
	case *pwStdin || isTTY():
		req.Password, err = readSecret("Password for " + email + ": ")
		if err != nil {
			return err
		}
	default:
		return errors.New("no password source: use --password-env VAR or --password-stdin")
	}
	res, err := app.AddAccount(ctx, actor(), req)
	if err != nil {
		return err
	}
	fmt.Printf("added %s (%s:%d). Run `openmail serve` to sync, or `openmail sync` for one pass.\n", res.Account.Email, res.Account.IMAPHost, res.Account.IMAPPort)
	return nil
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func splitHostPort(s string, def int) (string, int) {
	if i := strings.LastIndex(s, ":"); i > 0 {
		var p int
		if _, err := fmt.Sscanf(s[i+1:], "%d", &p); err == nil {
			return s[:i], p
		}
	}
	return s, def
}

func secFor(port, tlsPort int) string {
	if port == tlsPort || port == 993 || port == 465 {
		return "tls"
	}
	return "starttls"
}

func cmdImport(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "apple" {
		return errors.New("usage: openmail import apple [--from DIR] [--only a@x,b@y] [--keychain] [--dry-run] [--yes]")
	}
	fs := newFlags("import apple")
	from := fs.String("from", "", "read a copy of the home folder instead of the real one")
	only := fs.String("only", "", "import only these addresses (comma separated)")
	keychain := fs.Bool("keychain", true, "look for saved passwords in the macOS Keychain (macOS asks you to allow each)")
	dry := fs.Bool("dry-run", false, "show what would be imported")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	diag := fs.Bool("diagnose", false, "print the shape of the macOS account database (no personal data) for bug reports")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	if *diag {
		rep, err := apple.Diagnose(*from)
		if err != nil {
			return err
		}
		fmt.Print(rep)
		return nil
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	found, err := app.ScanApple(*from)
	if err != nil {
		return err
	}
	fmt.Printf("Apple Mail has %d account(s), %d rule(s), %d VIP(s).\n", len(found.Accounts), len(found.Rules), len(found.VIPs))
	for _, c := range found.Accounts {
		fmt.Printf("  %-34s %s\n", c.Email, c.Kind)
	}
	for _, w := range found.Warnings {
		fmt.Fprintln(os.Stderr, "  note:", w)
	}
	if len(found.Accounts) == 0 && len(found.Rules) == 0 {
		return nil
	}
	if *dry {
		_, err := app.ImportApple(ctx, actor(), core.ImportOptions{Root: *from, DryRun: true})
		return err
	}
	if !*yes && !confirm("Import these into OpenMail?") {
		if !isTTY() {
			return errors.New("not a terminal: pass --yes to import")
		}
		return errors.New("cancelled")
	}
	opts := core.ImportOptions{Root: *from, TryKeychain: *keychain, Passwords: map[string]string{}}
	if *only != "" {
		opts.Only = strings.Split(*only, ",")
	}
	rep, err := app.ImportApple(ctx, actor(), opts)
	if err != nil {
		return err
	}
	// Ask for passwords the Keychain could not give us, then import those accounts.
	for _, r := range rep.Results {
		if r.Status != "needs_password" || !isTTY() {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n%s: %s\n", r.Email, r.Detail)
		pw, err := readSecret("Password (Enter to skip): ")
		if err != nil || pw == "" {
			continue
		}
		res, err := app.ImportApple(ctx, actor(), core.ImportOptions{Root: *from, Only: []string{r.Email}, Passwords: map[string]string{strings.ToLower(r.Email): pw}, SkipRules: true, SkipVIPs: true, SkipSignature: true})
		if err != nil {
			fmt.Fprintln(os.Stderr, "  failed:", err)
			continue
		}
		for _, rr := range res.Results {
			if rr.Email == r.Email {
				fmt.Printf("  %s: %s %s\n", rr.Email, rr.Status, rr.Detail)
			}
		}
	}
	fmt.Println()
	for _, r := range rep.Results {
		fmt.Printf("%-34s %-15s %s\n", r.Email, r.Status, r.Detail)
	}
	fmt.Printf("\n%d rule(s), %d signature(s), %d VIP(s) imported.\nRun `openmail serve` to start syncing.\n", rep.Rules, rep.Sigs, rep.VIPs)
	return nil
}
