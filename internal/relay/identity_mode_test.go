package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// W1 S1 63fbe94b — the relay binds a token to its (project, name) principal
// under RELAY_IDENTITY_MODE off|warn|enforce (design identity-routing.md §1).

// passThrough stands in for a tool handler: it counts the calls the guard let
// through, so each case asserts the guard's decision and nothing else.
type passThrough struct{ n int }

func (p *passThrough) handle(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p.n++
	return mcp.NewToolResultText("ok"), nil
}

// guarded runs tool through guardAgentToken and reports (passed, error code).
func guarded(t *testing.T, h *Handlers, c context.Context, tool string, args map[string]any) (bool, any) {
	t.Helper()
	var p passThrough
	res, _ := h.guardAgentToken(tool, p.handle)(c, call(args))
	if p.n == 1 {
		return true, nil
	}
	return false, decodeToolError(t, res)["code"]
}

// captureIdentityLog swaps the std logger's output for the test's duration.
func captureIdentityLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// identityAudits returns the audit rows of one action for (project, agent).
func identityAudits(t *testing.T, h *Handlers, project, agent, action string) []map[string]any {
	t.Helper()
	rows, err := h.db.ListAudit(project, agent, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var out []map[string]any
	for _, r := range rows {
		if r.Action != action {
			continue
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(r.Details), &d)
		out = append(out, d)
	}
	return out
}

// AC1 — a token of (P, X) acting as Y, or in project Q != P, is
// AGENT_IDENTITY_MISMATCH (the project arm in enforce; warn counts it).
func TestIdentityTokenBindsProjectAndName(t *testing.T) {
	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeEnforce)
	h := testHandlers(t)
	tokA := tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-a"}), "minted")
	tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-b"}), "minted")
	// The homonym bot-a@p2 is a different principal with its own token.
	tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p2", "name": "bot-a"}), "minted")

	if ok, code := guarded(t, h, withToken(tokA), "get_inbox", map[string]any{"project": "p1", "as": "bot-b"}); ok || code != CodeAgentIdentityMismatch {
		t.Fatalf("token of bot-a acting as bot-b: want %s, got passed=%v %v", CodeAgentIdentityMismatch, ok, code)
	}
	if ok, code := guarded(t, h, withToken(tokA), "get_inbox", map[string]any{"project": "p2", "as": "bot-a"}); ok || code != CodeAgentIdentityMismatch {
		t.Fatalf("token of bot-a@p1 acting in p2: want %s, got passed=%v %v", CodeAgentIdentityMismatch, ok, code)
	}
	if ok, code := guarded(t, h, withToken(tokA), "get_inbox", map[string]any{"project": "p1", "as": "bot-a"}); !ok {
		t.Fatalf("token of bot-a@p1 acting as itself: refused %v", code)
	}
	// The same token may still mint bot-a's first token in a project it has
	// not joined yet (bootstrap), never act in one that has its own principal.
	if ok, code := guarded(t, h, withToken(tokA), "register_agent", map[string]any{"project": "p3", "name": "bot-a"}); !ok {
		t.Fatalf("first register of bot-a in p3 with its p1 token: refused %v", code)
	}

	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeWarn)
	buf := captureIdentityLog(t)
	if ok, code := guarded(t, h, withToken(tokA), "get_inbox", map[string]any{"project": "p2", "as": "bot-a"}); !ok {
		t.Fatalf("warn: project mismatch must pass (counted), got %v", code)
	}
	h.flushIdentity()
	if !strings.Contains(buf.String(), "identity.unverified project=p2 as=bot-a reason=project_mismatch count=1") {
		t.Fatalf("warn: project mismatch not logged; log:\n%s", buf.String())
	}
}

// AC2 — the token of a RELAY_OVERRIDE_ACTORS service may act as Y@P and writes
// identity.delegated {actor, on_behalf_of, tool}; is_service is no trust root.
func TestIdentityDelegatingServiceAudited(t *testing.T) {
	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeEnforce)
	h := testHandlers(t)
	niwaTok := tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "ops", "name": "niwa", "is_service": true}), "minted")
	svcTok := tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "ops", "name": "svc", "is_service": true}), "minted")
	tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-a"}), "minted")

	for i := 0; i < 2; i++ {
		if ok, code := guarded(t, h, withToken(niwaTok), "send_message", map[string]any{"project": "p1", "as": "bot-a", "to": "bot-b", "content": "x"}); !ok {
			t.Fatalf("niwa acting as bot-a@p1: refused %v", code)
		}
	}
	if ok, code := guarded(t, h, withToken(svcTok), "send_message", map[string]any{"project": "p1", "as": "bot-a", "to": "bot-b", "content": "x"}); ok || code != CodeAgentIdentityMismatch {
		t.Fatalf("self-declared is_service acting as bot-a: want %s, got passed=%v %v", CodeAgentIdentityMismatch, ok, code)
	}

	if rows := identityAudits(t, h, "p1", "bot-a", "identity.delegated"); len(rows) != 0 {
		t.Fatalf("delegation must not write synchronously, got %v", rows)
	}
	h.flushIdentity()
	rows := identityAudits(t, h, "p1", "bot-a", "identity.delegated")
	if len(rows) != 1 {
		t.Fatalf("want one aggregated identity.delegated row, got %v", rows)
	}
	if d := rows[0]; d["actor"] != "niwa" || d["on_behalf_of"] != "bot-a" || d["tool"] != "send_message" || d["count"] != float64(2) {
		t.Fatalf("identity.delegated details: %v", d)
	}

	// The operator list is the trust root: drop niwa from it and its token is
	// an ordinary principal again.
	t.Setenv("RELAY_OVERRIDE_ACTORS", "gate")
	if ok, code := guarded(t, h, withToken(niwaTok), "send_message", map[string]any{"project": "p1", "as": "bot-a", "to": "bot-b", "content": "x"}); ok || code != CodeAgentIdentityMismatch {
		t.Fatalf("niwa off the operator list: want %s, got passed=%v %v", CodeAgentIdentityMismatch, ok, code)
	}
}

// AC3 — a tokenless call: refused AGENT_TOKEN_REQUIRED in enforce, allowed in
// warn, unchanged in off; bootstrap tools and human/user on loopback pass in
// enforce.
func TestIdentityTokenlessByMode(t *testing.T) {
	h := testHandlers(t)
	tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-a"}), "minted")
	inbox := map[string]any{"project": "p1", "as": "bot-a"}

	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeEnforce)
	if ok, code := guarded(t, h, ctx, "get_inbox", inbox); ok || code != CodeAgentTokenRequired {
		t.Fatalf("enforce, tokenless get_inbox: want %s, got passed=%v %v", CodeAgentTokenRequired, ok, code)
	}
	if ok, code := guarded(t, h, ctx, "register_agent", map[string]any{"project": "p1", "name": "bot-a"}); ok || code != CodeAgentTokenRequired {
		t.Fatalf("enforce, tokenless re-register of a tokened name: want %s, got passed=%v %v", CodeAgentTokenRequired, ok, code)
	}
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"register_agent", map[string]any{"project": "p1", "name": "bot-new"}},
		{"create_project", map[string]any{"name": "p9"}},
		{"whoami", map[string]any{"salt": "one two three"}},
	} {
		if ok, code := guarded(t, h, ctx, c.tool, c.args); !ok {
			t.Fatalf("enforce, bootstrap %s: refused %v", c.tool, code)
		}
	}
	loopback := context.WithValue(ctx, peerLoopbackKey, true)
	if ok, code := guarded(t, h, loopback, "get_inbox", map[string]any{"project": "p1", "as": "human"}); !ok {
		t.Fatalf("enforce, human on loopback: refused %v", code)
	}
	if ok, code := guarded(t, h, ctx, "get_inbox", map[string]any{"project": "p1", "as": "human"}); ok || code != CodeAgentTokenRequired {
		t.Fatalf("enforce, human off loopback: want %s, got passed=%v %v", CodeAgentTokenRequired, ok, code)
	}
	h.flushIdentity()
	if rows := identityAudits(t, h, "p1", "human", "identity.exempt"); len(rows) != 1 || rows[0]["tool"] != "get_inbox" {
		t.Fatalf("human loopback exemption must be audited, got %v", rows)
	}

	buf := captureIdentityLog(t)
	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeWarn)
	if ok, code := guarded(t, h, ctx, "get_inbox", inbox); !ok {
		t.Fatalf("warn, tokenless get_inbox: refused %v", code)
	}
	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeOff)
	if ok, code := guarded(t, h, ctx, "get_inbox", map[string]any{"project": "p1", "as": "bot-off"}); !ok {
		t.Fatalf("off, tokenless get_inbox: refused %v", code)
	}
	h.flushIdentity()
	if strings.Contains(buf.String(), "as=bot-off") {
		t.Fatalf("off must record nothing; log:\n%s", buf.String())
	}
}

// AC4 — in warn, N tokenless calls from (P, X) yield ONE greppable
// identity.unverified line per (project, as) carrying the count.
func TestIdentityWarnAggregatesPerProjectAs(t *testing.T) {
	t.Setenv("RELAY_IDENTITY_MODE", "")
	if IdentityMode() != IdentityModeWarn {
		t.Fatalf("unset RELAY_IDENTITY_MODE must default to warn, got %q", IdentityMode())
	}
	h := testHandlers(t)
	buf := captureIdentityLog(t)
	for i := 0; i < 3; i++ {
		guarded(t, h, ctx, "get_inbox", map[string]any{"project": "p1", "as": "bot-a"})
	}
	guarded(t, h, ctx, "send_message", map[string]any{"project": "p1", "as": "bot-b", "to": "bot-a", "content": "x"})
	h.flushIdentity()

	var lines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "identity.unverified") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 ||
		!strings.HasSuffix(lines[0], "identity.unverified project=p1 as=bot-a reason=tokenless count=3") ||
		!strings.HasSuffix(lines[1], "identity.unverified project=p1 as=bot-b reason=tokenless count=1") {
		t.Fatalf("want one line per (project, as) with its count, got:\n%s", strings.Join(lines, "\n"))
	}
	// A crafted `as` stays one token: it can neither split the line nor forge
	// a second identity.unverified record.
	buf.Reset()
	guarded(t, h, ctx, "get_inbox", map[string]any{"project": "p1", "as": "x count=0\nidentity.unverified project=p1 as=y"})
	h.flushIdentity()
	if n := strings.Count(strings.TrimSpace(buf.String()), "\n"); n != 0 || !strings.Contains(buf.String(), "as=x_count_0_identity.unverified_project_p1_as_y reason=tokenless count=1") {
		t.Fatalf("crafted as must log as one sanitized token, got:\n%s", buf.String())
	}

	// The window resets: a flush with nothing new logs nothing.
	buf.Reset()
	h.flushIdentity()
	if strings.Contains(buf.String(), "identity.unverified") {
		t.Fatalf("an empty window must log nothing, got:\n%s", buf.String())
	}
}

// The human/user exemption keys on the real TCP peer, never a header.
func TestHTTPContextFuncCarriesPeerLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:5555": true, "[::1]:5555": true, "10.0.0.7:5555": false} {
		req := httptest.NewRequest(http.MethodPost, "/mcp?project=p1", nil)
		req.RemoteAddr = addr
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		if got := PeerIsLoopback(HTTPContextFunc(context.Background(), req)); got != want {
			t.Fatalf("peer %s: want loopback=%v, got %v", addr, want, got)
		}
	}
	if PeerIsLoopback(context.Background()) {
		t.Fatal("no HTTP request in context must not count as loopback")
	}
}
