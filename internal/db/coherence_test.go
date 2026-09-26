package db

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// coherenceFixture: key "auth-policy" v1 (project constraint) served at boot
// to the given agents, who are registered active.
type coherenceFixture struct {
	t  *testing.T
	d  *DB
	v1 string
}

func newCoherence(t *testing.T, holders ...string) *coherenceFixture {
	t.Helper()
	d := testDB(t)
	seedAgent(t, d.conn, "p", "cto", "active", "", "", 0)
	for _, h := range holders {
		seedAgent(t, d.conn, "p", h, "active", "dev", "boss", 0)
	}
	seedAgent(t, d.conn, "p", "boss", "active", "", "", 0)
	m, err := d.SetMemory("p", "cto", "auth-policy", "jwt allowed", "", "project", "", "constraints")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range holders {
		if _, _, err := d.RecordSnapshot("p", h, "", SnapshotBoot, []string{m.ID}); err != nil {
			t.Fatal(err)
		}
	}
	return &coherenceFixture{t: t, d: d, v1: m.ID}
}

func (f *coherenceFixture) supersede(value, layer string) string {
	f.t.Helper()
	m, err := f.d.SetMemory("p", "cto", "auth-policy", value, "", "project", "", layer)
	if err != nil {
		f.t.Fatal(err)
	}
	return m.ID
}

func (f *coherenceFixture) eval(at time.Time) CoherenceReport {
	f.t.Helper()
	rep, err := f.d.EvaluateCoherence(at)
	if err != nil {
		f.t.Fatal(err)
	}
	return rep
}

func (f *coherenceFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.d.ro().QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *coherenceFixture) obligationOf(bearer string) (id, state, kind string) {
	f.t.Helper()
	_ = f.d.ro().QueryRow(`SELECT id, state, subject_kind FROM obligations WHERE norm_id = ? AND bearer = ? ORDER BY created_at DESC LIMIT 1`,
		NormCoherenceReassess, bearer).Scan(&id, &state, &kind)
	return
}

func TestCoherence(t *testing.T) {
	t.Run("BreakingOpensOnePerConsumer", func(t *testing.T) {
		f := newCoherence(t, "a", "b")
		seedAgent(t, f.d.conn, "p", "c", "active", "dev", "", 0)
		_, _, _ = f.d.RecordSnapshot("p", "c", "", SnapshotBoot, []string{"something-else"})
		f.supersede("jwt banned", "constraints")
		rep := f.eval(time.Now())
		if rep.Rollouts != 1 || rep.Opened != 2 {
			t.Fatalf("report %+v, want 1 rollout / 2 obligations", rep)
		}
		for _, who := range []string{"a", "b"} {
			if _, state, kind := f.obligationOf(who); state != ObligationActive || kind != SubjectKnowledgeSession {
				t.Fatalf("%s: state=%q kind=%q", who, state, kind)
			}
		}
		if id, _, _ := f.obligationOf("c"); id != "" {
			t.Fatal("c never held v1 but was obliged")
		}
		if id, _, _ := f.obligationOf("cto"); id != "" {
			t.Fatal("the author of the change was obliged")
		}
		if len(rep.Notices) != 2 {
			t.Fatalf("notices = %d, want 2", len(rep.Notices))
		}
	})

	t.Run("AdditiveAndEditorialOpenNothing", func(t *testing.T) {
		d := testDB(t)
		seedAgent(t, d.conn, "p", "a", "active", "dev", "", 0)
		m, _ := d.SetMemory("p", "cto", "review-habit", "small diffs", "", "project", "", "behavior")
		_, _, _ = d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{m.ID})
		_, _ = d.SetMemory("p", "cto", "review-habit", "small diffs please", "", "project", "", "behavior") // additive
		c, _ := d.SetMemory("p", "cto", "auth-policy", "x", "", "project", "", "constraints")
		_, _, _ = d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{c.ID})
		_, _ = d.SetMemory("p", "cto", "auth-policy", "x", "", "project", "", "constraints") // editorial touch
		rep, err := d.EvaluateCoherence(time.Now())
		if err != nil || rep.Rollouts != 0 {
			t.Fatalf("additive/editorial opened rollouts: %+v %v", rep, err)
		}
	})

	t.Run("NewKeyOpensNothing", func(t *testing.T) {
		f := newCoherence(t, "a")
		_, _ = f.d.SetMemory("p", "cto", "brand-new-rule", "v", "", "project", "", "constraints")
		if rep := f.eval(time.Now()); rep.Rollouts != 0 {
			t.Fatalf("fresh key opened %d rollouts", rep.Rollouts)
		}
	})

	t.Run("TaskBeatsSession", func(t *testing.T) {
		f := newCoherence(t, "a")
		seedTask(t, f.d.conn, "t1", "p", "in-progress", "cto", "a", "a", "dev", "", "", false)
		if _, err := f.d.conn.Exec(`UPDATE tasks SET description = 'implement login per auth-policy' WHERE id = 't1'`); err != nil {
			t.Fatal(err)
		}
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		if n := f.count(`SELECT COUNT(*) FROM obligations WHERE norm_id = ? AND bearer = 'a'`, NormCoherenceReassess); n != 1 {
			t.Fatalf("a has %d obligations, want 1", n)
		}
		if _, _, kind := f.obligationOf("a"); kind != SubjectKnowledgeTask {
			t.Fatalf("a's obligation is on %s, want the task", kind)
		}
	})

	t.Run("PendingCitingTaskObligesDispatcher", func(t *testing.T) {
		f := newCoherence(t)
		seedAgent(t, f.d.conn, "p", "lead", "active", "", "", 0)
		seedTask(t, f.d.conn, "t2", "p", "pending", "lead", "", "", "dev", "", "", false)
		if _, err := f.d.conn.Exec(`UPDATE tasks SET goal = 'apply auth-policy to the API' WHERE id = 't2'`); err != nil {
			t.Fatal(err)
		}
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		var bearerKind, subject string
		_ = f.d.ro().QueryRow(`SELECT bearer_kind, subject_id FROM obligations WHERE norm_id = ? AND bearer = 'lead'`, NormCoherenceReassess).Scan(&bearerKind, &subject)
		if bearerKind != "dispatcher" || subject != "t2" {
			t.Fatalf("pending citing task: bearer_kind=%q subject=%q", bearerKind, subject)
		}
	})

	t.Run("CursorExactlyOnce", func(t *testing.T) {
		f := newCoherence(t, "a", "b")
		f.supersede("jwt banned", "constraints")
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := f.d.EvaluateCoherence(time.Now()); err != nil {
					t.Errorf("concurrent tick: %v", err)
				}
			}()
		}
		wg.Wait()
		if n := f.count(`SELECT COUNT(*) FROM knowledge_rollouts`); n != 1 {
			t.Fatalf("rollouts = %d, want 1", n)
		}
		if n := f.count(`SELECT COUNT(*) FROM obligations WHERE norm_id = ?`, NormCoherenceReassess); n != 2 {
			t.Fatalf("obligations = %d, want 2", n)
		}
	})

	t.Run("MootWhenTaskDone", func(t *testing.T) {
		f := newCoherence(t, "a")
		seedTask(t, f.d.conn, "t1", "p", "in-progress", "cto", "a", "a", "dev", "", "", false)
		_, _ = f.d.conn.Exec(`UPDATE tasks SET description = 'uses auth-policy' WHERE id = 't1'`)
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		_, _ = f.d.conn.Exec(`UPDATE tasks SET status = 'done' WHERE id = 't1'`)
		f.eval(time.Now())
		if _, state, _ := f.obligationOf("a"); state != ObligationInactive {
			t.Fatalf("task done: obligation %s, want inactive", state)
		}
	})

	t.Run("MootWhenSupersededAgain", func(t *testing.T) {
		f := newCoherence(t, "a")
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		f.supersede("jwt banned except webhooks", "constraints")
		f.eval(time.Now())
		if n := f.count(`SELECT COUNT(*) FROM knowledge_rollouts WHERE state = ?`, RolloutSuperseded); n != 1 {
			t.Fatalf("superseded rollouts = %d, want 1", n)
		}
		if _, state, _ := f.obligationOf("a"); state != ObligationInactive {
			t.Fatalf("v2 obligation %s, want inactive", state)
		}
	})

	t.Run("RebootAcks", func(t *testing.T) {
		f := newCoherence(t, "a")
		v2 := f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		_, _, _ = f.d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{v2})
		f.eval(time.Now())
		id, state, _ := f.obligationOf("a")
		var acked string
		_ = f.d.ro().QueryRow(`SELECT json_extract(discharge_evidence, '$.acked_by') FROM obligations WHERE id = ?`, id).Scan(&acked)
		if state != ObligationFulfilled || acked != "reboot" {
			t.Fatalf("reboot: state=%s acked_by=%s", state, acked)
		}
	})

	t.Run("LeaseExpiryAcks", func(t *testing.T) {
		f := newCoherence(t, "a")
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		_, _ = f.d.conn.Exec(`UPDATE agents SET status = 'inactive' WHERE name = 'a'`)
		f.eval(time.Now())
		if _, state, _ := f.obligationOf("a"); state != ObligationInactive {
			t.Fatalf("lease expiry: %s", state)
		}
		if n := f.count(`SELECT acked_by_expiry FROM knowledge_rollouts`); n != 1 {
			t.Fatalf("acked_by_expiry = %d", n)
		}
	})

	t.Run("PreparedRequiresServedNewVersion", func(t *testing.T) {
		f := newCoherence(t, "a")
		v2 := f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		id, _, _ := f.obligationOf("a")
		if ok, reason, _ := f.d.DischargeReassess("p", id, "a", "", time.Now()); ok || reason == "" {
			t.Fatal("prepared accepted before the new version was read")
		}
		if ok, _, _ := f.d.DischargeReassess("p", id, "b", "", time.Now()); ok {
			t.Fatal("a non-bearer discharged")
		}
		if _, err := f.d.RecordRecallBatch(map[RecallKey][]string{{Project: "p", Agent: "a"}: {v2}}); err != nil {
			t.Fatal(err)
		}
		if ok, reason, err := f.d.DischargeReassess("p", id, "a", "checked", time.Now()); !ok || err != nil {
			t.Fatalf("prepared after get_memory refused: %s %v", reason, err)
		}
	})

	t.Run("CommitWhenAllClosed", func(t *testing.T) {
		f := newCoherence(t, "a")
		v2 := f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		if n := f.count(`SELECT COUNT(*) FROM knowledge_rollouts WHERE state = ?`, RolloutPreparing); n != 1 {
			t.Fatal("rollout not preparing while an obligation is open")
		}
		_, _, _ = f.d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{v2})
		rep := f.eval(time.Now())
		if rep.Committed != 1 || f.count(`SELECT COUNT(*) FROM knowledge_rollouts WHERE state = ?`, RolloutCommitted) != 1 {
			t.Fatalf("rollout not committed: %+v", rep)
		}
	})

	t.Run("RoleRungNoHuman", func(t *testing.T) {
		f := newCoherence(t, "a")
		seedAgent(t, f.d.conn, "p", "user", "active", "", "", 0) // even a registered 'user' is never a bearer
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		f.eval(time.Now().Add(5 * time.Hour)) // breaking deadline 4h
		var role string
		_ = f.d.ro().QueryRow(`SELECT bearer FROM obligations WHERE norm_id = ? AND state = 'active'`, NormCoherenceReassessRole).Scan(&role)
		if role != "boss" {
			t.Fatalf("role rung bearer = %q, want boss (a's reports_to)", role)
		}
		f.eval(time.Now().Add(30 * time.Hour)) // role deadline 24h
		f.eval(time.Now().Add(31 * time.Hour))
		if n := f.count(`SELECT COUNT(*) FROM knowledge_rollouts WHERE state = ?`, RolloutCommittedWithOrphan); n != 1 {
			t.Fatalf("rollout not committed_with_orphans")
		}
		if n := f.count(`SELECT COUNT(*) FROM obligations WHERE norm_id LIKE 'coherence.%' AND bearer IN ('user', 'human', 'founder')`); n != 0 {
			t.Fatalf("%d coherence obligations on a human", n)
		}
	})

	t.Run("FanoutCapKeepsInFlight", func(t *testing.T) {
		f := newCoherence(t, "a")
		for i := 0; i < coherenceFanoutCap+5; i++ {
			name := fmt.Sprintf("h%03d", i)
			seedAgent(t, f.d.conn, "p", name, "active", "dev", "", 0)
			_, _, _ = f.d.RecordSnapshot("p", name, "", SnapshotBoot, []string{f.v1})
		}
		seedTask(t, f.d.conn, "t1", "p", "in-progress", "cto", "a", "a", "dev", "", "", false)
		_, _ = f.d.conn.Exec(`UPDATE tasks SET description = 'uses auth-policy' WHERE id = 't1'`)
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		if n := f.count(`SELECT COUNT(*) FROM obligations WHERE norm_id = ?`, NormCoherenceReassess); n != 1 {
			t.Fatalf("capped rollout opened %d obligations, want only the in-flight task", n)
		}
		if f.count(`SELECT fanout_capped FROM knowledge_rollouts`) != 1 ||
			f.count(`SELECT COUNT(*) FROM exceptions WHERE source_kind = 'coherence_fanout'`) != 1 {
			t.Fatal("fan-out cap not flagged with an exception row")
		}
	})

	t.Run("AdvisoryNeverRefusesClaim", func(t *testing.T) {
		f := newCoherence(t, "a")
		task, err := f.d.DispatchTask("p", "dev", "cto", "use auth-policy", "", "P2", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		if _, err := f.d.ClaimTask(task.ID, "a", "p"); err != nil {
			t.Fatalf("advisory refused a claim by a stale holder: %v", err)
		}
	})
}
