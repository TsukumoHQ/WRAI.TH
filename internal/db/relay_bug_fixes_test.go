package db

import (
	"strings"
	"testing"
	"time"
)

// Fix 1.7 — re-register with nil reports_to must preserve the existing value.
// Regression guard for the pool-burst bug that wiped reports_to on 10+ agents.
func TestRegisterAgent_PreservesReportsToOnRespawn(t *testing.T) {
	d := testDB(t)

	manager := "lead"
	_, _, err := d.RegisterAgent("p1", "lead", "team-lead", "", nil, nil, true, nil, "[]", 0)
	if err != nil {
		t.Fatalf("register lead: %v", err)
	}

	// Initial registration with reports_to set.
	_, _, err = d.RegisterAgent("p1", "worker", "dev", "", &manager, nil, false, nil, "[]", 0)
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	// Re-register WITHOUT reports_to — must preserve existing.
	w, _, err := d.RegisterAgent("p1", "worker", "dev", "", nil, nil, false, nil, "[]", 0)
	if err != nil {
		t.Fatalf("re-register worker: %v", err)
	}
	if w.ReportsTo == nil || *w.ReportsTo != "lead" {
		got := "<nil>"
		if w.ReportsTo != nil {
			got = *w.ReportsTo
		}
		t.Fatalf("reports_to wiped on respawn: got %q, want %q", got, "lead")
	}
}

// Fix 1.7 — re-register with nil profile_slug must preserve the existing value.
func TestRegisterAgent_PreservesProfileSlugOnRespawn(t *testing.T) {
	d := testDB(t)

	slug := "backend-dev"
	_, _, err := d.RegisterAgent("p1", "worker", "dev", "", nil, &slug, false, nil, "[]", 0)
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	w, _, err := d.RegisterAgent("p1", "worker", "dev", "", nil, nil, false, nil, "[]", 0)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if w.ProfileSlug == nil || *w.ProfileSlug != "backend-dev" {
		t.Fatalf("profile_slug wiped on respawn")
	}
}

// Fix 1.1 — dispatched_by_me must not include cancelled / done / failed tasks.
// Regression guard for the 303K session_context payload bug.
func TestGetAgentTasks_DispatchedExcludesInactiveStatuses(t *testing.T) {
	d := testDB(t)

	_, _, _ = d.RegisterAgent("p1", "cto", "lead", "", nil, nil, true, nil, "[]", 0)

	// 5 cancelled + 1 active task, all dispatched_by cto.
	for i := 0; i < 5; i++ {
		tsk, err := d.DispatchTask("p1", "dev", "cto", "cancelled task", "", "P2", nil, nil, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if _, err := d.CancelTask(tsk.ID, "cto", "p1", nil); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	}
	activeTsk, err := d.DispatchTask("p1", "dev", "cto", "active task", "", "P2", nil, nil, nil)
	if err != nil {
		t.Fatalf("dispatch active: %v", err)
	}

	_, dispatched, err := d.GetAgentTasks("p1", "cto")
	if err != nil {
		t.Fatalf("get agent tasks: %v", err)
	}
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 active dispatch, got %d", len(dispatched))
	}
	if dispatched[0].ID != activeTsk.ID {
		t.Fatalf("wrong task surfaced: got %s", dispatched[0].ID)
	}
}

// Fix 1.1/1.2 — GetAgentTasks caps dispatched_by_me at 20 items.
func TestGetAgentTasks_DispatchedLimited(t *testing.T) {
	d := testDB(t)

	_, _, _ = d.RegisterAgent("p1", "cto", "lead", "", nil, nil, true, nil, "[]", 0)
	for i := 0; i < 30; i++ {
		_, err := d.DispatchTask("p1", "dev", "cto", "task", "", "P2", nil, nil, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	}

	_, dispatched, err := d.GetAgentTasks("p1", "cto")
	if err != nil {
		t.Fatalf("get agent tasks: %v", err)
	}
	if len(dispatched) > 20 {
		t.Fatalf("expected ≤20 dispatched tasks, got %d", len(dispatched))
	}
}

// Fix 2.1 — register_profile merges instead of wiping unspecified fields.
func TestRegisterProfile_MergesOnUpdate(t *testing.T) {
	d := testDB(t)

	// Initial full registration
	_, err := d.RegisterProfile(
		"p1", "backend-dev", "Backend Dev", "dev",
		"full context pack here", `["identity"]`, `["go","sql"]`, `["DOCS.md"]`,
		WithAllowedTools(`["Read","Write"]`),
		WithPoolSize(5),
		WithExitPrompt("goodbye"),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Partial re-registration — only vault_paths specified. context_pack, role,
	// exit_prompt, allowed_tools, pool_size must all survive.
	_, err = d.RegisterProfile(
		"p1", "backend-dev", "Backend Dev", "",
		"", "", "", `["NEW_DOC.md"]`,
	)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}

	p, err := d.GetProfile("p1", "backend-dev")
	if err != nil || p == nil {
		t.Fatalf("get profile: %v", err)
	}
	if !strings.Contains(p.VaultPaths, "NEW_DOC.md") {
		t.Errorf("vault_paths not updated: %q", p.VaultPaths)
	}
	if p.ContextPack != "full context pack here" {
		t.Errorf("context_pack wiped: %q", p.ContextPack)
	}
	if p.Role != "dev" {
		t.Errorf("role wiped: %q", p.Role)
	}
	if p.ExitPrompt != "goodbye" {
		t.Errorf("exit_prompt wiped: %q", p.ExitPrompt)
	}
	if p.PoolSize != 5 {
		t.Errorf("pool_size wiped: %d", p.PoolSize)
	}
}

// Fix 3.3 — UpdateSpawnChild persists stdout and stderr tails (truncated).
func TestUpdateSpawnChild_PersistsTails(t *testing.T) {
	d := testDB(t)

	d.InsertSpawnChild("child-1", "parent", "p1", "dev", "prompt")

	longStdout := strings.Repeat("o", 3000)
	longStderr := strings.Repeat("e", 5000)
	d.UpdateSpawnChild("child-1", "finished", 0, "", longStdout, longStderr)

	row := d.conn.QueryRow(`SELECT stdout_tail, stderr_tail FROM spawn_children WHERE id = ?`, "child-1")
	var stdout, stderr string
	if err := row.Scan(&stdout, &stderr); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(stdout) != 2048 {
		t.Errorf("stdout_tail want 2048 bytes, got %d", len(stdout))
	}
	if len(stderr) != 4096 {
		t.Errorf("stderr_tail want 4096 bytes, got %d", len(stderr))
	}
}

// UpdateSpawnChild clears the stored prompt on completion (86%-of-DB leak).
func TestUpdateSpawnChild_ClearsPrompt(t *testing.T) {
	d := testDB(t)

	d.InsertSpawnChild("child-1", "parent", "p1", "dev", strings.Repeat("x", 90000))
	d.UpdateSpawnChild("child-1", "finished", 0, "", "", "")

	var prompt string
	if err := d.conn.QueryRow(`SELECT prompt FROM spawn_children WHERE id = ?`, "child-1").Scan(&prompt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if prompt != "" {
		t.Errorf("prompt not cleared on completion: %d bytes left", len(prompt))
	}
}

// PurgeSpawnChildren removes old finished rows, preserves running children.
func TestPurgeSpawnChildren(t *testing.T) {
	d := testDB(t)

	d.InsertSpawnChild("old-finished", "parent", "p1", "dev", "prompt")
	d.UpdateSpawnChild("old-finished", "finished", 0, "", "", "")
	// Backdate completion to 10 days ago.
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := d.conn.Exec(`UPDATE spawn_children SET finished_at = ? WHERE id = ?`, old, "old-finished"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	d.InsertSpawnChild("running", "parent", "p1", "dev", "prompt") // still running

	n, err := d.PurgeSpawnChildren(7 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 purged, got %d", n)
	}

	var count int
	_ = d.conn.QueryRow(`SELECT COUNT(*) FROM spawn_children WHERE id = ?`, "running").Scan(&count)
	if count != 1 {
		t.Error("running child must be preserved")
	}
}

// Fix 2.2 — GetOldestPendingTaskForProfile returns FIFO pending task.
func TestGetOldestPendingTaskForProfile(t *testing.T) {
	d := testDB(t)

	// No pending tasks yet.
	if got, _ := d.GetOldestPendingTaskForProfile("p1", "dev"); got != nil {
		t.Fatalf("expected nil when no pending, got %s", got.ID)
	}

	first, _ := d.DispatchTask("p1", "dev", "cto", "first", "", "P2", nil, nil, nil)
	_, _ = d.DispatchTask("p1", "dev", "cto", "second", "", "P0", nil, nil, nil)

	got, err := d.GetOldestPendingTaskForProfile("p1", "dev")
	if err != nil {
		t.Fatalf("get oldest: %v", err)
	}
	if got == nil || got.ID != first.ID {
		t.Fatalf("expected oldest pending to be first-dispatched, got %v", got)
	}
}

// Fix 3.5 — progress notes round-trip.
func TestProgressNotes_RoundTrip(t *testing.T) {
	d := testDB(t)

	tsk, _ := d.DispatchTask("p1", "dev", "cto", "long task", "", "P2", nil, nil, nil)

	if err := d.AddProgressNote(tsk.ID, "p1", "worker-1", "halfway through"); err != nil {
		t.Fatalf("add progress: %v", err)
	}
	if err := d.AddProgressNote(tsk.ID, "p1", "worker-1", "running tests"); err != nil {
		t.Fatalf("add progress: %v", err)
	}

	notes, err := d.GetProgressNotes(tsk.ID, "p1")
	if err != nil {
		t.Fatalf("get progress: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(notes))
	}
	if notes[0].Note != "halfway through" || notes[1].Note != "running tests" {
		t.Errorf("notes out of order: %+v", notes)
	}
}

// Fix 3.1 — UpdateTriggerFields applies partial updates without wiping other fields.
func TestUpdateTriggerFields_Partial(t *testing.T) {
	d := testDB(t)

	coolDown := 120
	created, err := d.UpsertTrigger("p1", "task.dispatched", `{}`, "dev", "review", "10m", &coolDown)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	newMax := "20m"
	updated, err := d.UpdateTriggerFields(created.ID, nil, nil, nil, &newMax, nil, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated == nil {
		t.Fatal("expected update to return trigger, got nil")
	}
	if updated.MaxDuration != "20m" {
		t.Errorf("max_duration not updated: %q", updated.MaxDuration)
	}
	if updated.ProfileSlug != "dev" {
		t.Errorf("profile_slug wiped: %q", updated.ProfileSlug)
	}
	if updated.CooldownSeconds != 120 {
		t.Errorf("cooldown wiped: %d", updated.CooldownSeconds)
	}
}
