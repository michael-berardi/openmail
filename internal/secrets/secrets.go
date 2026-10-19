// Package secrets keeps account passwords and OAuth tokens in the OS keychain.
// macOS Keychain, Windows Credential Manager or the Linux Secret Service are used through
// go-keyring. An environment variable overrides the keychain for headless servers.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

const service = "OpenMail"

var envKeyRe = regexp.MustCompile(`[^A-Za-z0-9]+`)

// EnvName is the override variable for an account: OPENMAIL_PASSWORD_<EMAIL_UPPER_UNDERSCORED>.
func EnvName(kind, account string) string {
	return "OPENMAIL_" + strings.ToUpper(kind) + "_" + strings.ToUpper(strings.Trim(envKeyRe.ReplaceAllString(account, "_"), "_"))
}

func key(kind, account string) string { return kind + ":" + strings.ToLower(account) }

// Get returns a stored secret. kind is "password", "oauth-refresh" or "oauth-access".
func Get(kind, account string) (string, error) {
	if v := os.Getenv(EnvName(kind, account)); v != "" {
		return v, nil
	}
	v, err := keyring.Get(service, key(kind, account))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", fmt.Errorf("no %s stored for %s: run `openmail accounts login %s` (or set %s)", kind, account, account, EnvName(kind, account))
		}
		return "", fmt.Errorf("read %s for %s from keychain: %w (set %s to bypass the keychain)", kind, account, err, EnvName(kind, account))
	}
	return v, nil
}

func Set(kind, account, value string) error {
	if err := keyring.Set(service, key(kind, account), value); err != nil {
		return fmt.Errorf("store %s for %s in keychain: %w", kind, account, err)
	}
	return nil
}

func Delete(kind, account string) error {
	err := keyring.Delete(service, key(kind, account))
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// AppleKeychainLookup asks the macOS Keychain for an internet password saved by Apple Mail
// or System Settings. macOS shows its own approval dialog; the item is only returned if the
// user allows it. Returns an error on other platforms or when nothing matches.
func AppleKeychainLookup(host, account string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("apple keychain is only available on macOS")
	}
	out, err := exec.Command("/usr/bin/security", "find-internet-password", "-s", host, "-a", account, "-w").Output()
	if err != nil {
		return "", fmt.Errorf("keychain has no readable password for %s@%s (denied or not stored): %w", account, host, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}
