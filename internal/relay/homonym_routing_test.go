package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// W2 S2 d6d29070 — homonyms across projects never receive each other's mail;
// ambiguity is a typed error that says exactly how to retry (design
// identity-routing.md §2, rules R1-R4).

func homonymRegister(t *testing.T, h *Handlers, project, name string, extra map[string]any) {
	t.Helper()
	args := map[string]any{"project": project, "name": name}
	for k, v := range extra {
		args[k] = v
	}
	if res, _ := h.HandleRegisterAgent(ctx, call(args)); res.IsError {
		t.Fatalf("register %s@%s: %s", name, project, expectError(t, res))
	}
}

// homonymSend sends and returns the stored message id, failing on an error.
func homonymSend(t *testing.T, h *Handlers, args map[string]any) string {
	t.Helper()
	res, _ := h.HandleSendMessage(ctx, call(args))
	if res.IsError {
		t.Fatalf("send %v: %s", args, expectError(t, res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("send %v: no message id in %v", args, out)
	}
	return id
}

// inboxContents lists the contents in (project, agent)'s inbox.
func inboxContents(t *testing.T, h *Handlers, project, agent string) []string {
	t.Helper()
	res, _ := h.HandleGetInbox(ctx, call(map[string]any{"project": project, "as": agent, "format": "json"}))
	if res.IsError {
		t.Fatalf("inbox %s@%s: %s", agent, project, expectError(t, res))
	}
	msgs, _ := parseJSON(t, res)["messages"].([]any)
	var out []string
	for _, m := range msgs {
		if c, ok := m.(map[string]any)["content"].(string); ok {
			out = append(out, c)
		}
	}
	return out
}

func hasContent(contents []string, want string) bool {
	for _, c := range contents {
		if c == want {
			return true
		}
	}
	return false
}

// ctoInTwoProjects registers the homonym pair of the field report: cto is an
// executive in both prod and dev.
func ctoInTwoProjects(t *testing.T) *Handlers {
	h := testHandlers(t)
	homonymRegister(t, h, "prod", "cto", map[string]any{"is_executive": true})
	homonymRegister(t, h, "dev", "cto", map[string]any{"is_executive": true})
	return h
}

// AC1 — prod->dev cross-project message, then a reply with target_project
// lands only in prod, never on dev's cto.
func TestHomonymCrossProjectReplyLandsInSourceProject(t *testing.T) {
	h := ctoInTwoProjects(t)
	parent := homonymSend(t, h, map[string]any{"project": "prod", "as": "cto", "target_project": "dev", "to": "cto", "content": "question from prod"})
	homonymSend(t, h, map[string]any{"project": "dev", "as": "cto", "target_project": "prod", "to": "cto", "reply_to": parent, "content": "answer from dev"})

	if got := inboxContents(t, h, "prod", "cto"); !hasContent(got, "answer from dev") {
		t.Fatalf("prod cto must receive the reply, inbox: %v", got)
	}
	if got := inboxContents(t, h, "dev", "cto"); hasContent(got, "answer from dev") || !hasContent(got, "question from prod") {
		t.Fatalf("dev cto must hold the question and never its own reply, inbox: %v", got)
	}
}

// AC2 — a bare reply (no target_project) to a cross-project parent returns
// RECIPIENT_AMBIGUOUS whose hint names the exact target_project. An explicit
// target_project naming the replier's own project addresses the local agent.
func TestHomonymBareCrossProjectReplyIsAmbiguous(t *testing.T) {
	h := ctoInTwoProjects(t)
	parent := homonymSend(t, h, map[string]any{"project": "prod", "as": "cto", "target_project": "dev", "to": "cto", "content": "question from prod"})

	res, _ := h.HandleSendMessage(ctx, call(map[string]any{"project": "dev", "as": "cto", "to": "cto", "reply_to": parent, "content": "bare answer"}))
	body := decodeToolError(t, res)
	if body["code"] != CodeRecipientAmbiguous || body["isRetryable"] != false {
		t.Fatalf("bare cross-project reply: want non-retryable %s, got %v", CodeRecipientAmbiguous, body)
	}
	if body["target_project"] != "prod" || !strings.Contains(body["hint"].(string), "target_project=prod") {
		t.Fatalf("hint must name target_project=prod, got %v", body)
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 2 {
		t.Fatalf("both cto@prod and the local cto@dev are candidates, got %v", body["candidates"])
	}
	for _, p := range []string{"prod", "dev"} {
		if got := inboxContents(t, h, p, "cto"); hasContent(got, "bare answer") {
			t.Fatalf("a refused reply must reach nobody, %s cto inbox: %v", p, got)
		}
	}

	homonymSend(t, h, map[string]any{"project": "dev", "as": "cto", "target_project": "dev", "to": "cto", "reply_to": parent, "content": "note to self"})
	if got := inboxContents(t, h, "dev", "cto"); !hasContent(got, "note to self") {
		t.Fatalf("explicit target_project=dev must address the local cto, inbox: %v", got)
	}
}

// AC3 — a cross-project message from X@Q opens no reply-path grant to X@P.
func TestHomonymCrossProjectOpensNoReplyPath(t *testing.T) {
	h := testHandlers(t)
	homonymRegister(t, h, "prod", "x", map[string]any{"is_executive": true})
	homonymRegister(t, h, "dev", "cto", map[string]any{"is_executive": true})
	homonymRegister(t, h, "dev", "x", map[string]any{"reports_to": "cto"})

	homonymSend(t, h, map[string]any{"project": "prod", "as": "x", "target_project": "dev", "to": "cto", "content": "from x@prod"})
	if h.db.CanReplyTo("dev", "cto", "x") {
		t.Fatal("a delivery from x@prod must not grant cto@dev a reply-path to the homonym x@dev")
	}

	// A local message from x@dev still opens the scoped grant (TSU-75).
	homonymSend(t, h, map[string]any{"project": "dev", "as": "x", "to": "cto", "content": "from x@dev"})
	if !h.db.CanReplyTo("dev", "cto", "x") {
		t.Fatal("a local message from x@dev must open the reply-path")
	}
}

// AC4 — an executive relaying cross-project (the cto-synergix pattern, the
// one path that works today) still delivers both ways, and the prod homonym
// of the dev cto sees none of it.
func TestHomonymExecRelayHopBothWays(t *testing.T) {
	h := testHandlers(t)
	homonymRegister(t, h, "prod", "cto-synergix", map[string]any{"is_executive": true})
	homonymRegister(t, h, "prod", "ai-lead", map[string]any{"reports_to": "cto-synergix"})
	homonymRegister(t, h, "prod", "cto", nil)
	homonymRegister(t, h, "dev", "cto", map[string]any{"is_executive": true})

	homonymSend(t, h, map[string]any{"project": "prod", "as": "ai-lead", "to": "cto-synergix", "content": "escalate: need dev cto"})
	up := homonymSend(t, h, map[string]any{"project": "prod", "as": "cto-synergix", "target_project": "dev", "to": "cto", "content": "relayed escalation"})
	homonymSend(t, h, map[string]any{"project": "dev", "as": "cto", "target_project": "prod", "to": "cto-synergix", "reply_to": up, "content": "dev ruling"})
	homonymSend(t, h, map[string]any{"project": "prod", "as": "cto-synergix", "to": "ai-lead", "content": "relayed ruling"})

	if got := inboxContents(t, h, "dev", "cto"); !hasContent(got, "relayed escalation") {
		t.Fatalf("dev cto must receive the relayed escalation, inbox: %v", got)
	}
	if got := inboxContents(t, h, "prod", "cto-synergix"); !hasContent(got, "dev ruling") || !hasContent(got, "escalate: need dev cto") {
		t.Fatalf("cto-synergix must receive the lead's escalation and the dev ruling, inbox: %v", got)
	}
	if got := inboxContents(t, h, "prod", "ai-lead"); !hasContent(got, "relayed ruling") {
		t.Fatalf("ai-lead must receive the relayed ruling, inbox: %v", got)
	}
	if got := inboxContents(t, h, "prod", "cto"); len(got) != 0 {
		t.Fatalf("the prod homonym cto must receive nothing, inbox: %v", got)
	}
}

// R4 — an unresolved project for a name registered in several projects is a
// typed PROJECT_AMBIGUOUS listing the candidates, not a generic string.
func TestHomonymUnresolvedProjectIsAmbiguous(t *testing.T) {
	h := testHandlers(t)
	homonymRegister(t, h, "prod", "infra-lead", nil)
	homonymRegister(t, h, "dev", "infra-lead", nil)

	res, _ := h.guardIdentity("send_message", h.HandleSendMessage)(ctx, call(map[string]any{"as": "infra-lead", "to": "cto", "content": "x"}))
	body := decodeToolError(t, res)
	if body["code"] != CodeProjectAmbiguous {
		t.Fatalf("want %s, got %v", CodeProjectAmbiguous, body)
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 2 || !strings.Contains(body["message"].(string), "project=") {
		t.Fatalf("candidates must list both projects and the message say how to retry, got %v", body)
	}
}
