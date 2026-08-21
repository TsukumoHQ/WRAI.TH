package db

import "testing"

// (regAgent helper is shared from task_lease_test.go)

// N agents can legitimately share one cwd (teams.json co-locates a lead +
// worker on the same worktree by design): claiming an already-bound cwd must
// NOT evict the prior holder — it only records the caller's own binding and
// reports the cohabitant, and the cohabitant's own binding stays intact.
func TestClaimCwdSharesWithoutDisplacing(t *testing.T) {
	d := testDB(t)
	cwd := "/wt/shared"
	regAgent(t, d, "p1", "lead")
	regAgent(t, d, "p1", "worker")

	if coh, err := d.ClaimCwd("p1", "lead", cwd); err != nil || len(coh) != 0 {
		t.Fatalf("first claim should report no cohabitants, got %v err %v", coh, err)
	}
	coh, err := d.ClaimCwd("p1", "worker", cwd)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(coh) != 1 || coh[0] != "lead" {
		t.Fatalf("expected lead reported as cohabitant, got %v", coh)
	}

	// lead's own binding must still be intact — not cleared by worker's claim.
	proj, name, found, ambiguous, err := d.RebindSession(cwd, "lead", "sess-lead")
	if err != nil {
		t.Fatalf("rebind lead: %v", err)
	}
	if ambiguous || !found || name != "lead" || proj != "p1" {
		t.Errorf("lead should still resolve by name+cwd, got name=%q found=%v ambiguous=%v", name, found, ambiguous)
	}
	proj, name, found, ambiguous, err = d.RebindSession(cwd, "worker", "sess-worker")
	if err != nil {
		t.Fatalf("rebind worker: %v", err)
	}
	if ambiguous || !found || name != "worker" || proj != "p1" {
		t.Errorf("worker should resolve by name+cwd, got name=%q found=%v ambiguous=%v", name, found, ambiguous)
	}
}

// Regression guard (no signal beyond a shared cwd): RebindSession's cwd-only
// fallback must still refuse to guess between N live cohabitants rather than
// binding the wrong one — a wrong identity is worse than none.
func TestRebindSessionCwdOnlyStillRefusesAmbiguous(t *testing.T) {
	d := testDB(t)
	cwd := "/wt/shared"
	regAgent(t, d, "p1", "lead")
	regAgent(t, d, "p1", "worker")
	if _, err := d.ClaimCwd("p1", "lead", cwd); err != nil {
		t.Fatalf("claim lead: %v", err)
	}
	if _, err := d.ClaimCwd("p1", "worker", cwd); err != nil {
		t.Fatalf("claim worker: %v", err)
	}

	_, _, found, ambiguous, err := d.RebindSession(cwd, "", "sess-x")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if !ambiguous || found {
		t.Errorf("cwd-only rebind with 2 cohabitants should refuse (ambiguous=true, found=false), got found=%v ambiguous=%v", found, ambiguous)
	}
}

// identity_check verdicts: unique bound (no cohabitants), bound with
// cohabitants (still uniquely wake-resolvable by name), ghost (no cwd), and
// unregistered.
func TestIdentityCheckVerdicts(t *testing.T) {
	d := testDB(t)
	cwd := "/wt/one"
	regAgent(t, d, "p1", "alpha")
	regAgent(t, d, "p1", "beta")
	if _, err := d.ClaimCwd("p1", "alpha", cwd); err != nil {
		t.Fatalf("claim alpha: %v", err)
	}

	// alpha: uniquely bound, no cohabitants yet.
	v, _ := d.IdentityCheck("p1", "alpha")
	if !v.Registered || v.Ghost || !v.BoundUniquely || len(v.ConflictingAgents) != 0 {
		t.Errorf("alpha should be uniquely bound with no cohabitants, got %+v", v)
	}

	// beta: registered but no cwd → ghost (not locally wake-resolvable).
	v, _ = d.IdentityCheck("p1", "beta")
	if !v.Registered || !v.Ghost || v.BoundUniquely {
		t.Errorf("beta should be a registered ghost (no cwd), got %+v", v)
	}

	// beta joins the same cwd as a teammate → BOTH stay bound_uniquely=true
	// (resolvable by name); cohabitants is informational only, no ghosting.
	if _, err := d.ClaimCwd("p1", "beta", cwd); err != nil {
		t.Fatalf("claim beta: %v", err)
	}
	v, _ = d.IdentityCheck("p1", "alpha")
	if v.Ghost || !v.BoundUniquely || len(v.ConflictingAgents) != 1 || v.ConflictingAgents[0] != "beta" {
		t.Errorf("alpha should stay bound_uniquely after beta joins its cwd, got %+v", v)
	}
	v, _ = d.IdentityCheck("p1", "beta")
	if v.Ghost || !v.BoundUniquely || len(v.ConflictingAgents) != 1 || v.ConflictingAgents[0] != "alpha" {
		t.Errorf("beta should be bound_uniquely as alpha's cwd cohabitant, got %+v", v)
	}

	// unknown name: unregistered ghost.
	v, _ = d.IdentityCheck("p1", "nobody")
	if v.Registered || !v.Ghost {
		t.Errorf("unknown name should be an unregistered ghost, got %+v", v)
	}
}
