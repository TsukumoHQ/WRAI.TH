package db

import "testing"

// T3: reassigning a task across two agents with different registered profiles
// makes the task's profile_slug follow the NEW assignee (display + skill routing
// must not keep lying about who the task is for).
func TestReassignRecomputesProfileSlug(t *testing.T) {
	d := testDB(t)
	pa, pb := "profile-a", "profile-b"
	if _, _, err := d.RegisterAgent("default", "agent-a", "r", "", nil, &pa, false, nil, "[]", 0, RegisterOptions{ProfileSlugSet: true}); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if _, _, err := d.RegisterAgent("default", "agent-b", "r", "", nil, &pb, false, nil, "[]", 0, RegisterOptions{ProfileSlugSet: true}); err != nil {
		t.Fatalf("register b: %v", err)
	}

	task, err := d.DispatchTask("default", pa, "cto", "t", "d", "P2", nil, nil, TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if task.ProfileSlug != pa {
		t.Fatalf("dispatched slug: want %q got %q", pa, task.ProfileSlug)
	}

	// Reassign to agent-b (different profile) -> slug follows.
	re, err := d.ReassignTask(task.ID, "default", "agent-b")
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if re.ProfileSlug != pb {
		t.Errorf("reassigned in-memory slug: want %q got %q", pb, re.ProfileSlug)
	}
	got, _ := d.GetTask(task.ID, "default")
	if got.ProfileSlug != pb {
		t.Errorf("persisted slug after reassign: want %q got %q", pb, got.ProfileSlug)
	}
}

// T3 guard: reassigning to an agent with NO registered profile_slug must NOT
// blank the task's existing slug.
func TestReassignToNoProfileKeepsSlug(t *testing.T) {
	d := testDB(t)
	pa := "profile-a"
	if _, _, err := d.RegisterAgent("default", "agent-a", "r", "", nil, &pa, false, nil, "[]", 0, RegisterOptions{ProfileSlugSet: true}); err != nil {
		t.Fatalf("register a: %v", err)
	}
	// agent-c registered without a profile slug.
	if _, _, err := d.RegisterAgent("default", "agent-c", "r", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("register c: %v", err)
	}
	task, err := d.DispatchTask("default", pa, "cto", "t", "d", "P2", nil, nil, TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	re, err := d.ReassignTask(task.ID, "default", "agent-c")
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if re.ProfileSlug != pa {
		t.Errorf("slug must be preserved when new assignee has no profile: want %q got %q", pa, re.ProfileSlug)
	}
}

// T3: the one-time backfill corrects a stale profile_slug row and re-running is
// a no-op.
func TestBackfillTaskProfileSlugsIdempotent(t *testing.T) {
	d := testDB(t)
	pa, pb := "profile-a", "profile-b"
	if _, _, err := d.RegisterAgent("default", "agent-a", "r", "", nil, &pa, false, nil, "[]", 0, RegisterOptions{ProfileSlugSet: true}); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if _, _, err := d.RegisterAgent("default", "agent-b", "r", "", nil, &pb, false, nil, "[]", 0, RegisterOptions{ProfileSlugSet: true}); err != nil {
		t.Fatalf("register b: %v", err)
	}
	task, err := d.DispatchTask("default", pa, "cto", "t", "d", "P2", nil, nil, TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Simulate a pre-T3 stale row: assigned to agent-b but slug still profile-a.
	if _, err := d.conn.Exec("UPDATE tasks SET assigned_to = 'agent-b', profile_slug = ? WHERE id = ?", pa, task.ID); err != nil {
		t.Fatalf("stale setup: %v", err)
	}

	n, err := d.BackfillTaskProfileSlugs()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Errorf("backfill should fix 1 row, got %d", n)
	}
	got, _ := d.GetTask(task.ID, "default")
	if got.ProfileSlug != pb {
		t.Errorf("backfilled slug: want %q got %q", pb, got.ProfileSlug)
	}

	// Re-run is a no-op.
	n2, err := d.BackfillTaskProfileSlugs()
	if err != nil {
		t.Fatalf("backfill re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("re-run backfill should be a no-op, changed %d rows", n2)
	}
}
