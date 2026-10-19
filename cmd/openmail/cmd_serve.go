package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/demo"
	"github.com/michael-berardi/openmail/internal/mcpserver"
	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/web"
)

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func cmdServe(ctx context.Context, args []string) error {
	fs := newFlags("serve")
	addr := fs.String("addr", "127.0.0.1:7777", "listen address")
	noOpen := fs.Bool("no-open", false, "do not open the browser")
	allow := fs.String("allow-host", "", "extra Host header values, comma separated (when listening beyond localhost)")
	noSync := fs.Bool("no-sync", false, "serve the UI without syncing")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	var hosts []string
	if *allow != "" {
		hosts = strings.Split(*allow, ",")
	}
	srv, err := web.New(app, web.Options{Addr: *addr, AllowedHosts: hosts})
	if err != nil {
		return err
	}
	if err := srv.Listen(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	if !*noSync {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := app.Run(ctx); err != nil {
				if errors.Is(err, core.ErrEngineBusy) {
					fmt.Fprintln(os.Stderr, "note: another OpenMail process is syncing; this one serves the UI only")
					return
				}
				fmt.Fprintf(os.Stderr, "sync engine stopped: %v\n", err)
			}
		}()
	}
	url := srv.URL()
	fmt.Printf("OpenMail %s running\n  app:  %s\n  mcp:  %s/mcp (bearer token from `openmail tokens create <name>`)\nPress Ctrl-C to stop.\n", core.Version, url, srv.BaseURL())
	if !*noOpen {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(os.Stderr, "could not open a browser (%v); open the app URL above\n", err)
		}
	}
	err = srv.Serve(ctx)
	cancel()
	wg.Wait()
	return err
}

func cmdMCP(ctx context.Context, args []string) error {
	fs := newFlags("mcp")
	scope := fs.String("scope", "write", "read | write | send | admin")
	name := fs.String("name", "stdio", "client name recorded in the audit log")
	noSync := fs.Bool("no-sync", false, "do not fetch new mail while connected")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if !model.ScopeAllows(*scope, "read") {
		return fmt.Errorf("--scope must be read, write, send or admin")
	}
	app, err := openApp()
	if err != nil {
		return err
	}
	defer app.Close()
	if !*noSync {
		go func() {
			if err := app.Run(ctx); err != nil && !errors.Is(err, core.ErrEngineBusy) {
				fmt.Fprintf(os.Stderr, "openmail: sync engine stopped: %v\n", err)
			}
		}()
	}
	srv := mcpserver.New(app, "mcp:"+*name, *scope)
	err = srv.Run(ctx, &mcp.StdioTransport{})
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}

// cmdDemo serves a fictional mailbox from a separate data directory.
func cmdDemo(ctx context.Context, args []string) error {
	fs := newFlags("demo")
	home := fs.String("home", "", "data directory for the demo (default: OPENMAIL_HOME, must be empty)")
	addr := fs.String("addr", "127.0.0.1:7788", "listen address")
	noOpen := fs.Bool("no-open", false, "do not open the browser")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	dir := *home
	if dir == "" {
		dir = os.Getenv("OPENMAIL_HOME")
	}
	if dir == "" {
		return errors.New("the demo needs its own data directory: pass --home DIR or set OPENMAIL_HOME, so it never mixes with your real mail")
	}
	app, err := core.OpenAt(dir)
	if err != nil {
		return err
	}
	defer app.Close()
	if accts, _ := app.Store.ListAccounts(); len(accts) == 0 {
		if err := demo.Seed(app); err != nil {
			return err
		}
	}
	srv, err := web.New(app, web.Options{Addr: *addr})
	if err != nil {
		return err
	}
	if err := srv.Listen(); err != nil {
		return err
	}
	fmt.Printf("OpenMail demo (fictional mail, no network)\n  app: %s\nPress Ctrl-C to stop.\n", srv.URL())
	if !*noOpen {
		_ = openBrowser(srv.URL())
	}
	return srv.Serve(ctx)
}
