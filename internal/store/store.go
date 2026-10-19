// Package store is OpenMail's local SQLite database: accounts, mail, full-text search,
// drafts, rules, audit log and settings. One file, WAL mode, safe across processes.
package store

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/michael-berardi/openmail/internal/model"
	"github.com/michael-berardi/openmail/internal/query"
)

type Store struct {
	db *sql.DB
}

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS accounts(
  id TEXT PRIMARY KEY, name TEXT, email TEXT, display_name TEXT,
  imap_host TEXT, imap_port INT, imap_security TEXT,
  smtp_host TEXT, smtp_port INT, smtp_security TEXT,
  username TEXT, auth TEXT, oauth_provider TEXT, signature TEXT, color TEXT,
  enabled INT DEFAULT 1, saves_sent INT DEFAULT 0, source TEXT, sync_days INT DEFAULT 0,
  created_at INT
);
CREATE TABLE IF NOT EXISTS mailboxes(
  id INTEGER PRIMARY KEY, account_id TEXT NOT NULL, name TEXT NOT NULL, delim TEXT, role TEXT,
  uid_validity INT DEFAULT 0, uid_next INT DEFAULT 0, last_uid INT DEFAULT 0,
  unread INT DEFAULT 0, total INT DEFAULT 0, subscribed INT DEFAULT 1, exists_count INT DEFAULT 0, mod_seq INT DEFAULT 0,
  UNIQUE(account_id, name)
);
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY, account_id TEXT NOT NULL, mailbox_id INT NOT NULL, uid INT NOT NULL,
  message_id TEXT, in_reply_to TEXT, refs TEXT, thread_id TEXT,
  subject TEXT, from_name TEXT, from_addr TEXT, to_json TEXT, cc_json TEXT, bcc_json TEXT, reply_to TEXT,
  date INT, size INT,
  seen INT, flagged INT, answered INT, draft INT, deleted INT, keywords TEXT,
  snippet TEXT, text TEXT, html TEXT, has_attach INT, attachments TEXT,
  list_unsub TEXT, list_unsub_post INT, auth_results TEXT, partial INT DEFAULT 0,
  UNIQUE(mailbox_id, uid)
);
CREATE INDEX IF NOT EXISTS idx_msg_mailbox_date ON messages(mailbox_id, date DESC);
CREATE INDEX IF NOT EXISTS idx_msg_thread ON messages(thread_id);
CREATE INDEX IF NOT EXISTS idx_msg_msgid ON messages(account_id, message_id);
CREATE INDEX IF NOT EXISTS idx_msg_from ON messages(from_addr);
CREATE TABLE IF NOT EXISTS raw(message_pk INTEGER PRIMARY KEY, data BLOB);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(subject, sender, recipients, body, tokenize='unicode61 remove_diacritics 2');
CREATE TABLE IF NOT EXISTS ops(
  id INTEGER PRIMARY KEY, account_id TEXT, kind TEXT, payload TEXT, created INT, attempts INT DEFAULT 0, last_error TEXT
);
CREATE TABLE IF NOT EXISTS drafts(
  id INTEGER PRIMARY KEY, account_id TEXT, json TEXT, updated INT, state TEXT DEFAULT 'draft', error TEXT, send_at INT DEFAULT 0, author TEXT, approved INT DEFAULT 0, server_uid INT DEFAULT 0, server_box TEXT, draft_key TEXT, queued_by TEXT
);
CREATE TABLE IF NOT EXISTS draft_files(id INTEGER PRIMARY KEY, draft_id INT, filename TEXT, content_type TEXT, data BLOB);
CREATE TABLE IF NOT EXISTS rules(
  id INTEGER PRIMARY KEY, name TEXT, enabled INT DEFAULT 1, account_id TEXT, match_all INT DEFAULT 1,
  conditions TEXT, actions TEXT, stop_after INT DEFAULT 0, source TEXT, sort INT DEFAULT 0
);
CREATE TABLE IF NOT EXISTS audit(
  id INTEGER PRIMARY KEY, ts INT, actor TEXT, action TEXT, args TEXT, ok INT, error TEXT
);
CREATE TABLE IF NOT EXISTS settings(k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE IF NOT EXISTS snoozed(message_pk INTEGER PRIMARY KEY, until INT, return_mailbox INT);
CREATE TABLE IF NOT EXISTS contacts(addr TEXT PRIMARY KEY, name TEXT, count INT DEFAULT 0, last INT DEFAULT 0, vip INT DEFAULT 0);
CREATE TABLE IF NOT EXISTS tokens(id INTEGER PRIMARY KEY, name TEXT UNIQUE, hash TEXT, scope TEXT, created INT, last_used INT);
CREATE TABLE IF NOT EXISTS remote_ok(addr TEXT PRIMARY KEY);
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(0)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	for _, c := range [][3]string{{"mailboxes", "exists_count", "INT DEFAULT 0"}, {"mailboxes", "mod_seq", "INT DEFAULT 0"}, {"messages", "partial", "INT DEFAULT 0"}, {"drafts", "server_uid", "INT DEFAULT 0"}, {"drafts", "server_box", "TEXT"}, {"drafts", "draft_key", "TEXT"}, {"drafts", "queued_by", "TEXT"}} {
		if err := addColumn(db, c[0], c[1], c[2]); err != nil {
			return nil, fmt.Errorf("migrate database %s: %w", path, err)
		}
	}
	_ = os.Chmod(path, 0o600)
	return &Store{db: db}, nil
}

func addColumn(db *sql.DB, table, col, def string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == col {
			has = true
		}
	}
	rows.Close()
	if has {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that need custom reads (tests, doctor).
func (s *Store) DB() *sql.DB { return s.db }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- settings ----

func (s *Store) GetSetting(k string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetSetting(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) DeleteSetting(k string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE k=?`, k)
	return err
}

func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT k,v FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---- accounts ----

const accountCols = `id,name,email,display_name,imap_host,imap_port,imap_security,smtp_host,smtp_port,smtp_security,username,auth,oauth_provider,signature,color,enabled,saves_sent,source,sync_days,created_at`

func scanAccount(sc interface{ Scan(...any) error }) (model.Account, error) {
	var a model.Account
	var en, ss int
	var oa, sig, col, src sql.NullString
	err := sc.Scan(&a.ID, &a.Name, &a.Email, &a.DisplayName, &a.IMAPHost, &a.IMAPPort, &a.IMAPSecurity, &a.SMTPHost, &a.SMTPPort, &a.SMTPSecurity, &a.Username, &a.Auth, &oa, &sig, &col, &en, &ss, &src, &a.SyncDays, &a.CreatedAt)
	a.OAuthProvider, a.Signature, a.Color, a.Source = oa.String, sig.String, col.String, src.String
	a.Enabled, a.SavesSent = en == 1, ss == 1
	return a, err
}

func (s *Store) PutAccount(a model.Account) error {
	if a.ID == "" {
		a.ID = strings.ToLower(a.Email)
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO accounts(`+accountCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET name=excluded.name,email=excluded.email,display_name=excluded.display_name,
	imap_host=excluded.imap_host,imap_port=excluded.imap_port,imap_security=excluded.imap_security,
	smtp_host=excluded.smtp_host,smtp_port=excluded.smtp_port,smtp_security=excluded.smtp_security,
	username=excluded.username,auth=excluded.auth,oauth_provider=excluded.oauth_provider,signature=excluded.signature,
	color=excluded.color,enabled=excluded.enabled,saves_sent=excluded.saves_sent,source=excluded.source,sync_days=excluded.sync_days`,
		a.ID, a.Name, a.Email, a.DisplayName, a.IMAPHost, a.IMAPPort, a.IMAPSecurity, a.SMTPHost, a.SMTPPort, a.SMTPSecurity, a.Username, a.Auth, a.OAuthProvider, a.Signature, a.Color, b2i(a.Enabled), b2i(a.SavesSent), a.Source, a.SyncDays, a.CreatedAt)
	return err
}

func (s *Store) GetAccount(id string) (model.Account, error) {
	a, err := scanAccount(s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id=?`, strings.ToLower(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("account %q: %w", id, ErrNotFound)
	}
	return a, err
}

func (s *Store) ListAccounts() ([]model.Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAccount removes an account and every local copy of its mail. The server is untouched.
func (s *Store) DeleteAccount(id string) error {
	id = strings.ToLower(id)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`DELETE FROM fts WHERE rowid IN (SELECT id FROM messages WHERE account_id=?)`,
		`DELETE FROM raw WHERE message_pk IN (SELECT id FROM messages WHERE account_id=?)`,
		`DELETE FROM snoozed WHERE message_pk IN (SELECT id FROM messages WHERE account_id=?)`,
		`DELETE FROM messages WHERE account_id=?`,
		`DELETE FROM mailboxes WHERE account_id=?`,
		`DELETE FROM ops WHERE account_id=?`,
		`DELETE FROM draft_files WHERE draft_id IN (SELECT id FROM drafts WHERE account_id=?)`,
		`DELETE FROM drafts WHERE account_id=?`,
		`DELETE FROM rules WHERE account_id=?`,
		`DELETE FROM accounts WHERE id=?`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q, id); err != nil {
			return fmt.Errorf("delete account data: %w", err)
		}
	}
	return tx.Commit()
}

// ---- mailboxes ----

func (s *Store) SetMailboxSubscribed(id int64, on bool) error {
	_, err := s.db.Exec(`UPDATE mailboxes SET subscribed=? WHERE id=?`, b2i(on), id)
	return err
}

func (s *Store) UpsertMailbox(m *model.Mailbox) error {
	res, err := s.db.Exec(`INSERT INTO mailboxes(account_id,name,delim,role,subscribed) VALUES(?,?,?,?,?)
	ON CONFLICT(account_id,name) DO UPDATE SET delim=excluded.delim, role=excluded.role`,
		m.AccountID, m.Name, m.Delim, m.Role, b2i(m.Subscribed))
	if err != nil {
		return err
	}
	if id, _ := res.LastInsertId(); id > 0 && m.ID == 0 {
		m.ID = id
	}
	return s.db.QueryRow(`SELECT id,uid_validity,uid_next,last_uid FROM mailboxes WHERE account_id=? AND name=?`, m.AccountID, m.Name).
		Scan(&m.ID, &m.UIDValidity, &m.UIDNext, &m.LastUID)
}

func (s *Store) SetMailboxSync(id int64, uidValidity, uidNext, lastUID uint32, exists int, modSeq uint64) error {
	_, err := s.db.Exec(`UPDATE mailboxes SET uid_validity=?, uid_next=?, last_uid=?, exists_count=?, mod_seq=? WHERE id=?`, uidValidity, uidNext, lastUID, exists, modSeq, id)
	return err
}

// HasUID reports whether the mailbox already holds a message with this UID.
func (s *Store) HasUID(mailboxID int64, uid uint32) bool {
	var n int
	_ = s.db.QueryRow(`SELECT 1 FROM messages WHERE mailbox_id=? AND uid=?`, mailboxID, uid).Scan(&n)
	return n == 1
}

// SetFlagsByUID updates the flags of the message with this UID, if held.
func (s *Store) SetFlagsByUID(mailboxID int64, uid uint32, seen, flagged, answered, draft, deleted bool, keywords []string) error {
	_, err := s.db.Exec(`UPDATE messages SET seen=?,flagged=?,answered=?,draft=?,deleted=?,keywords=? WHERE mailbox_id=? AND uid=?`,
		b2i(seen), b2i(flagged), b2i(answered), b2i(draft), b2i(deleted), jsonStr(keywords), mailboxID, uid)
	return err
}

func (s *Store) RefreshCounts(mailboxID int64) error {
	_, err := s.db.Exec(`UPDATE mailboxes SET
	  total=(SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND deleted=0),
	  unread=(SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND seen=0 AND deleted=0) WHERE id=?`, mailboxID, mailboxID, mailboxID)
	return err
}

func (s *Store) DeleteMailbox(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM fts WHERE rowid IN (SELECT id FROM messages WHERE mailbox_id=?)`,
		`DELETE FROM raw WHERE message_pk IN (SELECT id FROM messages WHERE mailbox_id=?)`,
		`DELETE FROM messages WHERE mailbox_id=?`,
		`DELETE FROM mailboxes WHERE id=?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearMailbox drops all local messages of a mailbox (used when UIDVALIDITY changes).
func (s *Store) ClearMailbox(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM fts WHERE rowid IN (SELECT id FROM messages WHERE mailbox_id=?)`,
		`DELETE FROM raw WHERE message_pk IN (SELECT id FROM messages WHERE mailbox_id=?)`,
		`DELETE FROM messages WHERE mailbox_id=?`,
		`UPDATE mailboxes SET last_uid=0, uid_next=0 WHERE id=?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const mailboxCols = `id,account_id,name,delim,role,uid_validity,uid_next,last_uid,unread,total,subscribed,COALESCE(exists_count,0),COALESCE(mod_seq,0)`

func scanMailbox(sc interface{ Scan(...any) error }) (model.Mailbox, error) {
	var m model.Mailbox
	var role sql.NullString
	var sub int
	err := sc.Scan(&m.ID, &m.AccountID, &m.Name, &m.Delim, &role, &m.UIDValidity, &m.UIDNext, &m.LastUID, &m.Unread, &m.Total, &sub, &m.Exists, &m.ModSeq)
	m.Role, m.Subscribed = role.String, sub == 1
	return m, err
}

func (s *Store) ListMailboxes(accountID string) ([]model.Mailbox, error) {
	q := `SELECT ` + mailboxCols + ` FROM mailboxes`
	var args []any
	if accountID != "" {
		q += ` WHERE account_id=?`
		args = append(args, strings.ToLower(accountID))
	}
	q += ` ORDER BY account_id, CASE role WHEN 'inbox' THEN 0 WHEN 'drafts' THEN 1 WHEN 'sent' THEN 2 WHEN 'archive' THEN 3 WHEN 'junk' THEN 4 WHEN 'trash' THEN 5 ELSE 9 END, name`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Mailbox
	for rows.Next() {
		m, err := scanMailbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetMailbox(id int64) (model.Mailbox, error) {
	m, err := scanMailbox(s.db.QueryRow(`SELECT `+mailboxCols+` FROM mailboxes WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, fmt.Errorf("mailbox %d: %w", id, ErrNotFound)
	}
	return m, err
}

// FindMailbox resolves a role or name within an account.
func (s *Store) FindMailbox(accountID, roleOrName string) (model.Mailbox, error) {
	accountID = strings.ToLower(accountID)
	m, err := scanMailbox(s.db.QueryRow(`SELECT `+mailboxCols+` FROM mailboxes WHERE account_id=? AND role=? ORDER BY length(name) LIMIT 1`, accountID, strings.ToLower(roleOrName)))
	if err == nil {
		return m, nil
	}
	m, err = scanMailbox(s.db.QueryRow(`SELECT `+mailboxCols+` FROM mailboxes WHERE account_id=? AND lower(name)=lower(?)`, accountID, roleOrName))
	if errors.Is(err, sql.ErrNoRows) {
		return m, fmt.Errorf("mailbox %q in %s: %w", roleOrName, accountID, ErrNotFound)
	}
	return m, err
}

// ---- messages ----

const msgCols = `m.id,m.account_id,m.mailbox_id,mb.name,COALESCE(mb.role,''),m.uid,m.message_id,m.in_reply_to,m.refs,m.thread_id,m.subject,m.from_name,m.from_addr,m.to_json,m.cc_json,m.bcc_json,m.reply_to,m.date,m.size,m.seen,m.flagged,m.answered,m.draft,m.deleted,m.keywords,m.snippet,m.has_attach,m.attachments,m.list_unsub,m.list_unsub_post,m.auth_results,COALESCE(sn.until,0),COALESCE(m.partial,0)`

// dupFilter hides the All Mail copy of a message that also lives in another folder (Gmail labels).
const dupFilter = `NOT (mb.role='all' AND m.message_id<>'' AND EXISTS (SELECT 1 FROM messages m2 JOIN mailboxes b2 ON b2.id=m2.mailbox_id WHERE m2.account_id=m.account_id AND m2.message_id=m.message_id AND m2.id<>m.id AND COALESCE(b2.role,'')<>'all'))`

const msgJoin = ` FROM messages m JOIN mailboxes mb ON mb.id=m.mailbox_id LEFT JOIN snoozed sn ON sn.message_pk=m.id`

func scanMessage(sc interface{ Scan(...any) error }) (model.Message, error) {
	var m model.Message
	var refs, to, cc, bcc, rt, kw, att, lu, ar sql.NullString
	var seen, fl, an, dr, de, ha, lup, part int
	err := sc.Scan(&m.ID, &m.AccountID, &m.MailboxID, &m.Mailbox, &m.MailboxRole, &m.UID, &m.MessageID, &m.InReplyTo, &refs, &m.ThreadID, &m.Subject, &m.From.Name, &m.From.Addr, &to, &cc, &bcc, &rt, &m.Date, &m.Size, &seen, &fl, &an, &dr, &de, &kw, &m.Snippet, &ha, &att, &lu, &lup, &ar, &m.SnoozedUntil, &part)
	if err != nil {
		return m, err
	}
	m.Seen, m.Flagged, m.Answered, m.Draft, m.Deleted, m.HasAttach, m.ListUnsubPost, m.Partial = seen == 1, fl == 1, an == 1, dr == 1, de == 1, ha == 1, lup == 1, part == 1
	m.ListUnsub, m.AuthResults = lu.String, ar.String
	_ = json.Unmarshal([]byte(refs.String), &m.References)
	_ = json.Unmarshal([]byte(to.String), &m.To)
	_ = json.Unmarshal([]byte(cc.String), &m.Cc)
	_ = json.Unmarshal([]byte(bcc.String), &m.Bcc)
	_ = json.Unmarshal([]byte(rt.String), &m.ReplyTo)
	_ = json.Unmarshal([]byte(kw.String), &m.Keywords)
	_ = json.Unmarshal([]byte(att.String), &m.Attachments)
	return m, nil
}

// SaveMessage inserts or updates a message (keyed by mailbox+uid) and its search row.
// text/html/raw are stored on insert; on update only flags change unless force is set.
func (s *Store) SaveMessage(m *model.Message, raw []byte) (inserted bool, err error) {
	var existing int64
	err = s.db.QueryRow(`SELECT id FROM messages WHERE mailbox_id=? AND uid=?`, m.MailboxID, m.UID).Scan(&existing)
	if err == nil {
		m.ID = existing
		_, err = s.db.Exec(`UPDATE messages SET seen=?,flagged=?,answered=?,draft=?,deleted=?,keywords=? WHERE id=?`,
			b2i(m.Seen), b2i(m.Flagged), b2i(m.Answered), b2i(m.Draft), b2i(m.Deleted), jsonStr(m.Keywords), m.ID)
		return false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO messages(account_id,mailbox_id,uid,message_id,in_reply_to,refs,thread_id,subject,from_name,from_addr,to_json,cc_json,bcc_json,reply_to,date,size,seen,flagged,answered,draft,deleted,keywords,snippet,text,html,has_attach,attachments,list_unsub,list_unsub_post,auth_results,partial)
	VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.AccountID, m.MailboxID, m.UID, m.MessageID, m.InReplyTo, jsonStr(m.References), m.ThreadID, m.Subject, m.From.Name, strings.ToLower(m.From.Addr), jsonStr(m.To), jsonStr(m.Cc), jsonStr(m.Bcc), jsonStr(m.ReplyTo), m.Date, m.Size,
		b2i(m.Seen), b2i(m.Flagged), b2i(m.Answered), b2i(m.Draft), b2i(m.Deleted), jsonStr(m.Keywords), m.Snippet, m.Text, m.HTML, b2i(m.HasAttach), jsonStr(m.Attachments), m.ListUnsub, b2i(m.ListUnsubPost), m.AuthResults, b2i(m.Partial))
	if err != nil {
		return false, fmt.Errorf("insert message: %w", err)
	}
	m.ID, _ = res.LastInsertId()
	body := m.Text
	if len(body) > 200_000 {
		body = body[:200_000]
	}
	if _, err = tx.Exec(`INSERT INTO fts(rowid,subject,sender,recipients,body) VALUES(?,?,?,?,?)`, m.ID, m.Subject, m.From.Name+" "+m.From.Addr, addrList(m.To)+" "+addrList(m.Cc), body); err != nil {
		return false, fmt.Errorf("index message: %w", err)
	}
	if len(raw) > 0 {
		if _, err = tx.Exec(`INSERT INTO raw(message_pk,data) VALUES(?,?)`, m.ID, packRaw(raw)); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func addrList(as []model.Address) string {
	var p []string
	for _, a := range as {
		p = append(p, a.Name+" "+a.Addr)
	}
	return strings.Join(p, " ")
}

func (s *Store) DeleteMessage(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM fts WHERE rowid=?`, `DELETE FROM raw WHERE message_pk=?`, `DELETE FROM snoozed WHERE message_pk=?`, `DELETE FROM messages WHERE id=?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetMessage returns a message with its body.
func (s *Store) GetMessage(id int64) (model.Message, error) {
	m, err := scanMessage(s.db.QueryRow(`SELECT `+msgCols+msgJoin+` WHERE m.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, fmt.Errorf("message %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return m, err
	}
	var t, h sql.NullString
	if err := s.db.QueryRow(`SELECT text, html FROM messages WHERE id=?`, id).Scan(&t, &h); err == nil {
		m.Text, m.HTML = t.String, h.String
	}
	return m, nil
}

func (s *Store) RawMessage(id int64) ([]byte, error) {
	var b []byte
	err := s.db.QueryRow(`SELECT data FROM raw WHERE message_pk=?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("raw source of message %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return unpackRaw(b)
}

func (s *Store) SetRaw(id int64, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO raw(message_pk,data) VALUES(?,?) ON CONFLICT(message_pk) DO UPDATE SET data=excluded.data`, id, packRaw(data))
	return err
}

// UIDsInMailbox returns uid -> local id for the mailbox.
func (s *Store) UIDsInMailbox(mailboxID int64) (map[uint32]int64, error) {
	rows, err := s.db.Query(`SELECT uid,id FROM messages WHERE mailbox_id=?`, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32]int64{}
	for rows.Next() {
		var u uint32
		var id int64
		if err := rows.Scan(&u, &id); err != nil {
			return nil, err
		}
		out[u] = id
	}
	return out, rows.Err()
}

// SetFlags updates the flag columns for one message.
func (s *Store) SetFlags(id int64, seen, flagged, answered, draft, deleted bool, keywords []string) error {
	_, err := s.db.Exec(`UPDATE messages SET seen=?,flagged=?,answered=?,draft=?,deleted=?,keywords=? WHERE id=?`,
		b2i(seen), b2i(flagged), b2i(answered), b2i(draft), b2i(deleted), jsonStr(keywords), id)
	return err
}

func (s *Store) UpdateMessageLocation(id, mailboxID int64, uid uint32) error {
	_, err := s.db.Exec(`UPDATE messages SET mailbox_id=?, uid=? WHERE id=?`, mailboxID, uid, id)
	return err
}

// FindThreadByMessageIDs returns the thread id of any local message whose Message-ID is in ids.
func (s *Store) FindThreadByMessageIDs(accountID string, ids []string) string {
	for _, id := range ids {
		if id == "" {
			continue
		}
		var t string
		if err := s.db.QueryRow(`SELECT thread_id FROM messages WHERE account_id=? AND message_id=? LIMIT 1`, accountID, id).Scan(&t); err == nil && t != "" {
			return t
		}
	}
	// A reply that arrived before its parent: look for messages that reference this id.
	return ""
}

// FindThreadByChildren finds a thread containing messages that reply to messageID.
func (s *Store) FindThreadByChildren(accountID, messageID string) string {
	if messageID == "" {
		return ""
	}
	var t string
	if err := s.db.QueryRow(`SELECT thread_id FROM messages WHERE account_id=? AND in_reply_to=? LIMIT 1`, accountID, messageID).Scan(&t); err == nil {
		return t
	}
	return ""
}

// Find runs a parsed query. mailboxIDs restricts the scope; excludeRoles hides trash/junk by default.
type FindOptions struct {
	Limit         int
	Offset        int
	IncludeJunk   bool // include junk and trash when the query names no mailbox
	Oldest        bool
	ExcludeSnooze bool
}

func (s *Store) Find(q query.Query, opt FindOptions) ([]model.Message, error) {
	if opt.Limit <= 0 || opt.Limit > 500 {
		opt.Limit = 50
	}
	var where []string
	var args []any
	join := ""
	if fts := query.FTS(q.Text); fts != "" {
		join = ` JOIN fts ON fts.rowid=m.id`
		where = append(where, `fts MATCH ?`)
		args = append(args, fts)
	}
	like := func(col string, vals []string) {
		for _, v := range vals {
			where = append(where, `(`+col+` LIKE ? ESCAPE '\')`)
			args = append(args, "%"+escLike(v)+"%")
		}
	}
	for _, v := range q.From {
		where = append(where, `(m.from_addr LIKE ? ESCAPE '\' OR m.from_name LIKE ? ESCAPE '\')`)
		args = append(args, "%"+escLike(v)+"%", "%"+escLike(v)+"%")
	}
	like(`m.to_json`, q.To)
	like(`m.cc_json`, q.Cc)
	like(`m.subject`, q.Subject)
	if q.Account != "" {
		where = append(where, `m.account_id=?`)
		args = append(args, q.Account)
	}
	if q.Mailbox != "" {
		where = append(where, `(mb.role=? OR lower(mb.name)=lower(?))`)
		args = append(args, strings.ToLower(q.Mailbox), q.Mailbox)
	} else if !opt.IncludeJunk {
		where = append(where, `COALESCE(mb.role,'') NOT IN ('junk','trash')`)
	}
	if q.Unread != nil {
		where = append(where, `m.seen=?`)
		args = append(args, b2i(!*q.Unread))
	}
	if q.Flagged != nil {
		where = append(where, `m.flagged=?`)
		args = append(args, b2i(*q.Flagged))
	}
	if q.HasAttachment != nil {
		where = append(where, `m.has_attach=?`)
		args = append(args, b2i(*q.HasAttachment))
	}
	if q.VIP != nil && *q.VIP {
		where = append(where, `m.from_addr IN (SELECT addr FROM contacts WHERE vip=1)`)
	}
	if q.Snoozed {
		where = append(where, `sn.until IS NOT NULL`)
	} else if opt.ExcludeSnooze {
		where = append(where, `sn.until IS NULL`)
	}
	if q.Before > 0 {
		where = append(where, `m.date<?`)
		args = append(args, q.Before)
	}
	if q.After > 0 {
		where = append(where, `m.date>=?`)
		args = append(args, q.After)
	}
	if q.Thread != "" {
		where = append(where, `m.thread_id=?`)
		args = append(args, q.Thread)
	}
	if q.Keyword != "" {
		where = append(where, `m.keywords LIKE ?`)
		args = append(args, `%"`+escJSONLike(q.Keyword)+`"%`)
	}
	where = append(where, `m.deleted=0`, dupFilter)
	order := `m.date DESC`
	if opt.Oldest {
		order = `m.date ASC`
	}
	sqlq := `SELECT ` + msgCols + msgJoin + join + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, opt.Limit, opt.Offset)
	rows, err := s.db.Query(sqlq, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	var out []model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func escLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func escJSONLike(s string) string {
	return escLike(strings.ReplaceAll(s, `"`, ""))
}

// ThreadMessages returns every message of a thread, oldest first, with bodies.
func (s *Store) ThreadMessages(threadID string) ([]model.Message, error) {
	rows, err := s.db.Query(`SELECT `+msgCols+msgJoin+` WHERE m.thread_id=? AND m.deleted=0 AND `+dupFilter+` ORDER BY m.date ASC`, threadID)
	if err != nil {
		return nil, err
	}
	var out []model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	for i := range out {
		var t, h sql.NullString
		if err := s.db.QueryRow(`SELECT text, html FROM messages WHERE id=?`, out[i].ID).Scan(&t, &h); err == nil {
			out[i].Text, out[i].HTML = t.String, h.String
		}
	}
	return out, nil
}

// ThreadSummary is one conversation row for list views.
type ThreadSummary struct {
	ThreadID string        `json:"thread_id"`
	Latest   model.Message `json:"latest"`
	Count    int           `json:"count"`
	Unread   int           `json:"unread"`
	Flagged  bool          `json:"flagged"`
	Senders  []string      `json:"senders"`
}

// Threads lists conversations matching q, newest activity first.
func (s *Store) Threads(q query.Query, opt FindOptions) ([]ThreadSummary, error) {
	// Fetch more messages than needed, then fold into threads in Go. Simple and predictable.
	limit := opt.Limit
	if limit <= 0 {
		limit = 50
	}
	fetch := opt
	fetch.Limit = 500
	fetch.Offset = 0
	seen := map[string]*ThreadSummary{}
	var order []string
	skip := opt.Offset
	for page := 0; page < 20 && len(order) < limit+skip; page++ {
		fetch.Offset = page * 500
		msgs, err := s.Find(q, fetch)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			t, ok := seen[m.ThreadID]
			if !ok {
				t = &ThreadSummary{ThreadID: m.ThreadID, Latest: m}
				seen[m.ThreadID] = t
				order = append(order, m.ThreadID)
			}
			t.Count++
			if !m.Seen {
				t.Unread++
			}
			if m.Flagged {
				t.Flagged = true
			}
			name := m.From.Name
			if name == "" {
				name = m.From.Addr
			}
			dup := false
			for _, n := range t.Senders {
				if n == name {
					dup = true
				}
			}
			if !dup && len(t.Senders) < 4 {
				t.Senders = append(t.Senders, name)
			}
		}
		if len(msgs) < 500 {
			break
		}
	}
	if skip > len(order) {
		skip = len(order)
	}
	order = order[skip:]
	if len(order) > limit {
		order = order[:limit]
	}
	out := make([]ThreadSummary, 0, len(order))
	for _, id := range order {
		out = append(out, *seen[id])
	}
	return out, nil
}

// ---- snooze ----

func (s *Store) Snooze(messageID int64, until int64) error {
	_, err := s.db.Exec(`INSERT INTO snoozed(message_pk,until) VALUES(?,?) ON CONFLICT(message_pk) DO UPDATE SET until=excluded.until`, messageID, until)
	return err
}

func (s *Store) Unsnooze(messageID int64) error {
	_, err := s.db.Exec(`DELETE FROM snoozed WHERE message_pk=?`, messageID)
	return err
}

// DueSnoozed returns message ids whose snooze has elapsed.
func (s *Store) DueSnoozed(now int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT message_pk FROM snoozed WHERE until<=?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---- contacts ----

func (s *Store) TouchContact(a model.Address, when int64) error {
	addr := strings.ToLower(strings.TrimSpace(a.Addr))
	if addr == "" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO contacts(addr,name,count,last) VALUES(?,?,1,?)
	ON CONFLICT(addr) DO UPDATE SET count=count+1, last=MAX(last,excluded.last), name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE name END`, addr, a.Name, when)
	return err
}

func (s *Store) SearchContacts(prefix string, limit int) ([]model.Contact, error) {
	if limit <= 0 {
		limit = 8
	}
	p := "%" + escLike(strings.ToLower(prefix)) + "%"
	rows, err := s.db.Query(`SELECT addr,name,count,last,vip FROM contacts WHERE lower(addr) LIKE ? ESCAPE '\' OR lower(name) LIKE ? ESCAPE '\' ORDER BY vip DESC, CASE WHEN lower(name) LIKE ? ESCAPE '\' OR lower(addr) LIKE ? ESCAPE '\' THEN 0 ELSE 1 END, count DESC LIMIT ?`, p, p, strings.ToLower(escLike(prefix))+"%", strings.ToLower(escLike(prefix))+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Contact
	for rows.Next() {
		var c model.Contact
		var vip int
		if err := rows.Scan(&c.Addr, &c.Name, &c.Count, &c.Last, &vip); err != nil {
			return nil, err
		}
		c.VIP = vip == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetVIP(addr string, vip bool) error {
	addr = strings.ToLower(strings.TrimSpace(addr))
	_, err := s.db.Exec(`INSERT INTO contacts(addr,name,count,last,vip) VALUES(?,?,0,0,?) ON CONFLICT(addr) DO UPDATE SET vip=excluded.vip`, addr, "", b2i(vip))
	return err
}

func (s *Store) VIPs() ([]model.Contact, error) {
	rows, err := s.db.Query(`SELECT addr,name,count,last FROM contacts WHERE vip=1 ORDER BY addr`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Contact
	for rows.Next() {
		var c model.Contact
		if err := rows.Scan(&c.Addr, &c.Name, &c.Count, &c.Last); err != nil {
			return nil, err
		}
		c.VIP = true
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- remote content allow-list ----

func (s *Store) AllowRemote(addr string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO remote_ok(addr) VALUES(?)`, strings.ToLower(addr))
	return err
}

func (s *Store) RemoteAllowed(addr string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM remote_ok WHERE addr=?`, strings.ToLower(addr)).Scan(&n)
	return n > 0
}

// ---- audit ----

func (s *Store) Audit(actor, action string, args any, opErr error) {
	ok, msg := 1, ""
	if opErr != nil {
		ok, msg = 0, opErr.Error()
	}
	a := jsonStr(args)
	if len(a) > 4000 {
		a = a[:4000] + "…"
	}
	_, _ = s.db.Exec(`INSERT INTO audit(ts,actor,action,args,ok,error) VALUES(?,?,?,?,?,?)`, time.Now().Unix(), actor, action, a, ok, msg)
}

func (s *Store) ListAudit(limit int, actor string) ([]model.AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id,ts,actor,action,args,ok,COALESCE(error,'') FROM audit`
	var args []any
	if actor != "" {
		q += ` WHERE actor LIKE ?`
		args = append(args, actor+"%")
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditEntry
	for rows.Next() {
		var e model.AuditEntry
		var ok int
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.Action, &e.Args, &ok, &e.Error); err != nil {
			return nil, err
		}
		e.OK = ok == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- ops queue (server writes to retry) ----

type Op struct {
	ID        int64
	AccountID string
	Kind      string
	Payload   string
	Attempts  int
}

func (s *Store) EnqueueOp(accountID, kind string, payload any) error {
	_, err := s.db.Exec(`INSERT INTO ops(account_id,kind,payload,created) VALUES(?,?,?,?)`, accountID, kind, jsonStr(payload), time.Now().Unix())
	return err
}

func (s *Store) PendingOps(accountID string, limit int) ([]Op, error) {
	rows, err := s.db.Query(`SELECT id,account_id,kind,payload,attempts FROM ops WHERE account_id=? ORDER BY id LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Op
	for rows.Next() {
		var o Op
		if err := rows.Scan(&o.ID, &o.AccountID, &o.Kind, &o.Payload, &o.Attempts); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) FinishOp(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ops WHERE id=?`, id)
	return err
}

func (s *Store) FailOp(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE ops SET attempts=attempts+1, last_error=? WHERE id=?`, msg, id)
	return err
}

func (s *Store) CountPendingOps() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM ops`).Scan(&n)
	return n, err
}

// ---- drafts ----

func (s *Store) SaveDraft(d *model.Draft) error {
	d.Updated = time.Now().Unix()
	if d.State == "" {
		d.State = "draft"
	}
	body := *d
	body.Attachments = nil
	if d.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO drafts(account_id,json,updated,state,error,send_at,author,approved,server_uid,server_box,draft_key,queued_by) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			d.AccountID, jsonStr(body), d.Updated, d.State, d.Error, d.SendAt, d.Author, b2i(d.Approved), d.ServerUID, d.ServerBox, d.DraftKey, d.QueuedBy)
		if err != nil {
			return err
		}
		d.ID, _ = res.LastInsertId()
	} else {
		// Content only. Workflow columns (state, send_at, approved, queued_by, error) change solely
		// through TransitionDraft/ClaimDraft/FailDraft so an edit can never overwrite a send in progress.
		if _, err := s.db.Exec(`UPDATE drafts SET account_id=?,json=?,updated=?,author=?,server_uid=?,server_box=?,draft_key=? WHERE id=?`,
			d.AccountID, jsonStr(body), d.Updated, d.Author, d.ServerUID, d.ServerBox, d.DraftKey, d.ID); err != nil {
			return err
		}
	}
	if d.Attachments != nil {
		if _, err := s.db.Exec(`DELETE FROM draft_files WHERE draft_id=?`, d.ID); err != nil {
			return err
		}
		for _, a := range d.Attachments {
			if _, err := s.db.Exec(`INSERT INTO draft_files(draft_id,filename,content_type,data) VALUES(?,?,?,?)`, d.ID, a.Filename, a.ContentType, a.Data); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) GetDraft(id int64) (model.Draft, error) {
	var d model.Draft
	var js, state, errs, author, sbox, dkey, qby sql.NullString
	var upd, sendAt, suid int64
	var appr int
	err := s.db.QueryRow(`SELECT json,updated,state,error,send_at,author,approved,COALESCE(server_uid,0),server_box,draft_key,queued_by FROM drafts WHERE id=?`, id).Scan(&js, &upd, &state, &errs, &sendAt, &author, &appr, &suid, &sbox, &dkey, &qby)
	if errors.Is(err, sql.ErrNoRows) {
		return d, fmt.Errorf("draft %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return d, err
	}
	_ = json.Unmarshal([]byte(js.String), &d)
	d.ID, d.Updated, d.State, d.Error, d.SendAt, d.Author, d.Approved = id, upd, state.String, errs.String, sendAt, author.String, appr == 1
	d.ServerUID, d.ServerBox, d.DraftKey, d.QueuedBy = uint32(suid), sbox.String, dkey.String, qby.String
	rows, err := s.db.Query(`SELECT filename,content_type,data FROM draft_files WHERE draft_id=? ORDER BY id`, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var a model.DraftAttachment
		if err := rows.Scan(&a.Filename, &a.ContentType, &a.Data); err != nil {
			return d, err
		}
		a.Size = len(a.Data)
		d.Attachments = append(d.Attachments, a)
	}
	return d, rows.Err()
}

func (s *Store) ListDrafts(state string) ([]model.Draft, error) {
	q := `SELECT id FROM drafts`
	var args []any
	if state != "" {
		q += ` WHERE state=?`
		args = append(args, state)
	}
	q += ` ORDER BY updated DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []model.Draft
	for _, id := range ids {
		d, err := s.GetDraft(id)
		if err != nil {
			return nil, err
		}
		for i := range d.Attachments {
			d.Attachments[i].Data = nil
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Store) DeleteDraft(id int64) error {
	for _, q := range []string{`DELETE FROM draft_files WHERE draft_id=?`, `DELETE FROM drafts WHERE id=?`} {
		if _, err := s.db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// ClaimDraft atomically moves a queued draft to "sending"; false means someone else has it.
func (s *Store) ClaimDraft(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE drafts SET state='sending' WHERE id=? AND state='queued'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CountAudit counts successful audit entries for an action by agent actors since a time.
func (s *Store) CountAudit(action string, actorPrefix string, since int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action=? AND ok=1 AND actor LIKE ? AND ts>=?`, action, actorPrefix+"%", since).Scan(&n)
	return n
}

// DueDrafts returns drafts queued to send whose send_at has passed.
func (s *Store) DueDrafts(now int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM drafts WHERE state='queued' AND send_at<=? ORDER BY send_at`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---- rules ----

func (s *Store) SaveRule(r *model.Rule) error {
	if r.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO rules(name,enabled,account_id,match_all,conditions,actions,stop_after,source,sort) VALUES(?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(sort),0)+1 FROM rules))`,
			r.Name, b2i(r.Enabled), r.AccountID, b2i(r.MatchAll), jsonStr(r.Conditions), jsonStr(r.Actions), b2i(r.StopAfter), r.Source)
		if err != nil {
			return err
		}
		r.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.db.Exec(`UPDATE rules SET name=?,enabled=?,account_id=?,match_all=?,conditions=?,actions=?,stop_after=?,source=? WHERE id=?`,
		r.Name, b2i(r.Enabled), r.AccountID, b2i(r.MatchAll), jsonStr(r.Conditions), jsonStr(r.Actions), b2i(r.StopAfter), r.Source, r.ID)
	return err
}

func (s *Store) ListRules() ([]model.Rule, error) {
	rows, err := s.db.Query(`SELECT id,name,enabled,COALESCE(account_id,''),match_all,conditions,actions,stop_after,COALESCE(source,'') FROM rules ORDER BY sort,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Rule
	for rows.Next() {
		var r model.Rule
		var en, ma, sa int
		var c, a string
		if err := rows.Scan(&r.ID, &r.Name, &en, &r.AccountID, &ma, &c, &a, &sa, &r.Source); err != nil {
			return nil, err
		}
		r.Enabled, r.MatchAll, r.StopAfter = en == 1, ma == 1, sa == 1
		_ = json.Unmarshal([]byte(c), &r.Conditions)
		_ = json.Unmarshal([]byte(a), &r.Actions)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRule(id int64) error {
	_, err := s.db.Exec(`DELETE FROM rules WHERE id=?`, id)
	return err
}

// ---- tokens ----

func (s *Store) CreateToken(name, hash, scope string) error {
	_, err := s.db.Exec(`INSERT INTO tokens(name,hash,scope,created) VALUES(?,?,?,?)`, name, hash, scope, time.Now().Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("a token named %q already exists", name)
	}
	return err
}

func (s *Store) TokenByHash(hash string) (model.Token, error) {
	var t model.Token
	err := s.db.QueryRow(`SELECT id,name,scope,created,COALESCE(last_used,0) FROM tokens WHERE hash=?`, hash).Scan(&t.ID, &t.Name, &t.Scope, &t.Created, &t.LastUse)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err == nil {
		_, _ = s.db.Exec(`UPDATE tokens SET last_used=? WHERE id=?`, time.Now().Unix(), t.ID)
	}
	return t, err
}

func (s *Store) ListTokens() ([]model.Token, error) {
	rows, err := s.db.Query(`SELECT id,name,scope,created,COALESCE(last_used,0) FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Token
	for rows.Next() {
		var t model.Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &t.Created, &t.LastUse); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(name string) error {
	res, err := s.db.Exec(`DELETE FROM tokens WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("token %q: %w", name, ErrNotFound)
	}
	return nil
}

// ---- raw message compression ----

// packRaw deflates a message at the fastest level; mail compresses 2-4x so the database stays small.
func packRaw(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = zw.Write(b)
	_ = zw.Close()
	if buf.Len() >= len(b) {
		return b
	}
	return append([]byte(zMagic), buf.Bytes()...)
}

// zMagic marks a compressed blob; a NUL first byte never starts an RFC 5322 message.
const zMagic = "\x00OMZ1"

func unpackRaw(b []byte) ([]byte, error) {
	if !bytes.HasPrefix(b, []byte(zMagic)) {
		return b, nil // stored uncompressed
	}
	zr, err := gzip.NewReader(bytes.NewReader(b[len(zMagic):]))
	if err != nil {
		return nil, fmt.Errorf("read stored message: %w", err)
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// UpdateFullMessage replaces a partial (large-message stub) row with the downloaded message.
func (s *Store) UpdateFullMessage(id int64, m *model.Message, raw []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE messages SET text=?,html=?,snippet=?,has_attach=?,attachments=?,size=?,partial=0 WHERE id=?`, m.Text, m.HTML, m.Snippet, b2i(m.HasAttach), jsonStr(m.Attachments), m.Size, id); err != nil {
		return err
	}
	body := m.Text
	if len(body) > 200_000 {
		body = body[:200_000]
	}
	if _, err := tx.Exec(`DELETE FROM fts WHERE rowid=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO fts(rowid,subject,sender,recipients,body) VALUES(?,?,?,?,?)`, id, m.Subject, m.From.Name+" "+m.From.Addr, addrList(m.To)+" "+addrList(m.Cc), body); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO raw(message_pk,data) VALUES(?,?) ON CONFLICT(message_pk) DO UPDATE SET data=excluded.data`, id, packRaw(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

// DraftByKey finds a local draft by its server-copy key.
func (s *Store) DraftByKey(key string) (int64, bool) {
	if key == "" {
		return 0, false
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM drafts WHERE draft_key=?`, key).Scan(&id); err != nil {
		return 0, false
	}
	return id, true
}

// SaveDraftMeta updates only the server-copy bookkeeping, leaving content and timestamps alone.
func (s *Store) SaveDraftMeta(d *model.Draft) error {
	_, err := s.db.Exec(`UPDATE drafts SET server_uid=?, server_box=?, draft_key=? WHERE id=?`, d.ServerUID, d.ServerBox, d.DraftKey, d.ID)
	return err
}

// ReplaceToken installs a fresh token under name, discarding any previous one.
func (s *Store) ReplaceToken(name, hash, scope string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM tokens WHERE name=?`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO tokens(name,hash,scope,created) VALUES(?,?,?,?)`, name, hash, scope, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// TransitionDraft atomically moves a draft between states. It reports false when the draft is not
// in one of the allowed source states (for example it is already being sent).
func (s *Store) TransitionDraft(id int64, from []string, to string, sendAt int64, approved bool, queuedBy string) (bool, error) {
	if len(from) == 0 {
		return false, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	args := []any{to, sendAt, b2i(approved), id}
	for _, f := range from {
		args = append(args, f)
	}
	args = append([]any{to, sendAt, b2i(approved), queuedBy}, args[3:]...)
	res, err := s.db.Exec(`UPDATE drafts SET state=?, send_at=?, approved=?, queued_by=?, error='', updated=`+strconv.FormatInt(time.Now().Unix(), 10)+` WHERE id=? AND state IN (`+marks+`)`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FailStuckDrafts marks drafts left in "sending" by a crash as failed so they can be retried.
func (s *Store) FailStuckDrafts() (int, error) {
	res, err := s.db.Exec(`UPDATE drafts SET state='failed', error='interrupted while sending: check Sent before retrying' WHERE state='sending'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// HasPlaceholderUID reports whether a mailbox holds locally assigned UIDs awaiting the real ones.
func (s *Store) HasPlaceholderUID(mailboxID int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT 1 FROM messages WHERE mailbox_id=? AND uid>=2147483648 LIMIT 1`, mailboxID).Scan(&n)
	return n == 1
}

// FailDraft records a failed send for a draft that is in "sending".
func (s *Store) FailDraft(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE drafts SET state='failed', error=? WHERE id=? AND state='sending'`, msg, id)
	return err
}

// CountQueuedBy counts drafts in the outbox that an agent (by actor prefix) put there.
func (s *Store) CountQueuedBy(prefixes ...string) int {
	total := 0
	for _, p := range prefixes {
		var n int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM drafts WHERE state IN ('queued','sending') AND queued_by LIKE ?`, p+"%").Scan(&n)
		total += n
	}
	return total
}
