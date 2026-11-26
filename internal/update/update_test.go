package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// isolate gives each test its own cache directory and a clean environment.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(CacheEnv, dir)
	for _, k := range []string{NoCheckEnv, "CI", "CLAUDECODE", "CODEX_SANDBOX", "GEMINI_CLI"} {
		t.Setenv(k, "")
	}
	return dir
}

// fakeRelease serves a release record plus files from memory.
type fakeRelease struct {
	tag, notes string
	files      map[string][]byte
	sums       string // SHA256SUMS; computed from files when empty
	calls      int
	ts         *httptest.Server
}

func serve(t *testing.T, f *fakeRelease) *fakeRelease {
	t.Helper()
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release" {
			f.calls++
			if r.Header.Get("User-Agent") != "openmail-update-check" || r.URL.RawQuery != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
				t.Errorf("check request is not anonymous: %s %v", r.URL, r.Header)
			}
			rel := Release{TagName: f.tag, Body: f.notes}
			names := []string{"SHA256SUMS"}
			for n := range f.files {
				names = append(names, n)
			}
			for _, n := range names {
				rel.Assets = append(rel.Assets, Asset{Name: n, URL: f.ts.URL + "/download/" + n})
			}
			_ = json.NewEncoder(w).Encode(rel)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		if name == "SHA256SUMS" {
			if f.sums == "" {
				for n, b := range f.files {
					fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(b), n)
				}
			}
			fmt.Fprint(w, f.sums)
			return
		}
		if b, ok := f.files[name]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.ts.Close)
	t.Setenv(APIEnv, f.ts.URL+"/release")
	return f
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

func TestVersionComparison(t *testing.T) {
	for _, tc := range []struct {
		current, candidate string
		want               bool
	}{
		{"0.2.0", "0.3.0", true}, {"0.2.0", "0.2.1", true}, {"0.2.9", "0.10.0", true}, {"v0.2.0", "1.0.0", true},
		{"0.2.0", "0.2.0", false}, {"0.2.0", "v0.2.0", false},
		{"0.2.0", "0.1.9", false}, {"1.0.0", "0.9.9", false},
		{"0.2.0", "garbage", false}, {"0.2.0", "0.3", false}, {"0.2.0", "", false}, {"0.2.0", "0.3.0.1", false},
		{"0.2.0", "0.3.0-beta.1", false}, {"0.2.0", "0.3.0+build", false},
		{"v0.1.1-3-gabcdef", "0.3.0", false}, // a development build has no comparable version
	} {
		if got := Newer(tc.current, tc.candidate); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.current, tc.candidate, got, tc.want)
		}
	}
	for _, r := range []Release{{TagName: "v0.3.0", Prerelease: true}, {TagName: "v0.3.0", Draft: true}, {TagName: "v0.3.0-rc.1"}, {TagName: "nightly"}} {
		if v := r.Version(); v != "" {
			t.Errorf("%+v counted as stable %q", r, v)
		}
	}
	if v := (Release{TagName: "v0.3.0"}).Version(); v != "0.3.0" {
		t.Errorf("stable tag: %q", v)
	}
}

func TestCheckIsAnonymousDailyAndCachedPrivately(t *testing.T) {
	dir := isolate(t)
	f := serve(t, &fakeRelease{tag: "v0.3.0", notes: "## Changes\n- Faster sync\n"})
	if !Due() {
		t.Fatal("an empty cache must be due")
	}
	r, err := Check("0.2.0", false)
	if err != nil || !r.Available || r.Version != "0.3.0" || r.Current != "0.2.0" {
		t.Fatalf("first check: %+v %v", r, err)
	}
	if _, err := Check("v0.2.0", false); err != nil || f.calls != 1 {
		t.Fatalf("a fresh cache must not make another request: calls=%d err=%v", f.calls, err)
	}
	if Due() || Claim() {
		t.Fatal("the daily slot is taken after a check")
	}
	if _, err := Check("0.2.0", true); err != nil || f.calls != 2 {
		t.Fatalf("an explicit check makes exactly one request: calls=%d", f.calls)
	}
	p, _ := CachePath()
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode: %v %v", info, err)
	}
	if got := entries(t, dir); !slices.Equal(got, []string{"update.json"}) {
		t.Fatalf("the check must write one cache file and nothing else, got %v", got)
	}
	b, _ := os.ReadFile(p)
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	for k := range fields {
		if !slices.Contains([]string{"checked_at", "latest", "notes", "error", "skipped", "shown", "shown_at"}, k) {
			t.Errorf("unexpected cache field %q", k)
		}
	}
	// A stale cache is due again and can be claimed by exactly one process.
	_ = change(func(st *state) { st.CheckedAt = time.Now().Add(-25 * time.Hour) })
	if !Due() || !Claim() || Claim() {
		t.Fatal("a day-old cache must be claimable once")
	}
}

func TestFailureIsRecordedWithCause(t *testing.T) {
	isolate(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	t.Setenv(APIEnv, ts.URL)
	r, err := Check("0.2.0", true)
	if err == nil || !strings.Contains(r.Error, "503") || r.Available {
		t.Fatalf("failure: %+v %v", r, err)
	}
	if c := Cached("0.2.0"); c.Error != r.Error || c.CheckedAt.IsZero() {
		t.Fatalf("cause not cached: %+v", c)
	}
	if Due() {
		t.Fatal("a failed check must not be retried before the next day")
	}
	ts.Close()
	if _, err := Check("0.2.0", true); err == nil || !strings.Contains(Cached("0.2.0").Error, "connect") {
		t.Fatalf("unreachable endpoint cause: %q", Cached("0.2.0").Error)
	}
}

func TestSkipNoticeTextAndFrequency(t *testing.T) {
	isolate(t)
	f := serve(t, &fakeRelease{tag: "v0.3.0"})
	r, _ := Check("0.2.0", true)
	lines := NoticeLines(r, "", false)
	if lines[0] != "openmail 0.3.0 available (you have 0.2.0) · run: openmail update" ||
		lines[1] != "turn on automatic updates: openmail update --auto on · skip: openmail update --skip" {
		t.Fatalf("notice: %q", lines)
	}
	if l := NoticeLines(r, "brew upgrade openmail", false); l[0] != "openmail 0.3.0 available (you have 0.2.0) · run: brew upgrade openmail" || l[1] != "skip: openmail update --skip" {
		t.Fatalf("package-manager notice: %q", l)
	}
	if l := NoticeLines(r, "", true); l[1] != "skip: openmail update --skip" {
		t.Fatalf("notice with automatic updates on: %q", l)
	}
	if !ShouldNotify(r) {
		t.Fatal("first notice")
	}
	MarkShown(r.Version)
	if ShouldNotify(r) {
		t.Fatal("same version shown twice within 24 hours")
	}
	_ = change(func(st *state) { st.ShownAt = time.Now().Add(-25 * time.Hour) })
	if !ShouldNotify(r) {
		t.Fatal("notice must return after 24 hours")
	}
	if err := Skip("0.3.0"); err != nil {
		t.Fatal(err)
	}
	if c := Cached("0.2.0"); c.Available || !c.Skipped || !c.Newer || ShouldNotify(c) {
		t.Fatalf("skipped version still notifies: %+v", c)
	}
	f.tag = "v0.4.0"
	if r, _ = Check("0.2.0", true); !r.Available || !ShouldNotify(r) {
		t.Fatalf("a newer release must notify again: %+v", r)
	}
}

func TestOptOuts(t *testing.T) {
	isolate(t)
	if OptedOut() {
		t.Fatal("clean environment opted out")
	}
	for _, kv := range [][2]string{{NoCheckEnv, "1"}, {"CI", "true"}, {"CLAUDECODE", "1"}} {
		t.Run(kv[0], func(t *testing.T) {
			t.Setenv(kv[0], kv[1])
			if !OptedOut() {
				t.Fatalf("%s=%s must turn checks off", kv[0], kv[1])
			}
		})
	}
	t.Setenv(NoCheckEnv, "0")
	if OptedOut() {
		t.Fatal("OPENMAIL_NO_UPDATE_CHECK=0 is not an opt-out")
	}
}

func cliArchive(t *testing.T, version, body string) (string, []byte) {
	t.Helper()
	name := CLIAsset(version)
	if name == "" {
		t.Skip("no published CLI build for this platform")
	}
	bin := "openmail"
	if strings.Contains(name, "windows") {
		bin += ".exe"
	}
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&buf)
		for _, n := range []string{"openmail-cli-v" + version + "/LICENSE", "openmail-cli-v" + version + "/" + bin} {
			w, _ := zw.Create(n)
			w.Write([]byte(body))
		}
		zw.Close()
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: "./" + bin, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write([]byte(body))
		tw.Close()
		gz.Close()
	}
	return name, buf.Bytes()
}

func TestCLIInstallVerifiesBeforeSwapAndRollsBack(t *testing.T) {
	isolate(t)
	name, archive := cliArchive(t, "0.3.0", "new")
	f := serve(t, &fakeRelease{tag: "v0.3.0", files: map[string][]byte{name: archive}})
	dir := t.TempDir()
	target := filepath.Join(dir, "openmail")
	os.WriteFile(target, []byte("old"), 0o755)
	inst := Installer{Target: target, Client: f.ts.Client()}
	rel, err := Fetch()
	if err != nil {
		t.Fatal(err)
	}

	f.sums = fmt.Sprintf("%064x  %s\n", 0, name)
	if err := inst.Install(t.Context(), rel); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum mismatch accepted: %v", err)
	}
	f.sums = "deadbeef  some-other-file\n"
	if err := inst.Install(t.Context(), rel); err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Fatalf("missing checksum accepted: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" || !slices.Equal(entries(t, dir), []string{"openmail"}) {
		t.Fatalf("refused install changed files: %q %v", b, entries(t, dir))
	}

	f.sums = ""
	if err := inst.Install(t.Context(), rel); err != nil {
		t.Fatalf("verified install: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Fatalf("installed: %q", b)
	}
	if b, _ := os.ReadFile(target + ".previous"); string(b) != "old" {
		t.Fatalf("rollback copy: %q", b)
	}
	if err := inst.Install(t.Context(), rel); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, dir); !slices.Equal(got, []string{"openmail", "openmail.previous"}) {
		t.Fatalf("install must leave the executable and one rollback copy, got %v", got)
	}
	os.WriteFile(target+".previous", []byte("old"), 0o755)
	if err := Rollback(target); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatalf("rollback restored %q", b)
	}
	if b, _ := os.ReadFile(target + ".previous"); string(b) != "new" || len(entries(t, dir)) != 2 {
		t.Fatalf("after rollback: %q %v", b, entries(t, dir))
	}
}

func TestRollbackCopyIsRemovedAfterSuccessOrSevenDays(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "openmail")
	os.WriteFile(target, []byte("new"), 0o755)
	prev := target + ".previous"
	os.WriteFile(prev, []byte("old"), 0o755)
	CleanPrevious(target, false)
	if _, err := os.Stat(prev); err != nil {
		t.Fatal("a failed run must keep a fresh rollback copy")
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(prev, old, old)
	CleanPrevious(target, false)
	if _, err := os.Stat(prev); !os.IsNotExist(err) {
		t.Fatal("a rollback copy older than seven days must be removed")
	}
	os.WriteFile(prev, []byte("old"), 0o755)
	CleanPrevious(target, true)
	if _, err := os.Stat(prev); !os.IsNotExist(err) {
		t.Fatal("a successful run must remove the rollback copy")
	}
	// Bundles are directories.
	os.MkdirAll(filepath.Join(dir, "OpenMail.app.previous", "Contents"), 0o755)
	CleanPrevious(filepath.Join(dir, "OpenMail.app"), true)
	if _, err := os.Stat(filepath.Join(dir, "OpenMail.app.previous")); !os.IsNotExist(err) {
		t.Fatal("app rollback copy not removed")
	}
}

func TestLogIsBoundedAndRemovable(t *testing.T) {
	dir := isolate(t)
	line := strings.Repeat("x", 1000)
	for range 600 {
		Log("%s", line)
	}
	info, err := os.Stat(LogPath())
	if err != nil || info.Size() > maxLog || info.Mode().Perm() != 0o600 {
		t.Fatalf("log: %v %v", info, err)
	}
	Log("%s", strings.Repeat("y", 2*maxLog))
	if info, _ := os.Stat(LogPath()); info.Size() > maxLog {
		t.Fatalf("one huge line grew the log to %d bytes", info.Size())
	}
	RemoveLog()
	if got := entries(t, dir); len(got) != 0 {
		t.Fatalf("log not removed: %v", got)
	}
}

func TestPackageManagerInstallsAreDetected(t *testing.T) {
	if got := PackageCommand("/opt/homebrew/Cellar/openmail/0.2.0/bin/openmail"); got != "brew upgrade openmail" {
		t.Fatalf("Homebrew: %q", got)
	}
	for _, p := range []string{"/usr/local/bin/openmail", "/opt/homebrew/bin/openmail-copied-by-hand", filepath.Join(t.TempDir(), "openmail")} {
		if got := PackageCommand(p); got != "" {
			t.Errorf("%s detected as %q", p, got)
		}
	}
}

func appZip(t *testing.T, plist string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("OpenMail.app/Contents/Info.plist")
	w.Write([]byte(plist))
	zw.Close()
	return buf.Bytes()
}

func TestAppInstallVerifiesSignatureAndCleansUp(t *testing.T) {
	if _, err := exec.LookPath("ditto"); err != nil {
		t.Skip("app installs need macOS ditto")
	}
	isolate(t)
	f := serve(t, &fakeRelease{tag: "v0.3.0", files: map[string][]byte{AppAsset("0.3.0"): appZip(t, "new")}})
	root := t.TempDir()
	bundle := filepath.Join(root, "OpenMail.app")
	os.MkdirAll(filepath.Join(bundle, "Contents"), 0o755)
	os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte("old"), 0o644)
	rel, err := Fetch()
	if err != nil {
		t.Fatal(err)
	}
	plist := func() string { b, _ := os.ReadFile(filepath.Join(bundle, "Contents", "Info.plist")); return string(b) }

	other := AppInstaller{Bundle: bundle, Client: f.ts.Client(), Verify: func(app string) (string, error) {
		if app == bundle {
			return "TEAM", nil
		}
		return "OTHER", nil
	}}
	if err := other.Install(t.Context(), rel); err == nil || !strings.Contains(err.Error(), "Team ID") {
		t.Fatalf("different signer accepted: %v", err)
	}
	unsigned := AppInstaller{Bundle: bundle, Client: f.ts.Client(), Verify: func(app string) (string, error) {
		if app == bundle {
			return "TEAM", nil
		}
		return "", fmt.Errorf("code signature is not valid")
	}}
	if err := unsigned.Install(t.Context(), rel); err == nil {
		t.Fatal("unsigned update accepted")
	}
	f.sums = fmt.Sprintf("%064x  %s\n", 0, AppAsset("0.3.0"))
	good := AppInstaller{Bundle: bundle, Client: f.ts.Client(), Verify: func(string) (string, error) { return "TEAM", nil }}
	if err := good.Install(t.Context(), rel); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("bad checksum accepted: %v", err)
	}
	if plist() != "old" || !slices.Equal(entries(t, root), []string{"OpenMail.app"}) {
		t.Fatalf("refused installs left changes: %q %v", plist(), entries(t, root))
	}
	f.sums = ""
	if err := good.Install(t.Context(), rel); err != nil {
		t.Fatalf("verified install: %v", err)
	}
	if plist() != "new" || !slices.Equal(entries(t, root), []string{"OpenMail.app", "OpenMail.app.previous"}) {
		t.Fatalf("after install: %q %v", plist(), entries(t, root))
	}
}
