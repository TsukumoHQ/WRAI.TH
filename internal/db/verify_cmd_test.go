package db

import "testing"

// verify_cmd (task 6c1c5167 follow-up, DEC-niwa-goal-validate-1) is the
// OPTIONAL sibling of goal/acceptance_criteria/dod — it must NEVER be part of
// what TypedTicket.Missing() enforces, on any project, enforced or not. A
// complete goal/AC/dod ticket with no verify_cmd must still dispatch cleanly
// on "niwa" (seeded with require_typed_ticket=on).
func TestVerifyCmd_NeverEnforcedOnDispatch(t *testing.T) {
	d := testDB(t)

	task, err := d.DispatchTask("niwa", "dev", "cto", "no verify_cmd", "", "P2", nil, nil,
		TypedTicket{Goal: "g", AcceptanceCriteria: `["a"]`, Dod: "d"}, false, nil)
	if err != nil {
		t.Fatalf("a complete ticket with no verify_cmd must dispatch on an enforced project: %v", err)
	}
	if task.VerifyCmd != nil {
		t.Fatalf("expected nil verify_cmd, got %q", *task.VerifyCmd)
	}

	got, err := d.GetTask(task.ID, "niwa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.VerifyCmd != nil {
		t.Fatalf("get_task should also read nil verify_cmd, got %q", *got.VerifyCmd)
	}
}

// A supplied verify_cmd round-trips verbatim — the relay stores it opaquely,
// no shape validation, no mutation (agentd owns validating the command).
func TestVerifyCmd_RoundTripVerbatim(t *testing.T) {
	d := testDB(t)

	cmd := `bash -c "go test ./... && echo PASS" ; exit $?`
	task, err := d.DispatchTask("niwa", "dev", "cto", "with verify_cmd", "", "P2", nil, nil,
		TypedTicket{Goal: "g", AcceptanceCriteria: `["a"]`, Dod: "d", VerifyCmd: &cmd}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if task.VerifyCmd == nil || *task.VerifyCmd != cmd {
		t.Fatalf("dispatch did not carry verify_cmd verbatim, got %v", task.VerifyCmd)
	}

	got, err := d.GetTask(task.ID, "niwa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.VerifyCmd == nil || *got.VerifyCmd != cmd {
		t.Fatalf("get_task lost verify_cmd, got %v, want %q", got.VerifyCmd, cmd)
	}
}

// A free-form dispatch with no ticket at all must default verify_cmd to nil,
// never an empty-string placeholder — it is a nullable column, unlike
// goal/dod which default to "".
func TestVerifyCmd_DefaultsToNilWhenAbsent(t *testing.T) {
	d := testDB(t)

	task, err := d.DispatchTask("proj", "backend", "cto", "no ticket at all", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if task.VerifyCmd != nil {
		t.Fatalf("expected nil verify_cmd on a free-form dispatch, got %q", *task.VerifyCmd)
	}
}

// The additive migration (ensureColumns) must be idempotent and
// backward-compatible: reopening the same DB file (simulating a binary
// restart) must not error, must not drop the column, and must not lose data
// already written through it.
func TestVerifyCmd_MigrationIdempotentOnReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/relay.db"

	d1, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	cmd := "make verify"
	task, err := d1.DispatchTask("proj", "backend", "cto", "pre-reopen", "", "P2", nil, nil,
		TypedTicket{VerifyCmd: &cmd}, false, nil)
	if err != nil {
		t.Fatalf("dispatch before reopen: %v", err)
	}
	_ = d1.Close()

	// Re-run migration on the SAME file — must not error (idempotent) and must
	// not lose the row or the column's data (backward/forward compatible).
	d2, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("reopen (re-migrate) must not error: %v", err)
	}
	defer func() { _ = d2.Close() }()

	got, err := d2.GetTask(task.ID, "proj")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got == nil {
		t.Fatal("task lost across reopen")
	}
	if got.VerifyCmd == nil || *got.VerifyCmd != cmd {
		t.Fatalf("verify_cmd lost or changed across reopen: got %v, want %q", got.VerifyCmd, cmd)
	}
}
