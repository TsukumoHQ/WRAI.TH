package db

import (
	"os"
	"testing"
)

// taskAssignee reads a task's current assigned_to (empty string when NULL).
func taskAssignee(t *testing.T, d *DB, id string) string {
	t.Helper()
	var a *string
	if err := d.conn.QueryRow(`SELECT assigned_to FROM tasks WHERE id = ?`, id).Scan(&a); err != nil {
		t.Fatalf("read assignee %s: %v", id, err)
	}
	if a == nil {
		return ""
	}
	return *a
}

// TestReconcileResolvesSoleAgentAssignee proves Rule B: an orphan assigned_to that
// is a profile slug with EXACTLY ONE active agent of that profile resolves to that
// agent's name; the run is snapshot-backed, marker-guarded, and idempotent.
func TestReconcileResolvesSoleAgentAssignee(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	// exactly one ACTIVE agent runs the 'backend' profile
	seedAgent(t, c, "p1", "worker-1", "active", "backend", "", 0)

	// task assigned to the SLUG 'backend' (no agent literally named 'backend')
	seedTask(t, c, "t-slug", "p1", "in-progress", "worker-1", "backend", "", "backend", "", "", false)

	d.maybeReconcileOrphans()

	if got := taskAssignee(t, d, "t-slug"); got != "worker-1" {
		t.Errorf("assigned_to = %q, want worker-1 (resolved to the sole live agent)", got)
	}
	// snapshot was taken before the mutation
	if _, err := os.Stat(d.path + reconcileSnapshotSuffix); err != nil {
		t.Errorf("pre-reconcile snapshot not found: %v", err)
	}
	// marker set → one-shot
	if d.GetSetting(reconcileOrphansMarker) != "done" {
		t.Error("reconcile marker not set after run")
	}
	// the healed assignee ref is stamped resolved in the quarantine table (audit
	// trail — not deleted).
	if openCount(t, c, "orphan_assignee") != 0 {
		t.Error("resolved assignee should drop out of the open orphan count")
	}

	// Idempotent: a second run is a marker-guarded no-op and changes nothing.
	before := taskAssignee(t, d, "t-slug")
	d.maybeReconcileOrphans()
	if after := taskAssignee(t, d, "t-slug"); after != before {
		t.Errorf("second reconcile mutated data: %q -> %q", before, after)
	}
}

// TestReconcileLeavesAmbiguousAndUnresolvable proves the conservative half of the
// ruling: ambiguous slugs (>1 candidate), non-slug dead-agent names, dispatched_by
// orphans, and limbo are all LEFT ALONE (marked, never mutated, never rerouted).
func TestReconcileLeavesAmbiguousAndUnresolvable(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedProfile(t, c, "p1", "frontend")
	// TWO active agents run 'backend' → ambiguous, must not resolve
	seedAgent(t, c, "p1", "be-1", "active", "backend", "", 0)
	seedAgent(t, c, "p1", "be-2", "active", "backend", "", 0)
	// one live 'frontend' agent, plus a DEAD one for the limbo case
	seedAgent(t, c, "p1", "fe-1", "active", "frontend", "", 0)
	seedAgent(t, c, "p1", "fe-dead", "deleted", "frontend", "", 0)

	seedTask(t, c, "t-ambig", "p1", "pending", "fe-1", "backend", "", "frontend", "", "", false)     // assigned_to slug 'backend' → 2 candidates → NOT resolved
	seedTask(t, c, "t-ghost", "p1", "pending", "fe-1", "ghost-agent", "", "frontend", "", "", false) // not a slug, no agent → NOT resolved
	seedTask(t, c, "t-limbo", "p1", "in-progress", "fe-1", "fe-dead", "", "frontend", "", "", false) // dead assignee → limbo, must not reroute

	d.maybeReconcileOrphans()

	if got := taskAssignee(t, d, "t-ambig"); got != "backend" {
		t.Errorf("ambiguous assignee mutated to %q, want unchanged 'backend'", got)
	}
	if got := taskAssignee(t, d, "t-ghost"); got != "ghost-agent" {
		t.Errorf("non-resolvable assignee mutated to %q, want unchanged 'ghost-agent'", got)
	}
	if got := taskAssignee(t, d, "t-limbo"); got != "fe-dead" {
		t.Errorf("limbo task was REROUTED to %q — ruling forbids rerouting; want unchanged 'fe-dead'", got)
	}
	// The limbo row is still marked (surfaced for triage), not silently healed.
	if !quarantineRowExists(t, c, "limbo", "t-limbo") {
		t.Error("limbo task must remain quarantined for triage")
	}
}

// TestReconcileSyncsProfileSlug covers Rule A: a task whose profile_slug is stale
// relative to its live assignee's own profile is synced (reused T3 rule).
func TestReconcileSyncsProfileSlug(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedAgent(t, c, "p1", "worker-1", "active", "backend", "", 0)
	// task's stored profile_slug ('stale') differs from its live assignee's ('backend')
	seedTask(t, c, "t-sync", "p1", "in-progress", "worker-1", "worker-1", "", "stale", "", "", false)

	d.maybeReconcileOrphans()

	var slug string
	if err := c.QueryRow(`SELECT profile_slug FROM tasks WHERE id = 't-sync'`).Scan(&slug); err != nil {
		t.Fatal(err)
	}
	if slug != "backend" {
		t.Errorf("profile_slug = %q, want 'backend' (synced to the live assignee's profile)", slug)
	}
}
