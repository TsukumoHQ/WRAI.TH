package relay

import (
	"net/http"
	"testing"

	"agent-relay/internal/db"
)

func TestIngestSessionStartRebindsByCwd(t *testing.T) {
	r := testRelay(t)
	old := "old-session"
	if _, _, err := r.DB.RegisterAgent("proj", "cto", "lead", "", nil, nil, false, &old, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.DB.SetAgentCwd("proj", "cto", "/wt/cto"); err != nil {
		t.Fatalf("set cwd: %v", err)
	}

	// SessionStart after /clear: same cwd, brand-new session_id.
	w := doAPI(r, "POST", "/ingest/session-start", `{"session_id":"new-session","cwd":"/wt/cto","source":"clear"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeJSON(t, w)
	if resp["bound"] != true {
		t.Fatalf("expected bound=true, got %v", resp["bound"])
	}
	if resp["agent"] != "cto" {
		t.Errorf("expected agent=cto, got %v", resp["agent"])
	}
	if ac, _ := resp["additionalContext"].(string); ac == "" {
		t.Error("expected non-empty additionalContext")
	}

	// The rotated session_id must now point at the agent.
	agent, err := r.DB.GetAgent("proj", "cto")
	if err != nil || agent == nil {
		t.Fatalf("get agent: %v", err)
	}
	if agent.SessionID == nil || *agent.SessionID != "new-session" {
		t.Errorf("expected session_id rebound to new-session, got %v", agent.SessionID)
	}
}

func TestIngestSessionStartUnknownCwd(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/ingest/session-start", `{"session_id":"s","cwd":"/nope"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if resp := decodeJSON(t, w); resp["bound"] != false {
		t.Errorf("expected bound=false for unknown cwd, got %v", resp["bound"])
	}
}

func TestIngestActivityNilIngesterIsNoOp(t *testing.T) {
	r := testRelay(t) // Ingester is nil in the test harness
	w := doAPI(r, "POST", "/ingest/activity", `{"session_id":"s","type":"tool_start","tool":"Edit"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIngestActivityValidation(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/ingest/activity", `{"type":"tool_start"}`) // missing session_id
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestIngestTokensBoundSession(t *testing.T) {
	r := testRelay(t)
	sid := "sess-tok"
	if _, _, err := r.DB.RegisterAgent("proj", "dev", "dev", "", nil, nil, false, &sid, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	w := doAPI(r, "POST", "/ingest/tokens", `{"session_id":"sess-tok","input":100,"output":50,"cache_read":2000}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIngestTokensUnboundSession(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/ingest/tokens", `{"session_id":"ghost","input":10}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for unbound session, got %d", w.Code)
	}
}

func TestIngestTokensValidation(t *testing.T) {
	r := testRelay(t)
	if w := doAPI(r, "POST", "/ingest/tokens", `{"input":10}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing session_id, got %d", w.Code)
	}
	if w := doAPI(r, "POST", "/ingest/tokens", `{"session_id":"s"}`); w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for zero usage, got %d", w.Code)
	}
}
