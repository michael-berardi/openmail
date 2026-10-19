package apple

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"
)

func bp(t *testing.T, v any) []byte {
	t.Helper()
	b, err := plist.Marshal(v, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildFixture lays out a fake home with the same table shapes macOS uses.
func buildFixture(t *testing.T) string {
	home := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(home, "Library", "Accounts"), 0o755))
	db, err := sql.Open("sqlite", filepath.Join(home, "Library", "Accounts", "Accounts4.sqlite"))
	must(err)
	for _, q := range []string{
		`CREATE TABLE ZACCOUNTTYPE(Z_PK INTEGER PRIMARY KEY, ZIDENTIFIER TEXT)`,
		`CREATE TABLE ZACCOUNT(Z_PK INTEGER PRIMARY KEY, ZACCOUNTTYPE INT, ZACTIVE INT, ZUSERNAME TEXT, ZACCOUNTDESCRIPTION TEXT, ZIDENTIFIER TEXT, ZPARENTACCOUNT INT)`,
		`CREATE TABLE ZACCOUNTPROPERTY(Z_PK INTEGER PRIMARY KEY, ZOWNER INT, ZKEY TEXT, ZVALUE BLOB)`,
		`INSERT INTO ZACCOUNTTYPE VALUES(1,'com.apple.account.IMAP'),(2,'com.apple.account.SMTP'),(3,'com.apple.account.Google'),(4,'com.apple.account.AppleAccount'),(5,'com.apple.account.CardDAV')`,
		`INSERT INTO ZACCOUNT VALUES(1,1,1,'pat@example.com','LDS','UUID-1',NULL)`,
		`INSERT INTO ZACCOUNT VALUES(2,2,1,'pat@example.com','smtp.zoho.com','UUID-2',NULL)`,
		`INSERT INTO ZACCOUNT VALUES(3,3,1,'sam@gmail.com','Gmail','UUID-3',NULL)`,
		`INSERT INTO ZACCOUNT VALUES(4,4,1,'apple.id@example.org','iCloud','UUID-4',NULL)`,
		`INSERT INTO ZACCOUNT VALUES(5,5,1,'contacts@example.com','Contacts','UUID-5',NULL)`,
	} {
		_, err := db.Exec(q)
		must(err)
	}
	ins := func(owner int, key string, v any) {
		_, err := db.Exec(`INSERT INTO ZACCOUNTPROPERTY(ZOWNER,ZKEY,ZVALUE) VALUES(?,?,?)`, owner, key, bp(t, v))
		must(err)
	}
	ins(1, "Hostname", "imap.zoho.com")
	ins(1, "PortNumber", uint64(993))
	ins(1, "SSLEnabled", true)
	ins(2, "Hostname", "smtp.zoho.com")
	ins(2, "PortNumber", uint64(465))
	ins(2, "SSLEnabled", true)
	ins(4, "EmailAddresses", []string{"apple.id@example.org", "pat@icloud.com"})
	must(db.Close())

	md := filepath.Join(home, "Library", "Mail", "V10", "MailData")
	must(os.MkdirAll(filepath.Join(md, "Signatures"), 0o755))
	rules := []any{
		map[string]any{
			"RuleName": "Invoices", "Active": true, "AllCriteriaMustBeSatisfied": "NO",
			"Criteria": []any{
				map[string]any{"Header": "From", "Qualifier": "Contains", "Expression": "billing@"},
				map[string]any{"Header": "Subject", "Qualifier": "BeginsWith", "Expression": "Invoice"},
			},
			"MarkRead": true, "MoveMessage": "imap://UUID-1/Finance%2FInvoices",
		},
		map[string]any{"RuleName": "Weird", "Criteria": []any{map[string]any{"Header": "From", "Qualifier": "IsInMyAddressBook"}}, "DeleteMessage": true},
	}
	must(os.WriteFile(filepath.Join(md, "SyncedRules.plist"), bp(t, rules), 0o644))
	must(os.WriteFile(filepath.Join(md, "Signatures", "S1.mailsignature"), []byte("Content-Type: text/html\r\n\r\n<div>Mike<br>Liberty Design Studio</div>"), 0o644))
	must(os.WriteFile(filepath.Join(md, "Signatures", "AccountsMap.plist"), bp(t, map[string]any{"UUID-1": map[string]any{"Signatures": []any{"S1"}}}), 0o644))
	must(os.WriteFile(filepath.Join(md, "VIPSenders.plist"), bp(t, []any{map[string]any{"emailAddresses": []any{"Boss@Example.com"}}}), 0o644))
	return home
}

func TestScan(t *testing.T) {
	f, err := Scan(buildFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	byEmail := map[string]Candidate{}
	for _, c := range f.Accounts {
		byEmail[c.Email] = c
	}
	z, ok := byEmail["pat@example.com"]
	if !ok || z.IMAPHost != "imap.zoho.com" || z.IMAPPort != 993 || !z.IMAPSSL || z.SMTPHost != "smtp.zoho.com" || z.SMTPPort != 465 {
		t.Fatalf("imap account not read correctly: %+v", z)
	}
	if g := byEmail["sam@gmail.com"]; g.Kind != "google" || g.Note == "" {
		t.Fatalf("google account: %+v", g)
	}
	if i := byEmail["pat@icloud.com"]; i.Kind != "icloud" {
		t.Fatalf("icloud mail address should come from the @icloud.com property, got %+v", f.Accounts)
	}
	if _, bad := byEmail["contacts@example.com"]; bad {
		t.Fatal("CardDAV account must not be listed as mail")
	}
	if len(f.Rules) != 1 {
		t.Fatalf("want 1 importable rule (the other has no equivalent), got %d: %+v", len(f.Rules), f.Rules)
	}
	r := f.Rules[0]
	if r.MatchAll || len(r.Conditions) != 2 || r.Conditions[1].Op != "begins" || r.Actions[1].Value != "Finance/Invoices" {
		t.Fatalf("rule converted wrong: %+v", r)
	}
	if len(f.Warnings) == 0 {
		t.Fatal("skipped rule must be reported in warnings")
	}
	if sig := f.Signatures["UUID-1"]; !strings.HasPrefix(sig, "Mike") || !strings.HasSuffix(sig, "Liberty Design Studio") {
		t.Fatalf("signature: %q", f.Signatures["UUID-1"])
	}
	if len(f.VIPs) != 1 || f.VIPs[0] != "boss@example.com" {
		t.Fatalf("vips: %v", f.VIPs)
	}
}

func TestScanNoAccess(t *testing.T) {
	_, err := Scan(t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a home without account data")
	}
}

func TestDiagnoseHasNoPersonalData(t *testing.T) {
	rep, err := Diagnose(buildFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"@", "imap.zoho.com", "pat@", "sam"} {
		if strings.Contains(rep, leak) {
			t.Fatalf("diagnose output leaks %q:\n%s", leak, rep)
		}
	}
	for _, want := range []string{"com.apple.account.IMAP: 1", "Hostname", "SyncedRules.plist"} {
		if !strings.Contains(rep, want) {
			t.Fatalf("diagnose missing %q:\n%s", want, rep)
		}
	}
}

// keyed wraps a value the way macOS stores account properties (NSKeyedArchiver).
func keyed(t *testing.T, objs ...any) []byte {
	t.Helper()
	return bp(t, map[string]any{"$archiver": "NSKeyedArchiver", "$version": uint64(100000), "$top": map[string]any{"root": plist.UID(1)}, "$objects": append([]any{"$null"}, objs...)})
}

func TestRealWorldShapes(t *testing.T) {
	home := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(home, "Library", "Accounts"), 0o755))
	db, err := sql.Open("sqlite", filepath.Join(home, "Library", "Accounts", "Accounts4.sqlite"))
	must(err)
	for _, q := range []string{
		`CREATE TABLE ZACCOUNTTYPE(Z_PK INTEGER PRIMARY KEY, ZIDENTIFIER TEXT)`,
		`CREATE TABLE ZACCOUNT(Z_PK INTEGER PRIMARY KEY, ZACCOUNTTYPE INT, ZACTIVE INT, ZUSERNAME TEXT, ZACCOUNTDESCRIPTION TEXT, ZIDENTIFIER TEXT, ZPARENTACCOUNT INT)`,
		`CREATE TABLE ZACCOUNTPROPERTY(Z_PK INTEGER PRIMARY KEY, ZOWNER INT, ZKEY TEXT, ZVALUE BLOB)`,
		`INSERT INTO ZACCOUNTTYPE VALUES(1,'com.apple.account.IMAP'),(2,'com.apple.account.SMTP')`,
		`INSERT INTO ZACCOUNT VALUES(17,1,1,'pat@example.org','Example','IMAP-UUID',NULL)`,
		`INSERT INTO ZACCOUNT VALUES(16,2,1,'pat@example.org','Example','SMTP-UUID',NULL)`,
	} {
		_, err := db.Exec(q)
		must(err)
	}
	ins := func(owner int, key string, blob []byte) {
		_, err := db.Exec(`INSERT INTO ZACCOUNTPROPERTY(ZOWNER,ZKEY,ZVALUE) VALUES(?,?,?)`, owner, key, blob)
		must(err)
	}
	ins(17, "Hostname", keyed(t, "imap.example.org"))
	ins(17, "PortNumber", keyed(t, uint64(993)))
	ins(17, "SSLEnabled", keyed(t, true))
	ins(17, "ACPropertyFullName", keyed(t, "Pat Example"))
	ins(17, "SendingAccountIdentifier", keyed(t, "SMTP-UUID"))
	ins(16, "Hostname", keyed(t, "smtp.example.org"))
	ins(16, "PortNumber", keyed(t, uint64(587)))
	ins(16, "SSLEnabled", keyed(t, true))
	must(db.Close())

	md := filepath.Join(home, "Library", "Mail", "V10", "MailData")
	must(os.MkdirAll(md, 0o755))
	rules := []any{map[string]any{
		"RuleName": "Client", "AllCriteriaMustBeSatisfied": false,
		"Criteria":              []any{map[string]any{"Header": "From", "Expression": "boss@client.example"}},
		"ShouldCopyMessage":     true,
		"CopyToMailboxURL":      "imap://pat%40example.org@imap.example.org/Clients/Acme",
		"ShouldTransferMessage": false, "MarkRead": false, "Deletes": false,
	}}
	must(os.WriteFile(filepath.Join(md, "SyncedRules.plist"), bp(t, rules), 0o644))

	f, err := Scan(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 {
		t.Fatalf("accounts: %+v", f.Accounts)
	}
	c := f.Accounts[0]
	if c.IMAPHost != "imap.example.org" || c.IMAPPort != 993 || !c.IMAPSSL || c.SMTPHost != "smtp.example.org" || c.SMTPPort != 587 || c.FullName != "Pat Example" {
		t.Fatalf("keyed-archive properties not decoded: %+v", c)
	}
	if len(f.Rules) != 1 || f.Rules[0].Actions[0].Kind != "copy" || f.Rules[0].Actions[0].Value != "Clients/Acme" || f.Rules[0].AccountID != "pat@example.org" || f.Rules[0].Conditions[0].Op != "contains" {
		t.Fatalf("real rule shape: %+v warnings=%v", f.Rules, f.Warnings)
	}
}
