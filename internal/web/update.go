package web

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/update"
)

// appInstaller installs a release into the running desktop app; tests replace it.
type appInstaller interface {
	Install(context.Context, update.Release) error
}

type updateStatus struct {
	Current        string `json:"current"`
	Version        string `json:"version,omitempty"`
	Notes          string `json:"notes,omitempty"`
	Newer          bool   `json:"newer"`
	Available      bool   `json:"available"`
	Skipped        bool   `json:"skipped"`
	Error          string `json:"error,omitempty"`
	Auto           bool   `json:"auto"`
	Desktop        bool   `json:"desktop"`
	PackageCommand string `json:"package_command,omitempty"`
}

func (s *Server) status(r update.Result) updateStatus {
	st := updateStatus{
		Current: r.Current, Version: r.Version, Notes: r.Notes, Newer: r.Newer, Available: r.Available, Skipped: r.Skipped,
		Error: r.Error, Auto: s.app.SettingBool("update.auto"), Desktop: s.opts.Desktop,
	}
	if exe, err := os.Executable(); err == nil {
		st.PackageCommand = update.PackageCommand(exe)
	}
	return st
}

// checkUpdates is the desktop app's daily check: in the background, at most
// once a day across the app and the CLI, never on the startup path.
func (s *Server) checkUpdates(ctx context.Context) {
	t := time.NewTimer(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		on := s.app.SettingBool("update.check") || s.app.SettingBool("update.auto")
		if on && !update.OptedOut() && update.Due() && update.Claim() {
			_, _ = update.Fetch()
			s.app.Emit(core.Event{Type: "update"})
		}
		t.Reset(time.Hour)
	}
}

// userOnly keeps agents away from update actions, like the update.* settings.
func userOnly(p principal) error {
	if core.IsAgent(p.actor) {
		return apiErr{403, "updates are the user's choice; ask the user"}
	}
	return nil
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request, p principal) error {
	writeJSON(w, s.status(update.Cached(core.Version)))
	return nil
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := userOnly(p); err != nil {
		return err
	}
	res, _ := update.Check(core.Version, true) // the cause is in res.Error
	s.app.Emit(core.Event{Type: "update"})
	writeJSON(w, s.status(res))
	return nil
}

func (s *Server) updateSkip(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := userOnly(p); err != nil {
		return err
	}
	var in struct {
		Version string `json:"version"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := update.Skip(strings.TrimSpace(in.Version)); err != nil {
		return apiErr{400, err.Error()}
	}
	s.app.Emit(core.Event{Type: "update"})
	writeJSON(w, s.status(update.Cached(core.Version)))
	return nil
}

func (s *Server) updateInstall(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := userOnly(p); err != nil {
		return err
	}
	if !s.opts.Desktop {
		return apiErr{400, "run `openmail update` to update the command-line app"}
	}
	rel, err := update.Fetch()
	if err != nil {
		return apiErr{502, "update check failed: " + err.Error()}
	}
	if !update.Newer(core.Version, rel.Version()) {
		return apiErr{409, "OpenMail is up to date"}
	}
	if err := s.installer.Install(r.Context(), rel); err != nil {
		update.Log("app install of %s failed: %v", rel.Version(), err)
		return apiErr{500, err.Error()}
	}
	update.Log("installed OpenMail %s", rel.Version())
	writeJSON(w, map[string]string{"installed": rel.Version()})
	return nil
}

func (s *Server) updateRelaunch(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := userOnly(p); err != nil {
		return err
	}
	if s.opts.Relaunch == nil {
		return apiErr{400, "relaunch is only available in the desktop app"}
	}
	writeJSON(w, map[string]bool{"ok": true})
	go s.opts.Relaunch()
	return nil
}
