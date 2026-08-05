package db

import "testing"

// The typed-ticket zone (goal / acceptance_criteria / dod) must round-trip
// through the column list ↔ scanTask lockstep, exactly like the git zone — the
// review gate reads the acceptance list off the task at review time.
func TestTypedTicketRoundTrip(t *testing.T) {
	d := testDB(t)

	ticket := TypedTicket{
		Goal:               "determinise task birth",
		AcceptanceCriteria: `["refuses without goal","get_task renders the fields"]`,
		Dod:                "go test ./... green",
	}
	task, err := d.DispatchTask("proj", "backend", "cto", "typed ticket", "", "P1", nil, nil, ticket)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if task.Goal != ticket.Goal || task.AcceptanceCriteria != ticket.AcceptanceCriteria || task.Dod != ticket.Dod {
		t.Fatalf("dispatch did not carry the ticket: %+v", task)
	}

	got, err := d.GetTask(task.ID, "proj")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Goal != ticket.Goal {
		t.Fatalf("goal lost: %q", got.Goal)
	}
	if got.AcceptanceCriteria != ticket.AcceptanceCriteria {
		t.Fatalf("acceptance_criteria lost: %q", got.AcceptanceCriteria)
	}
	if got.Dod != ticket.Dod {
		t.Fatalf("dod lost: %q", got.Dod)
	}
}

// A free-form dispatch (no ticket) must persist safe defaults, never NULL —
// the columns are NOT NULL and old clients never send a ticket.
func TestTypedTicketDefaultsWhenAbsent(t *testing.T) {
	d := testDB(t)

	task, err := d.DispatchTask("proj", "backend", "cto", "no ticket", "", "P2", nil, nil, TypedTicket{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, err := d.GetTask(task.ID, "proj")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Goal != "" || got.Dod != "" {
		t.Fatalf("expected empty goal/dod, got %q / %q", got.Goal, got.Dod)
	}
	if got.AcceptanceCriteria != "[]" {
		t.Fatalf("acceptance_criteria default should be '[]', got %q", got.AcceptanceCriteria)
	}
}

// The per-project enforcement flag: default off, niwa seeded on, setter flips.
func TestProjectRequiresTypedTicket(t *testing.T) {
	d := testDB(t)

	if d.ProjectRequiresTypedTicket("default") {
		t.Fatal("default project must not enforce typed tickets")
	}
	if d.ProjectRequiresTypedTicket("nonexistent") {
		t.Fatal("unknown project must read as not-enforcing")
	}
	if !d.ProjectRequiresTypedTicket("niwa") {
		t.Fatal("niwa must be seeded with typed-ticket enforcement on")
	}

	d.EnsureProject("proj")
	if d.ProjectRequiresTypedTicket("proj") {
		t.Fatal("a fresh project must default to off")
	}
	if err := d.SetProjectRequiresTypedTicket("proj", true); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !d.ProjectRequiresTypedTicket("proj") {
		t.Fatal("flag did not flip on")
	}
	if err := d.SetProjectRequiresTypedTicket("proj", false); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if d.ProjectRequiresTypedTicket("proj") {
		t.Fatal("flag did not flip off")
	}
}
