// Package query parses OpenMail's search syntax.
//
//	from:alice subject:"q3 plan" is:unread has:attachment after:7d in:inbox budget
//
// Free words go to the full-text index; everything else becomes a structured filter.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Query struct {
	Text          []string `json:"text,omitempty"`
	From          []string `json:"from,omitempty"`
	To            []string `json:"to,omitempty"`
	Cc            []string `json:"cc,omitempty"`
	Subject       []string `json:"subject,omitempty"`
	Account       string   `json:"account,omitempty"`
	Mailbox       string   `json:"mailbox,omitempty"` // role (inbox, sent...) or exact name
	Unread        *bool    `json:"unread,omitempty"`
	Flagged       *bool    `json:"flagged,omitempty"`
	HasAttachment *bool    `json:"has_attachment,omitempty"`
	VIP           *bool    `json:"vip,omitempty"`
	Snoozed       bool     `json:"snoozed,omitempty"`
	Before        int64    `json:"before,omitempty"`
	After         int64    `json:"after,omitempty"`
	Thread        string   `json:"thread,omitempty"`
	Keyword       string   `json:"keyword,omitempty"`
}

func boolp(b bool) *bool { return &b }

// Parse turns a search string into a Query. now is injectable for tests.
func Parse(s string, now time.Time) (Query, error) {
	var q Query
	for _, tok := range split(s) {
		key, val, ok := strings.Cut(tok, ":")
		if !ok || val == "" || strings.ContainsAny(key, " \"") {
			q.Text = append(q.Text, strings.Trim(tok, "\""))
			continue
		}
		val = strings.Trim(val, "\"")
		switch strings.ToLower(key) {
		case "from":
			q.From = append(q.From, val)
		case "to":
			q.To = append(q.To, val)
		case "cc":
			q.Cc = append(q.Cc, val)
		case "subject":
			q.Subject = append(q.Subject, val)
		case "account":
			q.Account = strings.ToLower(val)
		case "in", "mailbox", "folder":
			q.Mailbox = val
		case "thread":
			q.Thread = val
		case "label", "keyword", "tag":
			q.Keyword = val
		case "is":
			switch strings.ToLower(val) {
			case "unread":
				q.Unread = boolp(true)
			case "read":
				q.Unread = boolp(false)
			case "flagged", "starred":
				q.Flagged = boolp(true)
			case "unflagged":
				q.Flagged = boolp(false)
			case "vip":
				q.VIP = boolp(true)
			case "snoozed":
				q.Snoozed = true
			default:
				return q, fmt.Errorf("unknown filter is:%s (unread, read, flagged, unflagged, vip, snoozed)", val)
			}
		case "has":
			if strings.EqualFold(val, "attachment") || strings.EqualFold(val, "attachments") {
				q.HasAttachment = boolp(true)
			} else {
				return q, fmt.Errorf("unknown filter has:%s (attachment)", val)
			}
		case "after", "since", "newer":
			t, err := ParseWhen(val, now)
			if err != nil {
				return q, err
			}
			q.After = t.Unix()
		case "before", "until", "older":
			t, err := ParseWhen(val, now)
			if err != nil {
				return q, err
			}
			q.Before = t.Unix()
		default:
			q.Text = append(q.Text, strings.Trim(tok, "\""))
		}
	}
	return q, nil
}

// ParseWhen accepts YYYY-MM-DD, today, yesterday, or a relative age like 7d, 2w, 3m, 1y.
func ParseWhen(v string, now time.Time) (time.Time, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), nil
	case "yesterday":
		return time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, now.Location()), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, now.Location()); err == nil {
		return t, nil
	}
	if len(v) >= 2 {
		n, err := strconv.Atoi(v[:len(v)-1])
		if err == nil && n >= 0 {
			switch v[len(v)-1] {
			case 'd':
				return now.AddDate(0, 0, -n), nil
			case 'w':
				return now.AddDate(0, 0, -7*n), nil
			case 'm':
				return now.AddDate(0, -n, 0), nil
			case 'y':
				return now.AddDate(-n, 0, 0), nil
			case 'h':
				return now.Add(-time.Duration(n) * time.Hour), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("cannot read date %q (use YYYY-MM-DD, today, yesterday, or 7d/2w/3m/1y)", v)
}

// split tokenises on spaces, keeping "quoted phrases" (also after key:) together.
func split(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && !inQ:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// FTS builds an FTS5 MATCH expression from free text terms. Every term is quoted so
// user input can never inject FTS operators.
func FTS(terms []string) string {
	var parts []string
	for _, t := range terms {
		t = strings.TrimSpace(strings.ReplaceAll(t, `"`, ` `))
		if t == "" {
			continue
		}
		if strings.ContainsAny(t, " \t") {
			parts = append(parts, `"`+t+`"`)
		} else {
			parts = append(parts, `"`+t+`"*`)
		}
	}
	return strings.Join(parts, " AND ")
}
