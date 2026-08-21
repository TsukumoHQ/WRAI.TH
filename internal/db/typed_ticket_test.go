package db

import (
	"errors"
	"strings"
	"testing"
)

// TypedTicket.Missing is the single source of truth for "what makes a ticket
// bare". It must name the absent fields in dispatch order and treat an empty /
// non-array / all-blank acceptance_criteria as absent.
func TestTypedTicketMissing(t *testing.T) {
	cases := []struct {
		name   string
		ticket TypedTicket
		want   []string
	}{
		{"all missing", TypedTicket{}, []string{"goal", "acceptance_criteria", "dod"}},
		{"goal only", TypedTicket{Goal: "g"}, []string{"acceptance_criteria", "dod"}},
		{"complete", TypedTicket{Goal: "g", AcceptanceCriteria: `["a"]`, Dod: "d"}, nil},
		{"empty array ac", TypedTicket{Goal: "g", AcceptanceCriteria: `[]`, Dod: "d"}, []string{"acceptance_criteria"}},
		{"blank items ac", TypedTicket{Goal: "g", AcceptanceCriteria: `["  ",""]`, Dod: "d"}, []string{"acceptance_criteria"}},
		{"non-array ac", TypedTicket{Goal: "g", AcceptanceCriteria: `"x"`, Dod: "d"}, []string{"acceptance_criteria"}},
		{"whitespace goal/dod", TypedTicket{Goal: "  ", AcceptanceCriteria: `["a"]`, Dod: " "}, []string{"goal", "dod"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(tc.ticket.Missing(), ","); got != strings.Join(tc.want, ",") {
				t.Fatalf("Missing() = [%s], want [%s]", got, strings.Join(tc.want, ","))
			}
		})
	}
}

// The single enforcement choke: DispatchTask itself refuses a bare ticket on an
// enforced project (niwa is seeded on), with a typed *TypedTicketError naming the
// missing fields — proving the guard is path-agnostic (every caller funnels here,
// so cron / inbound-signal / self-dispatch cannot bypass it), not a per-handler
// check. A complete ticket, and any bare ticket on a free-form project, pass.
func TestDispatchTaskEnforcesTypedTicketAtChoke(t *testing.T) {
	d := testDB(t)

	// bare on enforced project → refused, all three named, in order.
	_, err := d.DispatchTask("niwa", "dev", "cto", "bare", "", "P2", nil, nil, TypedTicket{}, false, nil)
	var tte *TypedTicketError
	if !errors.As(err, &tte) {
		t.Fatalf("bare dispatch on enforced project must return *TypedTicketError, got %v", err)
	}
	if strings.Join(tte.Missing, ",") != "goal,acceptance_criteria,dod" {
		t.Fatalf("missing = %v, want all three in order", tte.Missing)
	}
	if tte.Project != "niwa" {
		t.Fatalf("error project = %q, want niwa", tte.Project)
	}

	// partial on enforced project → names only the absent field.
	_, err = d.DispatchTask("niwa", "dev", "cto", "partial", "", "P2", nil, nil,
		TypedTicket{Goal: "g", AcceptanceCriteria: `["a"]`}, false, nil)
	if !errors.As(err, &tte) || strings.Join(tte.Missing, ",") != "dod" {
		t.Fatalf("partial dispatch should refuse missing [dod], got %v", err)
	}

	// complete on enforced project → dispatches.
	task, err := d.DispatchTask("niwa", "dev", "cto", "complete", "", "P2", nil, nil,
		TypedTicket{Goal: "g", AcceptanceCriteria: `["a"]`, Dod: "d"}, false, nil)
	if err != nil {
		t.Fatalf("complete ticket on enforced project must dispatch: %v", err)
	}
	if task == nil || task.Status != "pending" {
		t.Fatalf("expected a pending task, got %+v", task)
	}

	// bare on a free-form project → dispatches unchanged (retrocompat).
	d.EnsureProject("free")
	if _, err := d.DispatchTask("free", "dev", "cto", "bare-ok", "", "P2", nil, nil, TypedTicket{}, false, nil); err != nil {
		t.Fatalf("bare dispatch on free-form project must succeed: %v", err)
	}
}

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
	task, err := d.DispatchTask("proj", "backend", "cto", "typed ticket", "", "P1", nil, nil, ticket, false, nil)
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

	task, err := d.DispatchTask("proj", "backend", "cto", "no ticket", "", "P2", nil, nil, TypedTicket{}, false, nil)
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
