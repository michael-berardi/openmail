package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/michael-berardi/openmail/internal/apple"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/secrets"
)

// ImportOptions control `openmail import apple`.
type ImportOptions struct {
	Root          string            // home directory to read; empty = the real one
	Only          []string          // import just these addresses; empty = all found
	Passwords     map[string]string // email -> password/app password the caller already has
	TryKeychain   bool              // ask the macOS Keychain (shows its own approval dialog)
	DryRun        bool
	SkipRules     bool
	SkipSignature bool
	SkipVIPs      bool
}

type ImportResult struct {
	Email  string `json:"email"`
	Status string `json:"status"` // added | exists | needs_password | needs_oauth | failed | would_add
	Detail string `json:"detail,omitempty"`
}

type ImportReport struct {
	Found   apple.Found    `json:"found"`
	Results []ImportResult `json:"results"`
	Rules   int            `json:"rules_imported"`
	VIPs    int            `json:"vips_imported"`
	Sigs    int            `json:"signatures_imported"`
}

// secOf maps Apple's SSLEnabled flag and port to a transport: implicit TLS on the standard TLS
// ports, STARTTLS when TLS is on for another port.
func secOf(ssl bool, port int) string {
	if port == 993 || port == 465 {
		return model.SecurityTLS
	}
	if ssl {
		return model.SecuritySTARTTLS
	}
	return model.SecuritySTARTTLS
}

// ScanApple reads Apple Mail's configuration without changing anything.
func (a *App) ScanApple(root string) (apple.Found, error) { return apple.Scan(root) }

// ImportApple brings Apple Mail accounts, rules, signatures and VIPs into OpenMail.
// Accounts whose password cannot be found are reported as needs_password so the caller
// can ask for an app password and call again.
func (a *App) ImportApple(ctx context.Context, actor string, o ImportOptions) (rep ImportReport, err error) {
	defer func() {
		a.Store.Audit(actor, "import.apple", map[string]any{"accounts": len(rep.Results), "rules": rep.Rules, "dry_run": o.DryRun}, err)
	}()
	rep.Found, err = apple.Scan(o.Root)
	if err != nil {
		return rep, err
	}
	only := map[string]bool{}
	for _, e := range o.Only {
		only[strings.ToLower(strings.TrimSpace(e))] = true
	}
	existing := map[string]bool{}
	if accts, lerr := a.Store.ListAccounts(); lerr == nil {
		for _, x := range accts {
			existing[x.ID] = true
		}
	}
	idByEmail := map[string]string{}
	for _, c := range rep.Found.Accounts {
		if len(only) > 0 && !only[strings.ToLower(c.Email)] {
			continue
		}
		res := ImportResult{Email: c.Email}
		if existing[strings.ToLower(c.Email)] {
			res.Status, res.Detail = "exists", "already in OpenMail"
			idByEmail[c.Identifier] = strings.ToLower(c.Email)
			rep.Results = append(rep.Results, res)
			continue
		}
		req := AddRequest{Email: c.Email, DisplayName: c.FullName, Username: c.Username, Source: "apple"}
		if c.IMAPHost != "" && c.SMTPHost != "" {
			req.IMAPHost, req.IMAPPort, req.IMAPSecurity = c.IMAPHost, c.IMAPPort, secOf(c.IMAPSSL, c.IMAPPort)
			req.SMTPHost, req.SMTPPort, req.SMTPSecurity = c.SMTPHost, c.SMTPPort, secOf(c.SMTPSSL, c.SMTPPort)
			if req.IMAPPort == 0 {
				req.IMAPPort = 993
			}
			if req.SMTPPort == 0 {
				req.SMTPPort = 465
			}
		}
		if pw, ok := o.Passwords[strings.ToLower(c.Email)]; ok {
			req.Password = pw
		} else if o.TryKeychain && !o.DryRun {
			host := c.IMAPHost
			if host == "" {
				if s, derr := a.Discover(ctx, c.Email); derr == nil {
					host = s.IMAPHost
				}
			}
			if host != "" {
				if pw, kerr := secrets.AppleKeychainLookup(host, c.Username); kerr == nil && pw != "" {
					req.Password = pw
				}
			}
		}
		if o.DryRun {
			res.Status = "would_add"
			if req.Password == "" {
				res.Detail = "needs a password"
			}
			rep.Results = append(rep.Results, res)
			continue
		}
		// Google and Microsoft sign in with OAuth when a client is configured.
		oauthCapable := (c.Kind == "google" && a.oauthClient("google").ID != "") || (c.Kind == "microsoft" && a.oauthClient("microsoft").ID != "")
		if req.Password == "" && !oauthCapable {
			res.Status = "needs_password"
			res.Detail = passwordHint(c.Kind)
			rep.Results = append(rep.Results, res)
			continue
		}
		ar, aerr := a.AddAccount(ctx, actor, req)
		switch {
		case aerr != nil:
			res.Status, res.Detail = "failed", aerr.Error()
		case ar.NeedsOAuth:
			res.Status, res.Detail = "needs_oauth", "run `openmail accounts login "+c.Email+"` to sign in"
			idByEmail[c.Identifier] = strings.ToLower(c.Email)
		default:
			res.Status = "added"
			idByEmail[c.Identifier] = strings.ToLower(c.Email)
		}
		rep.Results = append(rep.Results, res)
	}
	if o.DryRun {
		return rep, nil
	}
	if !o.SkipSignature {
		for ident, sig := range rep.Found.Signatures {
			var targets []string
			if ident == "*" {
				for _, e := range idByEmail {
					targets = append(targets, e)
				}
			} else if e, ok := idByEmail[ident]; ok {
				targets = append(targets, e)
			}
			for _, id := range targets {
				if cur, gerr := a.Store.GetAccount(id); gerr == nil && strings.TrimSpace(cur.Signature) != "" {
					continue // never overwrite a signature that is already set
				}
				s := sig
				if _, uerr := a.UpdateAccount(actor, id, AccountPatch{Signature: &s}); uerr == nil {
					rep.Sigs++
				}
			}
		}
	}
	if !o.SkipRules {
		have := map[string]bool{}
		if rs, lerr := a.Store.ListRules(); lerr == nil {
			for _, r := range rs {
				have[r.Name] = true
			}
		}
		for _, r := range rep.Found.Rules {
			if have[r.Name] {
				continue
			}
			if strings.HasPrefix(r.AccountID, "apple:") {
				mapped := ""
				for _, c := range rep.Found.Accounts {
					if strings.EqualFold(c.Identifier, strings.TrimPrefix(r.AccountID, "apple:")) {
						mapped = strings.ToLower(c.Email)
					}
				}
				if mapped == "" {
					// an unscoped copy/move rule would run on every account and create folders on each server
					rep.Found.Warnings = append(rep.Found.Warnings, fmt.Sprintf("rule %q not imported: could not tell which account it belongs to", r.Name))
					continue
				}
				r.AccountID = mapped
			}
			// Apple Mail keeps running these on its own; enabling both would duplicate copies and moves.
			r.Enabled = false
			if _, rerr := a.SaveRule(actor, r); rerr != nil {
				rep.Found.Warnings = append(rep.Found.Warnings, fmt.Sprintf("rule %q not imported: %v", r.Name, rerr))
				continue
			}
			rep.Rules++
		}
	}
	if !o.SkipVIPs {
		for _, v := range rep.Found.VIPs {
			if verr := a.Store.SetVIP(v, true); verr == nil {
				rep.VIPs++
			}
		}
	}
	return rep, nil
}

func passwordHint(kind string) string {
	switch kind {
	case "icloud":
		return "needs an app-specific password (appleid.apple.com → Sign-In and Security → App-Specific Passwords)"
	case "google":
		return "needs a Google app password (myaccount.google.com/apppasswords), or configure OAuth (docs/oauth.md)"
	case "microsoft":
		return "needs OAuth sign-in (docs/oauth.md) or an app password if your organisation allows them"
	case "exchange":
		return "Exchange (EWS/ActiveSync) is not IMAP; use the account's IMAP settings if your server offers them"
	}
	return "needs the account password (not found in the Keychain)"
}
