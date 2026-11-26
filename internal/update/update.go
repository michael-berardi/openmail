// Package update implements update contract v1 for the CLI and the desktop app:
// one anonymous, at most daily request for the public latest-release record, a
// private cache file, skip and notice bookkeeping, and verified installs that
// keep one rollback copy. It knows nothing about CLI flags or the web UI.
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

const (
	// Repo comes from the module path in go.mod.
	Repo = "michael-berardi/openmail"
	Tool = "openmail"

	// APIEnv points the check at another release record (tests, manual proofs).
	APIEnv = "OPENMAIL_UPDATE_API"
	// CacheEnv overrides the platform cache directory.
	CacheEnv = "OPENMAIL_CACHE_DIR"
	// NoCheckEnv=1 turns automatic checks off.
	NoCheckEnv = "OPENMAIL_NO_UPDATE_CHECK"

	Interval  = 24 * time.Hour
	userAgent = Tool + "-update-check"
	timeout   = 4 * time.Second
	maxNotes  = 4 << 10
	maxLog    = 256 << 10
	// keepPrevious bounds how long a rollback copy survives a new version
	// that never completes a run.
	keepPrevious = 7 * 24 * time.Hour
)

// Version is a stable major.minor.patch release number.
type Version [3]int

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// ParseVersion accepts only stable x.y.z with an optional leading v.
func ParseVersion(s string) (Version, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var v Version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || p[0] == '+' {
			return Version{}, false
		}
		v[i] = n
	}
	return v, true
}

// Newer reports whether candidate is a stable release strictly newer than
// current. Anything unparseable, on either side, is not newer.
func Newer(current, candidate string) bool {
	c, ok1 := ParseVersion(current)
	n, ok2 := ParseVersion(candidate)
	if !ok1 || !ok2 {
		return false
	}
	for i := range n {
		if n[i] != c[i] {
			return n[i] > c[i]
		}
	}
	return false
}

// canonical drops a leading v so notices read "0.2.0", not "v0.2.0".
func canonical(s string) string {
	if v, ok := ParseVersion(s); ok {
		return v.String()
	}
	return s
}

// Asset is one published release file.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Release is the part of the GitHub release record OpenMail reads.
type Release struct {
	TagName    string  `json:"tag_name"`
	Body       string  `json:"body"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Version returns the release's stable x.y.z, or "" for drafts, prereleases
// and tags that are not x.y.z.
func (r Release) Version() string {
	if r.Draft || r.Prerelease {
		return ""
	}
	if v, ok := ParseVersion(r.TagName); ok {
		return v.String()
	}
	return ""
}

func (r Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// state is the whole cache file: when the last check ran, what it found or why
// it failed, the skipped version and the last notice shown.
type state struct {
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Latest    string    `json:"latest,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	Error     string    `json:"error,omitempty"`
	Skipped   string    `json:"skipped,omitempty"`
	Shown     string    `json:"shown,omitempty"`
	ShownAt   time.Time `json:"shown_at,omitzero"`
}

// Result is what a check knows about the latest release.
type Result struct {
	Current   string
	Version   string // latest stable release, "" if unknown
	Notes     string
	Newer     bool // Version is strictly newer than Current
	Skipped   bool // the user skipped Version
	Available bool // Newer and not skipped: notify
	CheckedAt time.Time
	Error     string // why the last check failed
}

var mu sync.Mutex

func cacheDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(CacheEnv)); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find cache directory: %w", err)
	}
	return filepath.Join(base, "openmail"), nil
}

// CachePath is the single update cache file shared by the CLI and the app.
func CachePath() (string, error) {
	d, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "update.json"), nil
}

// LogPath is the bounded log written by background and in-app installs.
func LogPath() string {
	d, err := cacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "update.log")
}

func load() state {
	var st state
	if p, err := CachePath(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &st) // a corrupt cache counts as no cache
		}
	}
	return st
}

// save writes the cache atomically with mode 0600.
func save(st state) error {
	p, err := CachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".update-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

func change(fn func(*state)) error {
	mu.Lock()
	defer mu.Unlock()
	st := load()
	fn(&st)
	return save(st)
}

func endpoint() string {
	if u := strings.TrimSpace(os.Getenv(APIEnv)); u != "" {
		return u
	}
	return "https://api.github.com/repos/" + Repo + "/releases/latest"
}

// Fetch makes the one anonymous request (fixed User-Agent, no identifiers) and
// records the outcome, or the cause of its failure, in the cache.
func Fetch() (Release, error) {
	rel, err := fetch()
	now := time.Now()
	_ = change(func(st *state) {
		st.CheckedAt = now
		if err != nil {
			st.Error = err.Error()
			return
		}
		st.Error, st.Latest, st.Notes = "", rel.Version(), ""
		if st.Latest != "" {
			st.Notes = truncate(rel.Body, maxNotes)
		}
	})
	return rel, err
}

func fetch() (Release, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint(), nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return Release{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("release record returned %s", res.Status)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("unreadable release record: %w", err)
	}
	return rel, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

func resultFrom(st state, current string) Result {
	r := Result{Current: canonical(current), Version: st.Latest, Notes: st.Notes, CheckedAt: st.CheckedAt, Error: st.Error}
	r.Newer = Newer(current, st.Latest)
	r.Skipped = st.Latest != "" && st.Latest == st.Skipped
	r.Available = r.Newer && !r.Skipped
	return r
}

// Cached returns what the last check found, without a request.
func Cached(current string) Result {
	mu.Lock()
	defer mu.Unlock()
	return resultFrom(load(), current)
}

// Due reports whether the last check is older than the daily interval.
func Due() bool {
	mu.Lock()
	defer mu.Unlock()
	t := load().CheckedAt
	return t.IsZero() || time.Since(t) >= Interval || time.Until(t) > time.Minute
}

// Claim takes today's check slot so concurrent processes do not each make a
// request. It reports false when the slot was already taken.
func Claim() bool {
	mu.Lock()
	defer mu.Unlock()
	st := load()
	if t := st.CheckedAt; !t.IsZero() && time.Since(t) < Interval && time.Until(t) <= time.Minute {
		return false
	}
	st.CheckedAt = time.Now()
	return save(st) == nil
}

// RecordError stores a failure cause, shown by `openmail update --check`.
func RecordError(cause string) { _ = change(func(st *state) { st.Error = cause }) }

// Check returns the latest result, fetching only when forced or when the
// cached result is older than a day.
func Check(current string, force bool) (Result, error) {
	var err error
	if force || Due() {
		_, err = Fetch()
	}
	return Cached(current), err
}

// Skip hides version's notice until a newer release appears.
func Skip(version string) error {
	if version == "" {
		return errors.New("no release to skip")
	}
	return change(func(st *state) { st.Skipped = version })
}

// ShouldNotify applies skip and the once-per-version-per-day notice rule.
func ShouldNotify(r Result) bool {
	if !r.Available {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	st := load()
	return st.Shown != r.Version || time.Since(st.ShownAt) >= Interval
}

// MarkShown records that version's notice was displayed now.
func MarkShown(version string) {
	_ = change(func(st *state) { st.Shown, st.ShownAt = version, time.Now() })
}

// NoticeLines are the two CLI notice lines. packageCommand is set when a
// package manager owns the install; auto is the user's automatic-update choice.
func NoticeLines(r Result, packageCommand string, auto bool) [2]string {
	run := Tool + " update"
	if packageCommand != "" {
		run = packageCommand
	}
	second := fmt.Sprintf("turn on automatic updates: %s update --auto on · skip: %s update --skip", Tool, Tool)
	if packageCommand != "" || auto {
		second = fmt.Sprintf("skip: %s update --skip", Tool)
	}
	return [2]string{fmt.Sprintf("%s %s available (you have %s) · run: %s", Tool, r.Version, r.Current, run), second}
}

// NotesLines returns the first max non-heading lines of release notes.
func NotesLines(notes string, max int) []string {
	var out []string
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "-*"))
		if len(line) > 120 {
			line = strings.ToValidUTF8(line[:120], "") + "…"
		}
		if out = append(out, line); len(out) == max {
			break
		}
	}
	return out
}

// OptedOut reports environment opt-outs: OPENMAIL_NO_UPDATE_CHECK, CI and
// agent shells. It reads no identifiers.
func OptedOut() bool {
	if v := strings.TrimSpace(os.Getenv(NoCheckEnv)); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return true
	}
	for _, k := range []string{"CI", "CLAUDECODE", "CODEX_SANDBOX", "GEMINI_CLI"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// Interactive reports whether stdout and stderr are both terminals.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

func resolve(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}

// PackageCommand names the upgrade command when Homebrew owns the executable.
// OpenMail never overwrites files a package manager owns.
func PackageCommand(exe string) string {
	if strings.Contains(filepath.ToSlash(resolve(exe)), "/Cellar/openmail/") {
		return "brew upgrade openmail"
	}
	return ""
}

// Spawn starts `<this executable> args...` detached from the terminal, with no
// output; it may write only the update cache and log.
func Spawn(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Log appends one line to the update log, keeping the file under 256 KB.
func Log(format string, args ...any) {
	p := LogPath()
	if p == "" || os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	line := time.Now().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...) + "\n"
	if len(line) > maxLog/4 {
		line = line[:maxLog/4-1] + "\n"
	}
	mu.Lock()
	defer mu.Unlock()
	if b, err := os.ReadFile(p); err == nil && len(b)+len(line) > maxLog {
		keep := b[len(b)-maxLog/4:]
		if i := strings.IndexByte(string(keep), '\n'); i >= 0 {
			keep = keep[i+1:]
		}
		_ = os.WriteFile(p, keep, 0o600)
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_, _ = f.WriteString(line)
		f.Close()
	}
}

// RemoveLog deletes the update log (automatic updates turned off).
func RemoveLog() {
	if p := LogPath(); p != "" {
		_ = os.Remove(p)
	}
}

// CleanPrevious removes path's rollback copy once the new version has run
// successfully, or once the copy is older than seven days.
func CleanPrevious(path string, succeeded bool) {
	prev := resolve(path) + ".previous"
	info, err := os.Lstat(prev)
	if err != nil {
		return
	}
	if succeeded || time.Since(info.ModTime()) > keepPrevious {
		_ = os.RemoveAll(prev)
	}
}
