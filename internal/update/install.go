package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxAsset = 256 << 20

// CLIAsset is the published CLI archive for this platform, or "".
func CLIAsset(version string) string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64", "darwin/amd64":
		return "openmail-cli-v" + version + "-macos-universal.zip"
	case "linux/arm64", "linux/amd64":
		return "openmail-cli-v" + version + "-linux-" + runtime.GOARCH + ".tar.gz"
	case "windows/amd64":
		return "openmail-cli-v" + version + "-windows-amd64.zip"
	}
	return ""
}

// AppAsset is the signed, notarized macOS app archive.
func AppAsset(version string) string { return "OpenMail-v" + version + "-macos-universal.zip" }

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func download(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s", res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errors.New("download is too large")
	}
	return b, err
}

// verified downloads one release asset and checks it against the release's
// SHA256SUMS. Nothing is written to disk.
func verified(ctx context.Context, c *http.Client, rel Release, name string) ([]byte, error) {
	a, ok := rel.asset(name)
	if !ok || name == "" {
		return nil, fmt.Errorf("release %s has no %s for this platform", rel.TagName, name)
	}
	s, ok := rel.asset("SHA256SUMS")
	if !ok {
		return nil, fmt.Errorf("release %s has no SHA256SUMS", rel.TagName)
	}
	sums, err := download(ctx, c, s.URL, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("SHA256SUMS: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return nil, fmt.Errorf("%s is not listed in SHA256SUMS", name)
	}
	data, err := download(ctx, c, a.URL, maxAsset)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("checksum mismatch for %s: refusing to install", name)
	}
	return data, nil
}

// extract returns the one file called want from a .zip or .tar.gz, matched by
// base name so archive paths are never written to disk.
func extract(archive []byte, name, want string) ([]byte, error) {
	var r io.Reader
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.Mode().IsRegular() && path.Base(f.Name) == want {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				r = rc
				break
			}
		}
	} else {
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return nil, err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if h.Typeflag == tar.TypeReg && path.Base(h.Name) == want {
				r = tr
				break
			}
		}
	}
	if r == nil {
		return nil, fmt.Errorf("%s does not contain %s", name, want)
	}
	b, err := io.ReadAll(io.LimitReader(r, maxAsset+1))
	if err == nil && (len(b) == 0 || len(b) > maxAsset) {
		err = fmt.Errorf("%s in %s has a bad size", want, name)
	}
	return b, err
}

// swap moves incoming into place and keeps exactly one rollback copy at
// <current>.previous. If the second rename fails, current is restored.
func swap(current, incoming string) error {
	prev := current + ".previous"
	if err := os.RemoveAll(prev); err != nil {
		return fmt.Errorf("remove old rollback copy: %w", err)
	}
	if err := os.Rename(current, prev); err != nil {
		return fmt.Errorf("keep previous version: %w", err)
	}
	if err := os.Rename(incoming, current); err != nil {
		if rb := os.Rename(prev, current); rb != nil {
			return fmt.Errorf("install failed (%v); restore %s by hand: %w", err, prev, rb)
		}
		return fmt.Errorf("install failed, current version kept: %w", err)
	}
	now := time.Now() // the seven-day limit for the rollback copy starts now
	_ = os.Chtimes(prev, now, now)
	return nil
}

// Installer replaces the CLI executable with a verified release build.
type Installer struct {
	Target string       // default: the running executable
	Client *http.Client // default: 5-minute timeout
}

func (i Installer) target() (string, error) {
	if i.Target != "" {
		return resolve(i.Target), nil
	}
	exe, err := os.Executable()
	return resolve(exe), err
}

// Install verifies the platform archive against SHA256SUMS before anything is
// written, stages the new executable next to the old one and swaps it in. A
// failure at any step leaves the running version and no staged files.
func (i Installer) Install(ctx context.Context, rel Release) error {
	name := CLIAsset(rel.Version())
	if rel.Version() == "" || name == "" {
		return errors.New("no stable release build for this platform")
	}
	archive, err := verified(ctx, client(i.Client), rel, name)
	if err != nil {
		return err
	}
	bin := "openmail"
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	data, err := extract(archive, name, bin)
	if err != nil {
		return err
	}
	target, err := i.target()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".openmail-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", target, err)
	}
	staged := f.Name()
	defer os.Remove(staged) // no-op after a successful swap
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(staged, 0o755)
	}
	if err != nil {
		return err
	}
	return swap(target, staged)
}

// Rollback swaps the executable with <target>.previous.
func Rollback(target string) error {
	if target == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		target = exe
	}
	target = resolve(target)
	prev, held := target+".previous", target+".rollback"
	if _, err := os.Stat(prev); err != nil {
		return errors.New("no previous version to restore (it is removed after the new version runs successfully)")
	}
	_ = os.RemoveAll(held)
	if err := os.Rename(target, held); err != nil {
		return err
	}
	if err := os.Rename(prev, target); err != nil {
		_ = os.Rename(held, target)
		return err
	}
	// The version rolled back from becomes the one rollback copy.
	return os.Rename(held, prev)
}
