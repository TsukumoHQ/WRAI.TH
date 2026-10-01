package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// W9 S3 0bbbf8dd — a lead whose reports_to is name@project escalates to that
// executive across projects; the executive answers on the same thread only
// (design identity-routing.md §3). Fixture = the field case: synergix prod
// lead ai-lead escalating to cto@synergix-dev (founder Q4 2026-10-01).

func xpRegister(t *testing.T, h *Handlers, project, name string, extra map[string]any) {
	t.Helper()
	args := map[string]any{"project": project, "name": name}
	for k, v := range extra {
		args[k] = v
	}
	if res, _ := h.HandleRegisterAgent(ctx, call(args)); res.IsError {
		t.Fatalf("register %s@%s: %s", name, project, expectError(t, res))
	}
}

func xpSend(h *Handlers, args map[string]any) *mcp.CallToolResult {
	res, _ := h.HandleSendMessage(ctx, call(args))
	return res
}

func xpSentID(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("send: %s", expectError(t, res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out)
	id, _ := out["id"].(string)
	return id
}

func xpInbox(t *testing.T, h *Handlers, project, agent string) []string {
	t.Helper()
	res, _ := h.HandleGetInbox(ctx, call(map[string]any{"project": project, "as": agent, "format": "json"}))
	msgs, _ := parseJSON(t, res)["messages"].([]any)
	var out []string
	for _, m := range msgs {
		if c, ok := m.(map[string]any)["content"].(string); ok {
			out = append(out, c)
		}
	}
	return out
}

// synergixFleet: cto is an executive in synergix-dev; in synergix (prod)
// cto-synergix is the local executive (so teams exist and CanMessage is
// enforced), ai-lead reports to cto@synergix-dev, data-lead has no such chain,
// and a plain local agent is also named cto (the homonym).
func synergixFleet(t *testing.T) *Handlers {
	h := testHandlers(t)
	xpRegister(t, h, "synergix-dev", "cto", map[string]any{"is_executive": true})
	xpRegister(t, h, "synergix", "cto-synergix", map[string]any{"is_executive": true})
	xpRegister(t, h, "synergix", "cto", nil)
	xpRegister(t, h, "synergix", "ai-lead", map[string]any{"reports_to": "cto@synergix-dev"})
	xpRegister(t, h, "synergix", "data-lead", nil)
	return h
}

func escalate(from string) map[string]any {
	return map[string]any{"project": "synergix", "as": from, "target_project": "synergix-dev", "to": "cto", "content": "escalation from " + from}
}

// AC1 — reports_to=cto@synergix-dev reaches the executive; without the chain
// the send is FORBIDDEN and the hint names reports_to=cto@synergix-dev.
func TestXProjectEscalateUpByQualifiedReportsTo(t *testing.T) {
	h := synergixFleet(t)
	xpSentID(t, xpSend(h, escalate("ai-lead")))
	if got := xpInbox(t, h, "synergix-dev", "cto"); len(got) != 1 || got[0] != "escalation from ai-lead" {
		t.Fatalf("cto@synergix-dev inbox: %v", got)
	}

	body := decodeToolError(t, xpSend(h, escalate("data-lead")))
	if body["code"] != CodeForbidden || !strings.Contains(body["hint"].(string), "reports_to=cto@synergix-dev") {
		t.Fatalf("no chain: want FORBIDDEN hinting reports_to=cto@synergix-dev, got %v", body)
	}

	// Escalation only lands on an executive: a qualified chain to a plain
	// agent opens nothing.
	xpRegister(t, h, "synergix-dev", "qa-lead", nil)
	xpRegister(t, h, "synergix", "ops-lead", map[string]any{"reports_to": "qa-lead@synergix-dev"})
	res := xpSend(h, map[string]any{"project": "synergix", "as": "ops-lead", "target_project": "synergix-dev", "to": "qa-lead", "content": "x"})
	if decodeToolError(t, res)["code"] != CodeForbidden {
		t.Fatalf("qualified chain to a non-executive must be FORBIDDEN, got %v", decodeToolError(t, res))
	}
}

// AC2 — register_agent refuses reports_to=name@unknown-project (and a
// malformed qualifier); a known project is stored normalized.
func TestXProjectReportsToValidated(t *testing.T) {
	h := synergixFleet(t)
	for _, bad := range []string{"cto@no-such-project", "cto@", "@synergix-dev"} {
		res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "synergix", "name": "x-lead", "reports_to": bad}))
		if body := decodeToolError(t, res); body["code"] != CodeInvalidArgument {
			t.Fatalf("reports_to=%q: want %s, got %v", bad, CodeInvalidArgument, body)
		}
	}
	if a, _ := h.db.GetAgent("synergix", "x-lead"); a != nil {
		t.Fatal("a refused register must write nothing")
	}
	xpRegister(t, h, "synergix", "y-lead", map[string]any{"reports_to": "CTO@Synergix_Dev"})
	if a, _ := h.db.GetAgent("synergix", "y-lead"); a == nil || a.ReportsTo == nil || *a.ReportsTo != "cto@synergix-dev" {
		t.Fatalf("qualified reports_to must be stored normalized, got %+v", a)
	}
}

// AC3 — the qualified chain grants nothing to the local agent named cto, in
// either direction.
func TestXProjectQualifiedReportsToGrantsNoLocalHomonym(t *testing.T) {
	h := synergixFleet(t)
	if res := xpSend(h, map[string]any{"project": "synergix", "as": "ai-lead", "to": "cto", "content": "x"}); !res.IsError {
		t.Fatal("ai-lead -> local cto must stay blocked: reports_to=cto@synergix-dev is not the local cto")
	}
	if res := xpSend(h, map[string]any{"project": "synergix", "as": "cto", "to": "ai-lead", "content": "x"}); !res.IsError {
		t.Fatal("local cto -> ai-lead must stay blocked")
	}
	if ok, _ := h.db.CanMessage("synergix", "ai-lead", "cto"); ok {
		t.Fatal("CanMessage must not treat the qualified chain as a local reports_to")
	}
}

// AC4 — cto answers the lead on the escalation thread only; a fresh message,
// or a reply on a message that was not theirs, is FORBIDDEN.
func TestXProjectReplyDownThreadScoped(t *testing.T) {
	h := synergixFleet(t)
	up := xpSentID(t, xpSend(h, escalate("ai-lead")))

	xpSentID(t, xpSend(h, map[string]any{"project": "synergix-dev", "as": "cto", "target_project": "synergix", "to": "ai-lead", "reply_to": up, "content": "ruling"}))
	if got := xpInbox(t, h, "synergix", "ai-lead"); len(got) != 1 || got[0] != "ruling" {
		t.Fatalf("ai-lead inbox: %v", got)
	}

	fresh := xpSend(h, map[string]any{"project": "synergix-dev", "as": "cto", "target_project": "synergix", "to": "ai-lead", "content": "unsolicited"})
	if decodeToolError(t, fresh)["code"] != CodeForbidden {
		t.Fatalf("fresh cto -> ai-lead must be FORBIDDEN, got %v", decodeToolError(t, fresh))
	}
	// The thread grant is scoped to its author: replying on ai-lead's message
	// does not reach data-lead.
	other := xpSend(h, map[string]any{"project": "synergix-dev", "as": "cto", "target_project": "synergix", "to": "data-lead", "reply_to": up, "content": "x"})
	if decodeToolError(t, other)["code"] != CodeForbidden {
		t.Fatalf("reply on ai-lead's thread to data-lead must be FORBIDDEN, got %v", decodeToolError(t, other))
	}

	// Audited in the sender's project: escalate in synergix, reply in synergix-dev.
	rows, _ := h.db.ListAudit("synergix", "", 50)
	devRows, _ := h.db.ListAudit("synergix-dev", "", 50)
	arms := map[string]bool{}
	for _, r := range append(rows, devRows...) {
		if r.Action == "xproject.send" {
			var d map[string]any
			_ = json.Unmarshal([]byte(r.Details), &d)
			arms[d["arm"].(string)] = true
		}
	}
	if !arms["escalate"] || !arms["reply"] {
		t.Fatalf("every cross-project send is audited with its arm, got %v", arms)
	}
}

// A qualified manager is not a local inbox: the "manager" notify target falls
// back to the dispatcher instead of black-holing the event on "cto@synergix-dev".
func TestXProjectQualifiedManagerNotifyFallsBackToDispatcher(t *testing.T) {
	h := synergixFleet(t)
	n := &Notifier{db: h.db}
	got := n.resolveTargets("synergix", "manager", map[string]any{"agent": "ai-lead", "dispatched_by": "cto-synergix"})
	if len(got) != 1 || got[0] != "cto-synergix" {
		t.Fatalf("manager of ai-lead: want the dispatcher [cto-synergix], got %v", got)
	}
}
