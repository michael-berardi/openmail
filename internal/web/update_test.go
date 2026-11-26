package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/demo"
	"github.com/michael-berardi/openmail/internal/update"
)

type fakeInstaller struct {
	got []string
	err error
}

func (f *fakeInstaller) Install(_ context.Context, rel update.Release) error {
	f.got = append(f.got, rel.Version())
	return f.err
}

// fakeReleaseAPI serves a release record; no test touches the network.
func fakeReleaseAPI(t *testing.T, tag *string) {
	t.Helper()
	t.Setenv(update.CacheEnv, t.TempDir())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(update.Release{TagName: *tag, Body: "- Faster search\n- Fixes"})
	}))
	t.Cleanup(ts.Close)
	t.Setenv(update.APIEnv, ts.URL)
}

func desktopServer(t *testing.T, inst appInstaller, relaunch func()) (*Server, *httptest.Server) {
	t.Helper()
	app, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	if err := demo.Seed(app); err != nil {
		t.Fatal(err)
	}
	s, err := New(app, Options{Addr: "127.0.0.1:0", Desktop: true, UpdateInstaller: inst, Relaunch: relaunch})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	s.hosts[strings.TrimPrefix(ts.URL, "http://")] = true
	return s, ts
}

func statusOf(t *testing.T, r *http.Response) updateStatus {
	t.Helper()
	var st updateStatus
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || json.Unmarshal(b, &st) != nil {
		t.Fatalf("status %d: %s", r.StatusCode, b)
	}
	return st
}

func TestUpdateCheckAndSkip(t *testing.T) {
	tag := "v9.0.0"
	fakeReleaseAPI(t, &tag)
	s, ts := setup(t)
	st := statusOf(t, do(t, ts, "POST", "/api/update/check", s.webTok, nil, ""))
	if !st.Available || !st.Newer || st.Version != "9.0.0" || st.Current != core.Version || !strings.Contains(st.Notes, "Faster search") || st.Desktop || st.Auto {
		t.Fatalf("check: %+v", st)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/update/skip", strings.NewReader(`{"version":"9.0.0"}`))
	req.Header.Set("Authorization", "Bearer "+s.webTok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if st := statusOf(t, res); st.Available || !st.Skipped || !st.Newer {
		t.Fatalf("skip: %+v", st)
	}
	if st := statusOf(t, do(t, ts, "GET", "/api/update", s.webTok, nil, "")); st.Available || !st.Skipped {
		t.Fatalf("status after skip: %+v", st)
	}
	if r := do(t, ts, "POST", "/api/update/install", s.webTok, nil, ""); r.StatusCode != 400 {
		t.Fatalf("browser mode must point to `openmail update`, got %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/api/update/relaunch", s.webTok, nil, ""); r.StatusCode != 400 {
		t.Fatalf("relaunch outside the app: %d", r.StatusCode)
	}
}

func TestAgentsCannotActOnUpdates(t *testing.T) {
	tag := "v9.0.0"
	fakeReleaseAPI(t, &tag)
	inst := &fakeInstaller{}
	s, ts := desktopServer(t, inst, nil)
	bot, err := s.app.CreateToken("user", "bot", "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/update/check", "/api/update/skip", "/api/update/install", "/api/update/relaunch"} {
		if r := do(t, ts, "POST", path, bot, nil, ""); r.StatusCode != 403 {
			t.Errorf("agent %s: %d", path, r.StatusCode)
		}
	}
	if len(inst.got) != 0 {
		t.Fatal("an agent started an install")
	}
}

func TestDesktopInstallAndRelaunch(t *testing.T) {
	tag := "v9.0.0"
	fakeReleaseAPI(t, &tag)
	inst := &fakeInstaller{}
	relaunched := make(chan bool, 1)
	s, ts := desktopServer(t, inst, func() { relaunched <- true })
	if st := statusOf(t, do(t, ts, "POST", "/api/update/check", s.webTok, nil, "")); !st.Desktop || !st.Available {
		t.Fatalf("desktop status: %+v", st)
	}
	r := do(t, ts, "POST", "/api/update/install", s.webTok, nil, "")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), `"installed":"9.0.0"`) || len(inst.got) != 1 || inst.got[0] != "9.0.0" {
		t.Fatalf("install: %d %s %v", r.StatusCode, b, inst.got)
	}
	if r := do(t, ts, "POST", "/api/update/relaunch", s.webTok, nil, ""); r.StatusCode != 200 || !<-relaunched {
		t.Fatalf("relaunch: %d", r.StatusCode)
	}

	inst.err = errors.New("update rejected: signed by Team ID OTHER")
	if r := do(t, ts, "POST", "/api/update/install", s.webTok, nil, ""); r.StatusCode != 500 {
		t.Fatalf("failed install: %d", r.StatusCode)
	}
	if log, _ := os.ReadFile(update.LogPath()); !strings.Contains(string(log), "Team ID OTHER") {
		t.Fatalf("failure cause not logged: %q", log)
	}
	tag = "v" + core.Version
	if r := do(t, ts, "POST", "/api/update/install", s.webTok, nil, ""); r.StatusCode != 409 {
		t.Fatalf("install when current: %d", r.StatusCode)
	}
}

func TestUpdatePromptCopy(t *testing.T) {
	js, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"OpenMail ${st.version} is available — you have ${st.current}.",
		"'Install Update'", "'Remind Me Later'", "'Skip This Version'",
		"'Automatically download and install updates in the future'",
		"label: 'Check for updates automatically'", "label: 'Install updates automatically'", "'Check Now'",
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js is missing %q", want)
		}
	}
	if !strings.Contains(core.Defaults["update.check"], "true") || core.Defaults["update.auto"] != "false" {
		t.Error("defaults: checks on, automatic install off")
	}
}
