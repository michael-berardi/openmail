// Package oauth signs in to Google and Microsoft mail with OAuth 2.0 (loopback + PKCE)
// and produces XOAUTH2 SASL credentials. OpenMail ships no client IDs: you register your
// own (see docs/oauth.md) and enter them once with `openmail config set`.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/michael-berardi/openmail/internal/secrets"
)

type Provider struct {
	Name     string
	AuthURL  string
	TokenURL string
	Scopes   string
	Extra    map[string]string
}

var Providers = map[string]Provider{
	"google": {Name: "google", AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		Scopes: "https://mail.google.com/", Extra: map[string]string{"access_type": "offline", "prompt": "consent"}},
	"microsoft": {Name: "microsoft", AuthURL: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize", TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		Scopes: "https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send offline_access", Extra: map[string]string{"prompt": "select_account"}},
}

// Client identifies the registered OAuth application.
type Client struct {
	ID     string
	Secret string
}

func rnd(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func postToken(ctx context.Context, p Provider, form url.Values) (tokenResp, error) {
	var tr tokenResp
	req, err := http.NewRequestWithContext(ctx, "POST", p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tr, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return tr, fmt.Errorf("%s token endpoint: %w", p.Name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &tr); err != nil {
		return tr, fmt.Errorf("%s token endpoint returned HTTP %d: %s", p.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if tr.Error != "" {
		return tr, fmt.Errorf("%s token endpoint: %s: %s", p.Name, tr.Error, tr.ErrorDesc)
	}
	return tr, nil
}

// Login runs the browser sign-in. openURL receives the authorisation URL; the caller opens
// it (or prints it). The refresh token is stored in the keychain for account.
func Login(ctx context.Context, provider string, c Client, account string, openURL func(string) error) error {
	p, ok := Providers[provider]
	if !ok {
		return fmt.Errorf("unknown OAuth provider %q", provider)
	}
	if c.ID == "" {
		return fmt.Errorf("no %[1]s OAuth client id configured: register one (docs/oauth.md) then run `openmail config set oauth.%[1]s.client_id <id>`", provider)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start loopback listener: %w", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	verifier := rnd(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := rnd(16)
	q := url.Values{
		"client_id": {c.ID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {p.Scopes},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "login_hint": {account},
	}
	for k, v := range p.Extra {
		q.Set(k, v)
	}
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", 400)
			errCh <- errors.New("oauth state mismatch")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, e, 400)
			errCh <- fmt.Errorf("sign-in refused: %s %s", e, r.URL.Query().Get("error_description"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>OpenMail</title><body style="font-family:system-ui;margin:4rem auto;max-width:28rem"><h1>Signed in</h1><p>You can close this tab and return to OpenMail.</p>`)
		codeCh <- r.URL.Query().Get("code")
	})}
	go srv.Serve(ln)
	defer srv.Close()
	if err := openURL(p.AuthURL + "?" + q.Encode()); err != nil {
		return err
	}
	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for browser sign-in (5 minutes)")
	case <-ctx.Done():
		return ctx.Err()
	}
	form := url.Values{"client_id": {c.ID}, "code": {code}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}, "code_verifier": {verifier}}
	if c.Secret != "" {
		form.Set("client_secret", c.Secret)
	}
	tr, err := postToken(ctx, p, form)
	if err != nil {
		return err
	}
	if tr.RefreshToken == "" {
		return errors.New("provider returned no refresh token; remove OpenMail from the account's connected apps and sign in again")
	}
	return secrets.Set("oauth-refresh", account, tr.RefreshToken)
}

var (
	mu    sync.Mutex
	cache = map[string]cached{}
)

type cached struct {
	token   string
	expires time.Time
}

// AccessToken returns a valid access token for account, refreshing as needed.
func AccessToken(ctx context.Context, provider string, c Client, account string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if t, ok := cache[account]; ok && time.Until(t.expires) > time.Minute {
		return t.token, nil
	}
	p, ok := Providers[provider]
	if !ok {
		return "", fmt.Errorf("unknown OAuth provider %q", provider)
	}
	rt, err := secrets.Get("oauth-refresh", account)
	if err != nil {
		return "", err
	}
	form := url.Values{"client_id": {c.ID}, "refresh_token": {rt}, "grant_type": {"refresh_token"}}
	if c.Secret != "" {
		form.Set("client_secret", c.Secret)
	}
	if provider == "microsoft" {
		form.Set("scope", p.Scopes)
	}
	tr, err := postToken(ctx, p, form)
	if err != nil {
		return "", fmt.Errorf("refresh sign-in for %s: %w (run `openmail accounts login %s`)", account, err, account)
	}
	if tr.RefreshToken != "" && tr.RefreshToken != rt {
		_ = secrets.Set("oauth-refresh", account, tr.RefreshToken)
	}
	cache[account] = cached{token: tr.AccessToken, expires: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)}
	return tr.AccessToken, nil
}

// XOAuth2 is the SASL XOAUTH2 mechanism used by Gmail and Microsoft for IMAP and SMTP.
type XOAuth2 struct{ User, Token string }

func (x *XOAuth2) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + x.User + "\x01auth=Bearer " + x.Token + "\x01\x01"), nil
}

// Next answers the server's error challenge with an empty response, as the spec requires.
func (x *XOAuth2) Next(challenge []byte) ([]byte, error) {
	return nil, fmt.Errorf("XOAUTH2 rejected: %s", string(challenge))
}
