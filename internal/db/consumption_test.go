package db

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func totalChanges(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.conn.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
		t.Fatalf("total_changes: %v", err)
	}
	return n
}

func consumptionRows(t *testing.T, d *DB, table string) int {
	t.Helper()
	var n int
	if err := d.ro().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestConsumption(t *testing.T) {
	ids := []string{"m1", "m2", "m3"}

	t.Run("BootWritesOneTx", func(t *testing.T) {
		d := testDB(t)
		head, wrote, err := d.RecordSnapshot("p", "a", "", SnapshotBoot, ids)
		if err != nil || !wrote || head == nil {
			t.Fatalf("first boot: head=%v wrote=%v err=%v", head, wrote, err)
		}
		for table, want := range map[string]int{"context_sets": 1, "consumption_edges": 3, "context_snapshots": 1, "context_heads": 1} {
			if n := consumptionRows(t, d, table); n != want {
				t.Fatalf("%s = %d, want %d", table, n, want)
			}
		}
		if got := head.Basis(); got != "-:"+head.SnapshotID[:8] {
			t.Fatalf("basis = %q (no knowledge_log yet)", got)
		}
	})

	t.Run("UnchangedBootWritesNothing", func(t *testing.T) {
		d := testDB(t)
		first, _, _ := d.RecordSnapshot("p", "a", "", SnapshotBoot, ids)
		before := totalChanges(t, d)
		head, wrote, err := d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{"m3", "m1", "m2", "m1"})
		if err != nil || wrote {
			t.Fatalf("unchanged boot wrote=%v err=%v", wrote, err)
		}
		if delta := totalChanges(t, d) - before; delta != 0 {
			t.Fatalf("unchanged boot changed %d rows", delta)
		}
		if head.SnapshotID != first.SnapshotID {
			t.Fatal("unchanged boot moved the head")
		}
	})

	t.Run("SetSharedAcrossAgents", func(t *testing.T) {
		d := testDB(t)
		_, _, _ = d.RecordSnapshot("p", "a", "", SnapshotBoot, ids)
		_, wrote, _ := d.RecordSnapshot("p", "b", "", SnapshotBoot, ids)
		if !wrote {
			t.Fatal("second agent must get its own snapshot")
		}
		for table, want := range map[string]int{"context_sets": 1, "consumption_edges": 3, "context_snapshots": 2, "context_heads": 2} {
			if n := consumptionRows(t, d, table); n != want {
				t.Fatalf("%s = %d, want %d", table, n, want)
			}
		}
	})

	t.Run("ConcurrentBootsOneHead", func(t *testing.T) {
		d := testDB(t)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := d.RecordSnapshot("p", "a", "", SnapshotBoot, ids); err != nil {
					t.Errorf("concurrent boot: %v", err)
				}
			}()
		}
		wg.Wait()
		if h, s := consumptionRows(t, d, "context_heads"), consumptionRows(t, d, "context_snapshots"); h != 1 || s != 1 {
			t.Fatalf("heads=%d snapshots=%d, want 1/1", h, s)
		}
	})

	t.Run("MinimalAfterFullRecordsEmpty", func(t *testing.T) {
		d := testDB(t)
		_, _, _ = d.RecordSnapshot("p", "a", "", SnapshotBoot, ids)
		head, wrote, err := d.RecordSnapshot("p", "a", "", SnapshotBootMinimal, nil)
		if err != nil || !wrote || head.SetHash != ContextSetHash(nil) || head.Kind != SnapshotBootMinimal {
			t.Fatalf("minimal after full: head=%+v wrote=%v err=%v", head, wrote, err)
		}
	})

	t.Run("MinimalAfterMinimalWritesNothing", func(t *testing.T) {
		d := testDB(t)
		_, _, _ = d.RecordSnapshot("p", "a", "", SnapshotBoot, ids)
		_, _, _ = d.RecordSnapshot("p", "a", "", SnapshotBootMinimal, nil)
		before := totalChanges(t, d)
		if _, wrote, _ := d.RecordSnapshot("p", "a", "", SnapshotBootMinimal, nil); wrote {
			t.Fatal("second minimal boot wrote")
		}
		// An agent that never booted full: a minimal boot records nothing.
		if head, wrote, _ := d.RecordSnapshot("p", "fresh", "", SnapshotBootMinimal, nil); wrote || head != nil {
			t.Fatal("minimal boot without a head wrote a snapshot")
		}
		if delta := totalChanges(t, d) - before; delta != 0 {
			t.Fatalf("minimal boots changed %d rows", delta)
		}
	})

	t.Run("RecallBatchOneTxDropsHeadIds", func(t *testing.T) {
		d := testDB(t)
		ha, _, _ := d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{"m1", "m2"})
		_, _, _ = d.RecordSnapshot("p", "c", "", SnapshotBoot, []string{"m1", "m2"})
		n, err := d.RecordRecallBatch(map[RecallKey][]string{
			{Project: "p", Agent: "a"}: {"m2", "m3"}, // m2 already held
			{Project: "p", Agent: "b"}: {"m1"},       // no head
			{Project: "p", Agent: "c"}: {"m1", "m2"}, // all held: nothing
		})
		if err != nil || n != 2 {
			t.Fatalf("recall batch: n=%d err=%v, want 2", n, err)
		}
		var parent string
		var members int
		_ = d.ro().QueryRow(`SELECT COALESCE(s.parent_id, ''), (SELECT COUNT(*) FROM consumption_edges e WHERE e.set_hash = s.set_hash)
			FROM context_snapshots s WHERE s.agent_name = 'a' AND s.kind = 'recall'`).Scan(&parent, &members)
		if parent != ha.SnapshotID || members != 1 {
			t.Fatalf("a's recall: parent=%q members=%d, want head %s and 1 (m3 only)", parent, members, ha.SnapshotID)
		}
		before := totalChanges(t, d)
		if n, _ := d.RecordRecallBatch(map[RecallKey][]string{{Project: "p", Agent: "c"}: {"m1"}}); n != 0 {
			t.Fatal("all-held batch wrote")
		}
		if delta := totalChanges(t, d) - before; delta != 0 {
			t.Fatalf("empty batch changed %d rows", delta)
		}
	})

	t.Run("PruneKeepsHeadsAndTaskBasis", func(t *testing.T) {
		d := testDB(t)
		old, _, _ := d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{"old"})
		pinned, _, _ := d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{"pinned"})
		head, _, _ := d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{"now"})
		if _, err := d.writerExec(`UPDATE context_snapshots SET created_at = '2020-01-01T00:00:00.000000Z'`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.writerExec(`INSERT INTO task_basis (task_id, event, project, agent_name, snapshot_id, stamped_at)
			VALUES ('t1', 'claim', 'p', 'a', ?, '2020-01-01T00:00:00.000000Z')`, pinned.SnapshotID); err != nil {
			t.Fatal(err)
		}
		n, err := d.PruneConsumption(30*24*time.Hour, time.Now())
		if err != nil || n != 1 {
			t.Fatalf("prune: n=%d err=%v, want 1", n, err)
		}
		exists := func(id string) bool {
			var c int
			_ = d.ro().QueryRow(`SELECT COUNT(*) FROM context_snapshots WHERE id = ?`, id).Scan(&c)
			return c == 1
		}
		if exists(old.SnapshotID) || !exists(pinned.SnapshotID) || !exists(head.SnapshotID) {
			t.Fatal("prune must drop only the unreferenced old snapshot")
		}
		var edges int
		_ = d.ro().QueryRow(`SELECT COUNT(*) FROM consumption_edges WHERE memory_id = 'old'`).Scan(&edges)
		if edges != 0 {
			t.Fatal("orphan set's edges not pruned")
		}
		if n, _ := d.PruneConsumption(30*24*time.Hour, time.Now()); n != 0 {
			t.Fatal("second prune found work")
		}
	})

	t.Run("WhoConsumedAndBasisFlagOldVersion", func(t *testing.T) {
		d := testDB(t)
		v1, err := d.SetMemory("p", "cto", "rule", "v1", "", "project", "", "constraints")
		if err != nil {
			t.Fatal(err)
		}
		_, _, _ = d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{v1.ID})
		seedAgent(t, d.conn, "p", "a", "active", "", "", 0)
		v2, err := d.SetMemory("p", "cto", "rule", "v2", "", "project", "", "constraints")
		if err != nil {
			t.Fatal(err)
		}
		_, _, _ = d.RecordSnapshot("p", "b", "", SnapshotBoot, []string{v2.ID})
		got, err := d.WhoConsumed("p", "rule", "", false)
		if err != nil || len(got) != 2 {
			t.Fatalf("who_consumed: %+v %v", got, err)
		}
		for _, c := range got {
			if c.Agent == "a" && (c.Current || c.MemoryID != v1.ID) {
				t.Fatalf("a should hold the old version: %+v", c)
			}
			if c.Agent == "b" && !c.Current {
				t.Fatalf("b should hold the current version: %+v", c)
			}
		}
		if active, _ := d.WhoConsumed("p", "rule", "", true); len(active) != 1 || active[0].Agent != "a" {
			t.Fatalf("active_only: %+v (only a is registered active)", active)
		}
		_, mems, err := d.ContextBasis("p", "a")
		if err != nil || len(mems) != 1 || !mems[0].NewerVersionExists {
			t.Fatalf("basis of a: %+v %v, want newer_version_exists", mems, err)
		}
	})

	t.Run("BasisCarriesKnowledgeRev", func(t *testing.T) {
		d := testDB(t)
		m, err := d.SetMemory("p", "cto", "rule", "v1", "", "project", "", "constraints")
		if err != nil {
			t.Fatal(err)
		}
		var rev int64
		if err := d.ro().QueryRow(`SELECT MAX(rev) FROM knowledge_log`).Scan(&rev); err != nil || rev == 0 {
			t.Fatalf("knowledge_log head: %d %v", rev, err)
		}
		head, _, err := d.RecordSnapshot("p", "a", "", SnapshotBoot, []string{m.ID})
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("%d:%s", rev, head.SnapshotID[:8]); head.Basis() != want {
			t.Fatalf("basis = %q, want %q", head.Basis(), want)
		}
	})

	t.Run("OldDBMigrates", func(t *testing.T) {
		d := testDB(t)
		migrateConsumption(d.conn)
		migrateConsumption(d.conn)
		for _, table := range []string{"context_sets", "consumption_edges", "context_snapshots", "context_heads", "task_basis"} {
			consumptionRows(t, d, table)
		}
	})
}
