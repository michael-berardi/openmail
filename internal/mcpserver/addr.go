package mcpserver

import (
	"fmt"
	stdmail "net/mail"
	"strings"

	"github.com/michael-berardi/openmail/internal/model"
)

func mailParse(s string) (model.Address, error) {
	a, err := stdmail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return model.Address{}, fmt.Errorf("invalid address %q", s)
	}
	return model.Address{Name: a.Name, Addr: strings.ToLower(a.Address)}, nil
}
