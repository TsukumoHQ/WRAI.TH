package db

import "testing"

// (regAgent helper is shared from task_lease_test.go)

// Identity fail-closed: a second same-project agent claiming an already-bound cwd
// DISPLACES the prior holder (last live registrant wins) so the cwd stays
// unambiguous — and the displaced name is returned so the caller can flag it.
func TestClaimCwdDisplacesAndFlags(t *testing.T) {
	d := testDB(t)
	cwd := "/wt/shared"
	regAgent(t, d, "p1", "foo")
	regAgent(t, d, "p1", "foo-2")

	if disp, err := d.ClaimCwd("p1", "foo", cwd); err != nil || len(disp) != 0 {
		t.Fatalf("first claim should displace nobody, got %v err %v", disp, err)
	}
	// foo-2 (the live respawn) claims the same cwd → foo displaced.
	disp, err := d.ClaimCwd("p1", "foo-2", cwd)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(disp) != 1 || disp[0] != "foo" {
		t.Fatalf("expected foo displaced, got %v", disp)
	}

	// cwd is now unambiguous → RebindSession by cwd alone resolves to foo-2.
	proj, name, found, ambiguous, err := d.RebindSession(cwd, "", "sess-x")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if ambiguous || !found || name != "foo-2" || proj != "p1" {
		t.Errorf("cwd should resolve uniquely to foo-2, got name=%q found=%v ambiguous=%v", name, found, ambiguous)
	}
}

// identity_check verdicts: unique bound, ghost (displaced/no-cwd), and unregistered.
func TestIdentityCheckVerdicts(t *testing.T) {
	d := testDB(t)
	cwd := "/wt/one"
	regAgent(t, d, "p1", "alpha")
	regAgent(t, d, "p1", "beta")
	if _, err := d.ClaimCwd("p1", "alpha", cwd); err != nil {
		t.Fatalf("claim alpha: %v", err)
	}

	// alpha: uniquely bound.
	v, _ := d.IdentityCheck("p1", "alpha")
	if !v.Registered || v.Ghost || !v.BoundUniquely || len(v.ConflictingAgents) != 0 {
		t.Errorf("alpha should be uniquely bound, got %+v", v)
	}

	// beta: registered but no cwd → ghost (not locally wake-resolvable).
	v, _ = d.IdentityCheck("p1", "beta")
	if !v.Registered || !v.Ghost || v.BoundUniquely {
		t.Errorf("beta should be a registered ghost (no cwd), got %+v", v)
	}

	// beta steals the cwd → alpha becomes a ghost (cwd cleared).
	if _, err := d.ClaimCwd("p1", "beta", cwd); err != nil {
		t.Fatalf("claim beta: %v", err)
	}
	v, _ = d.IdentityCheck("p1", "alpha")
	if !v.Ghost {
		t.Errorf("alpha should be a ghost after being displaced, got %+v", v)
	}

	// unknown name: unregistered ghost.
	v, _ = d.IdentityCheck("p1", "nobody")
	if v.Registered || !v.Ghost {
		t.Errorf("unknown name should be an unregistered ghost, got %+v", v)
	}
}
