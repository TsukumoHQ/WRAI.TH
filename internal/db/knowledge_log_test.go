package db

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

type klRow struct {
	Rev                         int64
	Project, Scope, Key, Author string
	MemoryID, PrevMemoryID      string
	LiveIDs                     string
	Op, Layer                   string
	Durability                  int
	Class, Declared, Causal     string
}

func klRows(t *testing.T, d *DB) []klRow {
	t.Helper()
	rows, err := d.conn.Query(`SELECT rev, project, scope, key, author, COALESCE(memory_id,''), COALESCE(prev_memory_id,''), live_ids,
		op, layer, durability, change_class, COALESCE(declared_class,''), COALESCE(causal,'') FROM knowledge_log ORDER BY rev`)
	if err != nil {
		t.Fatalf("read knowledge_log: %v", err)
	}
	defer rows.Close()
	var out []klRow
	for rows.Next() {
		var r klRow
		if err := rows.Scan(&r.Rev, &r.Project, &r.Scope, &r.Key, &r.Author, &r.MemoryID, &r.PrevMemoryID, &r.LiveIDs,
			&r.Op, &r.Layer, &r.Durability, &r.Class, &r.Declared, &r.Causal); err != nil {
			t.Fatalf("scan knowledge_log: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func klLast(t *testing.T, d *DB) klRow {
	t.Helper()
	rs := klRows(t, d)
	if len(rs) == 0 {
		t.Fatal("knowledge_log is empty")
	}
	return rs[len(rs)-1]
}

func klCount(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.conn.QueryRow(`SELECT count(*) FROM knowledge_log`).Scan(&n); err != nil {
		t.Fatalf("count knowledge_log: %v", err)
	}
	return n
}

func klClock(t *testing.T, d *DB, project string) map[int]int64 {
	t.Helper()
	rows, err := d.conn.Query(`SELECT durability, last_changed_rev FROM knowledge_clock WHERE project = ?`, project)
	if err != nil {
		t.Fatalf("read clock: %v", err)
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var dur int
		var rev int64
		if err := rows.Scan(&dur, &rev); err != nil {
			t.Fatalf("scan clock: %v", err)
		}
		out[dur] = rev
	}
	return out
}

func klSet(t *testing.T, d *DB, key, value, scope, layer string, opts SetMemoryOpts, upsert ...bool) string {
	t.Helper()
	up := true
	if len(upsert) > 0 {
		up = upsert[0]
	}
	m, err := d.SetMemoryWith("p1", "dev-a", key, value, "[]", scope, "stated", layer, up, opts)
	if err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
	return m.ID
}

// klDelta runs fn and asserts it appended exactly want rows.
func klDelta(t *testing.T, d *DB, name string, want int, fn func() error) {
	t.Helper()
	before := klCount(t, d)
	if err := fn(); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if got := klCount(t, d) - before; got != want {
		t.Fatalf("%s: knowledge_log grew by %d, want %d", name, got, want)
	}
}

func TestKnowledgeLog(t *testing.T) {
	t.Run("EveryWritePathLogsExactlyOneRow", func(t *testing.T) {
		d := testDB(t)
		var first string
		klDelta(t, d, "K1 fresh", 1, func() error {
			m, err := d.SetMemory("p1", "dev-a", "k", "v1", "[]", "project", "stated", "behavior")
			first = m.ID
			return err
		})
		if r := klLast(t, d); r.Op != opSet {
			t.Fatalf("fresh op = %s", r.Op)
		}
		klDelta(t, d, "K1 touch", 1, func() error {
			_, err := d.SetMemory("p1", "dev-a", "k", "v1", "[]", "project", "stated", "behavior")
			return err
		})
		if r := klLast(t, d); r.Op != opTouch {
			t.Fatalf("touch op = %s", r.Op)
		}
		klDelta(t, d, "K1 supersede", 1, func() error {
			_, err := d.SetMemory("p1", "dev-a", "k", "v2", "[]", "project", "stated", "behavior")
			return err
		})
		if r := klLast(t, d); r.Op != opSupersede {
			t.Fatalf("supersede op = %s", r.Op)
		}
		klDelta(t, d, "K1 conflict", 1, func() error {
			_, err := d.SetMemory("p1", "dev-a", "k", "v3", "[]", "project", "stated", "behavior", false)
			return err
		})
		if r := klLast(t, d); r.Op != opConflict {
			t.Fatalf("conflict op = %s", r.Op)
		}
		klDelta(t, d, "K1 sibling", 1, func() error {
			_, err := d.SetMemoryWith("p1", "dev-a", "k", "v4", "[]", "project", "stated", "behavior", true, SetMemoryOpts{BasedOn: first})
			return err
		})
		if r := klLast(t, d); r.Op != opSibling {
			t.Fatalf("sibling op = %s", r.Op)
		}
		klDelta(t, d, "K5 resolve", 1, func() error {
			_, err := d.ResolveConflict("p1", "dev-a", "k", "v4", "project")
			return err
		})
		if r := klLast(t, d); r.Op != opResolve {
			t.Fatalf("resolve op = %s", r.Op)
		}
		klDelta(t, d, "K2 validity", 1, func() error {
			return d.SetMemoryValidity("p1", "dev-a", "k", "project", "", "2030-01-01T00:00:00Z")
		})
		klDelta(t, d, "K3 delete", 1, func() error { return d.DeleteMemory("p1", "dev-a", "k", "project") })
		var byID string
		klDelta(t, d, "K1 fresh #2", 1, func() error {
			m, err := d.SetMemory("p1", "dev-a", "k2", "x", "[]", "project", "stated", "context")
			byID = m.ID
			return err
		})
		klDelta(t, d, "K4 delete by id", 1, func() error { return d.DeleteMemoryByID(byID, "human") })
		var dec string
		klDelta(t, d, "remember", 1, func() error {
			m, err := d.RememberDecision("p1", "cto", "db", "use sqlite", "", nil, "", nil)
			dec = m.Key
			return err
		})
		// K6 + K1: two writes (the retraction and the successor), one tx.
		klDelta(t, d, "remember supersede", 2, func() error {
			_, err := d.RememberDecision("p1", "cto", "db", "use postgres", "", nil, dec, nil)
			return err
		})
		rs := klRows(t, d)
		if k6 := rs[len(rs)-2]; k6.Op != opRetract || k6.Key != dec || k6.Class != ChangeRetraction {
			t.Fatalf("K6 row = %+v, want retraction of %s", k6, dec)
		}
		before := klCount(t, d)
		_, err := d.SetMemoryWith("p1", "dev-a", "k3", "v", "[]", "project", "stated", "behavior", true, SetMemoryOpts{BasedOn: "no-such-id"})
		if !errors.Is(err, ErrBasedOnMismatch) {
			t.Fatalf("based_on mismatch err = %v", err)
		}
		if klCount(t, d) != before {
			t.Fatal("a rolled-back write (ErrBasedOnMismatch) left a log row")
		}
	})

	t.Run("RevMonotonicAcrossProjects", func(t *testing.T) {
		d := testDB(t)
		for _, p := range []string{"p1", "p2", "p1", "p3"} {
			if _, err := d.SetMemory(p, "dev-a", "k-"+p, time.Now().String(), "[]", "project", "stated", "behavior"); err != nil {
				t.Fatalf("set: %v", err)
			}
		}
		if _, err := d.SetMemory("p2", "dev-a", "g", "v", "[]", "global", "stated", "behavior"); err != nil {
			t.Fatalf("set global: %v", err)
		}
		rs := klRows(t, d)
		for i := 1; i < len(rs); i++ {
			if rs[i].Rev <= rs[i-1].Rev {
				t.Fatalf("rev %d after %d: not monotonic", rs[i].Rev, rs[i-1].Rev)
			}
		}
	})

	t.Run("RememberDecisionOneTx", func(t *testing.T) {
		d := testDB(t)
		old, err := d.RememberDecision("p1", "cto", "db", "use sqlite", "", nil, "", nil)
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		// Fail the successor's INSERT (after the retraction ran in the same tx).
		if _, err := d.conn.Exec(`CREATE TRIGGER kl_fail BEFORE INSERT ON memories WHEN NEW.layer = 'decision'
			BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
			t.Fatalf("trigger: %v", err)
		}
		before := klCount(t, d)
		if _, err := d.RememberDecision("p1", "cto", "db", "use postgres", "", nil, old.Key, nil); err == nil {
			t.Fatal("RememberDecision succeeded despite the forced INSERT failure")
		}
		var live int
		_ = d.conn.QueryRow(`SELECT count(*) FROM memories WHERE key = ? AND archived_at IS NULL`, old.Key).Scan(&live)
		if live != 1 {
			t.Fatalf("old decision live rows = %d, want 1 (retraction rolled back with the failed successor)", live)
		}
		if klCount(t, d) != before {
			t.Fatalf("log grew by %d on a failed remember, want 0", klCount(t, d)-before)
		}
	})

	t.Run("EditorialDoesNotAdvanceClock", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "k", "hello world", "project", "behavior", SetMemoryOpts{})
		clock := klClock(t, d, "p1")
		klSet(t, d, "k", "  hello   world ", "project", "behavior", SetMemoryOpts{})
		if r := klLast(t, d); r.Class != ChangeEditorial {
			t.Fatalf("whitespace-only class = %s, want editorial", r.Class)
		}
		if _, err := d.SetMemoryWith("p1", "dev-a", "k", "  hello   world ", `["t"]`, "project", "stated", "behavior", true, SetMemoryOpts{}); err != nil {
			t.Fatalf("tags-only: %v", err)
		}
		if r := klLast(t, d); r.Class != ChangeEditorial || r.Op != opSupersede {
			t.Fatalf("tags-only row = %+v, want editorial supersede", r)
		}
		if got := klClock(t, d, "p1"); len(got) != len(clock) || got[1] != clock[1] || got[0] != clock[0] {
			t.Fatalf("clock moved on editorial rows: %v -> %v", clock, got)
		}
	})

	t.Run("NormalizedEqualForcesEditorialOverDeclaredBreaking", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "k", "a b", "project", "behavior", SetMemoryOpts{})
		klSet(t, d, "k", "a  b", "project", "behavior", SetMemoryOpts{ChangeClass: ChangeBreaking})
		if r := klLast(t, d); r.Class != ChangeEditorial || r.Declared != ChangeBreaking {
			t.Fatalf("row = class %s declared %s, want editorial/breaking", r.Class, r.Declared)
		}
	})

	t.Run("LayerUpToConstraintsAtLeastBreaking", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "k", "v", "project", "context", SetMemoryOpts{})
		klSet(t, d, "k", "v", "project", "constraints", SetMemoryOpts{})
		if r := klLast(t, d); r.Class != ChangeBreaking {
			t.Fatalf("context->constraints same value: class %s, want breaking", r.Class)
		}
	})

	t.Run("LayerDownAtLeastNarrowing", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "k", "v", "project", "constraints", SetMemoryOpts{})
		klSet(t, d, "k", "v", "project", "context", SetMemoryOpts{ChangeClass: ChangeEditorial})
		if r := klLast(t, d); r.Class != ChangeNarrowing {
			t.Fatalf("constraints->context: class %s, want narrowing", r.Class)
		}
	})

	t.Run("UndeclaredHighIsBreakingLowIsAdditive", func(t *testing.T) {
		d := testDB(t)
		for layer, want := range map[string]string{"constraints": ChangeBreaking, "decision": ChangeBreaking, "behavior": ChangeAdditive, "context": ChangeAdditive} {
			klSet(t, d, "k-"+layer, "v", "project", layer, SetMemoryOpts{})
			if r := klLast(t, d); r.Class != want || r.Declared != "" {
				t.Fatalf("%s undeclared: class %s declared %q, want %s", layer, r.Class, r.Declared, want)
			}
		}
	})

	t.Run("DeclaredBelowFloorRaisedAndDeclaredKept", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "k1", "v", "project", "constraints", SetMemoryOpts{ChangeClass: ChangeAdditive})
		if r := klLast(t, d); r.Class != ChangeBreaking || r.Declared != ChangeAdditive {
			t.Fatalf("declared additive on HIGH: %s/%s, want breaking/additive", r.Class, r.Declared)
		}
		klSet(t, d, "k2", "v", "project", "context", SetMemoryOpts{ChangeClass: ChangeNarrowing})
		if r := klLast(t, d); r.Class != ChangeNarrowing {
			t.Fatalf("declared narrowing above LOW floor: %s, want kept narrowing", r.Class)
		}
		if _, err := d.SetMemoryWith("p1", "dev-a", "k3", "v", "[]", "project", "stated", "behavior", true, SetMemoryOpts{ChangeClass: "retraction"}); !errors.Is(err, ErrInvalidChangeClass) {
			t.Fatalf("declared retraction err = %v, want ErrInvalidChangeClass", err)
		}
	})

	t.Run("SiblingAndResolveAtLeastBreaking", func(t *testing.T) {
		d := testDB(t)
		first := klSet(t, d, "k", "v1", "project", "context", SetMemoryOpts{})
		klSet(t, d, "k", "v2", "project", "context", SetMemoryOpts{})
		klSet(t, d, "k", "v3", "project", "context", SetMemoryOpts{BasedOn: first})
		if r := klLast(t, d); r.Op != opSibling || r.Class != ChangeBreaking {
			t.Fatalf("sibling row = %s/%s, want sibling/breaking", r.Op, r.Class)
		}
		if _, err := d.ResolveConflict("p1", "dev-a", "k", "v3", "project"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if r := klLast(t, d); r.Op != opResolve || r.Class != ChangeBreaking {
			t.Fatalf("resolve row = %s/%s, want resolve/breaking", r.Op, r.Class)
		}
	})

	t.Run("DeleteIsRetraction", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "k", "v", "project", "context", SetMemoryOpts{})
		if err := d.DeleteMemory("p1", "dev-a", "k", "project"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		r := klLast(t, d)
		if r.Op != opRetract || r.Class != ChangeRetraction || r.LiveIDs != "[]" {
			t.Fatalf("delete row = %+v, want retract/retraction with empty live set", r)
		}
	})

	t.Run("ValidityShrinkNarrowingWidenAdditive", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "k", "v", "project", "behavior", SetMemoryOpts{})
		steps := []struct{ until, want string }{
			{"2030-06-01T00:00:00Z", ChangeNarrowing}, // an end appears
			{"2030-01-01T00:00:00Z", ChangeNarrowing}, // moves earlier
			{"2031-01-01T00:00:00Z", ChangeAdditive},  // moves later
			{"", ChangeAdditive},                      // cleared
		}
		for _, s := range steps {
			if err := d.SetMemoryValidity("p1", "dev-a", "k", "project", "", s.until); err != nil {
				t.Fatalf("validity %q: %v", s.until, err)
			}
			if r := klLast(t, d); r.Op != opValidity || r.Class != s.want {
				t.Fatalf("validity %q: %s/%s, want validity/%s", s.until, r.Op, r.Class, s.want)
			}
		}
		// A start moved later shrinks the window too.
		if err := d.SetMemoryValidity("p1", "dev-a", "k", "project", "2099-01-01T00:00:00Z", ""); err != nil {
			t.Fatalf("validity from: %v", err)
		}
		if r := klLast(t, d); r.Class != ChangeNarrowing {
			t.Fatalf("start moved later: %s, want %s", r.Class, ChangeNarrowing)
		}
	})

	t.Run("CausalColumnMatchesOpts", func(t *testing.T) {
		d := testDB(t)
		id := klSet(t, d, "k", "v1", "project", "behavior", SetMemoryOpts{})
		if r := klLast(t, d); r.Causal != "none" {
			t.Fatalf("no based_on: causal %q, want none", r.Causal)
		}
		klSet(t, d, "k", "v2", "project", "behavior", SetMemoryOpts{BasedOn: id})
		if r := klLast(t, d); r.Causal != "arg" {
			t.Fatalf("based_on set: causal %q, want arg", r.Causal)
		}
		klSet(t, d, "k2", "v", "project", "behavior", SetMemoryOpts{BasedOn: BasedOnNew, Causal: "cache"})
		if r := klLast(t, d); r.Causal != "cache" {
			t.Fatalf("explicit cache: causal %q, want cache", r.Causal)
		}
	})

	t.Run("ClockBumpsAllLowerDurabilities", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "hi", "v", "project", "constraints", SetMemoryOpts{})
		hi := klLast(t, d).Rev
		if c := klClock(t, d, "p1"); c[0] != hi || c[1] != hi || c[2] != hi {
			t.Fatalf("HIGH write clock = %v, want all three at %d", c, hi)
		}
		klSet(t, d, "lo", "v", "project", "context", SetMemoryOpts{})
		lo := klLast(t, d).Rev
		if c := klClock(t, d, "p1"); c[0] != lo || c[1] != hi || c[2] != hi {
			t.Fatalf("LOW write clock = %v, want 0->%d and 1,2 still %d", c, lo, hi)
		}
	})

	t.Run("GlobalScopeUsesStarClock", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "g", "v", "global", "behavior", SetMemoryOpts{})
		r := klLast(t, d)
		if r.Project != "*" {
			t.Fatalf("global row project = %q, want *", r.Project)
		}
		if c := klClock(t, d, "*"); c[1] != r.Rev {
			t.Fatalf("* clock = %v, want durability 1 at %d", c, r.Rev)
		}
		if c := klClock(t, d, "p1"); len(c) != 0 {
			t.Fatalf("p1 clock = %v, want untouched by a global write", c)
		}
	})

	t.Run("AgentScopeKeysOfTwoAgentsStaySeparate", func(t *testing.T) {
		d := testDB(t)
		if _, err := d.SetMemory("p1", "dev-a", "note", "a", "[]", "agent", "stated", "context"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SetMemory("p1", "dev-b", "note", "b", "[]", "agent", "stated", "context"); err != nil {
			t.Fatal(err)
		}
		rs := klRows(t, d)
		if rs[0].Author != "dev-a" || rs[1].Author != "dev-b" || rs[1].Op != opSet {
			t.Fatalf("agent-scope rows = %+v, want two fresh keys authored dev-a and dev-b", rs)
		}
	})
}

// klBackdate ages every log row by d so compaction's min lag has something to act on.
func klBackdate(t *testing.T, d *DB, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by).UTC().Format(memoryTimeFmt)
	if _, err := d.conn.Exec(`UPDATE knowledge_log SET created_at = ?`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

// liveMap is key -> sorted live memory ids, from the memories table (p1 and global).
func liveMap(t *testing.T, d *DB) map[string][]string {
	t.Helper()
	rows, err := d.conn.Query(`SELECT scope, key, CASE WHEN scope='agent' THEN agent_name ELSE '' END, id FROM memories
		WHERE archived_at IS NULL AND (project = 'p1' OR scope = 'global')`)
	if err != nil {
		t.Fatalf("live map: %v", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var scope, key, author, id string
		_ = rows.Scan(&scope, &key, &author, &id)
		k := scope + "|" + key + "|" + author
		out[k] = append(out[k], id)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func deltaMap(t *testing.T, delta *KnowledgeDelta) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, c := range delta.Changes { // ordered by rev: the last row per key wins
		k := c.Scope + "|" + c.Key + "|" + c.Author
		ids := append([]string{}, c.LiveIDs...)
		sort.Strings(ids)
		if len(ids) == 0 {
			delete(out, k)
			continue
		}
		out[k] = ids
	}
	return out
}

func TestKnowledgeLogCompactionAndReads(t *testing.T) {
	t.Run("CompactionKeepsLatestPerKeyAndBreakingRetraction", func(t *testing.T) {
		d := testDB(t)
		for _, v := range []string{"a1", "a2", "a3"} {
			klSet(t, d, "a", v, "project", "context", SetMemoryOpts{}) // additive x3
		}
		for _, v := range []string{"b1", "b2"} {
			klSet(t, d, "b", v, "project", "constraints", SetMemoryOpts{}) // breaking x2
		}
		klSet(t, d, "c", "c1", "project", "context", SetMemoryOpts{})
		if err := d.DeleteMemory("p1", "dev-a", "c", "project"); err != nil {
			t.Fatal(err)
		}
		all := klRows(t, d)
		klBackdate(t, d, 60*24*time.Hour)
		n, err := d.CompactKnowledgeLog("p1", 30*24*time.Hour, time.Now())
		if err != nil {
			t.Fatalf("compact: %v", err)
		}
		if n != 3 { // a1, a2, c1
			t.Fatalf("removed %d, want 3 (a1, a2, c1)", n)
		}
		var ops []string
		for _, r := range klRows(t, d) {
			ops = append(ops, r.Key+":"+r.Class)
		}
		want := "a:additive b:breaking b:breaking c:retraction"
		if got := strings.Join(ops, " "); got != want {
			t.Fatalf("kept %q, want %q", got, want)
		}
		var wm int64
		_ = d.conn.QueryRow(`SELECT low_watermark FROM knowledge_compaction WHERE project='p1'`).Scan(&wm)
		if wm != all[5].Rev { // c1 was the highest removed rev
			t.Fatalf("low_watermark = %d, want %d", wm, all[5].Rev)
		}
	})

	t.Run("CompactionRespectsMinLag", func(t *testing.T) {
		d := testDB(t)
		for _, v := range []string{"a1", "a2", "a3"} {
			klSet(t, d, "a", v, "project", "context", SetMemoryOpts{})
		}
		n, err := d.CompactKnowledgeLog("p1", 30*24*time.Hour, time.Now())
		if err != nil || n != 0 || klCount(t, d) != 3 {
			t.Fatalf("young rows: removed %d (%v), %d left, want 0 removed, 3 left", n, err, klCount(t, d))
		}
	})

	t.Run("DeltaCompactedFlagBelowWatermark", func(t *testing.T) {
		d := testDB(t)
		for _, v := range []string{"a1", "a2", "a3"} {
			klSet(t, d, "a", v, "project", "context", SetMemoryOpts{})
		}
		klBackdate(t, d, 60*24*time.Hour)
		if _, err := d.CompactKnowledgeLog("p1", 30*24*time.Hour, time.Now()); err != nil {
			t.Fatal(err)
		}
		below, err := d.KnowledgeDelta("p1", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		at, err := d.KnowledgeDelta("p1", below.HeadRev, false)
		if err != nil {
			t.Fatal(err)
		}
		if !below.Compacted || at.Compacted || len(at.Changes) != 0 {
			t.Fatalf("compacted flags below=%v at-head=%v (changes %d), want true/false/0", below.Compacted, at.Compacted, len(at.Changes))
		}
	})

	t.Run("DeltaStateCompleteAfterCompaction", func(t *testing.T) {
		d := testDB(t)
		first := klSet(t, d, "a", "a1", "project", "context", SetMemoryOpts{})
		klSet(t, d, "a", "a2", "project", "context", SetMemoryOpts{})
		klSet(t, d, "a", "a3", "project", "context", SetMemoryOpts{BasedOn: first}) // sibling: two live
		klSet(t, d, "b", "b1", "project", "behavior", SetMemoryOpts{})
		if _, err := d.SetMemoryWith("p1", "dev-a", "b", "b1", `["t"]`, "project", "stated", "behavior", true, SetMemoryOpts{}); err != nil {
			t.Fatal(err) // editorial tags-only version: new live id
		}
		klSet(t, d, "c", "c1", "project", "context", SetMemoryOpts{})
		cid := klSet(t, d, "c", "c2", "project", "context", SetMemoryOpts{BasedOn: BasedOnNew})
		if err := d.DeleteMemoryByID(cid, "human"); err != nil {
			t.Fatal(err)
		}
		klSet(t, d, "gone", "x", "project", "context", SetMemoryOpts{})
		if err := d.DeleteMemory("p1", "dev-a", "gone", "project"); err != nil {
			t.Fatal(err)
		}
		klSet(t, d, "g", "v", "global", "behavior", SetMemoryOpts{})
		if _, err := d.SetMemory("p1", "dev-b", "note", "n", "[]", "agent", "stated", "context"); err != nil {
			t.Fatal(err)
		}
		klBackdate(t, d, 60*24*time.Hour)
		for _, p := range []string{"p1", "*"} {
			if _, err := d.CompactKnowledgeLog(p, 30*24*time.Hour, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		delta, err := d.KnowledgeDelta("p1", 0, true)
		if err != nil {
			t.Fatal(err)
		}
		got, want := deltaMap(t, delta), liveMap(t, d)
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		if string(gj) != string(wj) {
			t.Fatalf("delta replay after compaction:\n got %s\nwant %s", gj, wj)
		}
	})

	t.Run("ReadPathsWriteNothing", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "k", "hello", "project", "behavior", SetMemoryOpts{})
		var before, after int64
		_ = d.conn.QueryRow(`SELECT total_changes()`).Scan(&before)
		if _, err := d.GetMemory("p1", "dev-a", "k", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SearchMemory("p1", "dev-a", "hello", nil, "", 10); err != nil {
			t.Fatal(err)
		}
		if _, err := d.KnowledgeDelta("p1", 0, true); err != nil {
			t.Fatal(err)
		}
		_ = d.conn.QueryRow(`SELECT total_changes()`).Scan(&after)
		if after != before {
			t.Fatalf("writer total_changes %d -> %d across reads, want unchanged", before, after)
		}
	})

	t.Run("OldDBMigrates", func(t *testing.T) {
		d := testDB(t)
		klSet(t, d, "pre", "v", "project", "behavior", SetMemoryOpts{}) // a memory that predates the log
		for _, tbl := range []string{"knowledge_log", "knowledge_clock", "knowledge_compaction"} {
			if _, err := d.conn.Exec(`DROP TABLE ` + tbl); err != nil {
				t.Fatalf("drop %s: %v", tbl, err)
			}
		}
		for i := 0; i < 2; i++ {
			if err := migrate(d.conn); err != nil {
				t.Fatalf("migrate #%d: %v", i+1, err)
			}
		}
		if n := klCount(t, d); n != 0 {
			t.Fatalf("log after migrate = %d rows, want 0 (starts empty, no backfill)", n)
		}
		klSet(t, d, "pre", "v2", "project", "behavior", SetMemoryOpts{})
		if n := klCount(t, d); n != 1 {
			t.Fatalf("log after one write = %d rows, want 1", n)
		}
	})
}
