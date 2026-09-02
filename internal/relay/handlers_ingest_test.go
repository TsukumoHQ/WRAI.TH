package relay

import (
	"net/http"
	"testing"

	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
)

// withIngester attaches a real ingester whose AgentResolver is wired to the test
// DB — mirroring the production wiring in main.go — so activity events resolve to
// their owning agent.
func withIngester(t *testing.T, r *Relay) {
	t.Helper()
	ing, err := ingest.New(ingest.Config{
		AgentResolver: func(sid string) (string, string, bool) {
			p, n, f, _ := r.DB.GetAgentBySessionID(sid)
			return p, n, f
		},
	})
	if err != nil {
		t.Fatalf("ingest.New: %v", err)
	}
	t.Cleanup(ing.Stop)
	r.Ingester = ing
}

// findAgent returns the agent row with the given name from a /api/agents array.
func findAgent(arr []any, name string) map[string]any {
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok && m["name"] == name {
			return m
		}
	}
	return nil
}

// TestActivityJoinsByAgentNameSurvivesSessionRotation is the regression guard for
// the dead state-stream bug: /api/agents joined live activity on the agent's
// stored session_id, which goes stale on /clear — so the board showed every agent
// as idle. The join now keys on the resolved agent name, which survives rotation.
func TestActivityJoinsByAgentNameSurvivesSessionRotation(t *testing.T) {
	r := testRelay(t)
	withIngester(t, r)

	sid := "sess-A"
	if _, _, err := r.DB.RegisterAgent("proj", "wraith-dev", "dev", "", nil, nil, false, &sid, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// An activity event for the live session → detector resolves sess-A→wraith-dev.
	if w := doAPI(r, "POST", "/ingest/activity", `{"session_id":"sess-A","type":"tool_start","tool":"Edit"}`); w.Code != http.StatusNoContent {
		t.Fatalf("ingest activity: %d %s", w.Code, w.Body.String())
	}

	w := doAPI(r, "GET", "/agents?project=proj", "")
	a := findAgent(decodeJSONArray(t, w), "wraith-dev")
	if a == nil {
		t.Fatal("wraith-dev not in /api/agents")
	}
	if a["activity"] != "typing" || a["activity_tool"] != "Edit" {
		t.Fatalf("expected activity=typing tool=Edit, got activity=%v tool=%v", a["activity"], a["activity_tool"])
	}

	// Simulate /clear: the agent's stored session_id rotates to a new value the
	// detector has never seen. The name-keyed join must still surface the activity
	// (the old session_id join would now miss → idle).
	newSid := "sess-B"
	if _, _, err := r.DB.RegisterAgent("proj", "wraith-dev", "dev", "", nil, nil, false, &newSid, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	w = doAPI(r, "GET", "/agents?project=proj", "")
	a = findAgent(decodeJSONArray(t, w), "wraith-dev")
	if a == nil || a["activity"] != "typing" {
		t.Fatalf("activity lost after session rotation — name-join regression: got %v", a["activity"])
	}
}

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

// Issue #153: N agents deliberately share one worktree. cwd alone cannot
// identify one, so the relay must refuse to guess rather than bind an arbitrary
// agent (which mis-attributes token usage and injects a wrong identity).
func TestIngestSessionStartAmbiguousCwdBindsNothing(t *testing.T) {
	r := testRelay(t)
	for _, n := range []string{"frontend", "documentor"} {
		if _, _, err := r.DB.RegisterAgent("proj", n, "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
		if err := r.DB.SetAgentCwd("proj", n, "/wt/shared"); err != nil {
			t.Fatalf("set cwd %s: %v", n, err)
		}
	}

	w := doAPI(r, "POST", "/ingest/session-start", `{"session_id":"unseen","cwd":"/wt/shared","source":"startup"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeJSON(t, w)
	if resp["bound"] != false {
		t.Fatalf("expected bound=false on ambiguous cwd, got %v", resp["bound"])
	}
	if _, hasAgent := resp["agent"]; hasAgent {
		t.Errorf("ambiguous cwd must not name an agent, got %v", resp["agent"])
	}
	// No agent's session_id may have been touched.
	for _, n := range []string{"frontend", "documentor"} {
		a, err := r.DB.GetAgent("proj", n)
		if err != nil || a == nil {
			t.Fatalf("get %s: %v", n, err)
		}
		if a.SessionID != nil {
			t.Errorf("%s session_id must stay unbound, got %v", n, *a.SessionID)
		}
	}
}

// Issue #153: an explicit agent (RELAY_AGENT) is authoritative and resolves the
// shared-worktree case with zero inference.
func TestIngestSessionStartExplicitAgentWinsOverAmbiguousCwd(t *testing.T) {
	r := testRelay(t)
	for _, n := range []string{"frontend", "documentor"} {
		if _, _, err := r.DB.RegisterAgent("proj", n, "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
		if err := r.DB.SetAgentCwd("proj", n, "/wt/shared"); err != nil {
			t.Fatalf("set cwd %s: %v", n, err)
		}
	}

	w := doAPI(r, "POST", "/ingest/session-start", `{"session_id":"doc-session","cwd":"/wt/shared","agent":"documentor"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if resp := decodeJSON(t, w); resp["bound"] != true || resp["agent"] != "documentor" {
		t.Fatalf("expected bound documentor, got %v", resp)
	}
	doc, _ := r.DB.GetAgent("proj", "documentor")
	if doc.SessionID == nil || *doc.SessionID != "doc-session" {
		t.Errorf("documentor session_id not bound, got %v", doc.SessionID)
	}
	fe, _ := r.DB.GetAgent("proj", "frontend")
	if fe.SessionID != nil {
		t.Errorf("frontend must be untouched, got %v", *fe.SessionID)
	}
}

// Issue #17: HTTP unread-count endpoint mirrors the get_inbox count without an
// MCP round-trip and without draining the inbox.
func TestAPIUnreadCount(t *testing.T) {
	r := testRelay(t)
	if _, _, err := r.DB.RegisterAgent("proj", "bob", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	w := doAPI(r, "GET", "/inbox/unread-count?agent=bob&project=proj", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if resp := decodeJSON(t, w); resp["unread"].(float64) != 0 {
		t.Fatalf("expected 0 unread, got %v", resp["unread"])
	}

	if _, _, err := r.DB.InsertMessageWithDeliveries("proj", "alice", "bob", "note", "hi", "body", "", "normal", 0, nil, nil, []string{"bob"}, ""); err != nil {
		t.Fatalf("insert: %v", err)
	}
	w = doAPI(r, "GET", "/inbox/unread-count?agent=bob&project=proj", "")
	if resp := decodeJSON(t, w); resp["unread"].(float64) != 1 {
		t.Fatalf("expected 1 unread, got %v", resp["unread"])
	}

	// Missing agent param → 400.
	if w := doAPI(r, "GET", "/inbox/unread-count?project=proj", ""); w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without agent, got %d", w.Code)
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
