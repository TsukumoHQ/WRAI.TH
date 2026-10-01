package db

import (
	"testing"
	"time"
)

// W8 D1 (ruling wraith-deploying-ruling, docs/design/deploying-status.md):
// 'deploying' sits between in-review and done for pipeline repos. The doer
// keeps the lease while the post-merge pipeline (niwa, an override actor)
// deploys and verifies; the sweeper never reclaims it, no ACK opens on it, and
// a retried deploy out of blocked must carry the same merge sha.

const shaA = "4d01274a056a"

// deployingTask claims + starts + reviews a task as dev-a and returns its id.
func deployingTask(t *testing.T, d *DB) string {
	t.Helper()
	id := edgeTask(t, d, TypedTicket{})
	if _, err := d.ClaimTask(id, "dev-a", "p1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := d.StartTask(id, "dev-a", "p1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := d.ReviewTask(id, "dev-a", "p1"); err != nil {
		t.Fatalf("review: %v", err)
	}
	return id
}

func assertDoer(t *testing.T, d *DB, id, status, holder string) {
	t.Helper()
	got, err := d.GetTask(id, "p1")
	if err != nil || got == nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != status {
		t.Fatalf("status = %q, want %q", got.Status, status)
	}
	if strVal(got.LeaseHolder) != holder {
		t.Fatalf("lease_holder = %q, want %q", strVal(got.LeaseHolder), holder)
	}
	if strVal(got.AssignedTo) != "dev-a" {
		t.Fatalf("assigned_to = %q, want dev-a (the doer)", strVal(got.AssignedTo))
	}
}

// AC1: in-review -> deploying -> done; deploying -> blocked; deploying ->
// in-review; in-progress -> deploying refused.
func TestDeploying_Transitions(t *testing.T) {
	d := testDB(t)

	early := edgeTask(t, d, TypedTicket{})
	if _, err := d.ClaimTask(early, "dev-a", "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.StartTask(early, "dev-a", "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeployTask(early, "niwa", "p1", shaA); err == nil {
		t.Fatal("in-progress -> deploying accepted, want refused (deploying only follows in-review)")
	}
	if _, err := d.ReviewTask(early, "dev-a", "p1"); err != nil { // frees dev-a's WIP slot
		t.Fatal(err)
	}

	done := deployingTask(t, d)
	if _, err := d.DeployTask(done, "niwa", "p1", shaA); err != nil {
		t.Fatalf("in-review -> deploying: %v", err)
	}
	assertDoer(t, d, done, "deploying", "dev-a")
	if _, err := d.CompleteTask(done, "niwa", "p1", nil); err != nil {
		t.Fatalf("deploying -> done: %v", err)
	}
	assertDoer(t, d, done, "done", "")

	blocked := deployingTask(t, d)
	if _, err := d.DeployTask(blocked, "niwa", "p1", shaA); err != nil {
		t.Fatal(err)
	}
	reason := "verify_failed: /healthz 502"
	if _, err := d.BlockTask(blocked, "niwa", "p1", &reason); err != nil {
		t.Fatalf("deploying -> blocked: %v", err)
	}
	assertDoer(t, d, blocked, "blocked", "")

	back := deployingTask(t, d)
	if _, err := d.DeployTask(back, "niwa", "p1", shaA); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReviewTask(back, "niwa", "p1"); err != nil {
		t.Fatalf("deploying -> in-review: %v", err)
	}
	assertDoer(t, d, back, "in-review", "dev-a")

	if _, err := d.DeployTask(deployingTask(t, d), "niwa", "p1", ""); err == nil {
		t.Fatal("deploy without a merge sha accepted, want refused")
	}
	if _, err := d.DeployTask(deployingTask(t, d), "dev-b", "p1", shaA); err == nil {
		t.Fatal("deploy by a non-holder, non-override agent accepted, want refused (fenced)")
	}
}

// AC2: blocked -> deploying carries the merge sha and is refused when it
// differs from the sha that entered deploying (or when the task never deployed).
func TestDeploying_BlockedRetrySameSHA(t *testing.T) {
	d := testDB(t)
	id := deployingTask(t, d)
	if _, err := d.DeployTask(id, "niwa", "p1", shaA); err != nil {
		t.Fatal(err)
	}
	reason := "deploy_failed"
	if _, err := d.BlockTask(id, "niwa", "p1", &reason); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeployTask(id, "niwa", "p1", "ffffffffffff"); err == nil {
		t.Fatal("blocked -> deploying with a different sha accepted, want refused")
	}
	if _, err := d.DeployTask(id, "niwa", "p1", ""); err == nil {
		t.Fatal("blocked -> deploying without a sha accepted, want refused")
	}
	if _, err := d.DeployTask(id, "niwa", "p1", shaA); err != nil {
		t.Fatalf("blocked -> deploying with the same sha: %v", err)
	}
	assertDoer(t, d, id, "deploying", "dev-a")

	never := edgeTask(t, d, TypedTicket{})
	if _, err := d.ClaimTask(never, "dev-a", "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BlockTask(never, "dev-a", "p1", &reason); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeployTask(never, "niwa", "p1", shaA); err == nil {
		t.Fatal("blocked -> deploying on a task that never deployed accepted, want refused")
	}
}

// AC3: lease_holder / assigned_to stay the doer through deploying; the lease
// sweeper never reclaims a deploying task; released at done/blocked (above).
func TestDeploying_LeaseKeptNeverSwept(t *testing.T) {
	d := testDB(t)
	id := deployingTask(t, d)
	if _, err := d.DeployTask(id, "niwa", "p1", shaA); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(`UPDATE tasks SET lease_expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatal(err)
	}
	swept, err := d.SweepExpiredLeases()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, s := range swept {
		if s.TaskID == id {
			t.Fatalf("sweeper reclaimed deploying task %s", id)
		}
	}
	assertDoer(t, d, id, "deploying", "dev-a")
}

// AC4: a dependent with an @in-review edge stays ready while its prerequisite
// is deploying; no ACK obligation opens on a deploying task.
func TestDeploying_EdgeReadyNoAck(t *testing.T) {
	d := testDB(t)
	prereq := deployingTask(t, d)
	dep := edgeTask(t, d, TypedTicket{BlockedBy: []string{prereq + "@in-review"}})
	if _, err := d.DeployTask(prereq, "niwa", "p1", shaA); err != nil {
		t.Fatal(err)
	}
	if ready, refs, err := d.TaskReadiness("p1", dep); err != nil || !ready || len(refs) != 0 {
		t.Fatalf("dependent ready=%v blockers=%v err=%v, want ready with its prerequisite deploying", ready, refs, err)
	}

	now := time.Now().UTC()
	if _, err := d.InstantiateTaskAck(now.Add(time.Hour), now); err != nil {
		t.Fatalf("instantiate ack: %v", err)
	}
	count := func(id string) int {
		var n int
		if err := d.conn.QueryRow(`SELECT COUNT(*) FROM obligations WHERE subject_kind = ? AND subject_id = ?`, SubjectTask, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(prereq); n != 0 {
		t.Fatalf("ACK obligations on the deploying task = %d, want 0", n)
	}
	if n := count(dep); n == 0 {
		t.Fatal("ready @in-review dependent got no ACK obligation: deploying prerequisite still treated as unsatisfied")
	}
}
