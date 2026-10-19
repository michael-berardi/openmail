package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// fakeProvider is a minimal OAuth server: it checks PKCE and issues refresh and access tokens.
func fakeProvider(t *testing.T) (Provider, *int) {
	t.Helper()
	var challenge string
	refreshes := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" || q.Get("client_id") != "cid" || q.Get("login_hint") != "me@x.com" {
			http.Error(w, "bad auth request", 400)
			return
		}
		challenge = q.Get("code_challenge")
		redir, _ := url.Parse(q.Get("redirect_uri"))
		v := redir.Query()
		v.Set("code", "the-code")
		v.Set("state", q.Get("state"))
		redir.RawQuery = v.Encode()
		http.Redirect(w, r, redir.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "pkce mismatch"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "a1", "refresh_token": "r1", "expires_in": 3600})
		case "refresh_token":
			refreshes++
			if r.Form.Get("refresh_token") != "r1" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "a2", "expires_in": 3600})
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return Provider{Name: "fake", AuthURL: ts.URL + "/auth", TokenURL: ts.URL + "/token", Scopes: "mail"}, &refreshes
}

func TestLoginRefreshAndCache(t *testing.T) {
	keyring.MockInit()
	p, refreshes := fakeProvider(t)
	Providers["fake"] = p
	defer delete(Providers, "fake")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := Client{ID: "cid"}
	err := Login(ctx, "fake", c, "me@x.com", func(u string) error {
		go func() { // the "browser": follows the redirect back to the loopback listener
			res, err := http.Get(u)
			if err == nil {
				res.Body.Close()
			}
		}()
		return nil
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	tok, err := AccessToken(ctx, "fake", c, "me@x.com")
	if err != nil || tok != "a2" {
		t.Fatalf("access token: %q %v", tok, err)
	}
	if again, _ := AccessToken(ctx, "fake", c, "me@x.com"); again != "a2" || *refreshes != 1 {
		t.Fatalf("token must be cached: refreshes=%d", *refreshes)
	}
}

func TestLoginNeedsClientAndRejectsBadState(t *testing.T) {
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Login(ctx, "google", Client{}, "me@x.com", func(string) error { return nil }); err == nil {
		t.Fatal("login without a client id must explain what to configure")
	}
	p, _ := fakeProvider(t)
	Providers["fake2"] = p
	defer delete(Providers, "fake2")
	err := Login(ctx, "fake2", Client{ID: "cid"}, "me@x.com", func(u string) error {
		go func() {
			pu, _ := url.Parse(u)
			redir, _ := url.Parse(pu.Query().Get("redirect_uri"))
			redir.RawQuery = "code=x&state=forged"
			if res, err := http.Get(redir.String()); err == nil {
				res.Body.Close()
			}
		}()
		return nil
	})
	if err == nil {
		t.Fatal("a forged state must abort the sign-in")
	}
}

func TestXOAuth2Format(t *testing.T) {
	mech, ir, _ := (&XOAuth2{User: "u@x.com", Token: "tok"}).Start()
	if mech != "XOAUTH2" || string(ir) != "user=u@x.com\x01auth=Bearer tok\x01\x01" {
		t.Fatalf("%s %q", mech, ir)
	}
}
