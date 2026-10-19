// Command openmail-desktop is the OpenMail desktop app: the same Go engine and interface as
// `openmail serve`, in a native window that uses the system web view (no bundled browser).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wr "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/web"
)

type windowState struct {
	W, H int
}

func loadState(dir string) windowState {
	st := windowState{W: 1280, H: 820}
	if b, err := os.ReadFile(filepath.Join(dir, "window.json")); err == nil {
		var s windowState
		if json.Unmarshal(b, &s) == nil && s.W >= 720 && s.H >= 520 && s.W < 8000 && s.H < 6000 {
			st = s
		}
	}
	return st
}

func saveState(dir string, ctx context.Context) {
	w, h := wr.WindowGetSize(ctx)
	if w >= 720 && h >= 520 {
		b, _ := json.Marshal(windowState{W: w, H: h})
		_ = os.WriteFile(filepath.Join(dir, "window.json"), b, 0o600)
	}
}

func isDarkMode() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("defaults", "read", "-g", "AppleInterfaceStyle").Output()
	return err == nil && strings.Contains(string(out), "Dark")
}

func main() {
	app, err := core.Open()
	if err != nil {
		fatal("open database", err)
	}
	srv, err := web.New(app, web.Options{Addr: "127.0.0.1:0"})
	if err != nil {
		fatal("start interface", err)
	}
	if err := srv.Listen(); err != nil {
		fatal("start interface", err)
	}
	bg, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = srv.Serve(bg) }()
	go func() {
		defer wg.Done()
		if err := app.Run(bg); err != nil && err != core.ErrEngineBusy {
			fmt.Fprintln(os.Stderr, "openmail: sync engine stopped:", err)
		}
	}()
	appURL := srv.URL() + "&desktop=1"

	var wctx context.Context
	st := loadState(app.Dir)
	js := func(code string) {
		if wctx != nil {
			wr.WindowExecJS(wctx, code)
		}
	}
	show := func() {
		if wctx != nil {
			wr.WindowShow(wctx)
			wr.WindowUnminimise(wctx)
		}
	}

	m := menu.NewMenu()
	if runtime.GOOS == "darwin" {
		m.Append(menu.AppMenu())
	}
	mail := m.AddSubmenu("Mailbox")
	mail.AddText("New Message", keys.CmdOrCtrl("n"), func(*menu.CallbackData) { show(); js("window.OM&&OM.newMessage()") })
	mail.AddText("Check for Mail", keys.CmdOrCtrl("r"), func(*menu.CallbackData) { js("window.OM&&OM.refresh()") })
	mail.AddSeparator()
	mail.AddText("All Inboxes", keys.CmdOrCtrl("1"), func(*menu.CallbackData) { show(); js("window.OM&&OM.go('inbox')") })
	mail.AddText("Unread", keys.CmdOrCtrl("2"), func(*menu.CallbackData) { show(); js("window.OM&&OM.go('unread')") })
	mail.AddText("Flagged", keys.CmdOrCtrl("3"), func(*menu.CallbackData) { show(); js("window.OM&&OM.go('flagged')") })
	mail.AddText("Drafts", keys.CmdOrCtrl("4"), func(*menu.CallbackData) { show(); js("window.OM&&OM.go('drafts')") })
	mail.AddSeparator()
	mail.AddText("Command Palette", keys.CmdOrCtrl("k"), func(*menu.CallbackData) { js("window.OM&&OM.palette()") })
	mail.AddText("Settings…", keys.CmdOrCtrl(","), func(*menu.CallbackData) { show(); js("window.OM&&OM.go('settings')") })
	if runtime.GOOS == "darwin" {
		m.Append(menu.EditMenu())
		m.Append(menu.WindowMenu())
	}

	bgColour, bgCSS := &options.RGBA{R: 255, G: 255, B: 255, A: 255}, "#fff"
	if isDarkMode() {
		bgColour, bgCSS = &options.RGBA{R: 28, G: 28, B: 30, A: 255}, "#1c1c1e"
	}

	err = wails.Run(&options.App{
		Title:             "OpenMail",
		Width:             st.W,
		Height:            st.H,
		MinWidth:          720,
		MinHeight:         520,
		HideWindowOnClose: runtime.GOOS == "darwin",
		BackgroundColour:  bgColour,
		Menu:              m,
		// The web view just follows the local interface; there is no second copy of the UI.
		AssetServer: &assetserver.Options{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The custom scheme does not follow HTTP redirects, so navigate from the page itself.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><body style="margin:0;background:%s"><script>location.replace(%q)</script>`, bgCSS, appURL)
		})},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "com.libertydesign.openmail",
			OnSecondInstanceLaunch: func(d options.SecondInstanceData) { show(); handleArgs(d.Args, js) },
		},
		Mac: &mac.Options{
			TitleBar:   mac.TitleBarHiddenInset(),
			Appearance: mac.DefaultAppearance,
			About:      &mac.AboutInfo{Title: "OpenMail " + core.Version, Message: "Open mail for people and agents.\nMIT licensed."},
			OnUrlOpen:  func(u string) { show(); handleArgs([]string{u}, js) },
		},
		OnStartup: func(ctx context.Context) {
			wctx = ctx
			go watchUnread(bg, app, show)
		},
		OnBeforeClose: func(ctx context.Context) bool { saveState(app.Dir, ctx); return false },
		OnShutdown:    func(ctx context.Context) { saveState(app.Dir, ctx) },
	})
	cancel()
	wg.Wait()
	_ = app.Close()
	if err != nil {
		fatal("window", err)
	}
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "openmail: %s: %v\n", what, err)
	os.Exit(1)
}

// handleArgs turns a mailto: link into a compose window.
func handleArgs(args []string, js func(string)) {
	for _, a := range args {
		if !strings.HasPrefix(strings.ToLower(a), "mailto:") {
			continue
		}
		u, err := url.Parse(a)
		if err != nil {
			continue
		}
		q := u.Query()
		payload, _ := json.Marshal(map[string]string{"to": u.Opaque, "subject": q.Get("subject"), "body": q.Get("body"), "cc": q.Get("cc")})
		js("window.OM&&OM.mailto(" + string(payload) + ")")
		return
	}
}

// watchUnread mirrors the inbox unread count to the Dock and announces new mail while the app is in the background.
func watchUnread(ctx context.Context, app *core.App, show func()) {
	events, cancel := app.Subscribe()
	defer cancel()
	last := -1
	var timer *time.Timer
	check := func() {
		ov, err := app.Overview()
		if err != nil {
			return
		}
		unread := 0
		for _, a := range ov.Accounts {
			for _, mb := range a.Mailboxes {
				if mb.Role == model.RoleInbox {
					unread += mb.Unread
				}
			}
		}
		setBadge(unread)
		if last >= 0 && unread > last && !appIsActive() {
			msgs, _ := app.Search("is:unread in:inbox", core.ListOptions{Limit: unread - last})
			title, body := "New mail", fmt.Sprintf("%d new messages", unread-last)
			if len(msgs) > 0 {
				title = msgs[0].From.Name
				if title == "" {
					title = msgs[0].From.Addr
				}
				body = msgs[0].Subject
				if len(msgs) > 1 {
					body += fmt.Sprintf("  (+%d more)", len(msgs)-1)
				}
			}
			notify(title, body)
		}
		last = unread
	}
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-events:
			if e.Type != "message" && e.Type != "sync" && e.Type != "account" {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(800*time.Millisecond, check)
		}
	}
}
