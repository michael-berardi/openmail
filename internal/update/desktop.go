package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// AppInstaller replaces OpenMail.app with a verified release build.
type AppInstaller struct {
	Bundle string       // default: the running OpenMail.app
	Client *http.Client // default: 5-minute timeout
	// Verify checks a bundle's signature and returns its Team ID. The default
	// runs codesign --verify --deep --strict and the Gatekeeper assessment.
	Verify func(app string) (string, error)
}

// RunningBundle is the .app that contains this executable.
func RunningBundle() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	for p := resolve(exe); p != filepath.Dir(p); p = filepath.Dir(p) {
		if strings.HasSuffix(p, ".app") {
			return p, nil
		}
	}
	return "", errors.New("not running from OpenMail.app")
}

// signedTeam verifies a bundle's code signature and notarization and returns
// its Developer ID Team ID.
func signedTeam(app string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("app updates need macOS")
	}
	if out, err := exec.Command("codesign", "--verify", "--deep", "--strict", app).CombinedOutput(); err != nil {
		return "", fmt.Errorf("code signature is not valid: %s", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("spctl", "--assess", "--type", "execute", app).CombinedOutput(); err != nil {
		return "", fmt.Errorf("Gatekeeper rejected it: %s", strings.TrimSpace(string(out)))
	}
	out, _ := exec.Command("codesign", "-dv", app).CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "TeamIdentifier="); ok && v != "not set" && v != "" {
			return v, nil
		}
	}
	return "", errors.New("it is not signed with a Developer ID")
}

// Install verifies the app archive against SHA256SUMS, extracts it next to the
// running bundle, requires a valid, notarized signature from the same Team ID
// as the running app, and swaps it in keeping OpenMail.app.previous. The
// staging folder is removed whether the install succeeds or fails.
func (i AppInstaller) Install(ctx context.Context, rel Release) error {
	bundle := i.Bundle
	if bundle == "" {
		var err error
		if bundle, err = RunningBundle(); err != nil {
			return err
		}
	}
	if strings.Contains(bundle, "/AppTranslocation/") {
		return errors.New("move OpenMail to the Applications folder, open it from there, then install the update")
	}
	verify := i.Verify
	if verify == nil {
		verify = signedTeam
	}
	version := rel.Version()
	if version == "" {
		return errors.New("no stable release to install")
	}
	running, err := verify(bundle)
	if err != nil {
		return fmt.Errorf("this copy of OpenMail cannot verify updates (%v); download the new version from the releases page", err)
	}
	name := AppAsset(version)
	archive, err := verified(ctx, client(i.Client), rel, name)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(bundle), ".openmail-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", bundle, err)
	}
	defer os.RemoveAll(tmp)
	zip := filepath.Join(tmp, name)
	if err := os.WriteFile(zip, archive, 0o600); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "ditto", "-x", "-k", zip, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("unpack %s: %s", name, strings.TrimSpace(string(out)))
	}
	incoming := filepath.Join(tmp, "OpenMail.app")
	team, err := verify(incoming)
	if err != nil {
		return fmt.Errorf("update rejected: %w", err)
	}
	if team != running {
		return fmt.Errorf("update rejected: signed by Team ID %s, this app by %s", team, running)
	}
	return swap(bundle, incoming)
}

// RelaunchAfterExit opens bundle again once this process has exited.
func RelaunchAfterExit(bundle string) error {
	cmd := exec.Command("/bin/sh", "-c", `while kill -0 "$1" 2>/dev/null; do sleep 0.2; done; exec open "$2"`,
		"sh", strconv.Itoa(os.Getpid()), bundle)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
