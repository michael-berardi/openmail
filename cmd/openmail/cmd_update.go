package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/update"
)

var errUpdateAvailable = errors.New("update available")

// Tests replace these so they never start processes or replace their binary.
var (
	spawnCheck = func() error { return update.Spawn("update", "--background") }
	installCLI = func(ctx context.Context, rel update.Release) error { return update.Installer{}.Install(ctx, rel) }
)

type updatePrefs struct{ Check, Auto bool }

// loadUpdatePrefs reads the update settings without ever creating a database.
func loadUpdatePrefs() updatePrefs {
	p := updatePrefs{Check: true}
	dir, err := core.DataDir()
	if err != nil {
		return p
	}
	if _, err := os.Stat(filepath.Join(dir, "openmail.db")); err != nil {
		return p
	}
	app, err := core.OpenAt(dir)
	if err != nil {
		return p
	}
	defer app.Close()
	return updatePrefs{Check: app.SettingBool("update.check"), Auto: app.SettingBool("update.auto")}
}

// noticeAllowed keeps notices and automatic checks away from anything that is
// not a person at a terminal: MCP stdio, JSON output, pipes and the update
// command itself.
func noticeAllowed(args []string, interactive bool) bool {
	if len(args) == 0 || !interactive {
		return false
	}
	switch args[0] {
	case "mcp", "update", "help", "-h", "--help":
		return false
	}
	for _, a := range args[1:] {
		if a == "--" {
			break
		}
		if f := strings.TrimLeft(a, "-"); f != a && (f == "json" || strings.HasPrefix(f, "json=")) {
			return false
		}
	}
	return true
}

// afterCommand runs once the user's command has finished.
func afterCommand(args []string, cmdErr error) {
	if exe, err := os.Executable(); err == nil {
		update.CleanPrevious(exe, cmdErr == nil && args[0] != "update")
	}
	updateNotice(os.Stderr, args, update.Interactive())
}

// updateNotice never waits for the network: the notice comes from the cache
// and the daily check runs in a detached process whose result the next
// command shows.
func updateNotice(w io.Writer, args []string, interactive bool) {
	if !noticeAllowed(args, interactive) || update.OptedOut() {
		return
	}
	r := update.Cached(core.Version)
	notify, due := update.ShouldNotify(r), update.Due()
	if !notify && !due {
		return
	}
	prefs := loadUpdatePrefs()
	if !prefs.Check && !prefs.Auto {
		return
	}
	if notify {
		exe, _ := os.Executable()
		lines := update.NoticeLines(r, update.PackageCommand(exe), prefs.Auto)
		fmt.Fprintf(w, "%s\n\x1b[2m%s\x1b[0m\n", lines[0], lines[1])
		update.MarkShown(r.Version)
	}
	if due && update.Claim() {
		if err := spawnCheck(); err != nil {
			update.RecordError("background check did not start: " + err.Error())
		}
	}
}

func cmdUpdate(ctx context.Context, args []string) error {
	fs := newFlags("update")
	check := fs.Bool("check", false, "report whether a newer release exists (exit 10 if so, 1 if the check failed)")
	skip := fs.Bool("skip", false, "hide the notice for the latest release until a newer one appears")
	auto := fs.String("auto", "", "on, off or status: install updates automatically")
	rollback := fs.Bool("rollback", false, "restore the previous executable")
	background := fs.Bool("background", false, "internal: the detached daily check")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case *background:
		backgroundUpdate(ctx)
		return nil
	case *check:
		return updateCheck()
	case *skip:
		return updateSkip()
	case *rollback:
		if err := update.Rollback(""); err != nil {
			return err
		}
		fmt.Println("previous version restored")
		return nil
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	if *auto != "" {
		return updateAuto(app, *auto)
	}
	return updateInstall(ctx, app)
}

func updateCheck() error {
	r, err := update.Check(core.Version, true)
	if err != nil {
		return fmt.Errorf("update check failed: %v", err)
	}
	if !r.Newer {
		fmt.Printf("openmail %s is up to date\n", r.Current)
		return nil
	}
	skipped := ""
	if r.Skipped {
		skipped = " (skipped)"
	}
	fmt.Printf("openmail %s available (you have %s)%s\n", r.Version, r.Current, skipped)
	for _, line := range update.NotesLines(r.Notes, 5) {
		fmt.Println("  " + line)
	}
	exe, _ := os.Executable()
	if cmd := update.PackageCommand(exe); cmd != "" {
		fmt.Println("run: " + cmd)
	} else {
		fmt.Println("run: openmail update")
	}
	return errUpdateAvailable
}

func updateSkip() error {
	r := update.Cached(core.Version)
	if !r.Newer {
		var err error
		if r, err = update.Check(core.Version, true); err != nil {
			return fmt.Errorf("update check failed: %v", err)
		}
	}
	if !r.Newer {
		fmt.Printf("openmail %s is up to date; nothing to skip\n", r.Current)
		return nil
	}
	if err := update.Skip(r.Version); err != nil {
		return err
	}
	fmt.Printf("skipped openmail %s; a newer release will notify again\n", r.Version)
	return nil
}

func updateAuto(app *core.App, value string) error {
	switch strings.ToLower(value) {
	case "status":
	case "on":
		if err := app.SetSetting("user", "update.auto", "true"); err != nil {
			return err
		}
	case "off":
		if err := app.SetSetting("user", "update.auto", "false"); err != nil {
			return err
		}
		update.RemoveLog()
	default:
		return errUsage
	}
	state := "off"
	if app.SettingBool("update.auto") {
		state = "on"
	}
	fmt.Println("automatic updates are " + state)
	return nil
}

// backgroundUpdate is the detached daily check. With automatic updates on it
// installs a verified release for the next run; failures go to the update log
// and the next command shows the normal notice instead.
func backgroundUpdate(ctx context.Context) {
	update.LowerPriority()
	rel, err := update.Fetch()
	if err != nil {
		update.Log("check failed: %v", err)
		return
	}
	r := update.Cached(core.Version)
	if !r.Available || !loadUpdatePrefs().Auto {
		return
	}
	exe, _ := os.Executable()
	if cmd := update.PackageCommand(exe); cmd != "" {
		update.Log("%s is managed by a package manager; run: %s", r.Version, cmd)
		return
	}
	if err := installCLI(ctx, rel); err != nil {
		update.Log("automatic install of %s failed: %v", r.Version, err)
		return
	}
	update.Log("installed openmail %s", r.Version)
}

func updateInstall(ctx context.Context, app *core.App) error {
	rel, err := update.Fetch()
	if err != nil {
		return fmt.Errorf("update check failed: %v", err)
	}
	r := update.Cached(core.Version)
	if !r.Newer {
		fmt.Printf("openmail %s is up to date\n", r.Current)
		return nil
	}
	fmt.Printf("openmail %s → %s\n", r.Current, r.Version)
	for _, line := range update.NotesLines(r.Notes, 5) {
		fmt.Println("  " + line)
	}
	if isTTY() {
		if a := strings.ToLower(ask("Install " + r.Version + " now? [Y/n] ")); a != "" && a != "y" && a != "yes" {
			return nil
		}
	}
	exe, _ := os.Executable()
	if cmd := update.PackageCommand(exe); cmd != "" {
		fmt.Println("running: " + cmd)
		c := exec.CommandContext(ctx, "brew", strings.Fields(cmd)[1:]...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}
	if err := installCLI(ctx, rel); err != nil {
		return err
	}
	fmt.Printf("installed openmail %s (undo: openmail update --rollback)\n", r.Version)
	if isTTY() && !app.SettingBool("update.auto") && !app.SettingBool("update.auto_asked") {
		on := confirm("Install future updates automatically?")
		if err := app.SetSetting("user", "update.auto_asked", "true"); err != nil {
			return err
		}
		if on {
			return updateAuto(app, "on")
		}
	}
	return nil
}
