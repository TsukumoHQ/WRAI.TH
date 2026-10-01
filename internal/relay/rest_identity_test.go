package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// W1 S4 8e2f69cd — REST writes follow RELAY_IDENTITY_MODE like MCP calls
// (design identity-routing.md §1 step 5, ruling Q5: the console human/user is
// exempt on a loopback TCP peer only, every exempt write audited).

const nonLoopbackPeer = "192.0.2.10:4242"

func restRegister(t *testing.T, r *Relay, project, name string) string {
	t.Helper()
	res, _ := r.Handlers.HandleRegisterAgent(ctx, call(map[string]any{"project": project, "name": name}))
	if res.IsError {
		t.Fatalf("register %s@%s: %s", name, project, expectError(t, res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out)
	tok, _ := out["agent_token"].(string)
	return tok
}

// restWrite POSTs body to path as the given TCP peer, with an optional token.
func restWrite(r *Relay, path, body, tok, peer string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, "/api"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = peer
	if tok != "" {
		req.Header.Set(AgentTokenHeader, tok)
	}
	w := httptest.NewRecorder()
	r.ServeAPI(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func restMsg(project, from string) string {
	return `{"project":"` + project + `","from":"` + from + `","to":"bot-b","content":"hi"}`
}

// AC1 — a tokenless REST write is 401 AGENT_TOKEN_REQUIRED in enforce,
// counted (allowed) in warn, untouched in off.
func TestRESTTokenlessWriteFollowsMode(t *testing.T) {
	r := testRelay(t)
	restRegister(t, r, "p1", "bot-a")
	restRegister(t, r, "p1", "bot-b")

	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeEnforce)
	if code, out := restWrite(r, "/messages", restMsg("p1", "bot-a"), "", nonLoopbackPeer); code != http.StatusUnauthorized || out["code"] != CodeAgentTokenRequired {
		t.Fatalf("enforce tokenless: want 401 %s, got %d %v", CodeAgentTokenRequired, code, out)
	}
	if code, out := restWrite(r, "/messages", restMsg("p1", "bot-a"), "", "127.0.0.1:5000"); code != http.StatusUnauthorized {
		t.Fatalf("enforce tokenless agent on loopback is not exempt: want 401, got %d %v", code, out)
	}

	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeWarn)
	buf := captureIdentityLog(t)
	if code, out := restWrite(r, "/messages", restMsg("p1", "bot-a"), "", nonLoopbackPeer); code >= 300 {
		t.Fatalf("warn tokenless: want 2xx, got %d %v", code, out)
	}
	r.Handlers.flushIdentity()
	if !strings.Contains(buf.String(), "identity.unverified project=p1 as=bot-a reason=tokenless count=1") {
		t.Fatalf("warn tokenless REST write must be counted; log:\n%s", buf.String())
	}

	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeOff)
	if code, out := restWrite(r, "/messages", restMsg("p1", "bot-a"), "", nonLoopbackPeer); code >= 300 {
		t.Fatalf("off tokenless: want 2xx, got %d %v", code, out)
	}
}

// AC2 — a token of (P, X) claiming Y is 403 AGENT_IDENTITY_MISMATCH in warn
// and enforce; claiming another project is refused in enforce.
func TestRESTTokenClaimingAnotherIdentity(t *testing.T) {
	r := testRelay(t)
	tokA := restRegister(t, r, "p1", "bot-a")
	restRegister(t, r, "p1", "bot-b")
	restRegister(t, r, "p2", "bot-b")

	for _, mode := range []string{IdentityModeWarn, IdentityModeEnforce} {
		t.Setenv("RELAY_IDENTITY_MODE", mode)
		if code, out := restWrite(r, "/messages", restMsg("p1", "bot-b"), tokA, nonLoopbackPeer); code != http.StatusForbidden || out["code"] != CodeAgentIdentityMismatch {
			t.Fatalf("%s: token of bot-a as bot-b: want 403 %s, got %d %v", mode, CodeAgentIdentityMismatch, code, out)
		}
		if code, out := restWrite(r, "/messages", restMsg("p1", "bot-a"), tokA, nonLoopbackPeer); code >= 300 {
			t.Fatalf("%s: token of bot-a as itself: want 2xx, got %d %v", mode, code, out)
		}
	}
	if code, out := restWrite(r, "/messages", restMsg("p2", "bot-a"), tokA, nonLoopbackPeer); code != http.StatusForbidden || out["code"] != CodeAgentIdentityMismatch {
		t.Fatalf("enforce: token of bot-a@p1 writing in p2: want 403 %s, got %d %v", CodeAgentIdentityMismatch, code, out)
	}
}

// AC3 — the console human/user write over loopback passes in enforce and is
// audited; the same write from a non-loopback peer is refused.
func TestRESTConsoleOperatorLoopbackOnly(t *testing.T) {
	r := testRelay(t)
	restRegister(t, r, "p1", "bot-b")
	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeEnforce)

	reply := `{"project":"p1","to":"bot-b","content":"go ahead"}`
	if code, out := restWrite(r, "/messages", restMsg("p1", "human"), "", "127.0.0.1:5000"); code >= 300 {
		t.Fatalf("console human message on loopback: want 2xx, got %d %v", code, out)
	}
	if code, out := restWrite(r, "/user-response", reply, "", "[::1]:5000"); code >= 300 {
		t.Fatalf("console user-response on loopback: want 2xx, got %d %v", code, out)
	}
	for _, c := range []struct{ path, body string }{
		{"/messages", restMsg("p1", "human")},
		{"/messages", restMsg("p1", "user")},
		{"/user-response", reply},
	} {
		if code, out := restWrite(r, c.path, c.body, "", nonLoopbackPeer); code != http.StatusUnauthorized || out["code"] != CodeAgentTokenRequired {
			t.Fatalf("%s from a non-loopback peer: want 401 %s, got %d %v", c.path, CodeAgentTokenRequired, code, out)
		}
	}

	r.Handlers.flushIdentity()
	rows, _ := r.DB.ListAudit("p1", "human", 50)
	tools := map[string]bool{}
	for _, row := range rows {
		if row.Action == "identity.exempt" {
			var d map[string]any
			_ = json.Unmarshal([]byte(row.Details), &d)
			tools[d["tool"].(string)] = true
		}
	}
	if !tools["rest:POST /api/messages"] || !tools["rest:POST /api/user-response"] {
		t.Fatalf("each exempt console write must be audited, got %v", tools)
	}
}

// S1 review follow-ups: a multi-byte UTF-8 `as` stays intact (and distinct)
// in the warn log while control runes are escaped, and a delegated call with
// no resolved project audits under the service's own project, not the retired
// 'default'.
func TestIdentityS1Followups(t *testing.T) {
	if a, b := logField("andré"), logField("andrè"); a != "andré" || b != "andrè" {
		t.Fatalf("multi-byte UTF-8 names must stay intact: %q, %q", a, b)
	}
	if got := logField("x y=z\nw\u00a0v"); got != `x_y_z\nw\u00a0v` {
		t.Fatalf("logField sanitising: got %q", got)
	}
	r := testRelay(t)
	buf := captureIdentityLog(t)
	t.Setenv("RELAY_IDENTITY_MODE", IdentityModeWarn)
	restWrite(r, "/messages", restMsg("p1", "andré"), "", nonLoopbackPeer)
	r.Handlers.flushIdentity()
	if !strings.Contains(buf.String(), "identity.unverified project=p1 as=andré reason=tokenless count=1") {
		t.Fatalf("the warn line must carry the UTF-8 name intact; log:\n%s", buf.String())
	}

	h := testHandlers(t)
	niwaTok := tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "ops", "name": "niwa"}), "minted")
	if ok, code := guarded(t, h, withToken(niwaTok), "get_inbox", map[string]any{"as": "bot-a"}); !ok {
		t.Fatalf("niwa delegating with no project: refused %v", code)
	}
	h.flushIdentity()
	if rows := identityAudits(t, h, "ops", "bot-a", "identity.delegated"); len(rows) != 1 {
		t.Fatalf("delegated audit with no resolved project must land in the service's project, got %v", rows)
	}
	if rows := identityAudits(t, h, "default", "bot-a", "identity.delegated"); len(rows) != 0 {
		t.Fatalf("delegated audit must not fall into 'default', got %v", rows)
	}
}
