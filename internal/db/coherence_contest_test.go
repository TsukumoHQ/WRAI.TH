package db

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// contest helpers on the T1 fixture: the author "cto" reports to "lead" (the
// round-1 reviewer) and "exec" is the project executive.
func newContest(t *testing.T, holders ...string) *coherenceFixture {
	t.Helper()
	f := newCoherence(t, holders...)
	seedAgent(t, f.d.conn, "p", "lead", "active", "", "", 0)
	seedAgent(t, f.d.conn, "p", "exec", "active", "", "", 0)
	if _, err := f.d.conn.Exec(`UPDATE agents SET reports_to = 'lead' WHERE name = 'cto'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.conn.Exec(`UPDATE agents SET is_executive = 1 WHERE name = 'exec'`); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *coherenceFixture) discharge(id, by, verdict, reason string) CoherenceDischargeResult {
	f.t.Helper()
	r, err := f.d.DischargeCoherence(CoherenceDischarge{Project: "p", ID: id, By: by, Verdict: verdict, Reason: reason, Now: time.Now()})
	if err != nil {
		f.t.Fatalf("discharge %s %s: %v", id, verdict, err)
	}
	return r
}

func (f *coherenceFixture) mustDischarge(id, by, verdict, reason string) CoherenceDischargeResult {
	f.t.Helper()
	r := f.discharge(id, by, verdict, reason)
	if !r.OK {
		f.t.Fatalf("discharge %s by %s (%s) refused: %s", id, by, verdict, r.Refusal)
	}
	return r
}

func (f *coherenceFixture) review() (id, bearer, bearerKind string, depth int) {
	f.t.Helper()
	_ = f.d.ro().QueryRow(`SELECT id, bearer, bearer_kind, escalation_depth FROM obligations WHERE norm_id = ? AND state = 'active'
		ORDER BY created_at DESC LIMIT 1`, NormCoherenceReview).Scan(&id, &bearer, &bearerKind, &depth)
	return
}

func (f *coherenceFixture) rolloutState() string {
	f.t.Helper()
	var s string
	_ = f.d.ro().QueryRow(`SELECT state FROM knowledge_rollouts ORDER BY rev DESC LIMIT 1`).Scan(&s)
	return s
}

func (f *coherenceFixture) read(agent, id string) {
	f.t.Helper()
	if _, err := f.d.RecordRecallBatch(map[RecallKey][]string{{Project: "p", Agent: agent}: {id}}); err != nil {
		f.t.Fatal(err)
	}
}

func staleCode(err error) bool {
	var te *TaskError
	return errors.As(err, &te) && te.Code == CodeStaleContext
}

const conflictReason = "jwt is still required by the mobile client"

func TestCoherenceContest(t *testing.T) {
	t.Run("UnaffectedClosesInactive", func(t *testing.T) {
		f := newContest(t, "a")
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		id, _, _ := f.obligationOf("a")
		if r := f.discharge(id, "a", "maybe", ""); r.OK {
			t.Fatal("an unknown verdict was accepted")
		}
		f.mustDischarge(id, "a", VerdictUnaffected, "")
		if _, state, _ := f.obligationOf("a"); state != ObligationInactive {
			t.Fatalf("state = %s, want inactive", state)
		}
		f.eval(time.Now())
		if s := f.rolloutState(); s != RolloutCommitted {
			t.Fatalf("rollout = %s, want committed", s)
		}
	})

	t.Run("ConflictOpensContradictionAgainstNewVersion", func(t *testing.T) {
		f := newContest(t, "a")
		v2 := f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		id, _, _ := f.obligationOf("a")
		if r := f.discharge(id, "a", VerdictConflict, "no"); r.OK {
			t.Fatal("a conflict without a reason was accepted")
		}
		r := f.mustDischarge(id, "a", VerdictConflict, conflictReason)
		if n := f.count(`SELECT COUNT(*) FROM exceptions WHERE source_kind = 'knowledge_conflict' AND source_ref = ?
			AND kind = 'contradiction' AND status = 'open' AND raised_by = 'a'`, v2); n != 1 {
			t.Fatalf("contradiction rows against v2 = %d, want 1", n)
		}
		if f.rolloutState() != RolloutContested {
			t.Fatalf("rollout = %s, want contested", f.rolloutState())
		}
		if _, bearer, _, depth := f.review(); bearer != "lead" || depth != 1 {
			t.Fatalf("review on %s depth %d, want lead/1", bearer, depth)
		}
		if len(r.Notices) != 1 || r.Notices[0].To != "lead" {
			t.Fatalf("notices = %+v, want one to lead", r.Notices)
		}
	})

	t.Run("UpheldReopensRound2", func(t *testing.T) {
		f := newContest(t, "a")
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		id, _, _ := f.obligationOf("a")
		f.mustDischarge(id, "a", VerdictConflict, conflictReason)
		rid, _, _, _ := f.review()
		if r := f.discharge(rid, "a", VerdictUpheld, ""); r.OK {
			t.Fatal("the raiser ruled its own contest")
		}
		if r := f.discharge(rid, "lead", VerdictPrepared, ""); r.OK {
			t.Fatal("a review accepted a reassess verdict")
		}
		r := f.mustDischarge(rid, "lead", VerdictUpheld, "")
		var depth int
		var state string
		_ = f.d.ro().QueryRow(`SELECT escalation_depth, state FROM obligations WHERE norm_id = ? AND bearer = 'a'
			ORDER BY created_at DESC LIMIT 1`, NormCoherenceReassess).Scan(&depth, &state)
		if depth != 1 || state != ObligationActive {
			t.Fatalf("raiser's reassess depth=%d state=%s, want 1/active (round 2)", depth, state)
		}
		if n := f.count(`SELECT COUNT(*) FROM exceptions WHERE source_kind = 'knowledge_conflict' AND status = 'resolved'
			AND resolution_reason = 'upheld' AND resolved_by = 'peer'`); n != 1 {
			t.Fatalf("upheld contests = %d, want 1", n)
		}
		if f.rolloutState() != RolloutPreparing || len(r.Notices) != 1 || r.Notices[0].To != "a" {
			t.Fatalf("rollout=%s notices=%+v, want preparing + one to a", f.rolloutState(), r.Notices)
		}
	})

	t.Run("OverturnedFixSupersedes", func(t *testing.T) {
		f := newContest(t, "a")
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		id, _, _ := f.obligationOf("a")
		f.mustDischarge(id, "a", VerdictConflict, conflictReason)
		rid, _, _, _ := f.review()
		if r := f.discharge(rid, "lead", VerdictOverturned, ""); r.OK || !strings.Contains(r.Refusal, "fix") {
			t.Fatalf("overturned before a fix: ok=%v refusal=%q", r.OK, r.Refusal)
		}
		if _, err := f.d.SetMemory("p", "lead", "auth-policy", "jwt banned except mobile", "", "project", "", "constraints"); err != nil {
			t.Fatal(err)
		}
		f.mustDischarge(rid, "lead", VerdictOverturned, "mobile needs jwt")
		f.eval(time.Now())
		if n := f.count(`SELECT COUNT(*) FROM knowledge_rollouts WHERE state = 'superseded'`); n != 1 {
			t.Fatalf("superseded rollouts = %d, want 1", n)
		}
		if n := f.count(`SELECT COUNT(*) FROM knowledge_rollouts WHERE state IN ('preparing', 'committed')`); n != 1 {
			t.Fatalf("fix rollouts = %d, want 1", n)
		}
		if n := f.count(`SELECT COUNT(*) FROM exceptions WHERE source_kind = 'knowledge_conflict' AND resolution_reason = 'overturned'`); n != 1 {
			t.Fatalf("overturned contests = %d, want 1", n)
		}
	})

	t.Run("SecondContestConstraintReachesHuman", func(t *testing.T) {
		f := newContest(t, "a", "b")
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		ida, _, _ := f.obligationOf("a")
		idb, _, _ := f.obligationOf("b")
		f.mustDischarge(ida, "a", VerdictConflict, conflictReason)
		r := f.mustDischarge(idb, "b", VerdictConflict, "the batch jobs cannot rotate tokens")
		rid, bearer, kind, depth := f.review()
		if bearer != "user" || kind != "human" || depth != 2 {
			t.Fatalf("round-2 review on %s (%s) depth %d, want user/human/2", bearer, kind, depth)
		}
		if len(r.Notices) != 1 || r.Notices[0].To != "user" || r.Notices[0].Action != "decide" {
			t.Fatalf("notice = %+v, want one decide to user", r.Notices)
		}
		if d, ok := r.Notices[0].Schema["decision"].([]string); !ok || strings.Join(d, ",") != "keep_new,revert,scope_split" {
			t.Fatalf("schema = %v, want the §5 decision enum", r.Notices[0].Schema)
		}
		if n := f.count(`SELECT COUNT(*) FROM obligations WHERE bearer IN ('user', 'human')`); n != 1 {
			t.Fatalf("obligations on the human = %d, want exactly the round-2 review", n)
		}
		// A third conflict joins the open round-2 review: the human is paged once.
		if r := f.mustDischarge(ida2(t, f, "b"), "b", VerdictConflict, "still wrong for the batch jobs"); len(r.Notices) != 0 {
			t.Fatalf("third conflict notices = %+v, want none", r.Notices)
		}
		if n := f.count(`SELECT COUNT(*) FROM obligations WHERE bearer IN ('user', 'human')`); n != 1 {
			t.Fatalf("obligations on the human = %d after a third conflict, want 1", n)
		}
		f.mustDischarge(rid, "human", VerdictUpheld, "keep_new")
	})

	t.Run("SecondContestBehaviorStaysExecutive", func(t *testing.T) {
		f := newContest(t, "a", "b")
		f.supersede("prefer short tokens", "behavior")
		f.eval(time.Now())
		ida, _, _ := f.obligationOf("a")
		idb, _, _ := f.obligationOf("b")
		f.mustDischarge(ida, "a", VerdictConflict, conflictReason)
		f.mustDischarge(idb, "b", VerdictConflict, "the batch jobs cannot rotate tokens")
		if _, bearer, _, depth := f.review(); bearer != "exec" || depth != 2 {
			t.Fatalf("round-2 review on %s depth %d, want exec/2", bearer, depth)
		}
		if n := f.count(`SELECT COUNT(*) FROM obligations WHERE bearer IN ('user', 'human')`); n != 0 {
			t.Fatalf("a behavior contest reached the human (%d)", n)
		}
	})

	t.Run("ReviewerNeverRaiserOrAuthorOrFounder", func(t *testing.T) {
		for _, tc := range []struct{ name, reportsTo string }{{"raiser", "a"}, {"founder", "founder"}, {"author", "cto"}} {
			f := newContest(t, "a")
			seedAgent(t, f.d.conn, "p", "founder", "active", "", "", 0)
			_, _ = f.d.conn.Exec(`UPDATE agents SET reports_to = ? WHERE name = 'cto'`, tc.reportsTo)
			_, _ = f.d.conn.Exec(`UPDATE agents SET is_executive = 1 WHERE name IN ('founder', 'cto', 'a')`)
			f.supersede("jwt banned", "constraints")
			f.eval(time.Now())
			id, _, _ := f.obligationOf("a")
			f.mustDischarge(id, "a", VerdictConflict, conflictReason)
			if _, bearer, _, _ := f.review(); bearer != "exec" {
				t.Fatalf("%s: reviewer = %q, want exec", tc.name, bearer)
			}
		}
	})

	t.Run("EnforceFencesOnlyStaleBearers", func(t *testing.T) {
		f := newContest(t, "a")
		seedAgent(t, f.d.conn, "p", "c", "active", "dev", "boss", 0)
		f.d.SetSetting(SettingCoherenceMode, CoherenceModeEnforce)
		t1, _ := f.d.DispatchTask("p", "dev", "cto", "unrelated work", "", "P2", nil, nil, TypedTicket{}, false, nil)
		t2, _ := f.d.DispatchTask("p", "dev", "cto", "other work", "", "P2", nil, nil, TypedTicket{}, false, nil)
		v2 := f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		if _, err := f.d.ClaimTask(t2.ID, "c", "p"); err != nil {
			t.Fatalf("an agent that never held v1 was fenced: %v", err)
		}
		_, err := f.d.ClaimTask(t1.ID, "a", "p")
		if !staleCode(err) || !strings.Contains(err.Error(), "auth-policy") {
			t.Fatalf("stale bearer claim err = %v, want STALE_CONTEXT naming the key", err)
		}
		if _, err := f.d.StartTask(t1.ID, "a", "p"); !staleCode(err) {
			t.Fatalf("stale bearer start-from-pending err = %v, want STALE_CONTEXT", err)
		}
		f.read("a", v2)
		id, _, _ := f.obligationOf("a")
		f.mustDischarge(id, "a", VerdictPrepared, "")
		if _, err := f.d.ClaimTask(t1.ID, "a", "p"); err != nil {
			t.Fatalf("claim after get_memory + prepared: %v", err)
		}
	})

	t.Run("NarrowingNeverFences", func(t *testing.T) {
		f := newContest(t, "a")
		f.d.SetSetting(SettingCoherenceMode, CoherenceModeEnforce)
		task, _ := f.d.DispatchTask("p", "dev", "cto", "unrelated work", "", "P2", nil, nil, TypedTicket{}, false, nil)
		f.supersede("jwt discouraged", "context") // constraints -> context: narrowing
		f.eval(time.Now())
		if n := f.count(`SELECT COUNT(*) FROM knowledge_rollouts WHERE change_class = 'narrowing' AND gate = 'advisory'`); n != 1 {
			t.Fatalf("narrowing rollouts = %d, want 1 advisory", n)
		}
		if _, err := f.d.ClaimTask(task.ID, "a", "p"); err != nil {
			t.Fatalf("a narrowing fenced a claim: %v", err)
		}
	})

	t.Run("ContestedDropsFence", func(t *testing.T) {
		f := newContest(t, "a", "b")
		f.d.SetSetting(SettingCoherenceMode, CoherenceModeEnforce)
		task, _ := f.d.DispatchTask("p", "dev", "cto", "unrelated work", "", "P2", nil, nil, TypedTicket{}, false, nil)
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		ida, _, _ := f.obligationOf("a")
		f.mustDischarge(ida, "a", VerdictConflict, conflictReason)
		// b still bears an open reassess, but the rollout is contested.
		if _, err := f.d.ClaimTask(task.ID, "b", "p"); err != nil {
			t.Fatalf("a contested rollout still fenced: %v", err)
		}
	})

	t.Run("GrandfatheredLeaseNotBlocked", func(t *testing.T) {
		f := newContest(t, "a")
		f.d.SetSetting(SettingCoherenceMode, CoherenceModeEnforce)
		task, _ := f.d.DispatchTask("p", "dev", "cto", "unrelated work", "", "P2", nil, nil, TypedTicket{}, false, nil)
		if _, err := f.d.ClaimTask(task.ID, "a", "p"); err != nil {
			t.Fatal(err)
		}
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		if _, err := f.d.StartTask(task.ID, "a", "p"); err != nil {
			t.Fatalf("an already-leased task was fenced: %v", err)
		}
	})

	t.Run("AdvisoryReturnsStaleList", func(t *testing.T) {
		f := newContest(t, "a")
		task, _ := f.d.DispatchTask("p", "dev", "cto", "unrelated work", "", "P2", nil, nil, TypedTicket{}, false, nil)
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		stale, err := f.d.StaleContextFor("p", "a")
		if err != nil || len(stale) != 1 || stale[0].Key != "auth-policy" || !stale[0].Fence {
			t.Fatalf("stale = %+v err=%v, want one fencing entry for auth-policy", stale, err)
		}
		if _, err := f.d.ClaimTask(task.ID, "a", "p"); err != nil {
			t.Fatalf("advisory refused a claim: %v", err)
		}
	})

	t.Run("ClaimBasisConsumer", func(t *testing.T) {
		// A1: the task never cites the key, but a claimed it on a basis holding v1.
		f := newContest(t, "a")
		task, _ := f.d.DispatchTask("p", "dev", "cto", "unrelated work", "", "P2", nil, nil, TypedTicket{}, false, nil)
		if _, err := f.d.ClaimTask(task.ID, "a", "p"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.d.StampTaskBasis("p", "a", BasisClaim, []string{task.ID}, nil); err != nil {
			t.Fatal(err)
		}
		f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		var subject, via string
		_ = f.d.ro().QueryRow(`SELECT subject_id, json_extract(discharge_evidence, '$.via') FROM obligations
			WHERE norm_id = ? AND bearer = 'a'`, NormCoherenceReassess).Scan(&subject, &via)
		if subject != task.ID || via != "claim_basis" {
			t.Fatalf("a's obligation on %s via %s, want the task via claim_basis", subject, via)
		}
		// prepared asks the relay to re-stamp that task's basis.
		v2 := ""
		_ = f.d.ro().QueryRow(`SELECT new_memory_id FROM knowledge_rollouts`).Scan(&v2)
		f.read("a", v2)
		id, _, _ := f.obligationOf("a")
		if r := f.mustDischarge(id, "a", "", ""); r.StampTaskID != task.ID {
			t.Fatalf("stamp task = %q, want %s", r.StampTaskID, task.ID)
		}
	})

	t.Run("StaleBasisCompletionCounted", func(t *testing.T) {
		f := newContest(t, "a", "b")
		ta, _ := f.d.DispatchTask("p", "dev", "cto", "work a", "", "P2", nil, nil, TypedTicket{}, false, nil)
		tb, _ := f.d.DispatchTask("p", "dev", "cto", "work b", "", "P2", nil, nil, TypedTicket{}, false, nil)
		v2 := f.supersede("jwt banned", "constraints")
		f.eval(time.Now())
		time.Sleep(2 * time.Millisecond)
		// a completes on its old basis without reassessing; b reads v2 and prepares first.
		f.read("b", v2)
		idb, _, _ := f.obligationOf("b")
		f.mustDischarge(idb, "b", VerdictPrepared, "")
		for agent, id := range map[string]string{"a": ta.ID, "b": tb.ID} {
			if _, err := f.d.StampTaskBasis("p", agent, BasisComplete, []string{id}, nil); err != nil {
				t.Fatal(err)
			}
		}
		got, err := f.d.StaleBasisCompletions("p")
		if err != nil || len(got) != 1 || got[0] != ta.ID {
			t.Fatalf("stale completions = %v err=%v, want [%s]", got, err, ta.ID)
		}
	})
}

// ida2 opens a fresh reassess for bearer on the current rollout (a stand-in for
// a second consumer's rung) and returns its id.
func ida2(t *testing.T, f *coherenceFixture, bearer string) string {
	t.Helper()
	var rollout string
	_ = f.d.ro().QueryRow(`SELECT id FROM knowledge_rollouts ORDER BY rev DESC LIMIT 1`).Scan(&rollout)
	id := "extra-" + bearer
	if _, err := f.d.conn.Exec(`INSERT INTO obligations (id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id,
		bearer_kind, bearer, state, created_at, escalation_depth, discharge_evidence)
		VALUES (?, 'p', ?, 1, ?, ?, ?, 'consumer', ?, 'active', ?, 0, ?)`,
		id, NormCoherenceReassess, "h-"+id, SubjectKnowledgeSession, "p/"+bearer+"-x", bearer,
		time.Now().UTC().Format(memoryTimeFmt), `{"rollout_id":"`+rollout+`","via":"context_boot"}`); err != nil {
		t.Fatal(err)
	}
	return id
}
