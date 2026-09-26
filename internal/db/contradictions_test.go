package db

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Gated contradiction detection (design 8d107daa T1, ruling ed744dee).
func TestContradictions(t *testing.T) {
	type memOpt struct {
		scope, layer, subject, agent, project string
		validFrom, validUntil                 string
	}
	write := func(t *testing.T, d *DB, key, value string, o memOpt) string {
		t.Helper()
		if o.scope == "" {
			o.scope = "project"
		}
		if o.layer == "" {
			o.layer = "constraints"
		}
		if o.agent == "" {
			o.agent = "alice"
		}
		if o.project == "" {
			o.project = "foo"
		}
		tags := "[]"
		if o.subject != "" {
			tags = `["subject:` + o.subject + `"]`
		}
		m, err := d.SetMemory(o.project, o.agent, key, value, tags, o.scope, "stated", o.layer)
		if err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
		if o.validFrom != "" || o.validUntil != "" {
			if err := d.SetMemoryValidity(o.project, o.agent, key, o.scope, o.validFrom, o.validUntil); err != nil {
				t.Fatalf("validity %s: %v", key, err)
			}
		}
		return m.ID
	}
	sweep := func(t *testing.T, d *DB) ContradictionReport {
		t.Helper()
		rep, err := d.EvaluateContradictions(time.Now())
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		return rep
	}
	type conflictRow struct {
		ID, Members, Evidence, State string
		Failed                       int
	}
	conflicts := func(t *testing.T, d *DB) []conflictRow {
		t.Helper()
		rows, err := d.ro().Query(`SELECT id, members, gate_evidence, state, failed_claims FROM knowledge_conflicts ORDER BY created_at`)
		if err != nil {
			t.Fatalf("conflicts: %v", err)
		}
		defer rows.Close()
		var out []conflictRow
		for rows.Next() {
			var c conflictRow
			if err := rows.Scan(&c.ID, &c.Members, &c.Evidence, &c.State, &c.Failed); err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
		}
		return out
	}
	edges := func(t *testing.T, d *DB) map[string]string {
		t.Helper()
		rows, err := d.ro().Query(`SELECT src_memory_id, dst_memory_id, kind, rule, declared FROM knowledge_edges`)
		if err != nil {
			t.Fatalf("edges: %v", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var s, dst, kind, rule string
			var declared int
			if err := rows.Scan(&s, &dst, &kind, &rule, &declared); err != nil {
				t.Fatal(err)
			}
			if declared != 0 {
				t.Fatalf("detector wrote a declared edge %s -> %s", s, dst)
			}
			out[s+">"+dst] = kind + "/" + rule
		}
		return out
	}
	day := func(n int) string {
		return time.Now().UTC().Add(time.Duration(n) * 24 * time.Hour).Format(memoryTimeFmt)
	}

	t.Run("PG151617NoConflict", func(t *testing.T) {
		d := testDB(t)
		subj := "db.postgres.version"
		m1 := write(t, d, "pg-new-projects", "new projects use PostgreSQL 17", memOpt{scope: "global", subject: subj})
		m2 := write(t, d, "foo-pg", "foo requires PostgreSQL 16", memOpt{subject: subj})
		m3 := write(t, d, "foo-pg-migration", "foo stays on PostgreSQL 15 until the migration", memOpt{layer: "decision", subject: subj, validUntil: day(30)})
		sweep(t, d)
		if c := conflicts(t, d); len(c) != 0 {
			t.Fatalf("PG15/16/17 opened %d conflicts, want 0: %+v", len(c), c)
		}
		want := map[string]string{
			m2 + ">" + m1: "overrides/scope_rank",
			m3 + ">" + m1: "overrides/scope_rank",
			m3 + ">" + m2: "overrides/layer_precedence",
		}
		got := edges(t, d)
		if len(got) != len(want) {
			t.Fatalf("edges = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("edge %s = %q, want %q (all: %v)", k, got[k], v, got)
			}
		}
	})

	t.Run("SameRankSameLayerDisagreeIsCandidate", func(t *testing.T) {
		d := testDB(t)
		subj := "db.postgres.version"
		a := write(t, d, "foo-pg", "foo requires PostgreSQL 16", memOpt{subject: subj})
		b := write(t, d, "foo-pg-legacy", "foo requires PostgreSQL 14", memOpt{subject: subj})
		sweep(t, d)
		c := conflicts(t, d)
		if len(c) != 1 {
			t.Fatalf("conflicts = %d, want 1", len(c))
		}
		want := []string{a, b}
		sort.Strings(want)
		wj, _ := json.Marshal(want)
		if c[0].Members != string(wj) || c[0].State != ConflictDetected {
			t.Fatalf("conflict = %+v, want members %s detected", c[0], wj)
		}
		for _, g := range []string{`"g1"`, `"g2"`, `"g3"`, `"g4"`} {
			if !strings.Contains(c[0].Evidence, g) {
				t.Fatalf("gate_evidence %s lacks %s", c[0].Evidence, g)
			}
		}
	})

	t.Run("DisjointValidityIsAmends", func(t *testing.T) {
		d := testDB(t)
		subj := "release.freeze"
		a := write(t, d, "freeze-q3", "release freeze for the Q3 window", memOpt{subject: subj, validUntil: day(1)})
		b := write(t, d, "freeze-q4", "release freeze for the Q4 window", memOpt{subject: subj, validFrom: day(2)})
		sweep(t, d)
		if c := conflicts(t, d); len(c) != 0 {
			t.Fatalf("disjoint windows opened %d conflicts", len(c))
		}
		if got := edges(t, d); len(got) != 1 || got[a+">"+b] != "amends/disjoint_validity" {
			t.Fatalf("edges = %v, want amends %s -> %s", got, a, b)
		}
	})

	t.Run("BehaviorContextNeverConflict", func(t *testing.T) {
		d := testDB(t)
		subj := "style.tabs"
		write(t, d, "tabs-a", "indent the makefiles with tabs", memOpt{layer: "behavior", subject: subj})
		write(t, d, "tabs-b", "indent the makefiles with spaces", memOpt{layer: "behavior", subject: subj})
		ctxID := write(t, d, "tabs-c", "indent the makefiles with tabs please", memOpt{layer: "context", subject: subj})
		sweep(t, d)
		if c, e := conflicts(t, d), edges(t, d); len(c) != 0 || len(e) != 0 {
			t.Fatalf("behavior/context: conflicts=%d edges=%v, want none", len(c), e)
		}
		if ov, _, err := d.OverlapHint(ctxID); err != nil || len(ov) != 0 {
			t.Fatalf("hint on a context write = %v, %v; want none", ov, err)
		}
	})

	t.Run("KeyFamilyAloneNotSubject", func(t *testing.T) {
		d := testDB(t)
		a, err := d.RememberDecision("foo", "alice", "general", "Makefiles are indented with tabs", "", nil, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ { // DEC-general-2..5
			if _, err := d.RememberDecision("foo", "alice", "", "filler decision number "+string(rune('a'+i))+" about lunch menus", "", nil, "", nil); err != nil {
				t.Fatal(err)
			}
		}
		b, err := d.RememberDecision("foo", "alice", "general", "Deploys on Fridays are forbidden", "", nil, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(a.Key, "DEC-general-") || !strings.HasPrefix(b.Key, "DEC-general-") {
			t.Fatalf("keys %s / %s, want the DEC-general family", a.Key, b.Key)
		}
		sweep(t, d)
		for _, c := range conflicts(t, d) {
			if strings.Contains(c.Members, a.ID) && strings.Contains(c.Members, b.ID) {
				t.Fatalf("%s / %s share only a key family, got conflict %+v", a.Key, b.Key, c)
			}
		}
		if e := edges(t, d); e[a.ID+">"+b.ID] != "" || e[b.ID+">"+a.ID] != "" {
			t.Fatalf("key family produced an edge: %v", e)
		}
	})

	t.Run("DeclaredSubjectMatches", func(t *testing.T) {
		d := testDB(t)
		write(t, d, "oncall-weekday", "pager rotates every monday morning", memOpt{subject: "oncall.rotation"})
		write(t, d, "oncall-policy", "escalations reach the secondary within ten minutes", memOpt{subject: "oncall.rotation"})
		sweep(t, d)
		c := conflicts(t, d)
		if len(c) != 1 || !strings.Contains(c[0].Evidence, `"signal":"subject"`) {
			t.Fatalf("declared subject: conflicts %+v, want 1 with signal subject", c)
		}
	})

	t.Run("DuplicateGroupOnce", func(t *testing.T) {
		d := testDB(t)
		write(t, d, "qa-submit-exe", "qa-submit requires the niwa executable built from the dot niwa source binary", memOpt{})
		write(t, d, "niwa-qa-exe-path", "qa-submit requires the niwa executable built from the dot niwa source binary path", memOpt{})
		sweep(t, d)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = d.EvaluateContradictions(time.Now())
			}()
		}
		wg.Wait()
		d.SetSetting(settingContradictionBackfill, "") // re-run the backfill too
		sweep(t, d)
		if c := conflicts(t, d); len(c) != 1 {
			t.Fatalf("duplicate pair gave %d conflict rows, want exactly 1", len(c))
		}
	})

	t.Run("WriteTimeHintNoWrite", func(t *testing.T) {
		d := testDB(t)
		old := write(t, d, "qa-submit-exe", "qa-submit requires the niwa executable built from the dot niwa source binary", memOpt{})
		fresh := write(t, d, "niwa-qa-binary", "qa-submit requires the niwa executable built from the dot niwa source binary path", memOpt{})
		changes := func() int64 {
			var n int64
			if err := d.conn.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		before := changes()
		ov, suggestion, err := d.OverlapHint(fresh)
		if err != nil {
			t.Fatalf("hint: %v", err)
		}
		if after := changes(); after != before {
			t.Fatalf("the D1 hint wrote: total_changes %d -> %d", before, after)
		}
		if len(ov) != 1 || ov[0].MemoryID != old || ov[0].Relation != "duplicate_candidate" {
			t.Fatalf("overlaps = %+v, want the old key as duplicate_candidate", ov)
		}
		if !strings.Contains(suggestion, "key=qa-submit-exe") || !strings.Contains(suggestion, "based_on="+old) {
			t.Fatalf("suggestion = %q", suggestion)
		}
	})

	t.Run("ClaimLeaseCAS", func(t *testing.T) {
		d := testDB(t)
		write(t, d, "foo-pg", "foo requires PostgreSQL 16", memOpt{subject: "pg"})
		write(t, d, "foo-pg-legacy", "foo requires PostgreSQL 14", memOpt{subject: "pg"})
		sweep(t, d)
		c := conflicts(t, d)
		if len(c) != 1 {
			t.Fatalf("setup: %d conflicts", len(c))
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for _, who := range []string{"alice", "bob", "carol"} {
			wg.Add(1)
			go func(who string) {
				defer wg.Done()
				ok, err := d.ClaimConflict(c[0].ID, who, time.Now())
				if err != nil {
					t.Errorf("claim %s: %v", who, err)
				}
				if ok {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(who)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d claimers won, want exactly 1", wins)
		}
	})

	t.Run("TwoFailedClaimsEscalateToException", func(t *testing.T) {
		d := testDB(t)
		write(t, d, "foo-pg", "foo requires PostgreSQL 16", memOpt{subject: "pg"})
		write(t, d, "foo-pg-legacy", "foo requires PostgreSQL 14", memOpt{subject: "pg"})
		sweep(t, d)
		id := conflicts(t, d)[0].ID
		now := time.Now()
		if ok, _ := d.ClaimConflict(id, "alice", now); !ok {
			t.Fatal("first claim refused")
		}
		if ok, _ := d.ReleaseConflict(id, "alice"); !ok { // "cannot": failed claim 1
			t.Fatal("release refused")
		}
		if ok, _ := d.ClaimConflict(id, "bob", now); !ok {
			t.Fatal("second claim refused")
		}
		// bob's lease lapses unresolved: failed claim 2, then escalation.
		rep, err := d.EvaluateContradictions(now.Add(3 * time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if rep.Expired != 1 || rep.Escalated != 1 {
			t.Fatalf("report = %+v, want 1 expired and 1 escalated", rep)
		}
		if c := conflicts(t, d)[0]; c.State != ConflictEscalated || c.Failed != 2 {
			t.Fatalf("conflict = %+v, want escalated after 2 failed claims", c)
		}
		var kind, source string
		if err := d.ro().QueryRow(`SELECT kind, source_kind FROM exceptions WHERE source_ref = ?`, id).Scan(&kind, &source); err != nil {
			t.Fatalf("no exception for the escalated conflict: %v", err)
		}
		if kind != "contradiction" || source != excSourceContradiction {
			t.Fatalf("exception kind=%s source=%s", kind, source)
		}
		var toUser int
		_ = d.ro().QueryRow(`SELECT COUNT(*) FROM messages WHERE to_agent = 'user'`).Scan(&toUser)
		if toUser != 0 {
			t.Fatalf("escalation messaged user %d times", toUser)
		}
		// A second tick does not escalate twice.
		if rep, _ := d.EvaluateContradictions(now.Add(4 * time.Hour)); rep.Escalated != 0 {
			t.Fatalf("re-escalated: %+v", rep)
		}
	})

	t.Run("ReplayProd", func(t *testing.T) {
		src := os.Getenv("RELAY_REPLAY_DB")
		if src == "" {
			home, _ := os.UserHomeDir()
			matches, _ := filepath.Glob(filepath.Join(home, ".agent-relay", "backups", "relay.2*.db"))
			sort.Strings(matches)
			if len(matches) > 0 {
				src = matches[len(matches)-1]
			}
		}
		if src == "" {
			t.Skip("no prod backup (set RELAY_REPLAY_DB)")
		}
		dst := filepath.Join(t.TempDir(), "replay.db")
		in, err := os.Open(src)
		if err != nil {
			t.Skipf("backup unreadable: %v", err)
		}
		out, _ := os.Create(dst)
		_, err = io.Copy(out, in)
		_ = in.Close()
		_ = out.Close()
		if err != nil {
			t.Fatalf("copy: %v", err)
		}
		d, err := NewTestDB(dst) // a COPY: migrations run on it, never on prod
		if err != nil {
			t.Fatalf("open copy: %v", err)
		}
		defer d.Close()
		rep, err := d.EvaluateContradictions(time.Now())
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		t.Logf("replay of %s: conflicts=%d edges=%d", filepath.Base(src), rep.Conflicts, rep.Edges)
		for _, p := range rep.Backfill {
			t.Logf("  %s", p)
		}
		hits := strings.Join(rep.Backfill, "\n")
		for _, pair := range [][2]string{
			{"qa-submit-niwa-exe-must-be-dot-niwa-src-bina", "qa-submit-niwa-exe-must-be-dot-niwa-src-bina"},
			{"DEC-general-4", "niwa-hook-live-patch-lost-on-provision"},
			{"niwa-no-git-amend-after-qa-submit", "niwa-scribe-commit-is-head-after-submit"},
			{"DEC-general-3", "DEC-general-4"},
			{"DEC-wraith-session-context-r2-1", "DEC-relay-session-context-1"},
		} {
			found := false
			for _, line := range rep.Backfill {
				if pairIn(line, pair[0], pair[1]) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("same-fact pair %s / %s not detected; hits:\n%s", pair[0], pair[1], hits)
			}
		}
	})
}

// pairIn reports whether a backfill line names both keys (by prefix: the
// design lists some keys truncated).
func pairIn(line, a, b string) bool {
	var keys []string
	for _, f := range strings.Fields(line) {
		f = strings.Trim(f, "()")
		keys = append(keys, f)
	}
	has := func(k string, skip int) int {
		for i, f := range keys {
			if i != skip && strings.HasPrefix(f, k) {
				return i
			}
		}
		return -1
	}
	i := has(a, -1)
	return i >= 0 && has(b, i) >= 0
}

// Conflict resolution, reversible invalidation, the sibling path and sampled
// audits (design 8d107daa T2, ruling ed744dee).
func TestContradictionResolution(t *testing.T) {
	// fixture: alice (profile dev, reports to boss, profile lead) resolves;
	// bob (profile ops) is a second resolver; cto is an executive; carol is
	// unrelated. a1 / a2 are two project constraints stating the same fact.
	type fixture struct {
		d      *DB
		a1, a2 string
	}
	now := time.Now().UTC()
	setup := func(t *testing.T) *fixture {
		t.Helper()
		d := testDB(t)
		seedAgent(t, d.conn, "foo", "alice", "active", "dev", "boss", 0)
		seedAgent(t, d.conn, "foo", "bob", "active", "ops", "", 0)
		seedAgent(t, d.conn, "foo", "boss", "active", "lead", "", 0)
		seedAgent(t, d.conn, "foo", "cto", "active", "cto", "", 0)
		seedAgent(t, d.conn, "foo", "carol", "active", "dev", "", 0)
		if _, err := d.conn.Exec(`UPDATE agents SET is_executive = 1 WHERE name = 'cto'`); err != nil {
			t.Fatal(err)
		}
		f := &fixture{d: d}
		m1, err := d.SetMemory("foo", "alice", "pg-version", "foo runs PostgreSQL 16", "[]", "project", "stated", "constraints")
		if err != nil {
			t.Fatal(err)
		}
		m2, err := d.SetMemory("foo", "bob", "foo-postgres", "foo runs PostgreSQL 16 in prod", "[]", "project", "stated", "constraints")
		if err != nil {
			t.Fatal(err)
		}
		f.a1, f.a2 = m1.ID, m2.ID
		return f
	}
	// open inserts a detected candidate conflict over ids.
	open := func(t *testing.T, d *DB, ids ...string) string {
		t.Helper()
		members := append([]string(nil), ids...)
		sort.Strings(members)
		mj, _ := json.Marshal(members)
		id := "c-" + strings.Join(members, "-")[:12] + "-" + time.Now().Format("150405.000000000")
		if _, err := d.conn.Exec(`INSERT INTO knowledge_conflicts (id, project, members, members_hash, kind, gate_evidence, detected_by, state, created_at)
			VALUES (?, 'foo', ?, ?, 'candidate', '{}', 'relay-sweeper', 'detected', ?)`, id, string(mj), membersHash(members)+id, now.Format(memoryTimeFmt)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	claim := func(t *testing.T, d *DB, id, agent string) {
		t.Helper()
		if ok, err := d.ClaimConflict(id, agent, now); err != nil || !ok {
			t.Fatalf("claim %s by %s: %v %v", id, agent, ok, err)
		}
	}
	resolve := func(d *DB, id, agent, res, keep string) (*ConflictOutcome, error) {
		return d.ResolveKnowledgeConflict(ConflictResolution{ID: id, Agent: agent, Resolution: res, Keep: keep, Rationale: "same fact", Now: now})
	}
	// rowOf is every memories column but invalidated_by, as one string.
	rowOf := func(t *testing.T, d *DB, id string) string {
		t.Helper()
		var s string
		if err := d.conn.QueryRow(`SELECT id || '|' || key || '|' || value || '|' || tags || '|' || scope || '|' || project || '|' ||
				agent_name || '|' || confidence || '|' || version || '|' || COALESCE(supersedes, '') || '|' || COALESCE(conflict_with, '') || '|' ||
				created_at || '|' || updated_at || '|' || COALESCE(archived_at, '') || '|' || COALESCE(archived_by, '') || '|' ||
				COALESCE(archived_reason, '') || '|' || layer || '|' || COALESCE(valid_from, '') || '|' || COALESCE(valid_until, '') || '|' || status
			FROM memories WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("row %s: %v", id, err)
		}
		return s
	}
	count := func(t *testing.T, d *DB, q string, args ...any) int {
		t.Helper()
		var n int
		if err := d.conn.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	head := func(t *testing.T, d *DB) int {
		return count(t, d, `SELECT COALESCE(MAX(rev), 0) FROM knowledge_log`)
	}

	t.Run("MergeArchivesWithInvalidatedByAndLogsRetraction", func(t *testing.T) {
		f := setup(t)
		// carol served a2 at boot: the retraction must reach her.
		if _, _, err := f.d.RecordSnapshot("foo", "carol", "", SnapshotBoot, []string{f.a2}); err != nil {
			t.Fatal(err)
		}
		cid := open(t, f.d, f.a1, f.a2)
		claim(t, f.d, cid, "alice")
		out, err := resolve(f.d, cid, "alice", ResolutionMerge, f.a1)
		if err != nil || out.State != ConflictResolved || out.Kept != f.a1 || len(out.Archived) != 1 || out.Archived[0] != f.a2 {
			t.Fatalf("merge = %+v, %v", out, err)
		}
		var status, inv, reason string
		if err := f.d.conn.QueryRow(`SELECT status, COALESCE(invalidated_by, ''), COALESCE(archived_reason, '') FROM memories WHERE id = ?`, f.a2).
			Scan(&status, &inv, &reason); err != nil || status != "archived" || inv != cid || reason != "conflict:merge" {
			t.Fatalf("loser = %s / invalidated_by %q / %q (%v), want archived / %s", status, inv, reason, err, cid)
		}
		if memRow(t, f.d, f.a1).ArchivedAt != nil {
			t.Fatal("kept member archived")
		}
		var class string
		if err := f.d.conn.QueryRow(`SELECT change_class FROM knowledge_log WHERE op = 'retract' AND prev_memory_id = ?`, f.a2).Scan(&class); err != nil || class != ChangeRetraction {
			t.Fatalf("retraction row class %q (%v)", class, err)
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM knowledge_edges WHERE src_memory_id = ? AND dst_memory_id = ? AND kind = 'supersedes'
			AND declared = 1 AND rule = ?`, f.a1, f.a2, "resolution:"+cid); n != 1 {
			t.Fatalf("supersedes edge = %d, want 1", n)
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM knowledge_conflicts WHERE id = ? AND state = 'resolved' AND resolution = 'merge'
			AND resolved_by = 'alice' AND resolution_memory_id = ? AND lease_holder IS NULL`, cid, f.a1); n != 1 {
			t.Fatal("conflict row not resolved as merge by alice")
		}
		rep, err := f.d.EvaluateCoherence(now)
		if err != nil || rep.Rollouts != 1 || rep.Opened != 1 {
			t.Fatalf("coherence after the merge = %+v (%v), want 1 rollout reaching carol", rep, err)
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM obligations WHERE norm_id = ? AND bearer = 'carol' AND state = 'active'`, NormCoherenceReassess); n != 1 {
			t.Fatalf("carol's reassess obligations = %d, want 1", n)
		}
	})

	t.Run("OnlyLeaseHolderResolves", func(t *testing.T) {
		f := setup(t)
		cid := open(t, f.d, f.a1, f.a2)
		before := head(t, f.d)
		if _, err := resolve(f.d, cid, "alice", ResolutionMerge, f.a1); !errors.Is(err, ErrConflictNotHeld) {
			t.Fatalf("unclaimed resolve err = %v", err)
		}
		claim(t, f.d, cid, "alice")
		if ok, _ := f.d.ClaimConflict(cid, "bob", now); ok {
			t.Fatal("bob claimed a leased conflict")
		}
		for _, who := range []string{"bob", "carol"} {
			if _, err := resolve(f.d, cid, who, ResolutionMerge, f.a1); !errors.Is(err, ErrConflictNotHeld) {
				t.Fatalf("%s resolve err = %v, want ErrConflictNotHeld", who, err)
			}
		}
		if _, err := f.d.ResolveKnowledgeConflict(ConflictResolution{ID: cid, Agent: "alice", Resolution: ResolutionMerge, Keep: f.a1,
			Now: now.Add(3 * time.Hour)}); !errors.Is(err, ErrConflictNotHeld) {
			t.Fatalf("expired-lease resolve err = %v", err)
		}
		if _, err := resolve(f.d, cid, "alice", ResolutionMerge, "not-a-member"); !errors.Is(err, ErrConflictResolution) {
			t.Fatalf("keep outside members err = %v", err)
		}
		if head(t, f.d) != before || count(t, f.d, `SELECT COUNT(*) FROM memories WHERE archived_at IS NOT NULL`) != 0 ||
			count(t, f.d, `SELECT COUNT(*) FROM knowledge_conflicts WHERE id = ? AND state = 'claimed' AND lease_holder = 'alice'`, cid) != 1 {
			t.Fatal("a refused resolve wrote something")
		}
	})

	t.Run("RevertRestoresExactRowAndLogsSet", func(t *testing.T) {
		f := setup(t)
		orig := rowOf(t, f.d, f.a2)
		cid := open(t, f.d, f.a1, f.a2)
		claim(t, f.d, cid, "alice")
		if _, err := resolve(f.d, cid, "alice", ResolutionSupersede, f.a1); err != nil {
			t.Fatal(err)
		}
		before := head(t, f.d)
		out, err := f.d.RevertConflict(cid, "boss", now)
		if err != nil || len(out.Restored) != 1 || out.Restored[0] != f.a2 || out.State != ConflictDetected {
			t.Fatalf("revert = %+v, %v", out, err)
		}
		if got := rowOf(t, f.d, f.a2); got != orig {
			t.Fatalf("restored row differs:\n got %s\nwant %s", got, orig)
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM memories WHERE id = ? AND invalidated_by = ?`, f.a2, cid); n != 1 {
			t.Fatal("invalidated_by not kept for history")
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM knowledge_log WHERE rev > ? AND op = 'set' AND memory_id = ?`, before, f.a2); n != 1 {
			t.Fatalf("set rows for the restore = %d, want 1", n)
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM knowledge_edges WHERE rule = ?`, "resolution:"+cid); n != 0 {
			t.Fatal("resolution edges kept after revert")
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM knowledge_conflicts WHERE id = ? AND state = 'detected' AND audit = 'reverted' AND resolution IS NULL`, cid); n != 1 {
			t.Fatal("conflict not back to detected / reverted")
		}

		// The legacy key/chosen_value path closes the validity window; revert
		// gives it back too.
		s1, _ := f.d.SetMemory("foo", "alice", "k", "one", "[]", "project", "stated", "behavior")
		s2, _ := f.d.SetMemoryWith("foo", "bob", "k", "two", "[]", "project", "stated", "behavior", true, SetMemoryOpts{BasedOn: BasedOnNew})
		origS2 := rowOf(t, f.d, s2.ID)
		if _, err := f.d.ResolveConflict("foo", "alice", "k", "one", "project"); err != nil {
			t.Fatal(err)
		}
		var legacy string
		if err := f.d.conn.QueryRow(`SELECT COALESCE(invalidated_by, '') FROM memories WHERE id = ?`, s2.ID).Scan(&legacy); err != nil || legacy == "" {
			t.Fatalf("legacy archive without invalidated_by (%v)", err)
		}
		if _, err := f.d.RevertConflict(legacy, "cto", now); err != nil {
			t.Fatal(err)
		}
		if got := rowOf(t, f.d, s2.ID); got != origS2 {
			t.Fatalf("legacy restore differs:\n got %s\nwant %s", got, origS2)
		}
		if memRow(t, f.d, s1.ID).ArchivedAt != nil {
			t.Fatal("legacy winner archived")
		}
	})

	t.Run("RevertAuthority", func(t *testing.T) {
		f := setup(t)
		cid := open(t, f.d, f.a1, f.a2)
		claim(t, f.d, cid, "alice")
		out, err := resolve(f.d, cid, "alice", ResolutionMerge, f.a1)
		if err != nil {
			t.Fatal(err)
		}
		// The first archiving resolution is sampled; its auditor is alice's
		// lead (profile lead != dev).
		if out.Audit != AuditSampled || out.Auditor != "boss" || out.AuditorProfile != "lead" {
			t.Fatalf("audit = %q auditor %q/%q, want sampled boss/lead", out.Audit, out.Auditor, out.AuditorProfile)
		}
		before := head(t, f.d)
		for _, who := range []string{"alice", "carol", "bob"} {
			if _, err := f.d.RevertConflict(cid, who, now); !errors.Is(err, ErrRevertNotAllowed) {
				t.Fatalf("%s revert err = %v, want ErrRevertNotAllowed", who, err)
			}
		}
		if head(t, f.d) != before || memRow(t, f.d, f.a2).ArchivedAt == nil {
			t.Fatal("a refused revert wrote something")
		}
		if _, err := f.d.RevertConflict(cid, "cto", now); err != nil {
			t.Fatalf("executive revert: %v", err)
		}
		if _, err := f.d.RevertConflict(cid, "cto", now); !errors.Is(err, ErrConflictNotResolved) {
			t.Fatalf("second revert err = %v", err)
		}

		// The auditor profile (not the lead) may revert: bob resolves, his
		// auditor is an executive of another profile (cto).
		claim(t, f.d, cid, "bob")
		if _, err := f.d.conn.Exec(`UPDATE settings SET value = '0' WHERE key = ?`, settingArchiveSeq); err != nil {
			t.Fatal(err)
		}
		out, err = resolve(f.d, cid, "bob", ResolutionMerge, f.a2)
		if err != nil || out.Audit != AuditSampled || out.AuditorProfile != "cto" {
			t.Fatalf("bob's merge = %+v, %v", out, err)
		}
		seedAgent(t, f.d.conn, "foo", "cto-2", "active", "cto", "", 0)
		if _, err := f.d.RevertConflict(cid, "cto-2", now); err != nil {
			t.Fatalf("auditor-profile revert: %v", err)
		}
	})

	t.Run("SiblingWritesConflictRowNotConflictWith", func(t *testing.T) {
		d := testDB(t)
		v1, _ := d.SetMemory("foo", "a", "k", "one", "[]", "project", "stated", "behavior")
		v2, err := d.SetMemoryWith("foo", "b", "k", "two", "[]", "project", "stated", "behavior", true, SetMemoryOpts{BasedOn: BasedOnNew})
		if err != nil || v2.ConflictWith == nil || *v2.ConflictWith != v1.ID {
			t.Fatalf("sibling result = %+v, %v (callers still see conflict_with)", v2, err)
		}
		if n := count(t, d, `SELECT COUNT(*) FROM memories WHERE conflict_with IS NOT NULL`); n != 0 {
			t.Fatalf("conflict_with written on %d rows", n)
		}
		want, _ := json.Marshal(func() []string { s := []string{v1.ID, v2.ID}; sort.Strings(s); return s }())
		if n := count(t, d, `SELECT COUNT(*) FROM knowledge_conflicts WHERE kind = 'sibling' AND state = 'detected' AND members = ?`, string(want)); n != 1 {
			t.Fatal("no sibling conflict row with both members")
		}
		// A third live value joins the same row; upsert=false does the same.
		v3, _ := d.SetMemoryWith("foo", "c", "k", "three", "[]", "project", "stated", "behavior", true, SetMemoryOpts{BasedOn: BasedOnNew})
		v4, _ := d.SetMemory("foo", "e", "k", "four", "[]", "project", "stated", "behavior", false)
		var members string
		if err := d.conn.QueryRow(`SELECT members FROM knowledge_conflicts WHERE kind = 'sibling'`).Scan(&members); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{v1.ID, v2.ID, v3.ID, v4.ID} {
			if !strings.Contains(members, id) {
				t.Fatalf("members %s miss %s", members, id)
			}
		}
		if n := count(t, d, `SELECT COUNT(*) FROM knowledge_conflicts`); n != 1 {
			t.Fatalf("conflict rows = %d, want one group", n)
		}
		if _, c, err := d.MemoryStats("foo"); err != nil || c != 3 {
			t.Fatalf("MemoryStats conflicts = %d (%v), want 3", c, err)
		}
		// The legacy resolve closes the group.
		if _, err := d.ResolveConflict("foo", "a", "k", "three", "project"); err != nil {
			t.Fatal(err)
		}
		if n := count(t, d, `SELECT COUNT(*) FROM knowledge_conflicts WHERE state = 'resolved' AND resolution = 'supersede' AND resolution_memory_id = ?`, v3.ID); n != 1 {
			t.Fatal("legacy resolve left the sibling conflict open")
		}
	})

	t.Run("AuditSampledAndPrecisionGateDowngrades", func(t *testing.T) {
		f := setup(t)
		f.d.SetSetting(settingAuditRate, "1") // audit every one: five audits
		pair := func(i int) (string, string) {
			x, _ := f.d.SetMemory("foo", "alice", "dup-a-"+strconv.Itoa(i), "fact number "+strconv.Itoa(i), "[]", "project", "stated", "constraints")
			y, _ := f.d.SetMemory("foo", "bob", "dup-b-"+strconv.Itoa(i), "fact number "+strconv.Itoa(i)+" again", "[]", "project", "stated", "constraints")
			return x.ID, y.ID
		}
		for i := 0; i < 5; i++ {
			x, y := pair(i)
			cid := open(t, f.d, x, y)
			claim(t, f.d, cid, "alice")
			out, err := resolve(f.d, cid, "alice", ResolutionMerge, x)
			if err != nil || out.Audit != AuditSampled || out.Auditor != "boss" {
				t.Fatalf("merge %d = %+v, %v", i, out, err)
			}
			if i < 3 {
				if up, err := resolve(f.d, cid, "boss", ResolutionMerge, x); err != nil || up.Audit != AuditUpheld {
					t.Fatalf("uphold %d = %+v, %v", i, up, err)
				}
			} else if _, err := f.d.RevertConflict(cid, "boss", now); err != nil {
				t.Fatalf("revert %d: %v", i, err)
			}
		}
		p, n, err := precisionTx(f.d.conn, "dev", "constraints", now.Add(-precisionWindow).Format(memoryTimeFmt))
		if err != nil || n != 5 || p != 0.6 {
			t.Fatalf("precision = %v over %d (%v), want 0.6 over 5", p, n, err)
		}

		// alice's next merge on constraints is only a proposal.
		cid := open(t, f.d, f.a1, f.a2)
		claim(t, f.d, cid, "alice")
		before := head(t, f.d)
		out, err := resolve(f.d, cid, "alice", ResolutionMerge, f.a1)
		if err != nil || !out.Proposed || out.State != ConflictDetected || len(out.Archived) != 0 {
			t.Fatalf("gated merge = %+v, %v, want a proposal", out, err)
		}
		if head(t, f.d) != before || memRow(t, f.d, f.a2).ArchivedAt != nil {
			t.Fatal("a proposal archived something")
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM knowledge_conflicts WHERE id = ? AND failed_claims = 0
			AND json_extract(gate_evidence, '$.proposal.by') = 'alice'`, cid); n != 1 {
			t.Fatal("proposal not recorded (or counted as a failed claim)")
		}
		// Archiving-free resolutions are not gated.
		claim(t, f.d, cid, "alice")
		if _, err := resolve(f.d, cid, "alice", ResolutionMerge, f.a1); !errors.Is(err, ErrConcurrenceSelf) {
			t.Fatalf("self-concurrence err = %v", err)
		}
		if ok, _ := f.d.ReleaseConflict(cid, "alice"); !ok {
			t.Fatal("release")
		}
		// bob's matching resolve is the second concurrence: now it archives.
		claim(t, f.d, cid, "bob")
		out, err = resolve(f.d, cid, "bob", ResolutionMerge, f.a1)
		if err != nil || out.State != ConflictResolved || len(out.Archived) != 1 {
			t.Fatalf("concurrence = %+v, %v", out, err)
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM knowledge_conflicts WHERE id = ? AND json_extract(gate_evidence, '$.concurred_by') = 'bob'`, cid); n != 1 {
			t.Fatal("concurrence not recorded")
		}
		// Without the setting the rate is 1 in 3 below 30 audits.
		f.d.SetSetting(settingAuditRate, "")
		sampled := 0
		for i := 10; i < 16; i++ {
			x, y := pair(i)
			c := open(t, f.d, x, y)
			claim(t, f.d, c, "bob")
			o, err := resolve(f.d, c, "bob", ResolutionMerge, x)
			if err != nil {
				t.Fatal(err)
			}
			if o.Audit == AuditSampled {
				sampled++
			}
		}
		if sampled != 2 {
			t.Fatalf("sampled %d of 6 at the early rate, want 2", sampled)
		}
	})

	t.Run("NothingArchivedWithoutReversibleRecord", func(t *testing.T) {
		f := setup(t)
		mk := func(k, v, agent string) string {
			m, err := f.d.SetMemory("foo", agent, k, v, "[]", "project", "stated", "constraints")
			if err != nil {
				t.Fatal(err)
			}
			return m.ID
		}
		for _, res := range []string{ResolutionMerge, ResolutionSupersede, ResolutionRejectNew} {
			x, y := mk(res+"-x", "value "+res, "alice"), mk(res+"-y", "value "+res+" too", "bob")
			cid := open(t, f.d, x, y)
			claim(t, f.d, cid, "alice")
			if _, err := resolve(f.d, cid, "alice", res, x); err != nil {
				t.Fatalf("%s: %v", res, err)
			}
		}
		// Legacy: winner found, and neither matched (a new resolution row).
		for i, chosen := range []string{"one", "brand new"} {
			k := "legacy-" + strconv.Itoa(i)
			_, _ = f.d.SetMemory("foo", "alice", k, "one", "[]", "project", "stated", "behavior")
			_, _ = f.d.SetMemoryWith("foo", "bob", k, "two", "[]", "project", "stated", "behavior", true, SetMemoryOpts{BasedOn: BasedOnNew})
			if _, err := f.d.ResolveConflict("foo", "alice", k, chosen, "project"); err != nil {
				t.Fatal(err)
			}
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM memories WHERE archived_at IS NOT NULL AND archived_reason LIKE 'conflict%'`); n != 6 {
			t.Fatalf("conflict archives = %d, want 6 (merge, supersede, reject_new, legacy 1 + 2)", n)
		}
		if n := count(t, f.d, `SELECT COUNT(*) FROM memories m WHERE m.archived_at IS NOT NULL AND m.archived_reason LIKE 'conflict%'
			AND (m.invalidated_by IS NULL OR NOT EXISTS (SELECT 1 FROM knowledge_conflicts c WHERE c.id = m.invalidated_by
			  AND json_extract(c.gate_evidence, '$.prior."' || m.id || '"') IS NOT NULL))`); n != 0 {
			t.Fatalf("%d archive(s) without a reversible record", n)
		}
	})
}
