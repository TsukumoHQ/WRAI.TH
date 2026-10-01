package relay

import (
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Park P6 (ruling wraith-park-ruling): a no-ACK escalation fires only for a
// lane that will not pick work up (no live agent, or a live agent idle past
// the window); a queue behind busy live agents becomes one hourly digest.

func liveDev(t *testing.T, w *twin, name string) {
	t.Helper()
	dev := "dev"
	if _, _, err := w.d.RegisterAgent("p1", name, "", "", nil, &dev, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

func digests(t *testing.T, w *twin) []chainMsg {
	t.Helper()
	var out []chainMsg
	for _, m := range chainMessages(t, w) {
		if strings.HasPrefix(m.subject, "QUEUE: ") {
			out = append(out, m)
		}
	}
	return out
}

func ladder(t *testing.T, w *twin) []chainMsg {
	t.Helper()
	var out []chainMsg
	for _, m := range chainMessages(t, w) {
		if !strings.HasPrefix(m.subject, "QUEUE: ") {
			out = append(out, m)
		}
	}
	return out
}

// busyLane: dev-1 is live and holds an in-progress task; t1 waits in the pool
// for 2h, past every ACK window.
func busyLane(t *testing.T, name string) *twin {
	t.Helper()
	w := newTwin(t, name)
	liveDev(t, w, "dev-1")
	run(t, w,
		seedTask("busy", "in-progress", ago(3*time.Hour)),
		setSQL(`UPDATE tasks SET assigned_to = 'dev-1', lease_holder = 'dev-1', claimed_by = 'dev-1', claimed_at = ? WHERE id = 'busy'`, ago(3*time.Hour)),
		seedTask("t1", "pending", ago(2*time.Hour)))
	return w
}

func TestBusyLane_NoEscalationOneDigest(t *testing.T) {
	w := busyLane(t, "busy-digest")
	now := time.Now().UTC()
	for _, h := range []time.Duration{0, 30 * time.Minute, 2 * time.Hour} {
		evaluateObligations(w.d, w.rec, now.Add(h))
	}
	if msgs := ladder(t, w); len(msgs) != 0 {
		t.Fatalf("busy live lane: want no ACK rung, got %+v", msgs)
	}
	evaluateLaneDigests(w.d, w.rec, now)
	d := digests(t, w)
	if len(d) != 1 || d[0].to != "cto" {
		t.Fatalf("want one digest to the dispatcher, got %+v", d)
	}
	if !strings.Contains(d[0].subject, "1 pending") || !strings.Contains(d[0].subject, "oldest 120min") {
		t.Errorf("digest must carry queue depth and oldest age: %q", d[0].subject)
	}
}

func TestBusyLane_DigestAtMostHourly(t *testing.T) {
	w := busyLane(t, "busy-hourly")
	now := time.Now().UTC()
	evaluateLaneDigests(w.d, w.rec, now)
	evaluateLaneDigests(w.d, w.rec, now.Add(30*time.Minute))
	if d := digests(t, w); len(d) != 1 {
		t.Fatalf("second sweep within the hour: want 1 digest total, got %d", len(d))
	}
	evaluateLaneDigests(w.d, w.rec, now.Add(61*time.Minute))
	if d := digests(t, w); len(d) != 2 {
		t.Fatalf("after the hour: want 2 digests total, got %d", len(d))
	}
}

func TestBusyLane_NoLiveAgentEscalates(t *testing.T) {
	w := newTwin(t, "dead-lane")
	run(t, w,
		seedTask("busy", "in-progress", ago(3*time.Hour)),
		setSQL(`UPDATE tasks SET assigned_to = 'ghost', lease_holder = 'ghost', claimed_by = 'ghost', claimed_at = ? WHERE id = 'busy'`, ago(3*time.Hour)),
		seedTask("t1", "pending", ago(2*time.Hour)))
	tick(w)
	if msgs := ladder(t, w); len(msgs) != 1 || !strings.Contains(msgs[0].subject, "title t1") {
		t.Fatalf("no live agent on the profile: want the ACK rung for t1, got %+v", msgs)
	}
}

func TestBusyLane_IdleLiveAgentEscalates(t *testing.T) {
	w := newTwin(t, "idle-lane")
	liveDev(t, w, "dev-1")
	liveDev(t, w, "dev-2")
	run(t, w,
		seedTask("busy", "in-progress", ago(3*time.Hour)),
		setSQL(`UPDATE tasks SET assigned_to = 'dev-1', lease_holder = 'dev-1', claimed_by = 'dev-1', claimed_at = ? WHERE id = 'busy'`, ago(3*time.Hour)),
		seedTask("old", "done", ago(5*time.Hour)),
		setSQL(`UPDATE tasks SET claimed_by = 'dev-2', claimed_at = ?, completed_at = ?, done_at = ? WHERE id = 'old'`, ago(4*time.Hour), ago(30*time.Minute), ago(30*time.Minute)),
		seedTask("t1", "pending", ago(2*time.Hour)))
	tick(w)
	if msgs := ladder(t, w); len(msgs) != 1 || !strings.Contains(msgs[0].subject, "title t1") {
		t.Fatalf("dev-2 idle 30min: want the ACK rung for t1, got %+v", msgs)
	}
}
