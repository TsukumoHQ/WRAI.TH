package relay

import (
	"path/filepath"
	"testing"

	"agent-relay/internal/db"
)

// TestSessionContext_InjectsDecisions covers TSU-51 slice-B: the accepted
// decision set is surfaced in the session-start context as its own `decisions`
// section, and decisions are NOT double-listed in relevant_memories.
func TestSessionContext_InjectsDecisions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.NewTestDB(dbPath)
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	h := NewHandlers(database, NewSessionRegistry(nil), nil, NewEventBus())
	const project = "p1"

	if _, err := database.RememberDecision(project, "wraith-dev", "ingest/hooks",
		"POST hook events to the relay; no file-drop watcher", "watcher deadlocked", nil, ""); err != nil {
		t.Fatalf("remember: %v", err)
	}

	ctx := h.buildSessionContext(project, "wraith-dev", nil)

	decs, ok := ctx["decisions"].([]DecisionSummary)
	if !ok || len(decs) != 1 {
		t.Fatalf("session context must carry 1 decision, got %v (%T)", ctx["decisions"], ctx["decisions"])
	}
	if decs[0].Key != "DEC-ingest-hooks-1" || decs[0].Decision == "" {
		t.Fatalf("decision summary wrong: %+v", decs[0])
	}

	// The decision must NOT also appear in relevant_memories (no double-listing).
	if mems, ok := ctx["relevant_memories"].([]MemorySummary); ok {
		for _, m := range mems {
			if m.Key == "DEC-ingest-hooks-1" {
				t.Fatalf("decision leaked into relevant_memories — should be decisions-only")
			}
		}
	}
}
