package rules

import (
	"testing"

	"github.com/michael-berardi/openmail/internal/model"
)

func TestEvaluate(t *testing.T) {
	m := model.Message{AccountID: "me@x.com", Subject: "Invoice 42", From: model.Address{Name: "Billing", Addr: "billing@acme.com"}, To: []model.Address{{Addr: "me@x.com"}}}
	raw := []byte("List-Id: <news.acme.com>\r\nSubject: Invoice 42\r\n\r\nbody")
	rs := []model.Rule{
		{Name: "a", Enabled: true, MatchAll: true, Conditions: []model.Condition{{Field: "from", Op: "contains", Value: "acme"}, {Field: "subject", Op: "begins", Value: "invoice"}}, Actions: []model.Action{{Kind: "move", Value: "Finance"}}},
		{Name: "b", Enabled: true, MatchAll: false, Conditions: []model.Condition{{Field: "subject", Op: "contains", Value: "nope"}, {Field: "list_id", Op: "exists"}}, Actions: []model.Action{{Kind: "mark_read"}}, StopAfter: true},
		{Name: "off", Enabled: false, MatchAll: true, Conditions: []model.Condition{{Field: "from", Op: "contains", Value: "acme"}}, Actions: []model.Action{{Kind: "flag"}}},
		{Name: "after-stop", Enabled: true, MatchAll: true, Conditions: []model.Condition{{Field: "from", Op: "contains", Value: "acme"}}, Actions: []model.Action{{Kind: "flag"}}},
		{Name: "other-account", Enabled: true, AccountID: "z@y.com", MatchAll: true, Conditions: []model.Condition{{Field: "from", Op: "contains", Value: "acme"}}, Actions: []model.Action{{Kind: "archive"}}},
	}
	acts := Evaluate(rs, &Input{Msg: m, Raw: raw})
	if len(acts) != 2 || acts[0].Kind != "move" || acts[1].Kind != "mark_read" {
		t.Fatalf("actions: %+v", acts)
	}
}

func TestValidate(t *testing.T) {
	bad := []model.Rule{
		{Name: "no conds", Actions: []model.Action{{Kind: "flag"}}},
		{Name: "no actions", Conditions: []model.Condition{{Field: "from", Op: "contains"}}},
		{Name: "regex", Conditions: []model.Condition{{Field: "from", Op: "regex", Value: "("}}, Actions: []model.Action{{Kind: "flag"}}},
		{Name: "move", Conditions: []model.Condition{{Field: "from", Op: "contains"}}, Actions: []model.Action{{Kind: "move"}}},
	}
	for _, r := range bad {
		if Validate(r) == nil {
			t.Errorf("%s should be invalid", r.Name)
		}
	}
}
