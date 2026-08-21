package db

import (
	"errors"
	"sync"
	"testing"
)

// runErrCode extracts a *TaskError's Code, or "" if err is not one.
func runErrCode(err error) string {
	var te *TaskError
	if errors.As(err, &te) {
		return te.Code
	}
	return ""
}

// The run zone (integration_branch + run_state) must round-trip through the
// column-list ↔ scanTask lockstep and default to nil on a fresh task.
func TestRunZoneRoundTrip(t *testing.T) {
	d := testDB(t)
	parent, err := d.DispatchTask("proj", "lead", "cto", "factory run", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if parent.IntegrationBranch != nil || parent.RunState != nil {
		t.Fatalf("fresh task must have empty run zone, got %+v/%+v", parent.IntegrationBranch, parent.RunState)
	}

	branch := "run/abc123"
	got, err := d.SetTaskRun(parent.ID, "proj", &branch, strptr(RunStateOpen))
	if err != nil {
		t.Fatalf("set run: %v", err)
	}
	if got.IntegrationBranch == nil || *got.IntegrationBranch != branch {
		t.Fatalf("integration_branch not stored: %+v", got.IntegrationBranch)
	}
	if got.RunState == nil || *got.RunState != RunStateOpen {
		t.Fatalf("run_state not stored: %+v", got.RunState)
	}

	// Re-read through the normal task path proves the lockstep scan carries it.
	reread, _ := d.GetTask(parent.ID, "proj")
	if reread.RunState == nil || *reread.RunState != RunStateOpen {
		t.Fatalf("run_state lost on re-read: %+v", reread.RunState)
	}
}

// A state-only advance must not wipe the integration_branch (COALESCE), and vice
// versa.
func TestRunZoneCoalesce(t *testing.T) {
	d := testDB(t)
	p, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false, nil)
	branch := "run/keepme"
	if _, err := d.SetTaskRun(p.ID, "proj", &branch, strptr(RunStateOpen)); err != nil {
		t.Fatalf("open: %v", err)
	}
	// state-only advance
	got, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateGating))
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got.IntegrationBranch == nil || *got.IntegrationBranch != branch {
		t.Fatalf("branch wiped by state-only advance: %+v", got.IntegrationBranch)
	}
	if *got.RunState != RunStateGating {
		t.Fatalf("state not advanced: %s", *got.RunState)
	}
}

// run_state transitions are enforced: open-first, forward-only DAG, no
// resurrection of a merged run.
func TestRunStateTransitionEnforced(t *testing.T) {
	d := testDB(t)

	// First stamp to a non-open state is rejected (must open first).
	p, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateGating)); runErrCode(err) != CodeRunStateInvalid {
		t.Fatalf("first stamp to gating must be RUN_STATE_INVALID, got %v", err)
	}

	// Happy path: open→gating→merging→merged.
	for _, st := range []string{RunStateOpen, RunStateGating, RunStateMerging, RunStateMerged} {
		if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(st)); err != nil {
			t.Fatalf("advance to %s: %v", st, err)
		}
	}
	// merged is terminal — no resurrection.
	if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateOpen)); runErrCode(err) != CodeRunStateInvalid {
		t.Fatalf("merged→open must be RUN_STATE_INVALID, got %v", err)
	}
	// idempotent same-state stamp is a no-op success even on a terminal state.
	if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateMerged)); err != nil {
		t.Fatalf("idempotent merged→merged should succeed, got %v", err)
	}

	// A skipping edge (open→merged) is rejected.
	p2, _ := d.DispatchTask("proj", "lead", "cto", "run2", "", "P1", nil, nil, TypedTicket{}, false, nil)
	_, _ = d.SetTaskRun(p2.ID, "proj", nil, strptr(RunStateOpen))
	if _, err := d.SetTaskRun(p2.ID, "proj", nil, strptr(RunStateMerged)); runErrCode(err) != CodeRunStateInvalid {
		t.Fatalf("open→merged must be RUN_STATE_INVALID, got %v", err)
	}
}

// amputation path: blocked→amputated→gating lets the green subset proceed.
func TestRunAmputatePath(t *testing.T) {
	d := testDB(t)
	p, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false, nil)
	for _, st := range []string{RunStateOpen, RunStateBlocked, RunStateAmputated, RunStateGating, RunStateMerging, RunStateMerged} {
		if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(st)); err != nil {
			t.Fatalf("amputate path step %s: %v", st, err)
		}
	}
}

func TestSetRunNotFound(t *testing.T) {
	d := testDB(t)
	if _, err := d.SetTaskRun("nope", "proj", nil, strptr(RunStateOpen)); runErrCode(err) != CodeTaskNotFound {
		t.Fatalf("expected TASK_NOT_FOUND, got %v", err)
	}
}

// A run container (run_state set) is NOT claimable/startable as work; its child
// slices are.
func TestRunContainerNotClaimable(t *testing.T) {
	d := testDB(t)
	parent, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if _, err := d.SetTaskRun(parent.ID, "proj", nil, strptr(RunStateOpen)); err != nil {
		t.Fatalf("open run: %v", err)
	}

	if _, err := d.ClaimTask(parent.ID, "worker", "proj"); runErrCode(err) != CodeRunContainer {
		t.Fatalf("claim of container must be RUN_CONTAINER_NOT_CLAIMABLE, got %v", err)
	}
	if _, err := d.StartTask(parent.ID, "worker", "proj"); runErrCode(err) != CodeRunContainer {
		t.Fatalf("start of container must be RUN_CONTAINER_NOT_CLAIMABLE, got %v", err)
	}

	// A child slice (no run_state) claims normally.
	slice, _ := d.DispatchTask("proj", "backend", "cto", "slice", "", "P1", &parent.ID, nil, TypedTicket{}, false, nil)
	if _, err := d.ClaimTask(slice.ID, "worker", "proj"); err != nil {
		t.Fatalf("child slice must be claimable, got %v", err)
	}
}

// GetRun returns the parent (with run zone) plus its subtask chain.
func TestGetRun(t *testing.T) {
	d := testDB(t)
	parent, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false, nil)
	_, _ = d.SetTaskRun(parent.ID, "proj", strptr("run/x"), strptr(RunStateOpen))
	_, _ = d.DispatchTask("proj", "backend", "cto", "slice-a", "", "P1", &parent.ID, nil, TypedTicket{}, false, nil)
	_, _ = d.DispatchTask("proj", "frontend", "cto", "slice-b", "", "P1", &parent.ID, nil, TypedTicket{}, false, nil)

	run, err := d.GetRun(parent.ID, "proj")
	if err != nil || run == nil {
		t.Fatalf("get run: %v / %v", run, err)
	}
	if run.RunState == nil || *run.RunState != RunStateOpen {
		t.Fatalf("run zone missing on run read: %+v", run.RunState)
	}
	if len(run.Subtasks) != 2 {
		t.Fatalf("expected 2 slices, got %d", len(run.Subtasks))
	}
}

// S2: SetTaskRun's CAS guard is the exact statement `UPDATE ... WHERE run_state
// IS <from>` — this proves it directly and deterministically: agent A reads
// run_state="open" (its "from" snapshot), then agent B independently completes
// a full SetTaskRun call that advances the row past that snapshot (simulating
// B's whole read-validate-write cycle landing inside A's read-to-write gap — a
// real window since GetTask and the guarded UPDATE are two separate statements,
// not one transaction). A's write, still keyed to the now-stale "open" snapshot,
// must then be refused (0 rows) rather than silently clobber B's result — the
// same guard clause SetTaskRun issues internally, exercised at the SQL level
// because the public API always re-reads fresh and so can never be handed a
// stale "from" to prove this from outside the package.
func TestSetTaskRunCASRefusesStaleWrite(t *testing.T) {
	d := testDB(t)
	p, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateOpen)); err != nil {
		t.Fatalf("open: %v", err)
	}

	// Agent A's read.
	curA, err := d.GetTask(p.ID, "proj")
	if err != nil || curA.RunState == nil {
		t.Fatalf("read: %v / %+v", err, curA)
	}
	fromA := *curA.RunState // "open"

	// Agent B completes an independent, legitimate SetTaskRun in between.
	if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateGating)); err != nil {
		t.Fatalf("B's set run: %v", err)
	}

	// Agent A's write, still keyed to its now-stale snapshot — same guard clause
	// SetTaskRun uses internally.
	res, err := d.conn.Exec(
		`UPDATE tasks SET run_state = ? WHERE id = ? AND project = ? AND run_state IS ?`,
		RunStateBlocked, p.ID, "proj", fromA,
	)
	if err != nil {
		t.Fatalf("A's exec: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 0 {
		t.Fatalf("stale write must be refused (0 rows affected), got %d — lost update / silent overwrite", n)
	}

	final, _ := d.GetTask(p.ID, "proj")
	if final.RunState == nil || *final.RunState != RunStateGating {
		t.Fatalf("B's committed write must survive untouched by A's stale attempt, got %v", final.RunState)
	}
}

// Same guarantee, exercised with real concurrent goroutines and the exact CAS
// clause SetTaskRun issues: N racers share ONE pre-read "from" snapshot (so the
// race is genuinely on the WRITE, not dependent on read-scheduling luck) and
// all attempt the guarded UPDATE concurrently. SQLite's single-writer
// serialization means exactly one Exec's WHERE clause can still match by the
// time it runs; every other racer sees 0 RowsAffected. Mirrors
// TestReclaimConcurrentOneWins' pattern (task_lease_test.go) for the run zone.
func TestSetTaskRunCASGuardSerializesConcurrentWriters(t *testing.T) {
	d := testDB(t)
	p, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateOpen)); err != nil {
		t.Fatalf("open: %v", err)
	}
	cur, _ := d.GetTask(p.ID, "proj")
	from := *cur.RunState // "open", shared by every racer below

	const racers = 10
	var wg sync.WaitGroup
	rowsAffected := make([]int64, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			<-start
			res, err := d.conn.Exec(
				`UPDATE tasks SET run_state = ? WHERE id = ? AND project = ? AND run_state IS ?`,
				RunStateGating, p.ID, "proj", from,
			)
			if err != nil {
				t.Errorf("racer %d exec: %v", i, err)
				return
			}
			n, _ := res.RowsAffected()
			rowsAffected[i] = n
		}()
	}
	close(start)
	wg.Wait()

	var winners int
	for _, n := range rowsAffected {
		if n > 0 {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly 1 racer to affect a row, got %d", winners)
	}
	final, _ := d.GetTask(p.ID, "proj")
	if final.RunState == nil || *final.RunState != RunStateGating {
		t.Fatalf("final run_state must be the winner's target, got %v", final.RunState)
	}
}

// review-5972890c finding: the two tests above exercise the guard clause as raw
// SQL, not through SetTaskRun itself — a revert-experiment on the production
// guard proved both still pass unmodified (720/720 racing calls landed as
// silent overwrites, zero CodeRunStateConflict). This test closes that gap: it
// hammers the PUBLIC SetTaskRun with a mix of two valid targets from the same
// "open" state (half → gating, half → blocked) across several rounds. Real
// concurrent calls (each doing its own read+write, not a shared snapshot) do
// overlap often enough at this racer count to hit the guard — a broken/removed
// guard would show zero conflicts across every round; a working one reliably
// shows at least one.
func TestSetTaskRunPublicAPIMixedTargetHammerPinsGuard(t *testing.T) {
	d := testDB(t)
	const racersPerRound = 20
	const rounds = 5

	var totalConflicts, totalWins int
	for r := 0; r < rounds; r++ {
		p, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false, nil)
		if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateOpen)); err != nil {
			t.Fatalf("round %d open: %v", r, err)
		}

		var wg sync.WaitGroup
		results := make(chan error, racersPerRound)
		start := make(chan struct{})
		for i := 0; i < racersPerRound; i++ {
			to := RunStateGating
			if i%2 == 1 {
				to = RunStateBlocked
			}
			wg.Add(1)
			go func(to string) {
				defer wg.Done()
				<-start
				_, err := d.SetTaskRun(p.ID, "proj", nil, strptr(to))
				results <- err
			}(to)
		}
		close(start)
		wg.Wait()
		close(results)

		for err := range results {
			switch {
			case err == nil:
				totalWins++
			case runErrCode(err) == CodeRunStateConflict:
				totalConflicts++
			default:
				t.Fatalf("round %d: unexpected error: %v", r, err)
			}
		}

		final, _ := d.GetTask(p.ID, "proj")
		if final.RunState == nil || (*final.RunState != RunStateGating && *final.RunState != RunStateBlocked) {
			t.Fatalf("round %d: final run_state must be a racer's target, got %v", r, final.RunState)
		}
	}

	if totalWins+totalConflicts != rounds*racersPerRound {
		t.Fatalf("every racer must resolve to exactly success or CodeRunStateConflict, got wins=%d conflicts=%d of %d", totalWins, totalConflicts, rounds*racersPerRound)
	}
	if totalConflicts == 0 {
		t.Fatal("want at least 1 CodeRunStateConflict across all rounds through the PUBLIC SetTaskRun — got 0, the guard may be broken/missing (this is exactly what an unpinned regression looks like)")
	}
	t.Logf("mixed-target hammer: %d wins, %d conflicts across %d rounds x %d racers", totalWins, totalConflicts, rounds, racersPerRound)
}
