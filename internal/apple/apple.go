// Package apple reads the configuration Apple Mail and macOS Internet Accounts already
// hold (accounts, rules, signatures, VIPs) so OpenMail can pick up where Mail left off.
//
// macOS protects these files: the process needs Full Disk Access, or point Scan at a copy
// of the folders. Reading is strictly read-only; Mail is never modified.
package apple

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"howett.net/plist"
	_ "modernc.org/sqlite"

	"github.com/michael-berardi/openmail/internal/compose"
	"github.com/michael-berardi/openmail/internal/model"
)

// ErrNoAccess means macOS blocked the read (Full Disk Access missing).
var ErrNoAccess = errors.New("macOS is blocking access to Apple Mail's settings")

// Candidate is one mail account found in the macOS account database.
type Candidate struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Username    string `json:"username"`
	Kind        string `json:"kind"` // icloud | google | microsoft | yahoo | imap | exchange | pop | other
	TypeID      string `json:"type_id"`
	IMAPHost    string `json:"imap_host,omitempty"`
	IMAPPort    int    `json:"imap_port,omitempty"`
	IMAPSSL     bool   `json:"imap_ssl,omitempty"`
	SMTPHost    string `json:"smtp_host,omitempty"`
	SMTPPort    int    `json:"smtp_port,omitempty"`
	SMTPSSL     bool   `json:"smtp_ssl,omitempty"`
	FullName    string `json:"full_name,omitempty"`
	Active      bool   `json:"active"`
	Identifier  string `json:"identifier"` // macOS account id (matches Mail's folder name)
	Note        string `json:"note,omitempty"`
}

// Found is everything Scan can read.
type Found struct {
	Accounts   []Candidate       `json:"accounts"`
	Rules      []model.Rule      `json:"rules"`
	Signatures map[string]string `json:"signatures"` // account identifier or email -> plain text signature
	VIPs       []string          `json:"vips"`
	Warnings   []string          `json:"warnings,omitempty"`
}

var emailRe = regexp.MustCompile(`^[A-Za-z0-9._%+'-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

func home(h string) string {
	if h != "" {
		return h
	}
	d, _ := os.UserHomeDir()
	return d
}

func isPerm(err error) bool {
	return errors.Is(err, fs.ErrPermission) || (err != nil && strings.Contains(err.Error(), "operation not permitted"))
}

func accessErr(path string, err error) error {
	return fmt.Errorf("%w. Allow it in System Settings → Privacy & Security → Full Disk Access for the app that runs OpenMail (your terminal), restart that app, and try again. Or copy ~/Library/Accounts and ~/Library/Mail into a folder and run `openmail import apple --from <folder>`", ErrNoAccess)
}

// Scan reads accounts, rules, signatures and VIPs. root is the home directory to read
// (default: the real one); it must contain Library/Accounts and Library/Mail.
func Scan(root string) (Found, error) {
	h := home(root)
	f := Found{Signatures: map[string]string{}}
	accts, err := scanAccounts(h)
	if err != nil {
		return f, err
	}
	f.Accounts = accts
	mailData, err := findMailData(h)
	if err != nil {
		f.Warnings = append(f.Warnings, err.Error())
		return f, nil
	}
	if mailData == "" {
		f.Warnings = append(f.Warnings, "no Mail data folder found (Mail has never been opened?); rules, signatures and VIPs were skipped")
		return f, nil
	}
	f.Rules, f.Warnings = scanRules(mailData, f.Warnings)
	sigs, w := scanSignatures(mailData, accts)
	f.Signatures = sigs
	f.Warnings = append(f.Warnings, w...)
	f.VIPs = scanVIPs(mailData)
	return f, nil
}

// findMailData returns ~/Library/Mail/V*/MailData with the highest version.
func findMailData(home string) (string, error) {
	base := filepath.Join(home, "Library", "Mail")
	ents, err := os.ReadDir(base)
	if err != nil {
		if isPerm(err) {
			return "", accessErr(base, err)
		}
		return "", nil
	}
	var vers []string
	for _, e := range ents {
		if e.IsDir() && regexp.MustCompile(`^V\d+$`).MatchString(e.Name()) {
			vers = append(vers, e.Name())
		}
	}
	sort.Slice(vers, func(i, j int) bool {
		var a, b int
		fmt.Sscanf(vers[i], "V%d", &a)
		fmt.Sscanf(vers[j], "V%d", &b)
		return a > b
	})
	for _, v := range vers {
		p := filepath.Join(base, v, "MailData")
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	return "", nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func decodeProp(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if _, err := plist.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	if m, ok := v.(map[string]any); ok {
		if _, keyed := m["$archiver"]; keyed {
			return unarchive(m)
		}
	}
	return v
}

// unarchive resolves an NSKeyedArchiver graph (what macOS stores for account properties) into
// plain strings, numbers, bools, slices and maps.
func unarchive(m map[string]any) any {
	objs, _ := m["$objects"].([]any)
	top, _ := m["$top"].(map[string]any)
	root, ok := top["root"].(plist.UID)
	if !ok {
		return nil
	}
	var res func(v any, depth int) any
	res = func(v any, depth int) any {
		if depth > 12 {
			return nil
		}
		if u, ok := v.(plist.UID); ok {
			if int(u) >= len(objs) {
				return nil
			}
			return res(objs[u], depth+1)
		}
		d, ok := v.(map[string]any)
		if !ok {
			if s, isStr := v.(string); isStr && s == "$null" {
				return nil
			}
			return v
		}
		if arr, ok := d["NS.objects"].([]any); ok {
			if keys, ok := d["NS.keys"].([]any); ok {
				out := map[string]any{}
				for i, k := range keys {
					if i < len(arr) {
						if ks, ok := res(k, depth+1).(string); ok {
							out[ks] = res(arr[i], depth+1)
						}
					}
				}
				return out
			}
			out := make([]any, 0, len(arr))
			for _, e := range arr {
				out = append(out, res(e, depth+1))
			}
			return out
		}
		if s, ok := d["NS.string"]; ok {
			return s
		}
		if rel, ok := d["NS.relative"]; ok {
			return res(rel, depth+1)
		}
		return nil
	}
	return res(root, 0)
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case uint64:
		return fmt.Sprint(x)
	case int64:
		return fmt.Sprint(x)
	case float64:
		return fmt.Sprint(int64(x))
	}
	return ""
}

func asInt(v any) int {
	switch x := v.(type) {
	case uint64:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		var n int
		fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

func asBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "yes") || strings.EqualFold(x, "true") || x == "1"
	case uint64:
		return x != 0
	case int64:
		return x != 0
	}
	return false
}

type rawAccount struct {
	pk                         int64
	username, desc, ident, typ string
	active                     bool
	parent                     int64
	props                      map[string]any
}

// openAccountsDB copies macOS's account database (and its WAL) to a temp file and opens the copy
// read-only, so Mail and the Accounts daemon are never disturbed.
func openAccountsDB(home string) (db *sql.DB, cleanup func(), err error) {
	var path string
	for _, n := range []string{"Accounts4.sqlite", "Accounts3.sqlite"} {
		p := filepath.Join(home, "Library", "Accounts", n)
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		} else if isPerm(err) {
			return nil, nil, accessErr(p, err)
		}
	}
	if path == "" {
		return nil, nil, fmt.Errorf("no macOS account database found under %s/Library/Accounts", home)
	}
	tmp, err := os.MkdirTemp("", "openmail-accounts-")
	if err != nil {
		return nil, nil, err
	}
	rm := func() { os.RemoveAll(tmp) }
	local := filepath.Join(tmp, "Accounts.sqlite")
	if err := copyFile(path, local); err != nil {
		rm()
		if isPerm(err) {
			return nil, nil, accessErr(path, err)
		}
		return nil, nil, fmt.Errorf("copy %s: %w", path, err)
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + ext); err == nil {
			_ = copyFile(path+ext, local+ext)
		}
	}
	db, err = sql.Open("sqlite", "file:"+local+"?mode=ro")
	if err != nil {
		rm()
		return nil, nil, err
	}
	return db, func() { db.Close(); rm() }, nil
}

// Diagnose prints the shape of the account database (table, type and property names and counts,
// never account names, addresses or values) so an import problem can be reported safely.
func Diagnose(root string) (string, error) {
	h := home(root)
	db, cleanup, err := openAccountsDB(h)
	if err != nil {
		return "", err
	}
	defer cleanup()
	var b strings.Builder
	fmt.Fprintf(&b, "home: %s\n", h)
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Z%' ORDER BY name`)
	if err == nil {
		var names []string
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			names = append(names, n)
		}
		rows.Close()
		fmt.Fprintf(&b, "tables: %s\n", strings.Join(names, ", "))
	}
	if cols, err := db.Query(`PRAGMA table_info(ZACCOUNT)`); err == nil {
		var names []string
		for cols.Next() {
			var cid, nn, pk int
			var name, typ string
			var d sql.NullString
			_ = cols.Scan(&cid, &name, &typ, &nn, &d, &pk)
			names = append(names, name)
		}
		cols.Close()
		fmt.Fprintf(&b, "ZACCOUNT columns: %s\n", strings.Join(names, ", "))
	}
	if r, err := db.Query(`SELECT COALESCE(t.ZIDENTIFIER,'(none)'), COUNT(*) FROM ZACCOUNT a LEFT JOIN ZACCOUNTTYPE t ON t.Z_PK=a.ZACCOUNTTYPE GROUP BY 1 ORDER BY 1`); err == nil {
		b.WriteString("account types (identifier: count)\n")
		for r.Next() {
			var id string
			var n int
			_ = r.Scan(&id, &n)
			fmt.Fprintf(&b, "  %s: %d\n", id, n)
		}
		r.Close()
	} else {
		fmt.Fprintf(&b, "account type query failed: %v\n", err)
	}
	if r, err := db.Query(`SELECT COALESCE(t.ZIDENTIFIER,'(none)'), COALESCE(p.ZKEY,''), COUNT(*) FROM ZACCOUNTPROPERTY p JOIN ZACCOUNT a ON a.Z_PK=p.ZOWNER LEFT JOIN ZACCOUNTTYPE t ON t.Z_PK=a.ZACCOUNTTYPE GROUP BY 1,2 ORDER BY 1,2`); err == nil {
		b.WriteString("property keys per type\n")
		for r.Next() {
			var id, key string
			var n int
			_ = r.Scan(&id, &key, &n)
			fmt.Fprintf(&b, "  %s . %s (%d)\n", id, key, n)
		}
		r.Close()
	}
	if md, err := findMailData(h); err == nil && md != "" {
		fmt.Fprintf(&b, "MailData: %s\n", filepath.Base(filepath.Dir(md)))
		if ents, err := os.ReadDir(md); err == nil {
			var names []string
			for _, e := range ents {
				names = append(names, e.Name())
			}
			fmt.Fprintf(&b, "  files: %s\n", strings.Join(names, ", "))
		}
	}
	return b.String(), nil
}

func scanAccounts(home string) ([]Candidate, error) {
	db, cleanup, err := openAccountsDB(home)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	rows, err := db.Query(`SELECT a.Z_PK, COALESCE(a.ZUSERNAME,''), COALESCE(a.ZACCOUNTDESCRIPTION,''), COALESCE(a.ZIDENTIFIER,''), COALESCE(t.ZIDENTIFIER,''), COALESCE(a.ZACTIVE,1), COALESCE(a.ZPARENTACCOUNT,0)
		FROM ZACCOUNT a LEFT JOIN ZACCOUNTTYPE t ON t.Z_PK=a.ZACCOUNTTYPE`)
	if err != nil {
		return nil, fmt.Errorf("read account table (unexpected Accounts database layout): %w", err)
	}
	byPK := map[int64]*rawAccount{}
	var all []*rawAccount
	for rows.Next() {
		var r rawAccount
		var act int
		if err := rows.Scan(&r.pk, &r.username, &r.desc, &r.ident, &r.typ, &act, &r.parent); err != nil {
			rows.Close()
			return nil, err
		}
		r.active, r.props = act != 0, map[string]any{}
		all = append(all, &r)
		byPK[r.pk] = &r
	}
	rows.Close()
	if prows, err := db.Query(`SELECT ZOWNER, COALESCE(ZKEY,''), ZVALUE FROM ZACCOUNTPROPERTY`); err == nil {
		for prows.Next() {
			var owner int64
			var key string
			var val []byte
			if err := prows.Scan(&owner, &key, &val); err == nil {
				if a := byPK[owner]; a != nil {
					a.props[key] = decodeProp(val)
				}
			}
		}
		prows.Close()
	}
	smtpByUser := map[string]*rawAccount{}
	smtpByIdent := map[string]*rawAccount{}
	for _, a := range all {
		if strings.Contains(a.typ, "SMTP") {
			smtpByUser[strings.ToLower(a.username)] = a
			smtpByIdent[a.ident] = a
		}
	}
	var out []Candidate
	for _, a := range all {
		kind := classify(a.typ)
		if kind == "" {
			continue
		}
		c := Candidate{Username: a.username, DisplayName: a.desc, Kind: kind, TypeID: a.typ, Active: a.active, Identifier: a.ident}
		c.Email = pickEmail(a)
		if c.Email == "" {
			continue
		}
		if h := asString(a.props["Hostname"]); h != "" {
			c.IMAPHost = h
			c.IMAPPort = asInt(a.props["PortNumber"])
			c.IMAPSSL = asBool(a.props["SSLEnabled"])
		}
		c.FullName = asString(a.props["ACPropertyFullName"])
		smtpAcct := smtpByIdent[asString(a.props["SendingAccountIdentifier"])]
		if smtpAcct == nil {
			smtpAcct = smtpByUser[strings.ToLower(a.username)]
		}
		if s := smtpAcct; s != nil {
			c.SMTPHost = asString(s.props["Hostname"])
			c.SMTPPort = asInt(s.props["PortNumber"])
			c.SMTPSSL = asBool(s.props["SSLEnabled"])
		}
		if kind == "google" || kind == "microsoft" || kind == "exchange" {
			c.Note = "Apple holds this sign-in as a protected token. OpenMail signs in again with an app password or its own OAuth sign-in."
		}
		if c.DisplayName == "" {
			c.DisplayName = c.Email
		}
		out = append(out, c)
	}
	// An address can appear twice (account + its mail child); keep the entry that has hosts.
	seen := map[string]int{}
	var dedup []Candidate
	for _, c := range out {
		k := strings.ToLower(c.Email)
		if i, ok := seen[k]; ok {
			if dedup[i].IMAPHost == "" && c.IMAPHost != "" {
				dedup[i] = c
			}
			continue
		}
		seen[k] = len(dedup)
		dedup = append(dedup, c)
	}
	return dedup, nil
}

func classify(typ string) string {
	t := strings.ToLower(typ)
	switch {
	case t == "":
		return ""
	case strings.Contains(t, "smtp"):
		return ""
	case strings.Contains(t, "google"):
		return "google"
	case strings.Contains(t, "exchange"), strings.Contains(t, "eas"):
		return "exchange"
	case strings.Contains(t, "yahoo"), strings.Contains(t, "aol"):
		return "yahoo"
	case strings.Contains(t, "hotmail"), strings.Contains(t, "outlook"), strings.Contains(t, "live"):
		return "microsoft"
	case strings.Contains(t, "appleaccount"), strings.Contains(t, "icloud"):
		return "icloud"
	case strings.Contains(t, "imap"):
		return "imap"
	case strings.Contains(t, "pop"):
		return "pop"
	}
	return ""
}

func pickEmail(a *rawAccount) string {
	if emailRe.MatchString(a.username) {
		if classify(a.typ) != "icloud" || isAppleDomain(a.username) {
			return strings.ToLower(a.username)
		}
	}
	// iCloud: the Apple ID may be a third-party address; the mail address is an @icloud.com value
	for _, v := range a.props {
		for _, s := range flattenStrings(v) {
			if emailRe.MatchString(s) && isAppleDomain(s) {
				return strings.ToLower(s)
			}
		}
	}
	for _, k := range []string{"EmailAddress", "PrimaryEmailAddress", "Email"} {
		if s := asString(a.props[k]); emailRe.MatchString(s) {
			return strings.ToLower(s)
		}
	}
	if emailRe.MatchString(a.desc) {
		return strings.ToLower(a.desc)
	}
	return ""
}

func isAppleDomain(e string) bool {
	e = strings.ToLower(e)
	return strings.HasSuffix(e, "@icloud.com") || strings.HasSuffix(e, "@me.com") || strings.HasSuffix(e, "@mac.com")
}

func flattenStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, i := range x {
			out = append(out, flattenStrings(i)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, i := range x {
			out = append(out, flattenStrings(i)...)
		}
		return out
	}
	return nil
}

// ---- rules ----

func readPlist(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = plist.Unmarshal(b, v)
	return err
}

func scanRules(mailData string, warnings []string) ([]model.Rule, []string) {
	var out []model.Rule
	for _, name := range []string{"SyncedRules.plist", "UnsyncedRules.plist"} {
		var raw any
		p := filepath.Join(mailData, name)
		if err := readPlist(p, &raw); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("could not read %s: %v", name, err))
			continue
		}
		list, _ := raw.([]any)
		if m, ok := raw.(map[string]any); ok {
			list, _ = m["rules"].([]any)
		}
		for _, item := range list {
			d, ok := item.(map[string]any)
			if !ok {
				continue
			}
			r, w := convertRule(d)
			if w != "" {
				warnings = append(warnings, w)
			}
			if r != nil {
				out = append(out, *r)
			}
		}
	}
	return out, warnings
}

var headerField = map[string]string{
	"from": "from", "to": "to", "cc": "cc", "subject": "subject", "anyrecipient": "any_recipient", "body": "body",
	"toorcc": "any_recipient", "listid": "list_id", "senderisnotinaddressbook": "", "sendervip": "",
}

var qualifierOp = map[string]string{
	"contains": "contains", "doesnotcontain": "not_contains", "isequalto": "equals", "beginswith": "begins", "endswith": "ends",
	"isnotequalto": "",
}

func convertRule(d map[string]any) (*model.Rule, string) {
	name := asString(d["RuleName"])
	if name == "" {
		name = "Imported rule"
	}
	r := &model.Rule{Name: name, Enabled: true, Source: "apple", MatchAll: true}
	if v, ok := d["Active"]; ok {
		r.Enabled = asBool(v)
	}
	if v, ok := d["AllCriteriaMustBeSatisfied"]; ok {
		r.MatchAll = asBool(v)
	}
	crit, _ := d["Criteria"].([]any)
	for _, c := range crit {
		cd, _ := c.(map[string]any)
		hdr := strings.ToLower(strings.ReplaceAll(asString(cd["Header"]), "-", ""))
		field, known := headerField[hdr]
		if !known {
			if hdr == "" {
				continue
			}
			field = "header:" + asString(cd["Header"])
		}
		if field == "" {
			return nil, fmt.Sprintf("rule %q skipped: condition %q has no OpenMail equivalent", name, asString(cd["Header"]))
		}
		op, ok := qualifierOp[strings.ToLower(asString(cd["Qualifier"]))]
		if !ok || op == "" {
			if q := asString(cd["Qualifier"]); q == "" {
				op = "contains"
			} else {
				return nil, fmt.Sprintf("rule %q skipped: qualifier %q is not supported", name, q)
			}
		}
		r.Conditions = append(r.Conditions, model.Condition{Field: field, Op: op, Value: asString(cd["Expression"])})
	}
	if asBool(d["MarkRead"]) {
		r.Actions = append(r.Actions, model.Action{Kind: "mark_read"})
	}
	if asBool(d["MarkFlagged"]) {
		r.Actions = append(r.Actions, model.Action{Kind: "flag"})
	}
	// Mail stores a destination in MailboxURL (move) or CopyToMailboxURL (copy), with flags saying which applies.
	if asBool(d["ShouldCopyMessage"]) {
		if dest := asString(d["CopyToMailboxURL"]); dest != "" {
			r.Actions = append(r.Actions, model.Action{Kind: "copy", Value: mailboxFromURL(dest)})
			r.AccountID = accountFromURL(dest)
		}
	}
	if asBool(d["ShouldTransferMessage"]) || asString(d["MoveMessage"]) != "" {
		dest := asString(d["MailboxURL"])
		if dest == "" {
			dest = asString(d["MoveMessage"])
		}
		if dest != "" {
			r.Actions = append(r.Actions, model.Action{Kind: "move", Value: mailboxFromURL(dest)})
			r.AccountID = accountFromURL(dest)
		}
	}
	if asBool(d["Deletes"]) || asBool(d["DeleteMessage"]) {
		r.Actions = append(r.Actions, model.Action{Kind: "delete"})
	}
	if asBool(d["StopEvaluatingRules"]) {
		r.StopAfter = true
	}
	if len(r.Conditions) == 0 || len(r.Actions) == 0 {
		return nil, fmt.Sprintf("rule %q skipped: Apple Mail rule uses conditions or actions OpenMail does not support", name)
	}
	return r, ""
}

// accountFromURL reads the account address from imap://user%40host@server/Folder.
func accountFromURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	if u.User != nil {
		if name := u.User.Username(); emailRe.MatchString(name) {
			return strings.ToLower(name)
		}
	}
	// imap://<account-uuid>/Folder: the caller maps the identifier to an address
	if u.Host != "" && !strings.Contains(u.Host, ".") {
		return "apple:" + u.Host
	}
	return ""
}

// mailboxFromURL turns imap://uuid/Folder%2FSub into "Folder/Sub".
func mailboxFromURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Path == "" {
		return s
	}
	p, err := url.PathUnescape(strings.TrimPrefix(u.Path, "/"))
	if err != nil {
		return strings.TrimPrefix(u.Path, "/")
	}
	return p
}

// ---- signatures ----

func scanSignatures(mailData string, accts []Candidate) (map[string]string, []string) {
	out := map[string]string{}
	var warn []string
	dir := filepath.Join(mailData, "Signatures")
	byUUID := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out, nil
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".mailsignature") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			warn = append(warn, fmt.Sprintf("signature %s unreadable: %v", e.Name(), err))
			continue
		}
		txt := ""
		if m, err := compose.Parse(b); err == nil && strings.TrimSpace(m.Text) != "" {
			txt = m.Text
		} else {
			// .mailsignature is a header block then HTML
			if i := strings.Index(string(b), "\n\n"); i > 0 {
				txt = stripHTML(string(b[i+2:]))
			}
		}
		byUUID[strings.TrimSuffix(e.Name(), ".mailsignature")] = strings.TrimSpace(txt)
	}
	var mapping map[string]any
	if err := readPlist(filepath.Join(dir, "AccountsMap.plist"), &mapping); err == nil {
		for acctID, v := range mapping {
			d, _ := v.(map[string]any)
			sigs, _ := d["Signatures"].([]any)
			for _, s := range sigs {
				if t := byUUID[asString(s)]; t != "" {
					out[acctID] = t
					break
				}
			}
		}
	}
	if len(out) == 0 && len(byUUID) == 1 {
		for _, t := range byUUID {
			out["*"] = t
		}
	}
	return out, warn
}

var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

func stripHTML(s string) string {
	s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</div>", "\n", "</p>", "\n").Replace(s)
	s = tagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(s))
}

// ---- VIPs ----

func scanVIPs(mailData string) []string {
	var raw any
	if err := readPlist(filepath.Join(mailData, "VIPSenders.plist"), &raw); err != nil {
		return nil
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if emailRe.MatchString(x) {
				out = append(out, strings.ToLower(x))
			}
		case []any:
			for _, i := range x {
				walk(i)
			}
		case map[string]any:
			for _, i := range x {
				walk(i)
			}
		}
	}
	walk(raw)
	sort.Strings(out)
	var dedup []string
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			dedup = append(dedup, s)
		}
	}
	return dedup
}
