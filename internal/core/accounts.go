package core

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/michael-berardi/openmail/internal/discover"
	"github.com/michael-berardi/openmail/internal/imapx"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/oauth"
	"github.com/michael-berardi/openmail/internal/secrets"
)

// AddRequest describes a new account. Only Email (and usually Password) is required:
// server settings are discovered unless supplied.
type AddRequest struct {
	Email        string `json:"email"`
	DisplayName  string `json:"display_name,omitempty"`
	Password     string `json:"password,omitempty"`
	Username     string `json:"username,omitempty"`
	IMAPHost     string `json:"imap_host,omitempty"`
	IMAPPort     int    `json:"imap_port,omitempty"`
	IMAPSecurity string `json:"imap_security,omitempty"`
	SMTPHost     string `json:"smtp_host,omitempty"`
	SMTPPort     int    `json:"smtp_port,omitempty"`
	SMTPSecurity string `json:"smtp_security,omitempty"`
	Color        string `json:"color,omitempty"`
	Source       string `json:"source,omitempty"`
	SkipTest     bool   `json:"skip_test,omitempty"`
	SyncDays     int    `json:"sync_days,omitempty"`
}

type AddResult struct {
	Account    model.Account     `json:"account"`
	Settings   discover.Settings `json:"settings"`
	Warnings   []string          `json:"warnings,omitempty"`
	NeedsOAuth bool              `json:"needs_oauth,omitempty"`
}

// Discover finds server settings for an address (no state changes).
func (a *App) Discover(ctx context.Context, email string) (discover.Settings, error) {
	s, err := discover.Discover(ctx, email)
	if err != nil {
		return s, err
	}
	if s.Auth == model.AuthOAuth2 && a.oauthClient(s.OAuthProvider).ID == "" {
		s.Help = fmt.Sprintf("OpenMail has no %s sign-in configured yet. Use an app password (works now), or register an OAuth client (docs/oauth.md).", s.OAuthProvider)
		s.Auth = model.AuthPassword
		s.OAuthProvider = ""
	}
	return s, nil
}

var accentColors = []string{"#0a84ff", "#30d158", "#ff9f0a", "#bf5af2", "#ff453a", "#64d2ff", "#ffd60a", "#ac8e68"}

// AddAccount discovers settings, verifies the login against the real servers, stores the
// password in the keychain and saves the account. Nothing is saved if verification fails.
func (a *App) AddAccount(ctx context.Context, actor string, r AddRequest) (res AddResult, err error) {
	defer func() { a.Store.Audit(actor, "account.add", map[string]string{"email": r.Email}, err) }()
	r.Email = strings.ToLower(strings.TrimSpace(r.Email))
	if _, perr := mail.ParseAddress(r.Email); perr != nil {
		return res, fmt.Errorf("%q is not a valid email address", r.Email)
	}
	if _, gerr := a.Store.GetAccount(r.Email); gerr == nil {
		return res, fmt.Errorf("account %s already exists (remove it first to re-add)", r.Email)
	}
	var s discover.Settings
	if r.IMAPHost == "" || r.SMTPHost == "" {
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s, err = a.Discover(dctx, r.Email)
		cancel()
		if err != nil {
			return res, err
		}
	} else {
		s.Source, s.Auth = "manual", model.AuthPassword
	}
	acct := model.Account{ID: r.Email, Email: r.Email, Name: r.Email, DisplayName: r.DisplayName, Enabled: true, Source: r.Source, SyncDays: r.SyncDays, Color: r.Color}
	if acct.Source == "" {
		acct.Source = "discovered"
		if s.Source == "manual" {
			acct.Source = "manual"
		}
	}
	if acct.DisplayName == "" {
		acct.DisplayName = strings.Title(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(r.Email[:strings.Index(r.Email, "@")])) //nolint:staticcheck
	}
	if n, _ := a.Store.ListAccounts(); acct.Color == "" {
		acct.Color = accentColors[len(n)%len(accentColors)]
	}
	s.Apply(&acct)
	if r.Username != "" {
		acct.Username = r.Username
	}
	if acct.Username == "" {
		acct.Username = r.Email
	}
	if r.IMAPHost != "" {
		acct.IMAPHost, acct.IMAPPort, acct.IMAPSecurity = r.IMAPHost, r.IMAPPort, r.IMAPSecurity
	}
	if r.SMTPHost != "" {
		acct.SMTPHost, acct.SMTPPort, acct.SMTPSecurity = r.SMTPHost, r.SMTPPort, r.SMTPSecurity
	}
	if acct.IMAPPort == 0 {
		acct.IMAPPort = 993
	}
	if acct.IMAPSecurity == "" {
		acct.IMAPSecurity = model.SecurityTLS
	}
	if acct.SMTPPort == 0 {
		acct.SMTPPort = 465
	}
	if acct.SMTPSecurity == "" {
		acct.SMTPSecurity = model.SecurityTLS
	}
	res.Settings = s
	res.Account = acct
	if s.Help != "" {
		res.Warnings = append(res.Warnings, s.Help)
	}

	if acct.Auth == model.AuthOAuth2 {
		// OAuth sign-in happens in the browser; the caller finishes with LoginOAuth.
		if err := a.Store.PutAccount(acct); err != nil {
			return res, err
		}
		res.NeedsOAuth = true
		return res, nil
	}
	if r.Password == "" {
		return res, fmt.Errorf("a password is required for %s%s", acct.Email, helpSuffix(s))
	}
	if !r.SkipTest {
		if err := a.TestAccount(ctx, acct, imapx.Creds{Password: r.Password}); err != nil {
			// Zoho paid plans use imappro/smtppro; the free plan uses imap/smtp. Try the other one.
			alt := acct
			if r.IMAPHost == "" && strings.HasPrefix(acct.IMAPHost, "imap.zoho.") {
				alt.IMAPHost = strings.Replace(acct.IMAPHost, "imap.", "imappro.", 1)
				alt.SMTPHost = strings.Replace(acct.SMTPHost, "smtp.", "smtppro.", 1)
				if a.TestAccount(ctx, alt, imapx.Creds{Password: r.Password}) == nil {
					acct = alt
					res.Account = acct
					err = nil
				}
			}
			if err != nil {
				return res, fmt.Errorf("%w%s", err, helpSuffix(s))
			}
		}
	}
	if err := secrets.Set("password", acct.ID, r.Password); err != nil {
		return res, err
	}
	if err := a.Store.PutAccount(acct); err != nil {
		return res, err
	}
	a.emit(Event{Type: "account", Account: acct.ID})
	a.startAccount(acct)
	return res, nil
}

func helpSuffix(s discover.Settings) string {
	if s.Help != "" {
		return " — " + s.Help
	}
	return ""
}

// TestAccount signs in to IMAP and SMTP without changing anything.
func (a *App) TestAccount(ctx context.Context, acct model.Account, cr imapx.Creds) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	c, err := imapx.DialIMAP(ctx, acct, cr)
	if err != nil {
		return err
	}
	_ = c.Logout().Wait()
	c.Close()
	return imapx.TestSMTP(ctx, acct, cr)
}

// CheckAccount tests a stored account using its saved credentials.
func (a *App) CheckAccount(ctx context.Context, id string) error {
	acct, err := a.Store.GetAccount(id)
	if err != nil {
		return err
	}
	cr, err := a.Creds(ctx, acct)
	if err != nil {
		return err
	}
	return a.TestAccount(ctx, acct, cr)
}

// LoginOAuth runs the browser sign-in for an OAuth account and starts syncing it.
func (a *App) LoginOAuth(ctx context.Context, actor, id string, openURL func(string) error) (err error) {
	defer func() { a.Store.Audit(actor, "account.login", map[string]string{"account": id}, err) }()
	acct, err := a.Store.GetAccount(id)
	if err != nil {
		return err
	}
	if acct.Auth != model.AuthOAuth2 {
		return fmt.Errorf("%s uses a password, not OAuth", id)
	}
	if err := oauth.Login(ctx, acct.OAuthProvider, a.oauthClient(acct.OAuthProvider), acct.ID, openURL); err != nil {
		return err
	}
	if err := a.CheckAccount(ctx, id); err != nil {
		return err
	}
	a.emit(Event{Type: "account", Account: id})
	a.startAccount(acct)
	return nil
}

// SetPassword replaces the stored password (after verifying it).
func (a *App) SetPassword(ctx context.Context, actor, id, password string) (err error) {
	defer func() { a.Store.Audit(actor, "account.password", map[string]string{"account": id}, err) }()
	acct, err := a.Store.GetAccount(id)
	if err != nil {
		return err
	}
	if err := a.TestAccount(ctx, acct, imapx.Creds{Password: password}); err != nil {
		return err
	}
	if err := secrets.Set("password", acct.ID, password); err != nil {
		return err
	}
	a.startAccount(acct)
	return nil
}

// UpdateAccount changes editable fields.
type AccountPatch struct {
	DisplayName *string `json:"display_name,omitempty"`
	Signature   *string `json:"signature,omitempty"`
	Color       *string `json:"color,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	SyncDays    *int    `json:"sync_days,omitempty"`
	Name        *string `json:"name,omitempty"`
}

func (a *App) UpdateAccount(actor, id string, p AccountPatch) (acct model.Account, err error) {
	defer func() { a.Store.Audit(actor, "account.update", map[string]any{"account": id, "patch": p}, err) }()
	acct, err = a.Store.GetAccount(id)
	if err != nil {
		return acct, err
	}
	if p.DisplayName != nil {
		acct.DisplayName = *p.DisplayName
	}
	if p.Signature != nil {
		acct.Signature = *p.Signature
	}
	if p.Color != nil {
		acct.Color = *p.Color
	}
	if p.Name != nil {
		acct.Name = *p.Name
	}
	if p.SyncDays != nil {
		acct.SyncDays = *p.SyncDays
	}
	if p.Enabled != nil {
		acct.Enabled = *p.Enabled
	}
	if err = a.Store.PutAccount(acct); err != nil {
		return acct, err
	}
	if !acct.Enabled {
		a.stopAccount(acct.ID)
	} else {
		a.startAccount(acct)
	}
	a.emit(Event{Type: "account", Account: id})
	return acct, nil
}

// RemoveAccount deletes the account, its local mail and its stored secrets.
// The mailbox on the server is not touched.
func (a *App) RemoveAccount(actor, id string) (err error) {
	defer func() { a.Store.Audit(actor, "account.remove", map[string]string{"account": id}, err) }()
	acct, err := a.Store.GetAccount(id)
	if err != nil {
		return err
	}
	a.stopAccount(acct.ID)
	a.dropOpsConn(acct.ID)
	if err = a.Store.DeleteAccount(acct.ID); err != nil {
		return err
	}
	for _, k := range []string{"password", "oauth-refresh"} {
		if derr := secrets.Delete(k, acct.ID); derr != nil && !errors.Is(derr, nil) {
			a.Store.Audit(actor, "account.remove.secret", map[string]string{"account": id, "kind": k}, derr)
		}
	}
	a.emit(Event{Type: "account", Account: id})
	return nil
}
