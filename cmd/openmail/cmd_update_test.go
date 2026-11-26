package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/update"
)

// isolate points the data and cache folders at temp dirs, serves a fake
// release record and counts background starts. Nothing touches the network.
func isolate(t *testing.T, tag string) (home string, spawns *int, release *string) {
	t.Helper()
	home, cache := t.TempDir(), t.TempDir()
	t.Setenv("OPENMAIL_HOME", home)
	t.Setenv(update.CacheEnv, cache)
	for _, k := range []string{update.NoCheckEnv, "CI", "CLAUDECODE", "CODEX_SANDBOX", "GEMINI_CLI"} {
		t.Setenv(k, "")
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(update.Release{TagName: tag, Body: "- Faster search"})
	}))
	t.Cleanup(ts.Close)
	t.Setenv(update.APIEnv, ts.URL)
	n := 0
	old := spawnCheck
	spawnCheck = func() error { n++; return nil }
	t.Cleanup(func() { spawnCheck = old })
	return home, &n, &tag
}

func TestNoticeOnlyForPeopleAtATerminal(t *testing.T) {
	for _, args := range [][]string{
		{"ls", "--json"}, {"read", "12", "-json"}, {"thread", "3", "--json=true"},
		{"mcp", "--scope", "write"}, {"update"}, {"update", "--check"}, {"help"}, nil,
	} {
		if noticeAllowed(args, true) {
			t.Errorf("notice allowed for %v", args)
		}
	}
	if !noticeAllowed([]string{"ls", "is:unread"}, true) || !noticeAllowed([]string{"send", "--", "--json"}, true) {
		t.Error("interactive commands should get the notice")
	}
	if noticeAllowed([]string{"ls"}, false) {
		t.Error("a redirected or non-terminal run must stay silent")
	}
}

func TestNoticeAfterCommandFromCacheOnly(t *testing.T) {
	home, spawns, _ := isolate(t, "v9.0.0")

	// Nothing cached yet: no output, one detached check, no database created.
	var out bytes.Buffer
	updateNotice(&out, []string{"ls"}, true)
	if out.Len() != 0 || *spawns != 1 {
		t.Fatalf("first run: %q, spawns=%d", out.String(), *spawns)
	}
	updateNotice(&out, []string{"ls"}, true)
	if *spawns != 1 {
		t.Fatal("the daily check started twice")
	}
	if _, err := os.Stat(filepath.Join(home, "openmail.db")); !os.IsNotExist(err) {
		t.Fatal("reading update settings created a database")
	}

	// The background check found 9.0.0; the next command shows it once.
	if _, err := update.Fetch(); err != nil {
		t.Fatal(err)
	}
	for _, quiet := range []func(){
		func() { updateNotice(&out, []string{"ls", "--json"}, true) },
		func() { updateNotice(&out, []string{"ls"}, false) },
		func() { t.Setenv("CI", "1"); updateNotice(&out, []string{"ls"}, true); t.Setenv("CI", "") },
		func() {
			t.Setenv(update.NoCheckEnv, "1")
			updateNotice(&out, []string{"ls"}, true)
			t.Setenv(update.NoCheckEnv, "")
		},
	} {
		quiet()
		if out.Len() != 0 {
			t.Fatalf("notice where it must be silent: %q", out.String())
		}
	}
	updateNotice(&out, []string{"version"}, true)
	want := "openmail 9.0.0 available (you have " + core.Version + ") · run: openmail update\n" +
		"\x1b[2mturn on automatic updates: openmail update --auto on · skip: openmail update --skip\x1b[0m\n"
	if out.String() != want {
		t.Fatalf("notice:\n%q\nwant\n%q", out.String(), want)
	}
	out.Reset()
	updateNotice(&out, []string{"ls"}, true)
	if out.Len() != 0 {
		t.Fatal("the same notice appeared twice within 24 hours")
	}
}

func TestSettingsTurnNoticeOff(t *testing.T) {
	home, spawns, _ := isolate(t, "v9.0.0")
	app, err := core.OpenAt(home)
	if err != nil {
		t.Fatal(err)
	}
	app.SetSetting("user", "update.check", "false")
	app.Close()
	var out bytes.Buffer
	updateNotice(&out, []string{"ls"}, true)
	if out.Len() != 0 || *spawns != 0 {
		t.Fatalf("check turned off in settings still ran: %q spawns=%d", out.String(), *spawns)
	}
}

func TestUpdateCheckExitCodes(t *testing.T) {
	_, _, tag := isolate(t, "v9.0.0")
	if err := updateCheck(); !errors.Is(err, errUpdateAvailable) {
		t.Fatalf("newer release: %v (want exit 10)", err)
	}
	*tag = "v" + core.Version
	if err := updateCheck(); err != nil {
		t.Fatalf("current release: %v (want exit 0)", err)
	}
	t.Setenv(update.APIEnv, "http://127.0.0.1:1/unreachable")
	if err := updateCheck(); err == nil || !strings.Contains(err.Error(), "update check failed") {
		t.Fatalf("failed check: %v (want exit 1 with cause)", err)
	}
	if update.Cached(core.Version).Error == "" {
		t.Fatal("failure cause not recorded")
	}
}

func TestSkipAndAutoPersist(t *testing.T) {
	home, _, _ := isolate(t, "v9.0.0")
	if err := updateSkip(); err != nil {
		t.Fatal(err)
	}
	if r := update.Cached(core.Version); !r.Skipped || r.Available {
		t.Fatalf("skip: %+v", r)
	}
	app, err := core.OpenAt(home)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.SettingBool("update.auto") || !app.SettingBool("update.check") {
		t.Fatal("defaults: checks on, automatic install off")
	}
	if err := updateAuto(app, "on"); err != nil || !app.SettingBool("update.auto") {
		t.Fatalf("--auto on: %v", err)
	}
	update.Log("automatic install started")
	if err := updateAuto(app, "off"); err != nil || app.SettingBool("update.auto") {
		t.Fatalf("--auto off: %v", err)
	}
	if _, err := os.Stat(update.LogPath()); !os.IsNotExist(err) {
		t.Fatal("--auto off must remove the update log")
	}
	if err := updateAuto(app, "status"); err != nil {
		t.Fatal(err)
	}
	if err := updateAuto(app, "maybe"); !errors.Is(err, errUsage) {
		t.Fatalf("bad value: %v", err)
	}
	if err := app.SetSetting("agent:bot", "update.auto", "true"); err == nil {
		t.Fatal("agents must not turn on automatic updates")
	}
}

func TestBackgroundInstallsOnlyWhenAutoIsOn(t *testing.T) {
	home, _, tag := isolate(t, "v9.0.0")
	installs := 0
	old := installCLI
	installCLI = func(context.Context, update.Release) error { installs++; return errors.New("disk full") }
	t.Cleanup(func() { installCLI = old })

	backgroundUpdate(t.Context())
	if installs != 0 {
		t.Fatal("installed with automatic updates off")
	}
	app, err := core.OpenAt(home)
	if err != nil {
		t.Fatal(err)
	}
	app.SetSetting("user", "update.auto", "true")
	app.Close()
	backgroundUpdate(t.Context())
	if installs != 1 {
		t.Fatalf("automatic install did not run: %d", installs)
	}
	if b, _ := os.ReadFile(update.LogPath()); !strings.Contains(string(b), "automatic install of 9.0.0 failed: disk full") {
		t.Fatalf("failure not logged: %q", b)
	}
	update.Skip("9.0.0")
	backgroundUpdate(t.Context())
	*tag = "v" + core.Version
	backgroundUpdate(t.Context())
	if installs != 1 {
		t.Fatal("installed a skipped or current release")
	}
}
