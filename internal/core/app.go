// Package core is OpenMail's service layer. The CLI, web UI and MCP server are thin
// shells over App, so every surface behaves identically and every change is audited.
package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/michael-berardi/openmail/internal/imapx"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/oauth"
	"github.com/michael-berardi/openmail/internal/secrets"
	"github.com/michael-berardi/openmail/internal/store"
)

// Version is set at build time.
var Version = "0.1.0"

// DataDir returns the directory holding the database. OPENMAIL_HOME overrides it.
func DataDir() (string, error) {
	if d := os.Getenv("OPENMAIL_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "OpenMail"), nil
	case "windows":
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "OpenMail"), nil
		}
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "openmail"), nil
	}
	return filepath.Join(home, ".local", "share", "openmail"), nil
}

// Settings defaults. Every key can be changed with `openmail config set`, in the web
// UI, or by an agent with admin scope.
var Defaults = map[string]string{
	"sync.days":                  "90",    // how far back the first sync reaches; 0 = everything
	"sync.max_per_mailbox":       "10000", // safety cap for the first sync
	"sync.max_message_mb":        "25",    // larger messages are fetched on demand; 0 = no limit
	"sync.poll_seconds":          "300",   // full refresh interval for non-INBOX folders
	"send.undo_seconds":          "10",
	"send.default_account":       "",
	"agent.send_policy":          "approve", // draft_only | approve | allow
	"agent.max_sends_hour":       "20",
	"agent.allowed_domains":      "",            // comma list; empty = any recipient domain (when send is permitted)
	"agent.expose_bodies":        "true",        // false hides bodies from agents, showing headers and snippets only
	"privacy.remote_content":     "block",       // block | allow
	"ui.theme":                   "system",      // system | light | dark | high-contrast
	"ui.density":                 "comfortable", // compact | comfortable | spacious
	"ui.layout":                  "three-pane",  // three-pane | two-pane | list-only
	"ui.font_scale":              "1",
	"ui.accent":                  "#0a84ff",
	"ui.threads":                 "true",
	"ui.mark_read_seconds":       "1",
	"ui.preview_lines":           "2",
	"ui.reduce_motion":           "system",
	"ui.keybindings":             "", // JSON object overriding defaults
	"oauth.google.client_id":     "",
	"oauth.google.client_secret": "",
	"oauth.microsoft.client_id":  "",
}

// SecretSettings are never shown back in full.
var SecretSettings = map[string]bool{"oauth.google.client_secret": true}

type Event struct {
	Type    string `json:"type"` // sync | message | account | outbox | error
	Account string `json:"account,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type AccountStatus struct {
	Account  string `json:"account"`
	State    string `json:"state"` // idle | syncing | connecting | error | disabled
	LastSync int64  `json:"last_sync"`
	Error    string `json:"error,omitempty"`
	Progress string `json:"progress,omitempty"`
}

type App struct {
	Store *store.Store
	Dir   string

	mu     sync.Mutex
	status map[string]*AccountStatus
	subs   map[int]chan Event
	nsub   int

	opsMu   sync.Mutex
	opsConn map[string]*imapx.Conn

	engineMu  sync.Mutex
	engineCtx context.Context
	running   map[string]context.CancelFunc
	kicks     map[string]chan struct{}

	draftMu     sync.Mutex
	pushMu      sync.Mutex // serialises server-side draft replacement
	draftTimers map[int64]*time.Timer
}

// Open opens the database in the default data directory.
func Open() (*App, error) {
	dir, err := DataDir()
	if err != nil {
		return nil, fmt.Errorf("find data directory: %w", err)
	}
	return OpenAt(dir)
}

func OpenAt(dir string) (*App, error) {
	st, err := store.Open(filepath.Join(dir, "openmail.db"))
	if err != nil {
		return nil, err
	}
	return &App{Store: st, Dir: dir, status: map[string]*AccountStatus{}, subs: map[int]chan Event{}, opsConn: map[string]*imapx.Conn{}, running: map[string]context.CancelFunc{}, draftTimers: map[int64]*time.Timer{}}, nil
}

func (a *App) Close() error {
	fctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	a.FlushDrafts(fctx)
	cancel()
	a.engineMu.Lock()
	for _, c := range a.running {
		c()
	}
	a.engineMu.Unlock()
	a.opsMu.Lock()
	for _, c := range a.opsConn {
		c.Close()
	}
	a.opsMu.Unlock()
	return a.Store.Close()
}

// ---- events ----

func (a *App) Subscribe() (<-chan Event, func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.nsub
	a.nsub++
	ch := make(chan Event, 32)
	a.subs[id] = ch
	return ch, func() {
		a.mu.Lock()
		delete(a.subs, id)
		a.mu.Unlock()
	}
}

func (a *App) emit(e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ch := range a.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (a *App) setStatus(acct string, f func(*AccountStatus)) {
	a.mu.Lock()
	s := a.status[acct]
	if s == nil {
		s = &AccountStatus{Account: acct, State: "idle"}
		a.status[acct] = s
	}
	f(s)
	a.mu.Unlock()
	a.emit(Event{Type: "sync", Account: acct})
}

func (a *App) Statuses() []AccountStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []AccountStatus
	for _, s := range a.status {
		out = append(out, *s)
	}
	return out
}

// ---- settings ----

func (a *App) Setting(k string) string {
	if v, ok, err := a.Store.GetSetting(k); err == nil && ok {
		return v
	}
	return Defaults[k]
}

func (a *App) SettingInt(k string) int {
	n, err := strconv.Atoi(strings.TrimSpace(a.Setting(k)))
	if err != nil {
		n, _ = strconv.Atoi(Defaults[k])
	}
	return n
}

func (a *App) SettingBool(k string) bool {
	return strings.EqualFold(strings.TrimSpace(a.Setting(k)), "true")
}

// Settings returns every setting, merged over defaults, with secrets masked.
func (a *App) Settings() map[string]string {
	out := map[string]string{}
	for k, v := range Defaults {
		out[k] = v
	}
	if all, err := a.Store.AllSettings(); err == nil {
		for k, v := range all {
			if !strings.HasPrefix(k, "_") { // underscore keys are internal, never shown to any client
				out[k] = v
			}
		}
	}
	for k := range SecretSettings {
		if out[k] != "" {
			out[k] = "••••••••"
		}
	}
	return out
}

func (a *App) SetSetting(actor, k, v string) error {
	if IsAgent(actor) && (strings.HasPrefix(k, "agent.") || strings.HasPrefix(k, "oauth.") || strings.HasPrefix(k, "send.")) {
		err := fmt.Errorf("agents cannot change %s (permissions, sign-in and sending settings are the user's); ask the user", k)
		a.Store.Audit(actor, "setting.set", map[string]string{"key": k}, err)
		return err
	}
	if _, ok := Defaults[k]; !ok {
		var keys []string
		for d := range Defaults {
			keys = append(keys, d)
		}
		return fmt.Errorf("unknown setting %q", k)
	}
	err := a.validateSetting(k, v)
	if err == nil {
		err = a.Store.SetSetting(k, v)
	}
	logged := v
	if SecretSettings[k] {
		logged = "(hidden)"
	}
	a.Store.Audit(actor, "setting.set", map[string]string{"key": k, "value": logged}, err)
	if err == nil {
		a.emit(Event{Type: "settings"})
	}
	return err
}

func (a *App) validateSetting(k, v string) error {
	enum := map[string][]string{
		"agent.send_policy":      {"draft_only", "approve", "allow"},
		"privacy.remote_content": {"block", "allow"},
		"ui.theme":               {"system", "light", "dark", "high-contrast"},
		"ui.density":             {"compact", "comfortable", "spacious"},
		"ui.layout":              {"three-pane", "two-pane", "list-only"},
	}
	if opts, ok := enum[k]; ok {
		for _, o := range opts {
			if o == v {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of %s", k, strings.Join(opts, ", "))
	}
	for _, nk := range []string{"sync.days", "sync.max_per_mailbox", "sync.max_message_mb", "sync.poll_seconds", "send.undo_seconds", "agent.max_sends_hour", "ui.mark_read_seconds", "ui.preview_lines"} {
		if k == nk {
			if n, err := strconv.Atoi(v); err != nil || n < 0 {
				return fmt.Errorf("%s must be a whole number ≥ 0", k)
			}
		}
	}
	if k == "ui.font_scale" {
		if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0.6 || f > 3 {
			return fmt.Errorf("ui.font_scale must be between 0.6 and 3")
		}
	}
	return nil
}

// ---- credentials ----

func (a *App) oauthClient(provider string) oauth.Client {
	return oauth.Client{ID: strings.TrimSpace(a.Setting("oauth." + provider + ".client_id")), Secret: strings.TrimSpace(a.Setting("oauth." + provider + ".client_secret"))}
}

// Creds resolves the credentials for an account (refreshing OAuth tokens).
func (a *App) Creds(ctx context.Context, acct model.Account) (imapx.Creds, error) {
	if acct.Auth == model.AuthOAuth2 {
		t, err := oauth.AccessToken(ctx, acct.OAuthProvider, a.oauthClient(acct.OAuthProvider), acct.ID)
		if err != nil {
			return imapx.Creds{}, err
		}
		return imapx.Creds{Token: t}, nil
	}
	pw, err := secrets.Get("password", acct.ID)
	if err != nil {
		return imapx.Creds{}, err
	}
	return imapx.Creds{Password: pw}, nil
}

// Dial opens a fresh IMAP connection for the account.
func (a *App) Dial(ctx context.Context, acct model.Account) (*imapx.Conn, error) {
	cr, err := a.Creds(ctx, acct)
	if err != nil {
		return nil, err
	}
	return imapx.DialIMAP(ctx, acct, cr)
}

// now is replaceable in tests.
var now = time.Now
