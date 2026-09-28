package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// S3b 05525713 (F-04/F-27) — per-agent relay tokens.

func withToken(tok string) context.Context {
	return context.WithValue(ctx, agentTokenKey, tok)
}

// registerTok runs register_agent through the token guard, as served.
func registerTok(t *testing.T, h *Handlers, c context.Context, args map[string]any) map[string]any {
	t.Helper()
	res, _ := h.guardAgentToken("register_agent", h.HandleRegisterAgent)(c, call(args))
	if res.IsError {
		t.Fatalf("register %v: %s", args["name"], expectError(t, res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out)
	return out
}

func tokenOf(t *testing.T, out map[string]any, wantStatus string) string {
	t.Helper()
	if out["agent_token_status"] != wantStatus {
		t.Fatalf("agent_token_status: want %q, got %v", wantStatus, out["agent_token_status"])
	}
	tok, _ := out["agent_token"].(string)
	if (wantStatus == "minted" || wantStatus == "rotated") != (tok != "") {
		t.Fatalf("status %q: agent_token presence wrong (%q)", wantStatus, tok)
	}
	return tok
}

// TestAgentTokenMintedOnceKept: the first register mints a token, returned
// once (storage is hash-only: db TestMintAgentTokenStoresHashOnly); a tokenless re-register of
// the same name keeps it (no token returned, named hint), and the token still
// resolves.
func TestAgentTokenMintedOnceKept(t *testing.T) {
	h := testHandlers(t)
	tok := tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-a"}), "minted")

	if !h.db.AgentTokenMatches("p1", "bot-a", tok) {
		t.Fatal("minted token must match bot-a")
	}

	out := registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-a"})
	tokenOf(t, out, "kept")
	if out["agent_token_hint"] == nil {
		t.Fatal("tokenless re-register of a tokened name must carry agent_token_hint")
	}
	if _, name, ok := h.db.AgentByToken(tok); !ok || name != "bot-a" {
		t.Fatalf("tokenless re-register must not invalidate the token (ok=%v name=%q)", ok, name)
	}
}

// TestAgentTokenRotation: the owner's own token keeps it (unchanged) unless
// rotate_token=true; a proven override actor (niwa, by its own token)
// rotates it for a respawn; a tokenless caller claiming to be niwa cannot.
// Every rotation invalidates the previous token.
func TestAgentTokenRotation(t *testing.T) {
	h := testHandlers(t)
	niwaTok := tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "niwa", "is_service": true}), "minted")
	t1 := tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-a"}), "minted")

	tokenOf(t, registerTok(t, h, withToken(t1), map[string]any{"project": "p1", "name": "bot-a"}), "unchanged")

	t2 := tokenOf(t, registerTok(t, h, withToken(t1), map[string]any{"project": "p1", "name": "bot-a", "rotate_token": true}), "rotated")
	if _, _, ok := h.db.AgentByToken(t1); ok {
		t.Fatal("self-rotation must invalidate the old token")
	}

	tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-a", "as": "niwa"}), "kept")
	if _, _, ok := h.db.AgentByToken(t2); !ok {
		t.Fatal("a tokenless 'as: niwa' register must not rotate")
	}

	t3 := tokenOf(t, registerTok(t, h, withToken(niwaTok), map[string]any{"project": "p1", "name": "bot-a", "as": "niwa"}), "rotated")
	if _, _, ok := h.db.AgentByToken(t2); ok {
		t.Fatal("niwa respawn rotation must invalidate the old token")
	}
	if _, name, ok := h.db.AgentByToken(t3); !ok || name != "bot-a" {
		t.Fatalf("rotated token must resolve to bot-a (ok=%v name=%q)", ok, name)
	}
}

// TestAgentTokenBindsAs: with X-Agent-Token, every tool (reads included) acts
// only as the token's agent; another `as`, another register name, or an
// unknown token is refused with a typed, non-retryable code.
func TestAgentTokenBindsAs(t *testing.T) {
	h := testHandlers(t)
	tokA := tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-a"}), "minted")
	tokenOf(t, registerTok(t, h, ctx, map[string]any{"project": "p1", "name": "bot-b"}), "minted")

	code := func(res *mcp.CallToolResult) any { return decodeToolError(t, res)["code"] }

	send := h.guardAgentToken("send_message", h.HandleSendMessage)
	res, _ := send(withToken(tokA), call(map[string]any{"project": "p1", "as": "bot-b", "to": "bot-a", "content": "spoof"}))
	if c := code(res); c != CodeAgentIdentityMismatch {
		t.Fatalf("send as another agent: want %s, got %v", CodeAgentIdentityMismatch, c)
	}
	inbox := h.guardAgentToken("get_inbox", h.HandleGetInbox)
	res, _ = inbox(withToken(tokA), call(map[string]any{"project": "p1", "as": "bot-b"}))
	if c := code(res); c != CodeAgentIdentityMismatch {
		t.Fatalf("read another agent's inbox: want %s, got %v", CodeAgentIdentityMismatch, c)
	}
	reg := h.guardAgentToken("register_agent", h.HandleRegisterAgent)
	res, _ = reg(withToken(tokA), call(map[string]any{"project": "p1", "name": "bot-b"}))
	if c := code(res); c != CodeAgentIdentityMismatch {
		t.Fatalf("register another name: want %s, got %v", CodeAgentIdentityMismatch, c)
	}
	res, _ = send(withToken("art_bogus"), call(map[string]any{"project": "p1", "as": "bot-a", "to": "bot-b", "content": "x"}))
	if body := decodeToolError(t, res); body["code"] != CodeAgentTokenInvalid || body["isRetryable"] != false {
		t.Fatalf("unknown token: want non-retryable %s, got %v", CodeAgentTokenInvalid, body)
	}

	res, _ = send(withToken(tokA), call(map[string]any{"project": "p1", "as": "bot-a", "to": "bot-b", "content": "hi"}))
	if res.IsError {
		t.Fatalf("send as self: %s", expectError(t, res))
	}
	// No token: unchanged behaviour.
	res, _ = send(ctx, call(map[string]any{"project": "p1", "as": "bot-b", "to": "bot-a", "content": "hi"}))
	if res.IsError {
		t.Fatalf("tokenless send: %s", expectError(t, res))
	}
}

// TestAPIAgentTokenBindsFrom: REST actor fields (from / agent / as) are bound
// to X-Agent-Token: another agent → 403 AGENT_IDENTITY_MISMATCH, unknown
// token → 401 AGENT_TOKEN_INVALID, self → accepted, no token → unchanged.
func TestAPIAgentTokenBindsFrom(t *testing.T) {
	r := testRelay(t)
	regAgent := func(name string) string {
		res, _ := r.Handlers.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": name}))
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out)
		tok, _ := out["agent_token"].(string)
		return tok
	}
	tokA := regAgent("bot-a")
	regAgent("bot-b")

	post := func(tok, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if tok != "" {
			req.Header.Set(AgentTokenHeader, tok)
		}
		w := httptest.NewRecorder()
		r.ServeAPI(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	msg := func(from string) string {
		return `{"project":"p1","from":"` + from + `","to":"bot-b","content":"hi"}`
	}
	if code, out := post(tokA, msg("bot-b")); code != http.StatusForbidden || out["code"] != CodeAgentIdentityMismatch {
		t.Fatalf("from another agent: want 403 %s, got %d %v", CodeAgentIdentityMismatch, code, out)
	}
	if code, out := post("art_bogus", msg("bot-a")); code != http.StatusUnauthorized || out["code"] != CodeAgentTokenInvalid {
		t.Fatalf("unknown token: want 401 %s, got %d %v", CodeAgentTokenInvalid, code, out)
	}
	if code, out := post(tokA, msg("bot-a")); code >= 300 {
		t.Fatalf("from self: want 2xx, got %d %v", code, out)
	}
	if code, out := post("", msg("bot-a")); code >= 300 {
		t.Fatalf("tokenless: want 2xx, got %d %v", code, out)
	}

	// Task transition: a token-bound caller cannot fall back to the "user" actor.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/x/transition", strings.NewReader(`{"project":"p1","status":"done"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(AgentTokenHeader, tokA)
	w := httptest.NewRecorder()
	r.ServeAPI(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("transition defaulting to user under a token: want 403, got %d %s", w.Code, w.Body.String())
	}
}

// TestAuthMiddleware_AgentTokenAndTrustLoopback: trust-loopback on (default)
// keeps a keyless relay open to tokenless loopback; off refuses tokenless
// loopback even with no API key, while a live X-Agent-Token is admitted.
func TestAuthMiddleware_AgentTokenAndTrustLoopback(t *testing.T) {
	okH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	valid := func(tok string) bool { return tok == "art_good" }
	hit := func(h http.Handler, tok string) int {
		req := httptest.NewRequest("GET", "/api/health", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		if tok != "" {
			req.Header.Set(AgentTokenHeader, tok)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	t.Setenv("RELAY_TRUST_LOOPBACK", "")
	if c := hit(authMiddleware("", valid, okH), ""); c != 200 {
		t.Fatalf("trust on, no key, tokenless loopback: want 200, got %d", c)
	}

	t.Setenv("RELAY_TRUST_LOOPBACK", "0")
	for _, key := range []string{"", "secret"} {
		h := authMiddleware(key, valid, okH)
		if c := hit(h, ""); c != 401 {
			t.Fatalf("trust off (key %q), tokenless loopback: want 401, got %d", key, c)
		}
		if c := hit(h, "art_bad"); c != 401 {
			t.Fatalf("trust off (key %q), unknown token: want 401, got %d", key, c)
		}
		if c := hit(h, "art_good"); c != 200 {
			t.Fatalf("trust off (key %q), live agent token: want 200, got %d", key, c)
		}
	}
}

// TestHTTPContextFuncCarriesAgentToken: the X-Agent-Token header of an /mcp
// request reaches the tool context the guard reads.
func TestHTTPContextFuncCarriesAgentToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp?project=p1", nil)
	req.Header.Set(AgentTokenHeader, " art_x ")
	if got := AgentTokenFromContext(HTTPContextFunc(context.Background(), req)); got != "art_x" {
		t.Fatalf("token in context: want art_x, got %q", got)
	}
	bare := httptest.NewRequest(http.MethodPost, "/mcp?project=p1", nil)
	if got := AgentTokenFromContext(HTTPContextFunc(context.Background(), bare)); got != "" {
		t.Fatalf("no header: want empty token, got %q", got)
	}
}
