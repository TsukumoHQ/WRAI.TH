package db

import (
	"errors"
	"testing"
)

// InvalidTitleReason is the single source of truth for "what makes a title
// bare" — pin every case named in the ticket (goal dbdf5f32): empty/whitespace,
// a bare UUID, known placeholder strings, and the ordinary pass-through.
func TestInvalidTitleReason(t *testing.T) {
	cases := []struct {
		title    string
		wantBare bool
	}{
		{"", true},
		{"   ", true},
		{"a4d0f829-3e63-4ae0-96f8-c86d5061f0f9", true},
		{"A4D0F829-3E63-4AE0-96F8-C86D5061F0F9", true},
		{"untitled", true},
		{"Untitled", true},
		{"TBD", true},
		{"todo", true},
		{"placeholder", true},
		{"test", true},
		{"n/a", true},
		{"Fix login race on double-submit", false},
		{"wraith-engine-2/product-board-routing", false},
	}
	for _, tc := range cases {
		got := InvalidTitleReason(tc.title)
		if tc.wantBare && got == "" {
			t.Errorf("InvalidTitleReason(%q) = \"\", want a reason", tc.title)
		}
		if !tc.wantBare && got != "" {
			t.Errorf("InvalidTitleReason(%q) = %q, want \"\" (real title)", tc.title, got)
		}
	}
}

// DispatchTask, on a project enforcing typed tickets, refuses a placeholder
// title alongside the existing goal/AC/dod checks — same choke, same error
// family (errors.As). A free-form project is unaffected (no title rule at
// all, exactly like the typed-ticket guard's own retrocompat clause).
func TestDispatchTaskRejectsPlaceholderTitleOnEnforcedProject(t *testing.T) {
	d := testDB(t)
	completeTicket := TypedTicket{Goal: "g", AcceptanceCriteria: `["a"]`, Dod: "d"}

	_, err := d.DispatchTask("niwa", "dev", "cto", "a4d0f829-3e63-4ae0-96f8-c86d5061f0f9", "", "P2", nil, nil, completeTicket, false, nil)
	var ite *InvalidTitleError
	if !errors.As(err, &ite) {
		t.Fatalf("bare-UUID title on enforced project must return *InvalidTitleError, got %v", err)
	}
	if ite.Project != "niwa" {
		t.Fatalf("error project = %q, want niwa", ite.Project)
	}

	_, err = d.DispatchTask("niwa", "dev", "cto", "", "", "P2", nil, nil, completeTicket, false, nil)
	if !errors.As(err, &ite) {
		t.Fatalf("empty title on enforced project must return *InvalidTitleError, got %v", err)
	}

	task, err := d.DispatchTask("niwa", "dev", "cto", "Fix login race on double-submit", "", "P2", nil, nil, completeTicket, false, nil)
	if err != nil {
		t.Fatalf("a real human title must dispatch: %v", err)
	}
	if task.Title != "Fix login race on double-submit" {
		t.Errorf("title not persisted, got %q", task.Title)
	}

	// Free-form project: no typed-ticket enforcement, so no title rule either —
	// a bare-UUID title dispatches unchanged (retrocompat, same clause as the
	// typed-ticket guard's own free-form pass-through).
	d.EnsureProject("free")
	if _, err := d.DispatchTask("free", "dev", "cto", "a4d0f829-3e63-4ae0-96f8-c86d5061f0f9", "", "P2", nil, nil, TypedTicket{}, false, nil); err != nil {
		t.Fatalf("bare-UUID title on a free-form project must succeed: %v", err)
	}
}
