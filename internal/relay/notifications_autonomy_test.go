package relay

import (
	"path/filepath"
	"testing"

	"agent-relay/internal/db"
)

func newAutonomyTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.NewTestDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// TestResolveOwnerTarget covers the lead-machine "owner-of-record" target used
// by the stale-deal re-nudge rule (TSU-146).
func TestResolveOwnerTarget(t *testing.T) {
	n := &Notifier{db: newAutonomyTestDB(t)}

	got := n.resolveTargets("default", "owner", map[string]any{"owner": "loic"})
	if len(got) != 1 || got[0] != "loic" {
		t.Fatalf("owner target: want [loic], got %v", got)
	}
	if got := n.resolveTargets("default", "owner", map[string]any{}); got != nil {
		t.Fatalf("owner target with no payload.owner: want nil, got %v", got)
	}
}

// TestEnsureAutonomyRulesIdempotent verifies the rules land on an already-seeded
// table and that re-running never duplicates them (the prod-relay path, where
// seedDefaults is a no-op because the table is non-empty).
func TestEnsureAutonomyRulesIdempotent(t *testing.T) {
	database := newAutonomyTestDB(t)
	n := &Notifier{db: database}

	names := map[string]bool{
		"Lead ready → donna (P0)":          true,
		"Stale deal → owner re-nudge (P1)": true,
		"Review aged → cto escalate (P1)":  true,
	}

	count := func() map[string]int {
		rules, err := database.ListNotificationRules("default")
		if err != nil {
			t.Fatalf("list rules: %v", err)
		}
		seen := map[string]int{}
		for _, r := range rules {
			if names[r.Name] {
				seen[r.Name]++
			}
		}
		return seen
	}

	n.ensureAutonomyRules()
	n.ensureAutonomyRules() // idempotent: a second pass must add nothing

	seen := count()
	if len(seen) != len(names) {
		t.Fatalf("want %d autonomy rules present, got %d: %v", len(names), len(seen), seen)
	}
	for name, c := range seen {
		if c != 1 {
			t.Errorf("rule %q present %d times, want exactly 1 (idempotency broken)", name, c)
		}
	}

	// Each must be enabled and target the right recipient.
	rules, _ := database.ListNotificationRules("default")
	for _, r := range rules {
		if !names[r.Name] {
			continue
		}
		if !r.Enabled {
			t.Errorf("autonomy rule %q seeded disabled", r.Name)
		}
	}
}
