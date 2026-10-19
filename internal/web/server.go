// Package web serves OpenMail's browser UI, its JSON API, live events and MCP over HTTP.
// It listens on localhost by default and requires a bearer token on every request.
package web

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/mcpserver"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/store"
)

//go:embed static
var staticFS embed.FS

type Options struct {
	Addr         string   // default 127.0.0.1:7777
	AllowedHosts []string // extra Host header values (when bound beyond localhost)
	OpenBrowser  func(url string) error
}

type Server struct {
	app    *core.App
	opts   Options
	webTok string
	hosts  map[string]bool
	mcpMu  sync.Mutex
	mcpSrv map[string]*mcp.Server
	http   *http.Server
	ln     net.Listener
}

type ctxKey int

const tokenKey ctxKey = 1

// New prepares the server. Call Serve to run it.
func New(app *core.App, o Options) (*Server, error) {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:7777"
	}
	tok, err := app.EnsureLocalToken()
	if err != nil {
		return nil, err
	}
	s := &Server{app: app, opts: o, webTok: tok, hosts: map[string]bool{}, mcpSrv: map[string]*mcp.Server{}}
	return s, nil
}

// URL is the address a person opens, including the sign-in token.
func (s *Server) URL() string {
	return "http://" + s.addr() + "/?token=" + s.webTok
}

// BaseURL is the server root without the token.
func (s *Server) BaseURL() string { return "http://" + s.addr() }

func (s *Server) addr() string {
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.opts.Addr
}

// Listen binds the socket.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w (is OpenMail already running? use --addr to pick another port)", s.opts.Addr, err)
	}
	s.ln = ln
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		s.hosts[h+":"+port] = true
	}
	for _, h := range s.opts.AllowedHosts {
		s.hosts[h] = true
	}
	return nil
}

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	s.http = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 15 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(s.ln) }()
	select {
	case <-ctx.Done():
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.http.Shutdown(sc)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ---- helpers ----

type apiErr struct {
	status int
	msg    string
}

func (e apiErr) Error() string { return e.msg }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// An empty list is [], never null: the UI iterates over it.
	if rv := reflect.ValueOf(v); rv.IsValid() && rv.Kind() == reflect.Slice && rv.IsNil() {
		v = []any{}
	}
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		status = ae.status
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 64<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return apiErr{400, "bad request body: " + err.Error()}
	}
	return nil
}

func idParam(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0, apiErr{400, "bad " + name}
	}
	return n, nil
}

type principal struct {
	tok   model.Token
	actor string
}

func (s *Server) auth(r *http.Request) (principal, error) {
	var raw string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		raw = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie("om_token"); err == nil {
		raw = c.Value
	}
	if raw == "" {
		return principal{}, apiErr{401, "sign-in required: open the URL printed by `openmail serve`, or send a bearer token"}
	}
	t, err := s.app.Authenticate(raw)
	if err != nil {
		return principal{}, apiErr{401, "invalid or revoked token"}
	}
	actor := "web"
	if t.Name != "web-ui" {
		actor = "agent:" + t.Name
	}
	return principal{tok: t, actor: actor}, nil
}

type handler func(w http.ResponseWriter, r *http.Request, p principal) error

// route wraps a handler with auth, scope and CSRF checks.
func (s *Server) route(scope string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if !model.ScopeAllows(p.tok.Scope, scope) {
			writeErr(w, apiErr{403, fmt.Sprintf("this token has %q access; %q is required", p.tok.Scope, scope)})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Cookie-authenticated browsers must prove the request came from our own page.
			if r.Header.Get("Authorization") == "" && r.Header.Get("X-OpenMail") != "1" {
				writeErr(w, apiErr{403, "missing X-OpenMail header"})
				return
			}
		}
		if err := h(w, r, p); err != nil {
			writeErr(w, err)
		}
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(sub))

	mux.HandleFunc("GET /{$}", s.index)
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.cacheless(files)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok", "version": core.Version})
	})

	mux.HandleFunc("GET /api/overview", s.route("read", s.overview))
	mux.HandleFunc("GET /api/threads", s.route("read", s.threads))
	mux.HandleFunc("GET /api/messages", s.route("read", s.messages))
	mux.HandleFunc("GET /api/thread/{id}", s.route("read", s.thread))
	mux.HandleFunc("GET /api/message/{id}", s.route("read", s.message))
	mux.HandleFunc("GET /api/message/{id}/html", s.route("read", s.messageHTML))
	mux.HandleFunc("GET /api/message/{id}/attachment/{idx}", s.route("read", s.attachment))
	mux.HandleFunc("GET /api/contacts", s.route("read", s.contacts))
	mux.HandleFunc("GET /api/events", s.route("read", s.events))
	mux.HandleFunc("GET /api/settings", s.route("read", func(w http.ResponseWriter, r *http.Request, p principal) error {
		writeJSON(w, s.app.Settings())
		return nil
	}))

	mux.HandleFunc("POST /api/act", s.route("write", s.act))
	mux.HandleFunc("POST /api/message/{id}/edit-draft", s.route("write", s.editDraft))
	mux.HandleFunc("POST /api/message/{id}/download", s.route("write", s.download))
	mux.HandleFunc("POST /api/vip", s.route("write", s.vip))
	mux.HandleFunc("POST /api/allow-remote", s.route("write", s.allowRemote))
	mux.HandleFunc("POST /api/sync", s.route("write", s.sync))
	mux.HandleFunc("POST /api/load-older", s.route("write", s.loadOlder))
	mux.HandleFunc("GET /api/drafts", s.route("read", s.drafts))
	mux.HandleFunc("POST /api/drafts", s.route("write", s.saveDraft))
	mux.HandleFunc("DELETE /api/drafts/{id}", s.route("write", s.deleteDraft))
	mux.HandleFunc("POST /api/drafts/{id}/send", s.route("send", s.sendDraft))
	mux.HandleFunc("POST /api/drafts/{id}/approve", s.route("admin", s.approveDraft))
	mux.HandleFunc("POST /api/drafts/{id}/cancel", s.route("write", s.cancelDraft))
	mux.HandleFunc("POST /api/reply", s.route("write", s.reply))
	mux.HandleFunc("POST /api/forward", s.route("write", s.forward))
	mux.HandleFunc("POST /api/unsubscribe", s.route("send", s.unsubscribe))

	mux.HandleFunc("GET /api/discover", s.route("admin", s.discover))
	mux.HandleFunc("POST /api/accounts", s.route("admin", s.addAccount))
	mux.HandleFunc("PATCH /api/accounts/{id}", s.route("admin", s.patchAccount))
	mux.HandleFunc("DELETE /api/accounts/{id}", s.route("admin", s.removeAccount))
	mux.HandleFunc("POST /api/accounts/{id}/login", s.route("admin", s.loginAccount))
	mux.HandleFunc("POST /api/accounts/{id}/password", s.route("admin", s.setPassword))
	mux.HandleFunc("POST /api/accounts/{id}/test", s.route("admin", s.testAccount))
	mux.HandleFunc("POST /api/mailboxes/sync", s.route("admin", s.mailboxSync))
	mux.HandleFunc("PUT /api/settings", s.route("admin", s.putSetting))
	mux.HandleFunc("GET /api/rules", s.route("read", s.rules))
	mux.HandleFunc("POST /api/rules", s.route("admin", s.saveRule))
	mux.HandleFunc("DELETE /api/rules/{id}", s.route("admin", s.deleteRule))
	mux.HandleFunc("POST /api/rules/run", s.route("admin", s.runRules))
	mux.HandleFunc("GET /api/audit", s.route("admin", s.audit))
	mux.HandleFunc("GET /api/tokens", s.route("admin", s.tokens))
	mux.HandleFunc("POST /api/tokens", s.route("admin", s.createToken))
	mux.HandleFunc("DELETE /api/tokens/{name}", s.route("admin", s.revokeToken))
	mux.HandleFunc("GET /api/import/apple", s.route("admin", s.scanApple))
	mux.HandleFunc("POST /api/import/apple", s.route("admin", s.importApple))

	mux.Handle("/mcp", s.mcpHandler())
	return s.guard(mux)
}

func (s *Server) cacheless(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// guard rejects requests whose Host header is not ours (DNS rebinding) and sets
// security headers.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts[r.Host] {
			http.Error(w, "unexpected Host header "+r.Host+": use http://127.0.0.1 or pass --allow-host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("token"); t != "" {
		if _, err := s.app.Authenticate(t); err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "om_token", Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600})
		dest := "/"
		if r.URL.Query().Get("desktop") == "1" {
			dest = "/?desktop=1"
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'")
	_, _ = w.Write(b)
}

// bodiesHidden reports whether this caller is an agent and the user has hidden message bodies from agents.
func (s *Server) bodiesHidden(p principal) bool {
	return core.IsAgent(p.actor) && !s.app.SettingBool("agent.expose_bodies")
}

func redact(m *model.Message) { m.Text, m.HTML, m.Snippet = "", "", "" }

// ---- reading ----

func qInt(r *http.Request, k string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
		return v
	}
	return def
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request, p principal) error {
	ov, err := s.app.Overview()
	if err != nil {
		return err
	}
	writeJSON(w, ov)
	return nil
}

func (s *Server) threads(w http.ResponseWriter, r *http.Request, p principal) error {
	ts, err := s.app.Threads(r.URL.Query().Get("q"), core.ListOptions{Limit: qInt(r, "limit", 50), Offset: qInt(r, "offset", 0), IncludeJunk: r.URL.Query().Get("junk") == "1"})
	if err != nil {
		return err
	}
	if s.bodiesHidden(p) {
		for i := range ts {
			redact(&ts[i].Latest)
		}
	}
	writeJSON(w, ts)
	return nil
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, p principal) error {
	ms, err := s.app.Search(r.URL.Query().Get("q"), core.ListOptions{Limit: qInt(r, "limit", 50), Offset: qInt(r, "offset", 0), IncludeJunk: r.URL.Query().Get("junk") == "1"})
	if err != nil {
		return err
	}
	if s.bodiesHidden(p) {
		for i := range ms {
			redact(&ms[i])
		}
	}
	writeJSON(w, ms)
	return nil
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request, p principal) error {
	ms, err := s.app.GetThread(r.PathValue("id"))
	if err != nil {
		return err
	}
	for i := range ms {
		ms[i].HasHTML = strings.TrimSpace(ms[i].HTML) != ""
		ms[i].HTML = "" // HTML is fetched separately, sanitised, in a sandboxed frame
		if s.bodiesHidden(p) {
			redact(&ms[i])
		}
	}
	writeJSON(w, ms)
	return nil
}

func (s *Server) message(w http.ResponseWriter, r *http.Request, p principal) error {
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	m, err := s.app.GetMessage(id)
	if err != nil {
		return err
	}
	m.HasHTML = strings.TrimSpace(m.HTML) != ""
	m.HTML = ""
	if s.bodiesHidden(p) {
		redact(&m)
	}
	writeJSON(w, m)
	return nil
}

func (s *Server) messageHTML(w http.ResponseWriter, r *http.Request, p principal) error {
	if s.bodiesHidden(p) {
		return apiErr{403, "message bodies are hidden from agents (agent.expose_bodies=false)"}
	}
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	body, hadRemote, blocked, err := s.app.RenderHTML(id, r.URL.Query().Get("remote") == "1")
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Remote-Content", strconv.FormatBool(blocked))
	img := "data:"
	if hadRemote && !blocked {
		img = "data: https: http:"
	}
	h.Set("Content-Security-Policy", "default-src 'none'; img-src "+img+"; style-src 'unsafe-inline'; font-src data:; sandbox allow-same-origin allow-popups allow-popups-to-escape-sandbox")
	fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><base target="_blank"><meta name="referrer" content="no-referrer">
<style>html{color-scheme:light}body{margin:0;padding:12px 4px;background:#fff;color:#111;font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;overflow-wrap:anywhere}img{max-width:100%;height:auto}table{max-width:100%}a{color:#0a58ca}blockquote{margin:0 0 0 .6em;padding-left:1em;border-left:2px solid #ccc;color:#555}img[data-blocked-src]{display:none}</style></head><body>`)
	fmt.Fprint(w, body)
	fmt.Fprint(w, `</body></html>`)
	return nil
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request, p principal) error {
	if s.bodiesHidden(p) {
		return apiErr{403, "attachments are hidden from agents (agent.expose_bodies=false)"}
	}
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		return apiErr{400, "bad attachment index"}
	}
	meta, data, err := s.app.Attachment(id, idx)
	if err != nil {
		return err
	}
	name := strings.NewReplacer(`"`, "", "\r", "", "\n", "", "/", "_", "\\", "_").Replace(meta.Filename)
	if name == "" {
		name = fmt.Sprintf("attachment-%d", idx)
	}
	h := w.Header()
	ct := meta.ContentType
	inline := r.URL.Query().Get("inline") == "1" && (ct == "image/png" || ct == "image/jpeg" || ct == "image/gif" || ct == "image/webp")
	if inline {
		h.Set("Content-Type", ct)
		h.Set("Content-Disposition", "inline")
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", `attachment; filename="`+name+`"; filename*=UTF-8''`+url.PathEscape(name))
	}
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
	return nil
}

func (s *Server) contacts(w http.ResponseWriter, r *http.Request, p principal) error {
	c, err := s.app.Store.SearchContacts(r.URL.Query().Get("q"), 8)
	if err != nil {
		return err
	}
	writeJSON(w, c)
	return nil
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, p principal) error {
	fl, ok := w.(http.Flusher)
	if !ok {
		return apiErr{500, "streaming unsupported"}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	ch, cancel := s.app.Subscribe()
	defer cancel()
	fmt.Fprint(w, ": ok\n\n")
	fl.Flush()
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-t.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

// ---- writing ----

func (s *Server) act(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		IDs    []int64 `json:"ids"`
		Action string  `json:"action"`
		Value  string  `json:"value"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.app.Act(r.Context(), p.actor, in.IDs, in.Action, in.Value); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) editDraft(w http.ResponseWriter, r *http.Request, p principal) error {
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	d, err := s.app.EditServerDraft(r.Context(), p.actor, id)
	if err != nil {
		return err
	}
	dto := draftDTO{Draft: d}
	for _, a := range d.Attachments {
		dto.Files = append(dto.Files, fileDTO{Filename: a.Filename, ContentType: a.ContentType, Size: a.Size})
	}
	dto.Attachments = nil
	writeJSON(w, dto)
	return nil
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, p principal) error {
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	if err := s.app.DownloadMessage(r.Context(), p.actor, id); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) vip(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Addr string `json:"addr"`
		VIP  bool   `json:"vip"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	err := s.app.Store.SetVIP(in.Addr, in.VIP)
	s.app.Store.Audit(p.actor, "contact.vip", in, err)
	if err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) allowRemote(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Addr string `json:"addr"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	err := s.app.Store.AllowRemote(in.Addr)
	s.app.Store.Audit(p.actor, "privacy.allow_remote", in, err)
	if err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Account string `json:"account"`
	}
	_ = decode(r, &in)
	s.app.Refresh(in.Account)
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) loadOlder(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Account string `json:"account"`
		Mailbox string `json:"mailbox"`
		Count   int    `json:"count"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	n, err := s.app.LoadOlder(r.Context(), in.Account, in.Mailbox, in.Count)
	if err != nil {
		return err
	}
	writeJSON(w, map[string]int{"loaded": n})
	return nil
}

type draftDTO struct {
	model.Draft
	Files []fileDTO `json:"files,omitempty"`
}

type fileDTO struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Data        string `json:"data_b64,omitempty"`
	Size        int    `json:"size"`
}

func (s *Server) drafts(w http.ResponseWriter, r *http.Request, p principal) error {
	ds, err := s.app.Store.ListDrafts(r.URL.Query().Get("state"))
	if err != nil {
		return err
	}
	if s.bodiesHidden(p) {
		for i := range ds {
			ds[i].Body, ds[i].HTMLBody = "", ""
		}
	}
	if id := r.URL.Query().Get("id"); id != "" {
		n, _ := strconv.ParseInt(id, 10, 64)
		d, err := s.app.Store.GetDraft(n)
		if err != nil {
			return err
		}
		if s.bodiesHidden(p) {
			d.Body, d.HTMLBody = "", ""
		}
		dto := draftDTO{Draft: d}
		for _, a := range d.Attachments {
			dto.Files = append(dto.Files, fileDTO{Filename: a.Filename, ContentType: a.ContentType, Size: a.Size})
		}
		writeJSON(w, dto)
		return nil
	}
	writeJSON(w, ds)
	return nil
}

func (s *Server) saveDraft(w http.ResponseWriter, r *http.Request, p principal) error {
	var in draftDTO
	if err := decode(r, &in); err != nil {
		return err
	}
	d := in.Draft
	if in.Files != nil {
		d.Attachments = []model.DraftAttachment{}
		for _, f := range in.Files {
			if f.Data == "" {
				// existing attachment kept: reload from the stored draft
				if d.ID != 0 {
					if old, err := s.app.Store.GetDraft(d.ID); err == nil {
						for _, oa := range old.Attachments {
							if oa.Filename == f.Filename {
								d.Attachments = append(d.Attachments, oa)
							}
						}
					}
				}
				continue
			}
			b, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				return apiErr{400, "attachment " + f.Filename + " is not valid base64"}
			}
			d.Attachments = append(d.Attachments, model.DraftAttachment{Filename: f.Filename, ContentType: f.ContentType, Size: len(b), Data: b})
		}
	}
	out, err := s.app.SaveDraft(p.actor, d)
	if err != nil {
		return err
	}
	out.Attachments = nil
	writeJSON(w, out)
	return nil
}

func (s *Server) deleteDraft(w http.ResponseWriter, r *http.Request, p principal) error {
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	if err := s.app.DeleteDraft(p.actor, id); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) sendDraft(w http.ResponseWriter, r *http.Request, p principal) error {
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	d, err := s.app.SendDraft(p.actor, id)
	if err != nil {
		return err
	}
	d.Attachments = nil
	writeJSON(w, d)
	return nil
}

func (s *Server) approveDraft(w http.ResponseWriter, r *http.Request, p principal) error {
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	d, err := s.app.ApproveDraft(p.actor, id)
	if err != nil {
		return err
	}
	d.Attachments = nil
	writeJSON(w, d)
	return nil
}

func (s *Server) cancelDraft(w http.ResponseWriter, r *http.Request, p principal) error {
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	d, err := s.app.CancelSend(p.actor, id)
	if err != nil {
		return err
	}
	d.Attachments = nil
	writeJSON(w, d)
	return nil
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		MessageID int64  `json:"message_id"`
		All       bool   `json:"all"`
		Body      string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	d, err := s.app.Reply(p.actor, in.MessageID, in.All, in.Body)
	if err != nil {
		return err
	}
	d.Attachments = nil
	writeJSON(w, d)
	return nil
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		MessageID int64  `json:"message_id"`
		Body      string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	d, err := s.app.Forward(p.actor, in.MessageID, in.Body)
	if err != nil {
		return err
	}
	dto := draftDTO{Draft: d}
	for _, a := range d.Attachments {
		dto.Files = append(dto.Files, fileDTO{Filename: a.Filename, ContentType: a.ContentType, Size: a.Size})
	}
	dto.Attachments = nil
	writeJSON(w, dto)
	return nil
}

func (s *Server) unsubscribe(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	res, err := s.app.Unsubscribe(r.Context(), p.actor, in.ID)
	if err != nil {
		return err
	}
	writeJSON(w, res)
	return nil
}

// ---- accounts ----

func (s *Server) discover(w http.ResponseWriter, r *http.Request, p principal) error {
	set, err := s.app.Discover(r.Context(), r.URL.Query().Get("email"))
	if err != nil {
		return err
	}
	writeJSON(w, set)
	return nil
}

func (s *Server) addAccount(w http.ResponseWriter, r *http.Request, p principal) error {
	var in core.AddRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	res, err := s.app.AddAccount(r.Context(), p.actor, in)
	if err != nil {
		return err
	}
	writeJSON(w, res)
	return nil
}

func (s *Server) patchAccount(w http.ResponseWriter, r *http.Request, p principal) error {
	var in core.AccountPatch
	if err := decode(r, &in); err != nil {
		return err
	}
	a, err := s.app.UpdateAccount(p.actor, r.PathValue("id"), in)
	if err != nil {
		return err
	}
	writeJSON(w, a)
	return nil
}

func (s *Server) removeAccount(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := s.app.RemoveAccount(p.actor, r.PathValue("id")); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) loginAccount(w http.ResponseWriter, r *http.Request, p principal) error {
	id := r.PathValue("id")
	urlCh := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		err := s.app.LoginOAuth(ctx, p.actor, id, func(u string) error {
			urlCh <- u
			return nil
		})
		if err != nil {
			select {
			case urlCh <- "":
			default:
			}
		}
	}()
	select {
	case u := <-urlCh:
		if u == "" {
			return apiErr{400, "sign-in could not start; check OAuth client settings (docs/oauth.md)"}
		}
		writeJSON(w, map[string]string{"auth_url": u})
	case <-time.After(10 * time.Second):
		return apiErr{500, "sign-in did not start"}
	}
	return nil
}

func (s *Server) setPassword(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.app.SetPassword(r.Context(), p.actor, r.PathValue("id"), in.Password); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) testAccount(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := s.app.CheckAccount(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) mailboxSync(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Account string `json:"account"`
		Mailbox string `json:"mailbox"`
		On      bool   `json:"on"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.app.SetMailboxSync(p.actor, in.Account, in.Mailbox, in.On); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) putSetting(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.app.SetSetting(p.actor, in.Key, in.Value); err != nil {
		return err
	}
	writeJSON(w, s.app.Settings())
	return nil
}

func (s *Server) rules(w http.ResponseWriter, r *http.Request, p principal) error {
	rs, err := s.app.Store.ListRules()
	if err != nil {
		return err
	}
	writeJSON(w, rs)
	return nil
}

func (s *Server) saveRule(w http.ResponseWriter, r *http.Request, p principal) error {
	var in model.Rule
	if err := decode(r, &in); err != nil {
		return err
	}
	out, err := s.app.SaveRule(p.actor, in)
	if err != nil {
		return err
	}
	writeJSON(w, out)
	return nil
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request, p principal) error {
	id, err := idParam(r, "id")
	if err != nil {
		return err
	}
	if err := s.app.DeleteRule(p.actor, id); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) runRules(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Query  string `json:"query"`
		DryRun bool   `json:"dry_run"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	n, err := s.app.RunRules(r.Context(), p.actor, in.Query, in.DryRun)
	if err != nil {
		return err
	}
	writeJSON(w, map[string]int{"matched": n})
	return nil
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request, p principal) error {
	e, err := s.app.Store.ListAudit(qInt(r, "limit", 100), r.URL.Query().Get("actor"))
	if err != nil {
		return err
	}
	writeJSON(w, e)
	return nil
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request, p principal) error {
	t, err := s.app.Store.ListTokens()
	if err != nil {
		return err
	}
	writeJSON(w, t)
	return nil
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Name == "web-ui" {
		return apiErr{400, "that name is reserved"}
	}
	tok, err := s.app.CreateToken(p.actor, in.Name, in.Scope)
	if err != nil {
		return err
	}
	writeJSON(w, map[string]string{"token": tok, "name": in.Name, "scope": in.Scope})
	return nil
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, p principal) error {
	name := r.PathValue("name")
	if name == "web-ui" {
		return apiErr{400, "the web UI token cannot be revoked here"}
	}
	if err := s.app.RevokeToken(p.actor, name); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

func (s *Server) scanApple(w http.ResponseWriter, r *http.Request, p principal) error {
	f, err := s.app.ScanApple(r.URL.Query().Get("from"))
	if err != nil {
		return err
	}
	writeJSON(w, f)
	return nil
}

func (s *Server) importApple(w http.ResponseWriter, r *http.Request, p principal) error {
	var in struct {
		From        string            `json:"from"`
		Only        []string          `json:"only"`
		Passwords   map[string]string `json:"passwords"`
		TryKeychain bool              `json:"try_keychain"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	pw := map[string]string{}
	for k, v := range in.Passwords {
		pw[strings.ToLower(k)] = v
	}
	rep, err := s.app.ImportApple(r.Context(), p.actor, core.ImportOptions{Root: in.From, Only: in.Only, Passwords: pw, TryKeychain: in.TryKeychain})
	if err != nil {
		return err
	}
	writeJSON(w, rep)
	return nil
}

// ---- MCP over HTTP ----

func (s *Server) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		p, ok := r.Context().Value(tokenKey).(principal)
		if !ok {
			return nil
		}
		key := p.tok.Name + "/" + p.tok.Scope
		s.mcpMu.Lock()
		defer s.mcpMu.Unlock()
		if srv, ok := s.mcpSrv[key]; ok {
			return srv
		}
		scope := p.tok.Scope
		srv := mcpserver.New(s.app, "mcp:"+p.tok.Name, scope)
		s.mcpSrv[key] = srv
		return srv
	}, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		// A browser token is a person, not an agent; MCP clients should have their own token
		if p.tok.Name == "web-ui" {
			writeErr(w, apiErr{403, "create a separate token for MCP clients: `openmail tokens create <name> --scope write`"})
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey, p)))
	})
}
