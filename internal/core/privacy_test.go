package core

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-berardi/openmail/internal/model"
)

// TestNoAnalyticsOrCrashReporters fails if a telemetry SDK or reporting endpoint is
// reintroduced in shipped source or dependencies. Needles are assembled from parts so
// this guard does not flag itself.
func TestNoAnalyticsOrCrashReporters(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"github.com/get" + "sentry/sentry-go", "sen" + "try.io",
		"github.com/post" + "hog/posthog-go", "post" + "hog.com",
		"github.com/seg" + "mentio/analytics-go", "seg" + "ment.com",
		"github.com/mix" + "panel/mixpanel-go", "mix" + "panel.com",
		"github.com/amp" + "litude/analytics-go", "amp" + "litude.com",
		"github.com/bug" + "snag/bugsnag-go", "bug" + "snag.com",
		"google-" + "analytics.com", "googletag" + "manager.com",
		"data" + "doghq.com", "heap" + "analytics.com",
		"open" + "telemetry.io", "prom" + "etheus/client_golang",
		"plau" + "sible.io", "hot" + "jar.com", "full" + "story.com",
	}
	exts := map[string]bool{".go": true, ".js": true, ".html": true, ".css": true, ".sh": true, ".mod": true, ".md": true}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "dist", "node_modules":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".test-install") {
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(path))] && d.Name() != "Makefile" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		low := strings.ToLower(string(b))
		for _, needle := range forbidden {
			if strings.Contains(low, needle) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s contains telemetry dependency or endpoint %q", rel, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDefaultRendersNoRemoteContent checks the default that protects readers from
// tracking pixels: no remote image is loaded until a person allows it.
func TestDefaultRendersNoRemoteContent(t *testing.T) {
	app := newTestApp(t)
	if got := app.Setting("privacy.remote_content"); got != "block" {
		t.Fatalf("default remote content policy = %q, want block", got)
	}
	mb := &model.Mailbox{AccountID: "me@x.com", Name: "INBOX", Role: "inbox"}
	if err := app.Store.UpsertMailbox(mb); err != nil {
		t.Fatal(err)
	}
	m := &model.Message{
		AccountID: "me@x.com", MailboxID: mb.ID, UID: 1, MessageID: "px@x", ThreadID: "px", Subject: "invoice",
		From: model.Address{Addr: "sender@tracker.example"},
		HTML: `<div style="background:url(https://tracker.example/bg)"><img src="https://tracker.example/pixel.gif" width="1" height="1">hello</div>`,
	}
	if _, err := app.Store.SaveMessage(m, nil); err != nil {
		t.Fatal(err)
	}
	out, had, blocked, err := app.RenderHTML(m.ID, false)
	if err != nil || !had || !blocked {
		t.Fatalf("remote content should be found and blocked: had=%v blocked=%v err=%v", had, blocked, err)
	}
	for _, bad := range []string{` src="https:`, ` src="http:`, `url(https:`, `url(http:`} {
		if strings.Contains(out, bad) {
			t.Fatalf("default render would load remote content: %s", out)
		}
	}
}

// TestReadmePrivacyStaysShort checks the release rule that the README carries only a
// short Privacy summary: at most six non-empty lines, with the detail in PRIVACY.md.
func TestReadmePrivacyStaysShort(t *testing.T) {
	root := filepath.Join("..", "..")
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	inPrivacy, lines := false, 0
	for _, ln := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(ln, "## ") {
			inPrivacy = strings.TrimSpace(strings.TrimPrefix(ln, "## ")) == "Privacy"
			continue
		}
		if inPrivacy && strings.TrimSpace(ln) != "" {
			lines++
		}
	}
	if lines == 0 {
		t.Fatal("README has no Privacy section")
	}
	if lines > 6 {
		t.Errorf("README Privacy section has %d non-empty lines, want at most 6; move the detail to PRIVACY.md", lines)
	}
	// Before the update check existed the README said there was none. It now says what the
	// anonymous daily check is and how to turn it off.
	if strings.Contains(strings.ToLower(string(readme)), "no update check") {
		t.Error("README still claims there is no update check; describe the anonymous daily check and its opt-out")
	}
	if !strings.Contains(string(readme), "[Privacy](PRIVACY.md)") {
		t.Error("README Privacy section must link to PRIVACY.md")
	}
	detail, err := os.ReadFile(filepath.Join(root, "PRIVACY.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"**Update checks:**", "openmail-update-check", "OPENMAIL_NO_UPDATE_CHECK",
		"**No telemetry", "**Storage:**", "audit log capped at 1,000 entries",
		"0700", "0600", "**Deleting it:**", "**Agents:**",
		"privacy.remote_content", "127.0.0.1", "OPENMAIL_HOME", "Application Support/OpenMail",
	} {
		if !strings.Contains(string(detail), want) {
			t.Errorf("PRIVACY.md does not document %q", want)
		}
	}
}
