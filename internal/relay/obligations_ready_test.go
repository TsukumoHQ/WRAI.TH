package relay

import (
	"strings"
	"testing"
	"time"
)

// Task 887351ac: ACK escalations point at real stalls, not at tickets
// correctly waiting their turn.

// blockedBy adds a live blocked_by edge src -> dst WITHOUT opening a hold —
// the state a task reaches when the edge was added while it sat in backlog and
// it was promoted later (field case 19a1986c / 2c901373: no task_holds row).
func blockedBy(src, dst string) step {
	return setSQL(`INSERT INTO org_edges (id, project, src_kind, src_id, type, dst_kind, dst_id, created_by, created_at)
		VALUES (?, 'p1', 'task', ?, 'blocked_by', 'task', ?, 'cto', ?)`, "e-"+src+"-"+dst, src, dst, ago(time.Hour))
}

func run(t *testing.T, w *twin, steps ...step) {
	t.Helper()
	for _, s := range steps {
		s(t, w)
	}
}

// AC1: a task whose blocked_by prerequisite is open gets no no-ACK alert, even
// with no hold row; once the prerequisite is done the clock starts from that
// moment, not from when the task entered pending.
func TestAckNoAlertWhileBlockedByUnmet(t *testing.T) {
	w := newTwin(t, "unmet")
	run(t, w,
		seedTask("pre", "in-progress", ago(3*time.Hour)),
		seedTask("t1", "pending", ago(2*time.Hour)),
		blockedBy("t1", "pre"))
	tick(w)
	if msgs := chainMessages(t, w); len(msgs) != 0 {
		t.Fatalf("unmet blocked_by: want no ACK alert, got %+v", msgs)
	}

	// Prerequisite just finished: ready now, the ACK clock starts now.
	run(t, w, setSQL(`UPDATE tasks SET status = 'done', done_at = ?, completed_at = ? WHERE id = 'pre'`, ago(time.Minute), ago(time.Minute)))
	tick(w)
	if msgs := chainMessages(t, w); len(msgs) != 0 {
		t.Fatalf("just became ready: want no ACK alert yet, got %+v", msgs)
	}

	// Ready for 20 minutes and still unclaimed: the first rung fires.
	run(t, w, setSQL(`UPDATE tasks SET done_at = ?, completed_at = ? WHERE id = 'pre'`, ago(20*time.Minute), ago(20*time.Minute)))
	tick(w)
	if msgs := chainMessages(t, w); len(msgs) != 1 || !strings.Contains(msgs[0].subject, "title t1") || strings.HasPrefix(msgs[0].subject, "ESCALATED") {
		t.Fatalf("ready 20min: want one notify for t1, got %+v", msgs)
	}
}

// AC2: an unassigned (pool) task waits silently while a doer of its profile is
// busy or just finished; it alerts once the profile's doers have been idle
// ack_notify_age. An assigned task is not affected by the pool rule.
func TestAckPoolWaitsWhileProfileDoerBusy(t *testing.T) {
	w := newTwin(t, "pool-busy")
	run(t, w,
		seedTask("busy", "in-progress", ago(3*time.Hour)),
		setSQL(`UPDATE tasks SET claimed_by = 'dev-1', claimed_at = ? WHERE id = 'busy'`, ago(3*time.Hour)),
		seedTask("t1", "pending", ago(2*time.Hour)))
	tick(w)
	if msgs := chainMessages(t, w); len(msgs) != 0 {
		t.Fatalf("doer busy: want no pool alert, got %+v", msgs)
	}

	run(t, w, setSQL(`UPDATE tasks SET status = 'done', done_at = ?, completed_at = ? WHERE id = 'busy'`, ago(5*time.Minute), ago(5*time.Minute)))
	tick(w)
	if msgs := chainMessages(t, w); len(msgs) != 0 {
		t.Fatalf("doer finished 5min ago: want no pool alert yet, got %+v", msgs)
	}

	run(t, w, setSQL(`UPDATE tasks SET done_at = ?, completed_at = ? WHERE id = 'busy'`, ago(20*time.Minute), ago(20*time.Minute)))
	tick(w)
	if msgs := chainMessages(t, w); len(msgs) != 1 || !strings.Contains(msgs[0].subject, "title t1") {
		t.Fatalf("doers idle 20min: want one pool alert for t1, got %+v", msgs)
	}

	// Assigned task: the named assignee owes the ACK regardless of the pool.
	w2 := newTwin(t, "assigned-busy")
	run(t, w2,
		seedTask("busy", "in-progress", ago(3*time.Hour)),
		setSQL(`UPDATE tasks SET claimed_by = 'dev-1', claimed_at = ? WHERE id = 'busy'`, ago(3*time.Hour)),
		seedTask("t2", "pending", ago(20*time.Minute)),
		setSQL(`UPDATE tasks SET assigned_to = 'dev-2' WHERE id = 't2'`))
	tick(w2)
	if msgs := chainMessages(t, w2); len(msgs) != 1 || !strings.Contains(msgs[0].subject, "title t2") {
		t.Fatalf("assigned task: want its ACK alert, got %+v", msgs)
	}
}

// AC3: an accepted / in-progress task with no heartbeat or activity for
// stale_task_age alerts its dispatcher once per stale episode; fresh work and
// in-review tasks never do.
func TestStaleHeldTaskAlertsDispatcherOnce(t *testing.T) {
	w := newTwin(t, "stale")
	run(t, w,
		seedTask("s1", "in-progress", ago(8*time.Hour)),
		setSQL(`UPDATE tasks SET assigned_to = 'dev-1', lease_holder = 'dev-1', last_activity_at = ?, lease_heartbeat_at = ? WHERE id = 's1'`, ago(6*time.Hour), ago(6*time.Hour)),
		seedTask("fresh", "accepted", ago(8*time.Hour)),
		setSQL(`UPDATE tasks SET assigned_to = 'dev-2', last_activity_at = ? WHERE id = 'fresh'`, ago(10*time.Minute)),
		seedTask("gate", "in-review", ago(8*time.Hour)),
		setSQL(`UPDATE tasks SET assigned_to = 'dev-3', last_activity_at = ? WHERE id = 'gate'`, ago(6*time.Hour)))

	var staleAt func(time.Time) []chainMsg
	stale := func() []chainMsg { return staleAt(time.Now().UTC()) }
	staleAt = func(now time.Time) []chainMsg {
		evaluateStaleHeldTasks(w.d, w.rec, now)
		var out []chainMsg
		for _, m := range chainMessages(t, w) {
			if strings.HasPrefix(m.subject, "STALE: ") && strings.Contains(m.subject, "title s1") {
				out = append(out, m)
			}
		}
		return out
	}
	msgs := stale()
	if len(msgs) != 1 || msgs[0].to != "cto" || msgs[0].priority != "P1" {
		t.Fatalf("want one P1 STALE alert for s1 to the dispatcher, got %+v", msgs)
	}
	for _, m := range chainMessages(t, w) {
		if strings.Contains(m.subject, "title fresh") || strings.Contains(m.subject, "title gate") {
			t.Fatalf("fresh accepted / in-review task must not alert: %+v", m)
		}
	}
	if msgs = stale(); len(msgs) != 1 {
		t.Fatalf("second sweep: want still one alert (once per episode), got %+v", msgs)
	}

	// Activity resumes, then stalls again: a new episode alerts again.
	resumed := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	run(t, w, setSQL(`UPDATE tasks SET last_activity_at = ? WHERE id = 's1'`, resumed))
	if msgs = stale(); len(msgs) != 1 {
		t.Fatalf("active again: want no new alert, got %+v", msgs)
	}
	if msgs = staleAt(time.Now().UTC().Add(4 * time.Hour)); len(msgs) != 2 {
		t.Fatalf("stalled again 4h later: want a second alert, got %+v", msgs)
	}
}
