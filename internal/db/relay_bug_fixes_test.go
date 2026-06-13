package db

import (
	"strings"
	"testing"
)

// Fix 1.7 — re-register with nil reports_to must preserve the existing value.
// Regression guard for the pool-burst bug that wiped reports_to on 10+ agents.
func TestRegisterAgent_PreservesReportsToOnRespawn(t *testing.T) {
	d := testDB(t)

	manager := "lead"
	_, _, err := d.RegisterAgent("p1", "lead", "team-lead", "", nil, nil, true, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("register lead: %v", err)
	}

	// Initial registration with reports_to set.
	_, _, err = d.RegisterAgent("p1", "worker", "dev", "", &manager, nil, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	// Re-register WITHOUT reports_to — must preserve existing.
	w, _, err := d.RegisterAgent("p1", "worker", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
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
	_, _, err := d.RegisterAgent("p1", "worker", "dev", "", nil, &slug, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	w, _, err := d.RegisterAgent("p1", "worker", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
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

	_, _, _ = d.RegisterAgent("p1", "cto", "lead", "", nil, nil, true, nil, "[]", 0, RegisterOptions{})

	// 5 cancelled + 1 active task, all dispatched_by cto.
	for i := 0; i < 5; i++ {
		tsk, err := d.DispatchTask("p1", "dev", "cto", "cancelled task", "", "P2", nil, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if _, err := d.CancelTask(tsk.ID, "cto", "p1", nil); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	}
	activeTsk, err := d.DispatchTask("p1", "dev", "cto", "active task", "", "P2", nil, nil)
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

	_, _, _ = d.RegisterAgent("p1", "cto", "lead", "", nil, nil, true, nil, "[]", 0, RegisterOptions{})
	for i := 0; i < 30; i++ {
		_, err := d.DispatchTask("p1", "dev", "cto", "task", "", "P2", nil, nil)
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

// register_profile merges instead of wiping unspecified identity fields.
func TestRegisterProfile_MergesOnUpdate(t *testing.T) {
	d := testDB(t)

	// Initial registration.
	if _, err := d.RegisterProfile("p1", "backend-dev", "Backend Dev", "dev", `["go","sql"]`); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Partial re-registration — only skills specified. role must survive.
	if _, err := d.RegisterProfile("p1", "backend-dev", "Backend Dev", "", `["go","sql","grpc"]`); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	p, err := d.GetProfile("p1", "backend-dev")
	if err != nil || p == nil {
		t.Fatalf("get profile: %v", err)
	}
	if !strings.Contains(p.Skills, "grpc") {
		t.Errorf("skills not updated: %q", p.Skills)
	}
	if p.Role != "dev" {
		t.Errorf("role wiped: %q", p.Role)
	}
}

// Fix 3.5 — progress notes round-trip.
func TestProgressNotes_RoundTrip(t *testing.T) {
	d := testDB(t)

	tsk, _ := d.DispatchTask("p1", "dev", "cto", "long task", "", "P2", nil, nil)

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
