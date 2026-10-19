// Package model holds the plain data types shared by every OpenMail layer.
package model

import "time"

// Security is the transport security of a mail server connection.
const (
	SecurityTLS      = "tls"      // implicit TLS (993 / 465)
	SecuritySTARTTLS = "starttls" // upgrade after greeting (143 / 587)
	SecurityNone     = "none"     // plaintext, only allowed for localhost bridges
)

// Auth methods.
const (
	AuthPassword = "password"
	AuthOAuth2   = "oauth2"
)

// Mailbox roles, derived from IMAP SPECIAL-USE attributes or well-known names.
const (
	RoleInbox   = "inbox"
	RoleSent    = "sent"
	RoleDrafts  = "drafts"
	RoleTrash   = "trash"
	RoleJunk    = "junk"
	RoleArchive = "archive"
	RoleAll     = "all"
)

type Account struct {
	ID            string `json:"id"` // lower-cased email address
	Name          string `json:"name"`
	Email         string `json:"email"`
	DisplayName   string `json:"display_name"`
	IMAPHost      string `json:"imap_host"`
	IMAPPort      int    `json:"imap_port"`
	IMAPSecurity  string `json:"imap_security"`
	SMTPHost      string `json:"smtp_host"`
	SMTPPort      int    `json:"smtp_port"`
	SMTPSecurity  string `json:"smtp_security"`
	Username      string `json:"username"`
	Auth          string `json:"auth"`
	OAuthProvider string `json:"oauth_provider,omitempty"`
	Signature     string `json:"signature,omitempty"`
	Color         string `json:"color,omitempty"`
	Enabled       bool   `json:"enabled"`
	SavesSent     bool   `json:"saves_sent"` // server files sent mail itself (Gmail, iCloud)
	Source        string `json:"source"`     // manual | discovered | apple
	SyncDays      int    `json:"sync_days"`  // 0 = use global default
	CreatedAt     int64  `json:"created_at"`
}

type Mailbox struct {
	ID          int64  `json:"id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Delim       string `json:"delim"`
	Role        string `json:"role,omitempty"`
	UIDValidity uint32 `json:"uid_validity"`
	UIDNext     uint32 `json:"uid_next"`
	LastUID     uint32 `json:"last_uid"`
	Unread      int    `json:"unread"`
	Total       int    `json:"total"`
	Subscribed  bool   `json:"subscribed"`
	Exists      int    `json:"-"`
	ModSeq      uint64 `json:"-"`
}

type Address struct {
	Name string `json:"name,omitempty"`
	Addr string `json:"addr"`
}

type Attachment struct {
	Index       int    `json:"index"` // position in the MIME walk, used to fetch it
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Inline      bool   `json:"inline,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
}

type Message struct {
	ID            int64        `json:"id"`
	AccountID     string       `json:"account_id"`
	MailboxID     int64        `json:"mailbox_id"`
	Mailbox       string       `json:"mailbox,omitempty"`
	MailboxRole   string       `json:"mailbox_role,omitempty"`
	UID           uint32       `json:"uid"`
	MessageID     string       `json:"message_id"`
	InReplyTo     string       `json:"in_reply_to,omitempty"`
	References    []string     `json:"references,omitempty"`
	ThreadID      string       `json:"thread_id"`
	Subject       string       `json:"subject"`
	From          Address      `json:"from"`
	To            []Address    `json:"to"`
	Cc            []Address    `json:"cc,omitempty"`
	Bcc           []Address    `json:"bcc,omitempty"`
	ReplyTo       []Address    `json:"reply_to,omitempty"`
	Date          int64        `json:"date"`
	Size          int64        `json:"size"`
	Seen          bool         `json:"seen"`
	Flagged       bool         `json:"flagged"`
	Answered      bool         `json:"answered"`
	Draft         bool         `json:"draft"`
	Deleted       bool         `json:"deleted"`
	Keywords      []string     `json:"keywords,omitempty"`
	Snippet       string       `json:"snippet"`
	Text          string       `json:"text,omitempty"`
	HTML          string       `json:"html,omitempty"`
	HasHTML       bool         `json:"has_html,omitempty"`
	Partial       bool         `json:"partial,omitempty"` // large message not downloaded yet
	Attachments   []Attachment `json:"attachments,omitempty"`
	HasAttach     bool         `json:"has_attachments"`
	ListUnsub     string       `json:"list_unsubscribe,omitempty"`
	ListUnsubPost bool         `json:"list_unsubscribe_post,omitempty"`
	AuthResults   string       `json:"authentication_results,omitempty"`
	SnoozedUntil  int64        `json:"snoozed_until,omitempty"`
	VIP           bool         `json:"vip,omitempty"`
}

// Time returns the message date.
func (m *Message) Time() time.Time { return time.Unix(m.Date, 0) }

type Rule struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Enabled    bool        `json:"enabled"`
	AccountID  string      `json:"account_id,omitempty"` // empty = all accounts
	MatchAll   bool        `json:"match_all"`            // all conditions vs any
	Conditions []Condition `json:"conditions"`
	Actions    []Action    `json:"actions"`
	StopAfter  bool        `json:"stop_after,omitempty"`
	Source     string      `json:"source,omitempty"` // manual | apple | agent
}

// Condition fields: from, to, cc, subject, body, any_recipient, header:<Name>, has_attachment, list_id.
// Ops: contains, not_contains, equals, begins, ends, regex, exists.
type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// Action kinds: move, mark_read, mark_unread, flag, unflag, delete, archive, keyword, stop, notify.
type Action struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

type Contact struct {
	Addr  string `json:"addr"`
	Name  string `json:"name"`
	Count int    `json:"count"`
	Last  int64  `json:"last"`
	VIP   bool   `json:"vip"`
}

type AuditEntry struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"` // cli | web | mcp:<token name>
	Action string `json:"action"`
	Args   string `json:"args"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

type Draft struct {
	ID          int64             `json:"id"`
	AccountID   string            `json:"account_id"`
	To          []Address         `json:"to"`
	Cc          []Address         `json:"cc"`
	Bcc         []Address         `json:"bcc"`
	Subject     string            `json:"subject"`
	Body        string            `json:"body"`
	HTMLBody    string            `json:"html_body,omitempty"`
	InReplyTo   string            `json:"in_reply_to,omitempty"`
	References  []string          `json:"references,omitempty"`
	ReplyToMsg  int64             `json:"reply_to_message,omitempty"`
	Attachments []DraftAttachment `json:"attachments,omitempty"`
	Author      string            `json:"author"` // user | agent:<name>
	Updated     int64             `json:"updated"`
	Approved    bool              `json:"approved"`            // a human approved sending an agent draft
	State       string            `json:"state"`               // draft | queued | sent | failed
	QueuedBy    string            `json:"queued_by,omitempty"` // who put it in the outbox
	Error       string            `json:"error,omitempty"`
	SendAt      int64             `json:"send_at,omitempty"`
	ServerUID   uint32            `json:"-"` // copy in the server Drafts folder
	ServerBox   string            `json:"-"`
	DraftKey    string            `json:"-"` // ties the server copy to this draft
}

type DraftAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Data        []byte `json:"-"`
}

type Token struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Scope   string `json:"scope"` // read | write | send | admin
	Created int64  `json:"created"`
	LastUse int64  `json:"last_used"`
}

// Scope ordering: each scope includes the ones before it.
var scopeRank = map[string]int{"read": 1, "write": 2, "send": 3, "admin": 4}

// ScopeAllows reports whether scope have satisfies need.
func ScopeAllows(have, need string) bool {
	return scopeRank[have] >= scopeRank[need] && scopeRank[need] > 0
}
