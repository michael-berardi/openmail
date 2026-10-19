package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-berardi/openmail/internal/core"
	"github.com/michael-berardi/openmail/internal/demo"
)

func connect(t *testing.T, app *core.App, scope string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	srv := New(app, "mcp:test-"+scope, scope)
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func tools(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, tl := range res.Tools {
		out[tl.Name] = true
	}
	return out
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}
	return out, res.IsError
}

func TestScopesAndSafety(t *testing.T) {
	app, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := demo.Seed(app); err != nil {
		t.Fatal(err)
	}

	// read scope: no writing tools at all
	ro := tools(t, connect(t, app, "read"))
	for _, n := range []string{"search_messages", "read_message", "list_threads"} {
		if !ro[n] {
			t.Errorf("read scope should have %s", n)
		}
	}
	for _, n := range []string{"create_draft", "update_messages", "send_draft", "set_setting"} {
		if ro[n] {
			t.Errorf("read scope must not expose %s", n)
		}
	}
	// write scope: drafts and organising, but never sending
	wr := connect(t, app, "write")
	wt := tools(t, wr)
	if !wt["create_draft"] || !wt["update_messages"] || wt["send_draft"] || wt["create_rule"] {
		t.Fatalf("write scope tools wrong: %v", wt)
	}

	// search + read, with injection and spoof detection
	out, isErr := call(t, wr, "search_messages", map[string]any{"query": "verify your account"})
	if isErr || out["count"].(float64) != 1 {
		t.Fatalf("search: %v %v", out, isErr)
	}
	id := out["messages"].([]any)[0].(map[string]any)["id"]
	msg, isErr := call(t, wr, "read_message", map[string]any{"id": id})
	if isErr {
		t.Fatalf("read: %v", msg)
	}
	if !strings.Contains(msg["notice"].(string), "untrusted") {
		t.Error("read_message must carry the untrusted-content notice")
	}
	notes, _ := json.Marshal(msg["security_notes"])
	for _, want := range []string{"instructions aimed at an AI", "authentication failed", "Reply-To differs"} {
		if !strings.Contains(string(notes), want) {
			t.Errorf("security notes missing %q: %s", want, notes)
		}
	}

	// triage view
	th, _ := call(t, wr, "list_threads", map[string]any{"query": "is:unread in:inbox"})
	if th["count"].(float64) < 3 {
		t.Fatalf("threads: %v", th)
	}

	// drafting works and sends nothing
	d, isErr := call(t, wr, "create_draft", map[string]any{"to": []string{"Pat <pat@example.com>"}, "subject": "Hi", "body": "Hello"})
	if isErr || d["status"] != "saved; not sent" {
		t.Fatalf("create_draft: %v", d)
	}
	if _, isErr := call(t, wr, "update_messages", map[string]any{"ids": []int{}, "action": "read"}); !isErr {
		t.Error("empty id list must be an error")
	}

	// send scope: policy decides. Default is approve -> pending, not queued.
	sd := connect(t, app, "send")
	draftID := d["draft"].(map[string]any)["id"]
	res, isErr := call(t, sd, "send_draft", map[string]any{"draft_id": draftID})
	if isErr || res["draft"].(map[string]any)["state"] != "pending_approval" {
		t.Fatalf("agent send must wait for approval: %v", res)
	}
	// admin: agents cannot change their own permissions
	ad := connect(t, app, "admin")
	if _, isErr := call(t, ad, "set_setting", map[string]any{"key": "agent.send_policy", "value": "allow"}); !isErr {
		t.Error("agent must not be able to change agent.* settings")
	}
	if _, isErr := call(t, ad, "set_setting", map[string]any{"key": "ui.theme", "value": "dark"}); isErr {
		t.Error("agent may change appearance settings")
	}
	if _, isErr := call(t, ad, "create_rule", map[string]any{"name": "x", "match_all": true,
		"conditions": []map[string]string{{"field": "from", "op": "contains", "value": "a"}}, "actions": []map[string]string{{"kind": "delete"}}}); !isErr {
		t.Error("agent rules must not delete mail")
	}
	if v, _, _ := app.Store.GetSetting("agent.send_policy"); v != "" {
		t.Errorf("policy changed to %q", v)
	}
	// audit trail records the actor
	entries, _ := app.Store.ListAudit(200, "mcp:test-")
	if len(entries) == 0 {
		t.Error("mcp calls must be audited")
	}
}
