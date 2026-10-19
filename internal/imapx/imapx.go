// Package imapx dials IMAP and SMTP servers for an account and detects mailbox roles.
package imapx

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/oauth"
)

// Creds are resolved per connection so OAuth tokens are always fresh.
type Creds struct {
	Password string
	Token    string // OAuth access token; when set it is used with XOAUTH2
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func tlsConfig(host string, security string) *tls.Config {
	c := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	// Local bridges (Proton Mail Bridge) present self-signed certificates.
	if isLoopback(host) {
		c.InsecureSkipVerify = true
	}
	return c
}

// ConnectError means the network connection (not the sign-in) failed. Callers treat it as
// "offline" and queue work for later.
type ConnectError struct {
	Addr string
	Err  error
}

func (e *ConnectError) Error() string {
	return fmt.Sprintf("connect to IMAP server %s: %v", e.Addr, e.Err)
}
func (e *ConnectError) Unwrap() error { return e.Err }

type Conn struct {
	*imapclient.Client
	Changes chan struct{} // signalled on server-side mailbox changes
}

// DialIMAP connects and authenticates. Errors say which step failed.
func DialIMAP(ctx context.Context, a model.Account, cr Creds) (*Conn, error) {
	addr := net.JoinHostPort(a.IMAPHost, fmt.Sprint(a.IMAPPort))
	changes := make(chan struct{}, 1)
	signal := func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	opts := &imapclient.Options{
		TLSConfig: tlsConfig(a.IMAPHost, a.IMAPSecurity),
		Dialer:    &net.Dialer{Timeout: 20 * time.Second},
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) { signal() },
			Expunge: func(uint32) { signal() },
			Fetch:   func(*imapclient.FetchMessageData) { signal() },
		},
	}
	var c *imapclient.Client
	var err error
	switch a.IMAPSecurity {
	case model.SecurityTLS:
		c, err = imapclient.DialTLS(addr, opts)
	case model.SecuritySTARTTLS:
		c, err = imapclient.DialStartTLS(addr, opts)
	case model.SecurityNone:
		if !isLoopback(a.IMAPHost) {
			return nil, fmt.Errorf("refusing plaintext IMAP to %s: only localhost may skip TLS", a.IMAPHost)
		}
		c, err = imapclient.DialInsecure(addr, opts)
	default:
		return nil, fmt.Errorf("unknown IMAP security %q (tls, starttls)", a.IMAPSecurity)
	}
	if err != nil {
		return nil, &ConnectError{Addr: addr, Err: err}
	}
	if cr.Token != "" {
		err = c.Authenticate(&oauth.XOAuth2{User: a.Username, Token: cr.Token})
	} else {
		err = c.Login(a.Username, cr.Password).Wait()
	}
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("IMAP sign-in to %s as %s failed: %w", a.IMAPHost, a.Username, err)
	}
	return &Conn{Client: c, Changes: changes}, nil
}

// RoleAttr returns the role the server declared with a SPECIAL-USE attribute, or "".
func RoleAttr(d *imap.ListData) string {
	for _, at := range d.Attrs {
		switch at {
		case imap.MailboxAttrSent:
			return model.RoleSent
		case imap.MailboxAttrDrafts:
			return model.RoleDrafts
		case imap.MailboxAttrTrash:
			return model.RoleTrash
		case imap.MailboxAttrJunk:
			return model.RoleJunk
		case imap.MailboxAttrArchive:
			return model.RoleArchive
		case imap.MailboxAttrAll:
			return model.RoleAll
		}
	}
	return ""
}

// Role maps IMAP attributes and well-known names to a role.
func Role(d *imap.ListData) string {
	for _, at := range d.Attrs {
		switch at {
		case imap.MailboxAttrSent:
			return model.RoleSent
		case imap.MailboxAttrDrafts:
			return model.RoleDrafts
		case imap.MailboxAttrTrash:
			return model.RoleTrash
		case imap.MailboxAttrJunk:
			return model.RoleJunk
		case imap.MailboxAttrArchive:
			return model.RoleArchive
		case imap.MailboxAttrAll:
			return model.RoleAll
		}
	}
	n := strings.ToLower(d.Mailbox)
	if n == "inbox" {
		return model.RoleInbox
	}
	// fall back to names used by providers that skip SPECIAL-USE
	base := n
	if i := strings.LastIndexAny(n, "/."); i >= 0 {
		base = n[i+1:]
	}
	switch base {
	case "sent", "sent items", "sent messages", "sent mail", "enviados", "gesendet":
		return model.RoleSent
	case "drafts", "draft", "borradores", "entwürfe":
		return model.RoleDrafts
	case "trash", "deleted items", "deleted messages", "bin", "papelera", "papierkorb":
		return model.RoleTrash
	case "junk", "junk email", "junk e-mail", "spam", "bulk mail":
		return model.RoleJunk
	case "archive", "archives", "archivo":
		return model.RoleArchive
	}
	return ""
}

func hasAttr(d *imap.ListData, a imap.MailboxAttr) bool {
	for _, x := range d.Attrs {
		if x == a {
			return true
		}
	}
	return false
}

// Selectable reports whether the mailbox holds messages.
func Selectable(d *imap.ListData) bool {
	return !hasAttr(d, imap.MailboxAttrNoSelect) && !hasAttr(d, imap.MailboxAttrNonExistent)
}

// ---- SMTP ----

// Send delivers raw to recipients through the account's SMTP server.
func Send(ctx context.Context, a model.Account, cr Creds, from string, to []string, raw []byte) error {
	addr := net.JoinHostPort(a.SMTPHost, fmt.Sprint(a.SMTPPort))
	var c *smtp.Client
	var err error
	cfg := tlsConfig(a.SMTPHost, a.SMTPSecurity)
	switch a.SMTPSecurity {
	case model.SecurityTLS:
		c, err = smtp.DialTLS(addr, cfg)
	case model.SecuritySTARTTLS:
		c, err = smtp.DialStartTLS(addr, cfg)
	case model.SecurityNone:
		if !isLoopback(a.SMTPHost) {
			return fmt.Errorf("refusing plaintext SMTP to %s: only localhost may skip TLS", a.SMTPHost)
		}
		c, err = smtp.Dial(addr)
	default:
		return fmt.Errorf("unknown SMTP security %q", a.SMTPSecurity)
	}
	if err != nil {
		return fmt.Errorf("connect to SMTP server %s: %w", addr, err)
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("SMTP greeting from %s: %w", addr, err)
	}
	var auth sasl.Client
	if cr.Token != "" {
		auth = &oauth.XOAuth2{User: a.Username, Token: cr.Token}
	} else {
		auth = sasl.NewPlainClient("", a.Username, cr.Password)
	}
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("SMTP sign-in to %s as %s failed: %w", a.SMTPHost, a.Username, err)
	}
	if err := c.SendMail(from, to, strings.NewReader(string(raw))); err != nil {
		return fmt.Errorf("SMTP server %s rejected the message: %w", a.SMTPHost, err)
	}
	return c.Quit()
}

// TestSMTP signs in without sending.
func TestSMTP(ctx context.Context, a model.Account, cr Creds) error {
	addr := net.JoinHostPort(a.SMTPHost, fmt.Sprint(a.SMTPPort))
	var c *smtp.Client
	var err error
	cfg := tlsConfig(a.SMTPHost, a.SMTPSecurity)
	switch a.SMTPSecurity {
	case model.SecurityTLS:
		c, err = smtp.DialTLS(addr, cfg)
	case model.SecuritySTARTTLS:
		c, err = smtp.DialStartTLS(addr, cfg)
	default:
		c, err = smtp.Dial(addr)
	}
	if err != nil {
		return fmt.Errorf("connect to SMTP server %s: %w", addr, err)
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("SMTP greeting from %s: %w", addr, err)
	}
	var auth sasl.Client
	if cr.Token != "" {
		auth = &oauth.XOAuth2{User: a.Username, Token: cr.Token}
	} else {
		auth = sasl.NewPlainClient("", a.Username, cr.Password)
	}
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("SMTP sign-in to %s as %s failed: %w", a.SMTPHost, a.Username, err)
	}
	return c.Quit()
}

var _ io.Reader = (*strings.Reader)(nil)
