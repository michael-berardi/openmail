// Package rules evaluates mail rules against incoming messages.
package rules

import (
	"bufio"
	"bytes"
	"fmt"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/michael-berardi/openmail/internal/model"
)

// Input is one message under evaluation.
type Input struct {
	Msg model.Message
	Raw []byte // optional; needed only for header:<Name> conditions

	hdr textproto.MIMEHeader
}

func (in *Input) header(name string) string {
	if in.hdr == nil {
		in.hdr = textproto.MIMEHeader{}
		if len(in.Raw) > 0 {
			if h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(in.Raw))).ReadMIMEHeader(); err == nil || len(h) > 0 {
				in.hdr = h
			}
		}
	}
	return strings.Join(in.hdr.Values(name), "\n")
}

func addrs(a []model.Address) string {
	var p []string
	for _, x := range a {
		p = append(p, strings.TrimSpace(x.Name+" <"+x.Addr+">"))
	}
	return strings.Join(p, ", ")
}

func (in *Input) field(f string) string {
	m := in.Msg
	switch strings.ToLower(f) {
	case "from":
		return strings.TrimSpace(m.From.Name + " <" + m.From.Addr + ">")
	case "to":
		return addrs(m.To)
	case "cc":
		return addrs(m.Cc)
	case "any_recipient", "recipient":
		return addrs(m.To) + ", " + addrs(m.Cc)
	case "subject":
		return m.Subject
	case "body":
		return m.Text
	case "list_id":
		return in.header("List-Id")
	}
	if strings.HasPrefix(strings.ToLower(f), "header:") {
		return in.header(f[7:])
	}
	return ""
}

// Validate reports problems with a rule so errors surface when it is saved, not at 3 a.m.
func Validate(r model.Rule) error {
	if len(r.Conditions) == 0 {
		return fmt.Errorf("rule %q has no conditions", r.Name)
	}
	if len(r.Actions) == 0 {
		return fmt.Errorf("rule %q has no actions", r.Name)
	}
	for _, c := range r.Conditions {
		switch c.Op {
		case "contains", "not_contains", "equals", "begins", "ends", "exists":
		case "regex":
			if _, err := regexp.Compile(c.Value); err != nil {
				return fmt.Errorf("rule %q: bad regex %q: %w", r.Name, c.Value, err)
			}
		default:
			return fmt.Errorf("rule %q: unknown condition op %q (contains, not_contains, equals, begins, ends, regex, exists)", r.Name, c.Op)
		}
		if c.Field == "" {
			return fmt.Errorf("rule %q: condition has no field", r.Name)
		}
	}
	for _, a := range r.Actions {
		switch a.Kind {
		case "move", "copy", "keyword":
			if a.Value == "" {
				return fmt.Errorf("rule %q: action %s needs a value", r.Name, a.Kind)
			}
		case "mark_read", "mark_unread", "flag", "unflag", "delete", "archive", "stop":
		default:
			return fmt.Errorf("rule %q: unknown action %q (move, copy, mark_read, mark_unread, flag, unflag, delete, archive, keyword, stop)", r.Name, a.Kind)
		}
	}
	return nil
}

func (in *Input) cond(c model.Condition) bool {
	if strings.EqualFold(c.Field, "has_attachment") {
		return in.Msg.HasAttach == (c.Value == "" || strings.EqualFold(c.Value, "true"))
	}
	v := in.field(c.Field)
	lv, lc := strings.ToLower(v), strings.ToLower(c.Value)
	switch c.Op {
	case "contains":
		return strings.Contains(lv, lc)
	case "not_contains":
		return !strings.Contains(lv, lc)
	case "equals":
		return lv == lc
	case "begins":
		return strings.HasPrefix(lv, lc)
	case "ends":
		return strings.HasSuffix(lv, lc)
	case "exists":
		return strings.TrimSpace(v) != ""
	case "regex":
		re, err := regexp.Compile("(?i)" + c.Value)
		return err == nil && re.MatchString(v)
	}
	return false
}

// Matches reports whether the rule applies to the input.
func Matches(r model.Rule, in *Input) bool {
	if !r.Enabled || len(r.Conditions) == 0 {
		return false
	}
	if r.AccountID != "" && !strings.EqualFold(r.AccountID, in.Msg.AccountID) {
		return false
	}
	for _, c := range r.Conditions {
		ok := in.cond(c)
		if r.MatchAll && !ok {
			return false
		}
		if !r.MatchAll && ok {
			return true
		}
	}
	return r.MatchAll
}

// Evaluate returns the actions to run for a message, in rule order, honouring stop.
func Evaluate(rs []model.Rule, in *Input) []model.Action {
	var out []model.Action
	for _, r := range rs {
		if !Matches(r, in) {
			continue
		}
		stop := r.StopAfter
		for _, a := range r.Actions {
			if a.Kind == "stop" {
				stop = true
				continue
			}
			out = append(out, a)
		}
		if stop {
			break
		}
	}
	return out
}
