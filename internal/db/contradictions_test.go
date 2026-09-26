package db

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
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
