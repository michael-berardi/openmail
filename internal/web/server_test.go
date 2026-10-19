package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/demo"
)

func setup(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	app, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	if err := demo.Seed(app); err != nil {
		t.Fatal(err)
	}
	s, err := New(app, Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	host := strings.TrimPrefix(ts.URL, "http://")
	s.hosts[host] = true
	return s, ts
}

func do(t *testing.T, ts *httptest.Server, method, path, tok string, hdr map[string]string, host string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(`{}`))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if host != "" {
		req.Host = host
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestAuthScopesAndHostGuard(t *testing.T) {
	s, ts := setup(t)
	if r := do(t, ts, "GET", "/api/overview", "", nil, ""); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r := do(t, ts, "GET", "/api/overview", s.webTok, nil, "evil.example"); r.StatusCode != 403 {
		t.Fatalf("rebinding host must be refused: %d", r.StatusCode)
	}
	if r := do(t, ts, "GET", "/api/overview", s.webTok, nil, ""); r.StatusCode != 200 {
		t.Fatalf("web token: %d", r.StatusCode)
	}
	ro, _ := s.app.CreateToken("test", "ro", "read")
	if r := do(t, ts, "GET", "/api/threads?q=in:inbox", ro, nil, ""); r.StatusCode != 200 {
		t.Fatalf("read token reads: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/api/act", ro, nil, ""); r.StatusCode != 403 {
		t.Fatalf("read token must not write: %d", r.StatusCode)
	}
	wr, _ := s.app.CreateToken("test", "wr", "write")
	if r := do(t, ts, "POST", "/api/drafts/1/send", wr, nil, ""); r.StatusCode != 403 {
		t.Fatalf("write token must not send: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/api/drafts/1/approve", wr, nil, ""); r.StatusCode != 403 {
		t.Fatalf("only admin approves: %d", r.StatusCode)
	}
	if r := do(t, ts, "GET", "/api/tokens", wr, nil, ""); r.StatusCode != 403 {
		t.Fatalf("write token must not list tokens: %d", r.StatusCode)
	}
	// browsers authenticate with a cookie and must send the CSRF header
	req, _ := http.NewRequest("POST", ts.URL+"/api/act", strings.NewReader(`{"ids":[1],"action":"read"}`))
	req.AddCookie(&http.Cookie{Name: "om_token", Value: s.webTok})
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 403 {
		t.Fatalf("cookie POST without X-OpenMail must fail: %d", res.StatusCode)
	}
	res.Body.Close()
	// MCP over HTTP refuses the browser token
	if r := do(t, ts, "POST", "/mcp", s.webTok, nil, ""); r.StatusCode != 403 {
		t.Fatalf("mcp with web token: %d", r.StatusCode)
	}
}

func TestMessageHTMLIsLockedDown(t *testing.T) {
	s, ts := setup(t)
	msgs, _ := s.app.Search("subject:invoice", core.ListOptions{})
	if len(msgs) != 1 {
		t.Fatalf("seed: %d", len(msgs))
	}
	r := do(t, ts, "GET", "/api/message/"+itoa(msgs[0].ID)+"/html", s.webTok, nil, "")
	csp := r.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "https:") || strings.Contains(csp, "script-src") {
		t.Fatalf("csp: %s", csp)
	}
	if !strings.Contains(csp, "sandbox") {
		t.Fatalf("csp must sandbox: %s", csp)
	}
	r2 := do(t, ts, "GET", "/api/message/"+itoa(msgs[0].ID)+"/html?remote=1", s.webTok, nil, "")
	if !strings.Contains(r2.Header.Get("Content-Security-Policy"), "img-src data: https:") {
		t.Fatalf("remote=1 should allow images: %s", r2.Header.Get("Content-Security-Policy"))
	}
}

func itoa(n int64) string {
	const d = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{d[n%10]}, b...)
	}
	return string(b)
}

func TestEmptyListsAreArraysNotNull(t *testing.T) {
	app, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	s, err := New(app, Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	s.hosts[strings.TrimPrefix(ts.URL, "http://")] = true
	for _, path := range []string{"/api/drafts", "/api/rules", "/api/threads?q=in:inbox", "/api/messages?q=in:inbox", "/api/contacts?q=x", "/api/tokens", "/api/audit"} {
		r := do(t, ts, "GET", path, s.webTok, nil, "")
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		if got := strings.TrimSpace(string(b[:n])); path != "/api/tokens" && path != "/api/audit" && got != "[]" {
			t.Errorf("%s returned %q, want []", path, got)
		}
	}
}

func TestReadTokenCannotSeeAdminTokenOrHiddenBodies(t *testing.T) {
	s, ts := setup(t)
	ro, _ := s.app.CreateToken("test", "bot", "read")
	get := func(path string) string {
		r := do(t, ts, "GET", path, ro, nil, "")
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if body := get("/api/settings"); strings.Contains(body, s.webTok) || strings.Contains(body, "_web_token") {
		t.Fatalf("settings leak the admin token: %s", body)
	}
	msgs, _ := s.app.Search("subject:invoice", core.ListOptions{})
	tid := msgs[0].ThreadID
	if body := get("/api/thread/" + tid); !strings.Contains(body, "invoice") && !strings.Contains(body, "Amount due") {
		t.Fatalf("with bodies exposed an agent reads the thread: %s", body)
	}
	if err := s.app.SetSetting("test", "agent.expose_bodies", "false"); err != nil {
		t.Fatal(err)
	}
	if body := get("/api/thread/" + tid); strings.Contains(body, "Amount due") || strings.Contains(body, "$184") {
		t.Fatalf("hidden bodies still served: %s", body)
	}
	if r := do(t, ts, "GET", "/api/message/"+itoa(msgs[0].ID)+"/html", ro, nil, ""); r.StatusCode != 403 {
		t.Fatalf("html endpoint must refuse agents when bodies are hidden: %d", r.StatusCode)
	}
	// the person using the browser is unaffected
	r := do(t, ts, "GET", "/api/thread/"+tid, s.webTok, nil, "")
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), "Amount due") {
		t.Fatalf("the user lost access to their own mail: %s", b)
	}
}
