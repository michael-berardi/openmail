package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/model"
)

var errUsage = errors.New("usage")

func openApp() (*core.App, error) { return core.Open() }

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("openmail "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parse allows flags after positional arguments (openmail read 12 --json).
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errUsage
			}
			return nil, errUsage
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func isTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// readSecret reads a password without echo; from stdin when it is not a terminal.
func readSecret(prompt string) (string, error) {
	if isTTY() {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	s, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func ask(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(s)
}

func confirm(prompt string) bool {
	if !isTTY() {
		return false
	}
	a := strings.ToLower(ask(prompt + " [y/N] "))
	return a == "y" || a == "yes"
}

func parseAddrList(s string) ([]model.Address, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	list, err := mail.ParseAddressList(s)
	if err != nil {
		return nil, fmt.Errorf("bad address list %q: %w", s, err)
	}
	var out []model.Address
	for _, a := range list {
		out = append(out, model.Address{Name: a.Name, Addr: strings.ToLower(a.Address)})
	}
	return out, nil
}

func ids(args []string) ([]int64, error) {
	var out []int64
	for _, a := range args {
		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a message id", a)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("no message ids given")
	}
	return out, nil
}

func when(ts int64) string {
	t := time.Unix(ts, 0)
	now := time.Now()
	switch {
	case t.After(now.Add(-20 * time.Hour)):
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan _2")
	}
	return t.Format("2006-01-02")
}

func clip(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func fromLabel(a model.Address) string {
	if a.Name != "" {
		return a.Name
	}
	return a.Addr
}

func actor() string { return "cli" }
