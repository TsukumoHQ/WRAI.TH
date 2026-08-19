package relay

import (
	"fmt"
	"path/filepath"
	"strings"
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
		"POST hook events to the relay; no file-drop watcher", "watcher deadlocked", nil, "", nil); err != nil {
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

// TestSessionContext_DecisionsOmitted_ReflectsByteBudget locks the handler-level
// overflow signal (handlers.go): decisions_omitted must be computed from the
// PROJECTED result, not the count cap. It seeds fewer decisions than
// sessionDecisionMax but fat enough that the byte budget drops several — the old
// count-cap formula reports 0 omitted (test would go green) while agents silently
// miss accepted decisions. Reverting the handler formula fails this test.
func TestSessionContext_DecisionsOmitted_ReflectsByteBudget(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.NewTestDB(dbPath)
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	h := NewHandlers(database, NewSessionRegistry(nil), nil, NewEventBus())
	const project = "p1"

	// 10 fat decisions (< sessionDecisionMax=40) — distinct areas so each is a
	// separate DEC key, each a multi-line blob that dominates the byte budget.
	const seeded = 10
	blob := strings.Repeat("y", 3000)
	for i := 0; i < seeded; i++ {
		area := fmt.Sprintf("ops/area-%02d", i)
		if _, err := database.RememberDecision(project, "wraith-dev", area, blob, blob, nil, "", nil); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}

	ctx := h.buildSessionContext(project, "wraith-dev", nil)

	decs, ok := ctx["decisions"].([]DecisionSummary)
	if !ok {
		t.Fatalf("decisions section missing/typed wrong: %T", ctx["decisions"])
	}
	// The byte budget must have dropped some (count cap alone would keep all 10).
	if len(decs) >= seeded {
		t.Fatalf("byte budget did not truncate: surfaced %d of %d", len(decs), seeded)
	}
	omitted, ok := ctx["decisions_omitted"].(int)
	if !ok {
		t.Fatalf("decisions_omitted missing/typed wrong: %T (%v)", ctx["decisions_omitted"], ctx["decisions_omitted"])
	}
	if want := seeded - len(decs); omitted != want {
		t.Fatalf("decisions_omitted = %d, want %d (seeded %d − surfaced %d)", omitted, want, seeded, len(decs))
	}
	// And the count-cap formula would have reported nothing — proving the fix.
	if seeded <= sessionDecisionMax && omitted == 0 {
		t.Fatal("count-cap regression: omitted is 0 despite byte-budget truncation")
	}
}
