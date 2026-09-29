package db

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// regAgent registers an active agent so agentLive() sees a live holder. Tests
// that want a DEAD holder register then DeactivateAgent (status inactive).
func regAgent(t *testing.T, d *DB, project, name string) {
	t.Helper()
	if _, _, err := d.RegisterAgent(project, name, "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

// dispatchClaimed dispatches a task and claims it under holder, returning the
// task id. The holder is registered active.
func dispatchClaimed(t *testing.T, d *DB, project, holder string) string {
	t.Helper()
	regAgent(t, d, project, holder)
	task, err := d.DispatchTask(project, "", "dispatcher", "lease me", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := d.ClaimTask(task.ID, holder, project); err != nil {
		t.Fatalf("claim by %s: %v", holder, err)
	}
	return task.ID
}

// forceLeaseExpired backdates lease_expires_at so the lease reads as lapsed.
func forceLeaseExpired(t *testing.T, d *DB, taskID, project string) {
	t.Helper()
	past := time.Now().UTC().Add(-1 * time.Hour).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(
		"UPDATE tasks SET lease_expires_at = ? WHERE id = ? AND project = ?", past, taskID, project,
	); err != nil {
		t.Fatalf("force expire: %v", err)
	}
}

// TestClaimSetsLease: a normal claim establishes a live lease on the holder with
// a fresh heartbeat and an expiry in the future.
func TestClaimSetsLease(t *testing.T) {
	d := testDB(t)
	const project, holder = "p1", "worker-a"
	id := dispatchClaimed(t, d, project, holder)

	task, err := d.GetTask(id, project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.LeaseHolder == nil || *task.LeaseHolder != holder {
		t.Fatalf("want lease_holder %q, got %v", holder, task.LeaseHolder)
	}
	if task.LeaseExpiresAt == nil || *task.LeaseExpiresAt <= time.Now().UTC().Format(memoryTimeFmt) {
		t.Fatalf("want future lease_expires_at, got %v", task.LeaseExpiresAt)
	}
	if task.LeaseHeartbeatAt == nil || *task.LeaseHeartbeatAt == "" {
		t.Fatalf("want lease_heartbeat_at stamped, got %v", task.LeaseHeartbeatAt)
	}
}

// TestReclaimExpiredLease: once the lease has lapsed, another agent may take the
// task over; the transfer reason is "expired".
func TestReclaimExpiredLease(t *testing.T) {
	d := testDB(t)
	const project, holder, taker = "p1", "worker-a", "worker-b"
	id := dispatchClaimed(t, d, project, holder)
	regAgent(t, d, project, taker)
	forceLeaseExpired(t, d, id, project)

	task, err := d.ReclaimTask(id, taker, project)
	if err != nil {
		t.Fatalf("reclaim expired: %v", err)
	}
	if task.Status != "accepted" {
		t.Fatalf("want status accepted, got %q", task.Status)
	}
	if task.LeaseHolder == nil || *task.LeaseHolder != taker {
		t.Fatalf("want holder %q, got %v", taker, task.LeaseHolder)
	}
	if task.LeaseTransfer == nil || task.LeaseTransfer.Reason != "expired" {
		t.Fatalf("want transfer reason expired, got %+v", task.LeaseTransfer)
	}
	if task.LeaseTransfer.From != holder || task.LeaseTransfer.To != taker {
		t.Fatalf("want transfer %s→%s, got %+v", holder, taker, task.LeaseTransfer)
	}
	// The persisted expiry is fresh (in the future) again.
	if task.LeaseExpiresAt == nil || *task.LeaseExpiresAt <= time.Now().UTC().Format(memoryTimeFmt) {
		t.Fatalf("want fresh future expiry after reclaim, got %v", task.LeaseExpiresAt)
	}
}

// TestReclaimLiveLeaseRefused: a live holder's task refuses a re-claim with the
// typed TASK_LEASE_HELD code so the caller parks instead of stealing it.
func TestReclaimLiveLeaseRefused(t *testing.T) {
	d := testDB(t)
	const project, holder, taker = "p1", "worker-a", "worker-b"
	id := dispatchClaimed(t, d, project, holder)
	regAgent(t, d, project, taker)

	_, err := d.ReclaimTask(id, taker, project)
	if err == nil {
		t.Fatal("want TASK_LEASE_HELD, got nil (live lease was stolen)")
	}
	te, ok := err.(*TaskError)
	if !ok || te.Code != CodeTaskLeaseHeld {
		t.Fatalf("want typed %s, got %v", CodeTaskLeaseHeld, err)
	}
	// The task is untouched — holder unchanged.
	task, _ := d.GetTask(id, project)
	if task.LeaseHolder == nil || *task.LeaseHolder != holder {
		t.Fatalf("holder must stay %q after refused reclaim, got %v", holder, task.LeaseHolder)
	}
}

// TestReclaimDeregisteredHolder: even with an unexpired lease, a DEAD (inactive)
// holder's task is re-claimable; the transfer reason is "deregistered".
func TestReclaimDeregisteredHolder(t *testing.T) {
	d := testDB(t)
	const project, holder, taker = "p1", "worker-a", "worker-b"
	id := dispatchClaimed(t, d, project, holder)
	regAgent(t, d, project, taker)
	if err := d.DeactivateAgent(project, holder); err != nil {
		t.Fatalf("deactivate holder: %v", err)
	}

	task, err := d.ReclaimTask(id, taker, project)
	if err != nil {
		t.Fatalf("reclaim deregistered: %v", err)
	}
	if task.LeaseHolder == nil || *task.LeaseHolder != taker {
		t.Fatalf("want holder %q, got %v", taker, task.LeaseHolder)
	}
	if task.LeaseTransfer == nil || task.LeaseTransfer.Reason != "deregistered" {
		t.Fatalf("want transfer reason deregistered, got %+v", task.LeaseTransfer)
	}
}

// TestReclaimConcurrentOneWins: N agents race to reclaim one dead-holder task;
// exactly one wins and the losers get a typed conflict/held error — never two
// winners (the reclaim CAS holds).
func TestReclaimConcurrentOneWins(t *testing.T) {
	d := testDB(t)
	const project, holder = "p1", "worker-dead"
	id := dispatchClaimed(t, d, project, holder)
	forceLeaseExpired(t, d, id, project)

	const racers = 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		losers  int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		taker := "taker-" + string(rune('a'+i))
		regAgent(t, d, project, taker)
		go func() {
			defer wg.Done()
			<-start
			got, err := d.ReclaimTask(id, taker, project)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && got != nil {
				winners = append(winners, taker)
			} else {
				losers++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("want exactly 1 reclaim winner, got %d (%v)", len(winners), winners)
	}
	if losers != racers-1 {
		t.Fatalf("want %d losers, got %d", racers-1, losers)
	}
	final, _ := d.GetTask(id, project)
	if final.LeaseHolder == nil || *final.LeaseHolder != winners[0] {
		t.Fatalf("persisted holder must be the winner %q, got %v", winners[0], final.LeaseHolder)
	}
}

// TestLeaseReleasedOnComplete: complete/block/cancel clear the lease (holder,
// expiry, heartbeat all NULL) and stamp a voluntary transfer.
func TestLeaseReleasedOnComplete(t *testing.T) {
	d := testDB(t)
	const project, holder = "p1", "worker-a"
	id := dispatchClaimed(t, d, project, holder)
	if _, err := d.StartTask(id, holder, project); err != nil {
		t.Fatalf("start: %v", err)
	}

	task, err := d.CompleteTask(id, holder, project, nil)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if task.LeaseHolder != nil || task.LeaseExpiresAt != nil || task.LeaseHeartbeatAt != nil {
		t.Fatalf("lease must be released on complete, got holder=%v exp=%v hb=%v",
			task.LeaseHolder, task.LeaseExpiresAt, task.LeaseHeartbeatAt)
	}
	if task.LeaseTransfer == nil || task.LeaseTransfer.Reason != "voluntary" || task.LeaseTransfer.From != holder {
		t.Fatalf("want voluntary release transfer from %s, got %+v", holder, task.LeaseTransfer)
	}
	// Persisted row agrees.
	fresh, _ := d.GetTask(id, project)
	if fresh.LeaseHolder != nil {
		t.Fatalf("persisted lease_holder must be NULL after complete, got %v", fresh.LeaseHolder)
	}
}

// TestReclaimNonReclaimableStateRefused: reclaim must not resurrect a terminal
// task nor steal a pending one — only held-work states (accepted/in-progress/
// in-review) are reclaimable. Both refusals are typed TASK_STATE_CONFLICT.
func TestReclaimNonReclaimableStateRefused(t *testing.T) {
	d := testDB(t)
	const project, taker = "p1", "taker"
	regAgent(t, d, project, taker)

	// (a) a pending task — has no holder; claim_task is the right call.
	pend, err := d.DispatchTask(project, "", "dispatcher", "pending", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch pending: %v", err)
	}
	if _, err := d.ReclaimTask(pend.ID, taker, project); err == nil {
		t.Fatal("reclaim of a pending task must be refused")
	} else if te, ok := err.(*TaskError); !ok || te.Code != CodeTaskStateConflict {
		t.Fatalf("want typed %s for pending reclaim, got %v", CodeTaskStateConflict, err)
	}

	// (b) a completed task — must never be resurrected to accepted.
	const holder = "worker-done"
	doneID := dispatchClaimed(t, d, project, holder)
	if _, err := d.StartTask(doneID, holder, project); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := d.CompleteTask(doneID, holder, project, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := d.ReclaimTask(doneID, taker, project); err == nil {
		t.Fatal("reclaim of a done task must be refused (no resurrection)")
	} else if te, ok := err.(*TaskError); !ok || te.Code != CodeTaskStateConflict {
		t.Fatalf("want typed %s for done reclaim, got %v", CodeTaskStateConflict, err)
	}
	final, _ := d.GetTask(doneID, project)
	if final.Status != "done" {
		t.Fatalf("done task must stay done after refused reclaim, got %q", final.Status)
	}
}

// TestDoubleClaimTypedConflict: the losers of a double-claim get the typed
// TASK_STATE_CONFLICT code (not a bare string), so a client can distinguish a
// lost race from a transport error.
func TestDoubleClaimTypedConflict(t *testing.T) {
	d := testDB(t)
	const project = "p1"
	task, err := d.DispatchTask(project, "", "dispatcher", "race", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := d.ClaimTask(task.ID, "first", project); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// A second claim on the now-accepted task loses the CAS.
	_, err = d.ClaimTask(task.ID, "second", project)
	if err == nil {
		t.Fatal("want a conflict on second claim, got nil")
	}
	te, ok := err.(*TaskError)
	if !ok || te.Code != CodeTaskStateConflict {
		t.Fatalf("want typed %s, got %v", CodeTaskStateConflict, err)
	}
}

// releaseState reads the columns a release must clear, straight from the row.
func releaseState(t *testing.T, d *DB, id string) (status, claimedBy, assignedTo, holder, pendingSince string) {
	t.Helper()
	if err := d.conn.QueryRow(`SELECT status, COALESCE(claimed_by,''), COALESCE(assigned_to,''),
		COALESCE(lease_holder,''), COALESCE(pending_since,'') FROM tasks WHERE id = ?`, id).
		Scan(&status, &claimedBy, &assignedTo, &holder, &pendingSince); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return
}

func releaseAudits(t *testing.T, d *DB, id, actor string) int {
	t.Helper()
	var n int
	if err := d.conn.QueryRow(`SELECT count(*) FROM audit_log WHERE resource_id = ? AND action = 'lease_transferred'
		AND reason = 'released' AND actor = ?`, id, actor).Scan(&n); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	return n
}

// TestReleaseTask covers claim_task release=true at the db layer: holder or
// dispatcher only, accepted only, everything cleared and audited, CAS-safe.
func TestReleaseTask(t *testing.T) {
	const project, holder = "p1", "worker-a"

	t.Run("ReleaseByHolderReturnsToPending", func(t *testing.T) {
		d := testDB(t)
		id := dispatchClaimed(t, d, project, holder)
		before := time.Now().UTC().Format(memoryTimeFmt)
		got, err := d.ReleaseTask(id, holder, project)
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		status, claimedBy, assignedTo, lease, pendingSince := releaseState(t, d, id)
		if status != "pending" || claimedBy != "" || assignedTo != "" || lease != "" {
			t.Fatalf("status=%s claimed_by=%q assigned_to=%q lease=%q, want pending and all cleared", status, claimedBy, assignedTo, lease)
		}
		if pendingSince < before {
			t.Errorf("pending_since=%s, want reset to >= %s (ACK clock restarts)", pendingSince, before)
		}
		if got.LeaseTransfer == nil || got.LeaseTransfer.From != holder || got.LeaseTransfer.Reason != "released" {
			t.Errorf("LeaseTransfer = %+v, want from %s reason released", got.LeaseTransfer, holder)
		}
		if n := releaseAudits(t, d, id, holder); n != 1 {
			t.Errorf("release audit rows naming %s = %d, want 1", holder, n)
		}
	})

	t.Run("ReleaseByDispatcher", func(t *testing.T) {
		d := testDB(t)
		id := dispatchClaimed(t, d, project, holder)
		if _, err := d.ReleaseTask(id, "dispatcher", project); err != nil {
			t.Fatalf("release by dispatcher: %v", err)
		}
		if status, _, _, _, _ := releaseState(t, d, id); status != "pending" {
			t.Fatalf("status=%s, want pending", status)
		}
		if n := releaseAudits(t, d, id, "dispatcher"); n != 1 {
			t.Errorf("release audit rows naming dispatcher = %d, want 1", n)
		}
	})

	t.Run("ReleaseByOtherRefused", func(t *testing.T) {
		d := testDB(t)
		id := dispatchClaimed(t, d, project, holder)
		_, err := d.ReleaseTask(id, "bystander", project)
		if !errors.Is(err, ErrTaskReleaseForbidden) {
			t.Fatalf("err = %v, want ErrTaskReleaseForbidden", err)
		}
		status, claimedBy, _, lease, _ := releaseState(t, d, id)
		if status != "accepted" || claimedBy != holder || lease != holder {
			t.Fatalf("status=%s claimed_by=%q lease=%q, want accepted/%s/%s unchanged", status, claimedBy, lease, holder, holder)
		}
		if n := releaseAudits(t, d, id, "bystander"); n != 0 {
			t.Errorf("audit rows = %d, want 0", n)
		}
	})

	t.Run("ReleaseRefusedUnlessAccepted", func(t *testing.T) {
		d := testDB(t)
		unlimitedWIP(t, d, project) // the holder takes one task per status
		started := dispatchClaimed(t, d, project, holder)
		if _, err := d.StartTask(started, holder, project); err != nil {
			t.Fatalf("start: %v", err)
		}
		done := dispatchClaimed(t, d, project, holder)
		if _, err := d.CompleteTask(done, holder, project, nil); err != nil {
			t.Fatalf("complete: %v", err)
		}
		pending, err := d.DispatchTask(project, "", "dispatcher", "never claimed", "", "P2", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		for want, id := range map[string]string{"in-progress": started, "done": done, "pending": pending.ID} {
			before, cb, _, lease, ps := releaseState(t, d, id)
			_, err := d.ReleaseTask(id, "dispatcher", project)
			var te *TaskError
			if !errors.As(err, &te) || te.Code != CodeTaskStateConflict {
				t.Errorf("%s: err = %v, want TASK_STATE_CONFLICT", want, err)
			}
			after, cb2, _, lease2, ps2 := releaseState(t, d, id)
			if before != want || after != want || cb != cb2 || lease != lease2 || ps != ps2 {
				t.Errorf("%s: row changed by a refused release (%s/%s/%s -> %s/%s/%s)", want, before, cb, lease, after, cb2, lease2)
			}
		}
	})

	t.Run("ReleaseStartRace", func(t *testing.T) {
		d := testDB(t)
		unlimitedWIP(t, d, project) // several race rounds, one holder
		for i := 0; i < 25; i++ {
			id := dispatchClaimed(t, d, project, holder)
			var wg sync.WaitGroup
			var relErr, startErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() { defer wg.Done(); <-start; _, relErr = d.ReleaseTask(id, "dispatcher", project) }()
			go func() { defer wg.Done(); <-start; _, startErr = d.StartTask(id, holder, project) }()
			close(start)
			wg.Wait()
			status, claimedBy, assignedTo, lease, _ := releaseState(t, d, id)
			switch {
			case relErr != nil && startErr != nil:
				t.Fatalf("iter %d: both lost (release %v, start %v)", i, relErr, startErr)
			case relErr == nil && startErr != nil:
				// Release won: nothing of the claim survives.
				if status != "pending" || claimedBy != "" || assignedTo != "" || lease != "" {
					t.Fatalf("iter %d: release won but row is %s/%q/%q/%q", i, status, claimedBy, assignedTo, lease)
				}
			default:
				// Start won the CAS (release refused), or ran after a completed
				// release as a fresh implicit claim from pending: either way the
				// row is a coherent in-progress task held by the starter.
				if status != "in-progress" || assignedTo != holder || lease != holder {
					t.Fatalf("iter %d: start succeeded but row is %s/%q/%q (release err %v)", i, status, assignedTo, lease, relErr)
				}
			}
		}
	})
}
