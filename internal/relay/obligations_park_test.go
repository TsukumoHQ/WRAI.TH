package relay

import (
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Park P2 (ruling wraith-park-ruling): once work is parked or demoted, nobody
// is paged about it again, and a promote / unpark restarts the ACK clock.

// openAck counts the task's ACK obligations still active.
func (f *oblFixture) openAck(t *testing.T, task string) int {
	t.Helper()
	var n int
	if err := f.raw.QueryRow(`SELECT COUNT(*) FROM obligations WHERE subject_id = ? AND state = ? AND norm_id LIKE 'ack.%'`,
		task, db.ObligationActive).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *oblFixture) ackRows(t *testing.T, task string) int {
	t.Helper()
	var n int
	if err := f.raw.QueryRow(`SELECT COUNT(*) FROM obligations WHERE subject_id = ? AND norm_id LIKE 'ack.%'`, task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// activeAckIDs lists the task's open (active) ACK obligation ids.
func (f *oblFixture) activeAckIDs(t *testing.T, task string) []string {
	t.Helper()
	rows, err := f.raw.Query(`SELECT id FROM obligations WHERE subject_id = ? AND state = ? AND norm_id LIKE 'ack.%'`, task, db.ObligationActive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		t.Fatalf("precondition: %s has no active ACK obligation", task)
	}
	return ids
}

func TestParkClosesOpenAckInactive(t *testing.T) {
	f := newOblFixture(t)
	ids := f.activeAckIDs(t, "pool")
	if _, err := f.h.db.ParkTask("p1", "pool", "cto", "waiting on founder", db.ParkUntilFounder, ""); err != nil {
		t.Fatal(err)
	}
	// Same tx as the park: closed before any sweeper run.
	for _, id := range ids {
		if st, _ := f.state(t, id); st != db.ObligationInactive {
			t.Errorf("after park, ACK obligation %s state %q, want inactive", id, st)
		}
	}
}

func TestDemoteClosesOpenAckInactive(t *testing.T) {
	f := newOblFixture(t)
	ids := f.activeAckIDs(t, "pool")
	f.exec(t, `UPDATE tasks SET status = 'backlog' WHERE id = 'pool'`)
	evaluateObligations(f.h.db, f.h.registry, time.Now().UTC())
	for _, id := range ids {
		if st, _ := f.state(t, id); st != db.ObligationInactive {
			t.Errorf("after demote to backlog, ACK obligation %s state %q, want inactive (not fulfilled)", id, st)
		}
	}
}

func TestNoAckRungOpensOnParkedOrBacklog(t *testing.T) {
	f := newOblFixture(t)
	if _, err := f.h.db.ParkTask("p1", "pool", "cto", "waiting", db.ParkUntilFounder, ""); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE tasks SET status = 'backlog' WHERE id = 'mine'`)
	before := map[string]int{"pool": f.ackRows(t, "pool"), "mine": f.ackRows(t, "mine")}
	for _, h := range []time.Duration{time.Hour, 6 * time.Hour, 48 * time.Hour} {
		evaluateObligations(f.h.db, f.h.registry, time.Now().UTC().Add(h))
	}
	for task, n := range before {
		if got := f.ackRows(t, task); got != n {
			t.Errorf("%s: %d ACK rows after sweeps past every window, want %d (none opened)", task, got, n)
		}
		if open := f.openAck(t, task); open != 0 {
			t.Errorf("%s: %d ACK obligation(s) active, want 0", task, open)
		}
	}
}

func TestPromoteAndUnparkRestartAckClock(t *testing.T) {
	f := newOblFixture(t)
	pendingSince := func(task string) time.Time {
		t.Helper()
		var s string
		if err := f.raw.QueryRow(`SELECT COALESCE(pending_since, dispatched_at) FROM tasks WHERE id = ?`, task).Scan(&s); err != nil {
			t.Fatal(err)
		}
		ts, err := time.Parse("2006-01-02T15:04:05.999999Z", s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return ts
	}

	if _, err := f.h.db.ParkTask("p1", "pool", "cto", "waiting", db.ParkUntilFounder, ""); err != nil {
		t.Fatal(err)
	}
	unparkAt := time.Now().UTC().Add(-time.Second)
	if _, err := f.h.db.UnparkTask("p1", "pool", "cto"); err != nil {
		t.Fatal(err)
	}
	if ps := pendingSince("pool"); ps.Before(unparkAt) {
		t.Errorf("unpark: ACK clock %s, want restarted at/after %s (not dispatched_at)", ps, unparkAt)
	}

	f.exec(t, `UPDATE tasks SET status = 'backlog' WHERE id = 'mine'`)
	promoteAt := time.Now().UTC().Add(-time.Second)
	if _, _, err := f.h.db.PromoteTask("mine", "cto", "p1"); err != nil {
		t.Fatal(err)
	}
	if ps := pendingSince("mine"); ps.Before(promoteAt) {
		t.Errorf("promote: ACK clock %s, want restarted at/after %s (not dispatched_at)", ps, promoteAt)
	}
}
