// Package discover finds IMAP and SMTP settings from an email address, so adding an
// account is: type your address, confirm, done.
//
// Order: built-in providers by domain, Mozilla-style autoconfig (the domain's own
// server, then the Thunderbird ISP database), DNS SRV records (RFC 6186), MX hints for
// hosted domains (Google Workspace, Microsoft 365, Zoho, Fastmail, iCloud custom domains),
// then a probe of common host names.
package discover

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/michael-berardi/openmail/internal/model"
)

// Settings is the discovered configuration.
type Settings struct {
	Provider      string `json:"provider"`
	Source        string `json:"source"` // preset | autoconfig | srv | mx | probe
	IMAPHost      string `json:"imap_host"`
	IMAPPort      int    `json:"imap_port"`
	IMAPSecurity  string `json:"imap_security"`
	SMTPHost      string `json:"smtp_host"`
	SMTPPort      int    `json:"smtp_port"`
	SMTPSecurity  string `json:"smtp_security"`
	Username      string `json:"username"`
	Auth          string `json:"auth"`
	OAuthProvider string `json:"oauth_provider,omitempty"`
	SavesSent     bool   `json:"saves_sent"`
	// Help is a one-line hint shown to the user (for example "use an app-specific password").
	Help    string `json:"help,omitempty"`
	HelpURL string `json:"help_url,omitempty"`
}

type preset struct {
	name    string
	domains []string
	mx      []string // MX host suffixes that identify this provider
	s       Settings
}

func tls(host string, port int) (string, int, string) { return host, port, model.SecurityTLS }

var presets = []preset{
	{name: "iCloud", domains: []string{"icloud.com", "me.com", "mac.com"}, mx: []string{"mail.icloud.com", "icloud.com"},
		s: Settings{IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPSecurity: "starttls", Auth: model.AuthPassword, SavesSent: false,
			Help: "iCloud needs an app-specific password (appleid.apple.com → Sign-In and Security).", HelpURL: "https://support.apple.com/102654"}},
	{name: "Gmail", domains: []string{"gmail.com", "googlemail.com"}, mx: []string{"google.com", "googlemail.com"},
		s: Settings{IMAPHost: "imap.gmail.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthOAuth2, OAuthProvider: "google", SavesSent: true,
			Help: "Sign in with Google, or use an app password if you have 2-Step Verification.", HelpURL: "https://support.google.com/accounts/answer/185833"}},
	{name: "Microsoft 365 / Outlook", domains: []string{"outlook.com", "hotmail.com", "live.com", "msn.com", "outlook.co.uk", "hotmail.co.uk"}, mx: []string{"outlook.com", "protection.outlook.com", "mail.protection.outlook.com"},
		s: Settings{IMAPHost: "outlook.office365.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.office365.com", SMTPPort: 587, SMTPSecurity: "starttls", Auth: model.AuthOAuth2, OAuthProvider: "microsoft", SavesSent: true,
			Help: "Microsoft requires OAuth sign-in for most accounts."}},
	{name: "Yahoo", domains: []string{"yahoo.com", "ymail.com", "rocketmail.com", "yahoo.co.uk", "yahoo.ca"}, mx: []string{"yahoodns.net"},
		s: Settings{IMAPHost: "imap.mail.yahoo.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.mail.yahoo.com", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword, SavesSent: true,
			Help: "Yahoo needs an app password (Account Security → Generate app password).", HelpURL: "https://help.yahoo.com/kb/SLN15241.html"}},
	{name: "AOL", domains: []string{"aol.com"}, mx: []string{"aol.com"},
		s: Settings{IMAPHost: "imap.aol.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.aol.com", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword, SavesSent: true, Help: "AOL needs an app password."}},
	{name: "Fastmail", domains: []string{"fastmail.com", "fastmail.fm", "messagingengine.com"}, mx: []string{"messagingengine.com"},
		s: Settings{IMAPHost: "imap.fastmail.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.fastmail.com", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword,
			Help: "Fastmail needs an app password (Settings → Privacy & Security → App passwords)."}},
	{name: "Zoho Mail", domains: []string{"zoho.com", "zohomail.com"}, mx: []string{"zoho.com", "zohomail.com"},
		s: Settings{IMAPHost: "imap.zoho.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.zoho.com", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword,
			Help: "Zoho: enable IMAP in Settings → Mail Accounts → IMAP Access. Use an app-specific password if 2FA is on."}},
	{name: "Zoho Mail (EU)", domains: []string{"zoho.eu", "zohomail.eu"}, mx: []string{"zoho.eu", "zohomail.eu"},
		s: Settings{IMAPHost: "imap.zoho.eu", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.zoho.eu", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword}},
	{name: "Zoho Mail (India)", domains: []string{"zoho.in", "zohomail.in"}, mx: []string{"zoho.in", "zohomail.in"},
		s: Settings{IMAPHost: "imap.zoho.in", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.zoho.in", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword}},
	{name: "GMX", domains: []string{"gmx.com", "gmx.net", "gmx.de", "gmx.at"}, mx: []string{"gmx.net"},
		s: Settings{IMAPHost: "imap.gmx.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "mail.gmx.com", SMTPPort: 587, SMTPSecurity: "starttls", Auth: model.AuthPassword}},
	{name: "Mail.com", domains: []string{"mail.com"}, s: Settings{IMAPHost: "imap.mail.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.mail.com", SMTPPort: 587, SMTPSecurity: "starttls", Auth: model.AuthPassword}},
	{name: "Yandex", domains: []string{"yandex.com", "yandex.ru", "ya.ru"}, mx: []string{"yandex.net", "yandex.ru"},
		s: Settings{IMAPHost: "imap.yandex.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.yandex.com", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword}},
	{name: "Proton Mail (Bridge)", domains: []string{"proton.me", "protonmail.com", "pm.me", "protonmail.ch"}, mx: []string{"protonmail.ch"},
		s: Settings{IMAPHost: "127.0.0.1", IMAPPort: 1143, IMAPSecurity: "starttls", SMTPHost: "127.0.0.1", SMTPPort: 1025, SMTPSecurity: "starttls", Auth: model.AuthPassword,
			Help: "Install Proton Mail Bridge and use the Bridge password it shows, not your Proton password.", HelpURL: "https://proton.me/mail/bridge"}},
	{name: "Hostinger", mx: []string{"hostinger.com", "mx1.hostinger.com"}, s: Settings{IMAPHost: "imap.hostinger.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtp.hostinger.com", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword}},
	{name: "Namecheap Private Email", mx: []string{"privateemail.com"}, s: Settings{IMAPHost: "mail.privateemail.com", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "mail.privateemail.com", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword}},
	{name: "GoDaddy (Workspace)", mx: []string{"secureserver.net"}, s: Settings{IMAPHost: "imap.secureserver.net", IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "smtpout.secureserver.net", SMTPPort: 465, SMTPSecurity: "tls", Auth: model.AuthPassword}},
	{name: "Cloudflare Email Routing", mx: []string{"mx.cloudflare.net"}, s: Settings{Help: "Cloudflare Email Routing only forwards mail; it has no mailbox to read. Add the destination mailbox instead."}},
}

// Discover resolves settings for an email address.
func Discover(ctx context.Context, email string) (Settings, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at < 1 || at == len(email)-1 || strings.ContainsAny(email, " <>,;") {
		return Settings{}, fmt.Errorf("%q is not an email address", email)
	}
	domain := email[at+1:]

	for _, p := range presets {
		for _, d := range p.domains {
			if d == domain {
				return fromPreset(p, email, "preset"), nil
			}
		}
	}
	if s, err := autoconfig(ctx, email, domain); err == nil {
		return s, nil
	}
	if s, err := srv(ctx, email, domain); err == nil {
		return s, nil
	}
	if s, ok := mxLookup(ctx, email, domain); ok {
		return s, nil
	}
	if s, err := probe(ctx, email, domain); err == nil {
		return s, nil
	}
	return Settings{}, fmt.Errorf("could not find mail servers for %s: tried provider list, autoconfig, DNS SRV, MX hints and common host names. Enter the IMAP and SMTP hosts manually", domain)
}

func fromPreset(p preset, email, source string) Settings {
	s := p.s
	s.Provider, s.Source, s.Username = p.name, source, email
	if s.Auth == "" {
		s.Auth = model.AuthPassword
	}
	return s
}

func mxLookup(ctx context.Context, email, domain string) (Settings, bool) {
	var r net.Resolver
	mxs, err := r.LookupMX(ctx, domain)
	if err != nil || len(mxs) == 0 {
		return Settings{}, false
	}
	for _, mx := range mxs {
		host := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
		for _, p := range presets {
			if len(p.mx) == 0 || p.s.IMAPHost == "" {
				continue
			}
			for _, suf := range p.mx {
				if host == suf || strings.HasSuffix(host, "."+suf) {
					s := fromPreset(p, email, "mx")
					// A custom domain on a hosted provider keeps the provider's servers.
					return s, true
				}
			}
		}
	}
	return Settings{}, false
}

// ---- Mozilla autoconfig ----

type acXML struct {
	Provider struct {
		ID      string `xml:"id,attr"`
		Display string `xml:"displayName"`
		Servers []struct {
			Type     string `xml:"type,attr"`
			Hostname string `xml:"hostname"`
			Port     int    `xml:"port"`
			Socket   string `xml:"socketType"`
			Username string `xml:"username"`
			Auth     string `xml:"authentication"`
		} `xml:"incomingServer"`
		Out []struct {
			Hostname string `xml:"hostname"`
			Port     int    `xml:"port"`
			Socket   string `xml:"socketType"`
			Username string `xml:"username"`
			Auth     string `xml:"authentication"`
		} `xml:"outgoingServer"`
	} `xml:"emailProvider"`
}

func autoconfig(ctx context.Context, email, domain string) (Settings, error) {
	q := url.QueryEscape(email)
	urls := []string{
		"https://autoconfig." + domain + "/mail/config-v1.1.xml?emailaddress=" + q,
		"https://" + domain + "/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=" + q,
		"https://autoconfig.thunderbird.net/v1.1/" + domain,
	}
	client := &http.Client{Timeout: 6 * time.Second}
	var lastErr error
	for i, u := range urls {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
			continue
		}
		var x acXML
		if err := xml.Unmarshal(body, &x); err != nil {
			lastErr = err
			continue
		}
		s := Settings{Source: "autoconfig", Provider: x.Provider.Display, Auth: model.AuthPassword}
		if i == 2 {
			s.Source = "autoconfig"
		}
		local := email[:strings.Index(email, "@")]
		expand := func(u string) string {
			u = strings.ReplaceAll(u, "%EMAILADDRESS%", email)
			u = strings.ReplaceAll(u, "%EMAILLOCALPART%", local)
			u = strings.ReplaceAll(u, "%EMAILDOMAIN%", domain)
			return u
		}
		for _, sv := range x.Provider.Servers {
			if sv.Type == "imap" && s.IMAPHost == "" {
				s.IMAPHost, s.IMAPPort, s.IMAPSecurity = sv.Hostname, sv.Port, sock(sv.Socket)
				s.Username = expand(sv.Username)
				if strings.Contains(strings.ToLower(sv.Auth), "oauth") {
					s.Auth = model.AuthOAuth2
				}
			}
		}
		for _, sv := range x.Provider.Out {
			if s.SMTPHost == "" {
				s.SMTPHost, s.SMTPPort, s.SMTPSecurity = sv.Hostname, sv.Port, sock(sv.Socket)
			}
		}
		if s.IMAPHost != "" && s.SMTPHost != "" {
			if s.Username == "" {
				s.Username = email
			}
			if s.Auth == model.AuthOAuth2 {
				// OAuth needs a known provider with registered endpoints; fall back to password.
				s.Auth = model.AuthPassword
			}
			return s, nil
		}
		lastErr = fmt.Errorf("%s: no imap/smtp servers in config", u)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no autoconfig")
	}
	return Settings{}, lastErr
}

func sock(s string) string {
	switch strings.ToUpper(s) {
	case "SSL":
		return model.SecurityTLS
	case "STARTTLS":
		return model.SecuritySTARTTLS
	}
	return model.SecurityNone
}

// ---- SRV ----

func srv(ctx context.Context, email, domain string) (Settings, error) {
	var r net.Resolver
	lookup := func(service string) (string, int, error) {
		_, addrs, err := r.LookupSRV(ctx, service, "tcp", domain)
		if err != nil || len(addrs) == 0 {
			return "", 0, fmt.Errorf("no SRV %s", service)
		}
		sort.Slice(addrs, func(i, j int) bool { return addrs[i].Priority < addrs[j].Priority })
		h := strings.TrimSuffix(addrs[0].Target, ".")
		if h == "" {
			return "", 0, fmt.Errorf("SRV %s disabled", service)
		}
		return h, int(addrs[0].Port), nil
	}
	s := Settings{Source: "srv", Auth: model.AuthPassword, Username: email}
	if h, p, err := lookup("imaps"); err == nil {
		s.IMAPHost, s.IMAPPort, s.IMAPSecurity = h, p, model.SecurityTLS
	} else if h, p, err := lookup("imap"); err == nil {
		s.IMAPHost, s.IMAPPort, s.IMAPSecurity = h, p, model.SecuritySTARTTLS
	} else {
		return s, err
	}
	if h, p, err := lookup("submissions"); err == nil {
		s.SMTPHost, s.SMTPPort, s.SMTPSecurity = h, p, model.SecurityTLS
	} else if h, p, err := lookup("submission"); err == nil {
		s.SMTPHost, s.SMTPPort, s.SMTPSecurity = h, p, model.SecuritySTARTTLS
	} else {
		return s, err
	}
	return s, nil
}

// ---- probe ----

func probe(ctx context.Context, email, domain string) (Settings, error) {
	dial := func(host string, port int) bool {
		d := net.Dialer{Timeout: 3 * time.Second}
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	s := Settings{Source: "probe", Auth: model.AuthPassword, Username: email}
	for _, h := range []string{"imap." + domain, "mail." + domain, domain} {
		if dial(h, 993) {
			s.IMAPHost, s.IMAPPort, s.IMAPSecurity = h, 993, model.SecurityTLS
			break
		}
		if dial(h, 143) {
			s.IMAPHost, s.IMAPPort, s.IMAPSecurity = h, 143, model.SecuritySTARTTLS
			break
		}
	}
	if s.IMAPHost == "" {
		return s, fmt.Errorf("no IMAP server answered on imap.%[1]s, mail.%[1]s or %[1]s", domain)
	}
	for _, h := range []string{"smtp." + domain, "mail." + domain, domain, s.IMAPHost} {
		if dial(h, 465) {
			s.SMTPHost, s.SMTPPort, s.SMTPSecurity = h, 465, model.SecurityTLS
			break
		}
		if dial(h, 587) {
			s.SMTPHost, s.SMTPPort, s.SMTPSecurity = h, 587, model.SecuritySTARTTLS
			break
		}
	}
	if s.SMTPHost == "" {
		return s, fmt.Errorf("found IMAP at %s but no SMTP server answered", s.IMAPHost)
	}
	return s, nil
}

// Apply copies discovered settings onto an account.
func (s Settings) Apply(a *model.Account) {
	a.IMAPHost, a.IMAPPort, a.IMAPSecurity = s.IMAPHost, s.IMAPPort, s.IMAPSecurity
	a.SMTPHost, a.SMTPPort, a.SMTPSecurity = s.SMTPHost, s.SMTPPort, s.SMTPSecurity
	a.Auth, a.OAuthProvider, a.SavesSent = s.Auth, s.OAuthProvider, s.SavesSent
	if a.Username == "" {
		a.Username = s.Username
	}
}
