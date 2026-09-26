package relay

import (
	"database/sql"
	"fmt"
	"testing"

	"agent-relay/internal/db"
)

// Consumption T2 (design 54e529d8 §6 T2, task ab5a9a77): claim/complete stamp
// the knowledge basis; who_consumed answers who holds which version of a key.

type consFixture struct {
	h    *Handlers
	d    *db.DB
	conn *sql.Conn
}

func newConsFixture(t *testing.T, agents ...string) *consFixture {
	t.Helper()
	d, path := memDB(t)
	for _, a := range agents {
		if _, _, err := d.RegisterAgent("p1", a, "test", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", a, err)
		}
	}
	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &consFixture{h: memHandlersAt(t, d), d: d, conn: conn}
}

func (f *consFixture) setMem(t *testing.T, key, value string) string {
	t.Helper()
	m, err := f.d.SetMemoryWith("p1", "cto", key, value, "[]", "project", "stated", "constraints", true, db.SetMemoryOpts{})
	if err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
	return m.ID
}

func (f *consFixture) boot(agent string) {
	f.h.buildSessionContext("p1", agent, nil)
}

func (f *consFixture) task(t *testing.T) string {
	t.Helper()
	task, err := f.d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, db.TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return task.ID
}

func (f *consFixture) claim(t *testing.T, agent, taskID string) map[string]any {
	t.Helper()
	res, _ := f.h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": agent, "task_id": taskID}))
	return parseJSON(t, res)
}

func (f *consFixture) dataVersion(t *testing.T) int {
	t.Helper()
	var v int
	if err := f.conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&v); err != nil {
		t.Fatalf("data_version: %v", err)
	}
	return v
}

type basisRow struct {
	Event, Snapshot, RecallThrough, StampedAt string
}

func (f *consFixture) basisRows(t *testing.T, taskID string) []basisRow {
	t.Helper()
	rows, err := f.conn.QueryContext(ctx, `SELECT event, COALESCE(snapshot_id,''), COALESCE(recall_through,''), stamped_at
		FROM task_basis WHERE task_id = ? ORDER BY stamped_at, event`, taskID)
	if err != nil {
		t.Fatalf("task_basis: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []basisRow
	for rows.Next() {
		var r basisRow
		if err := rows.Scan(&r.Event, &r.Snapshot, &r.RecallThrough, &r.StampedAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func (f *consFixture) head(t *testing.T, agent string) string {
	t.Helper()
	h, err := f.d.GetContextHead("p1", agent)
	if err != nil || h == nil {
		t.Fatalf("head %s: %v %v", agent, h, err)
	}
	return h.SnapshotID
}

func (f *consFixture) snapshotHolds(t *testing.T, snapshotID, memoryID string) bool {
	t.Helper()
	var n int
	if err := f.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_snapshots s JOIN consumption_edges e ON e.set_hash = s.set_hash
		WHERE s.id = ? AND e.memory_id = ?`, snapshotID, memoryID).Scan(&n); err != nil {
		t.Fatalf("holds: %v", err)
	}
	return n > 0
}

func (f *consFixture) whoConsumed(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	a := map[string]any{"project": "p1", "as": "a"}
	for k, v := range args {
		a[k] = v
	}
	res, _ := f.h.HandleWhoConsumed(ctx, call(a))
	return parseJSON(t, res)
}

func holders(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	m := map[string]map[string]any{}
	for _, x := range out["holders"].([]any) {
		c := x.(map[string]any)
		m[c["agent"].(string)] = c
	}
	return m
}

func TestConsumptionTools(t *testing.T) {
	t.Run("ClaimStampsHeadAndRecalls", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.setMem(t, "k-boot", "v")
		f.boot("a")
		recalled := f.setMem(t, "k-recall", "v") // written after boot: not in the head
		res, _ := f.h.HandleGetMemory(ctx, call(map[string]any{"project": "p1", "as": "a", "key": "k-recall"}))
		parseJSON(t, res)
		id := f.task(t)
		out := f.claim(t, "a", id)
		rows := f.basisRows(t, id)
		if len(rows) != 1 || rows[0].Event != "claim" || rows[0].Snapshot != f.head(t, "a") {
			t.Fatalf("claim stamp = %+v, want one claim row on the head", rows)
		}
		if rows[0].RecallThrough == "" || !f.snapshotHolds(t, rows[0].RecallThrough, recalled) {
			t.Fatalf("recall_through %q does not hold the recalled id", rows[0].RecallThrough)
		}
		if b, _ := out["basis"].(string); b == "" || b == basisUnknown || out["id"] != id {
			t.Fatalf("claim result basis/id = %v/%v", out["basis"], out["id"])
		}
		// The buffered recall was drained by the stamp, not left for the tick.
		if left := f.h.recalls.take("p1", "a"); len(left) != 0 {
			t.Fatalf("recall buffer still holds %v", left)
		}
	})

	t.Run("CompleteStampSeparateRow", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.setMem(t, "k", "v")
		f.boot("a")
		id := f.task(t)
		f.claim(t, "a", id)
		res, _ := f.h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "a", "task_id": id, "result": "ok"}))
		if out := parseJSON(t, res); out["basis"] == nil || out["status"] != "done" {
			t.Fatalf("complete result = %v", out)
		}
		rows := f.basisRows(t, id)
		if len(rows) != 2 || rows[0].Event != "claim" || rows[1].Event != "complete" {
			t.Fatalf("stamps = %+v, want claim then complete", rows)
		}
	})

	t.Run("StartFromPendingStampsClaim", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.boot("a")
		id := f.task(t)
		res, _ := f.h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "a", "task_id": id}))
		if out := parseJSON(t, res); out["basis"] == nil {
			t.Fatalf("start from pending: no basis in %v", out)
		}
		if rows := f.basisRows(t, id); len(rows) != 1 || rows[0].Event != "claim" {
			t.Fatalf("stamps = %+v, want one claim", rows)
		}
	})

	t.Run("BatchCompleteOneTx", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.boot("a")
		t1, t2 := f.task(t), f.task(t)
		f.claim(t, "a", t1)
		f.claim(t, "a", t2)
		before := f.dataVersion(t)
		res, _ := f.h.HandleBatchCompleteTasks(ctx, call(map[string]any{"project": "p1", "as": "a",
			"tasks": fmt.Sprintf(`[{"task_id":%q},{"task_id":%q}]`, t1, t2)}))
		out := parseJSON(t, res)
		if b, _ := out["basis"].(string); b == "" || b == basisUnknown {
			t.Fatalf("batch basis = %v", out["basis"])
		}
		r1, r2 := f.basisRows(t, t1), f.basisRows(t, t2)
		if len(r1) != 2 || len(r2) != 2 || r1[1].Event != "complete" || r1[1].StampedAt != r2[1].StampedAt {
			t.Fatalf("batch stamps = %+v / %+v, want one complete row each with one stamped_at", r1, r2)
		}
		if f.dataVersion(t) == before {
			t.Fatal("batch complete wrote nothing")
		}
	})

	t.Run("StampFailureKeepsTransition", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.boot("a")
		id := f.task(t)
		if _, err := f.conn.ExecContext(ctx, `DROP TABLE task_basis`); err != nil {
			t.Fatalf("drop: %v", err)
		}
		out := f.claim(t, "a", id)
		if out["basis"] != basisUnknown || out["status"] != "accepted" {
			t.Fatalf("claim with failing stamp = basis %v status %v, want unknown/accepted", out["basis"], out["status"])
		}
		if task, _ := f.d.GetTask(id, "p1"); task == nil || task.Status != "accepted" {
			t.Fatalf("transition lost: %+v", task)
		}
	})

	t.Run("WhoConsumedFlagsOldVersion", func(t *testing.T) {
		f := newConsFixture(t, "a", "b")
		v1 := f.setMem(t, "k", "v1")
		f.boot("a")
		id := f.task(t)
		f.claim(t, "a", id)
		v2 := f.setMem(t, "k", "v2")
		f.boot("b")
		hs := holders(t, f.whoConsumed(t, map[string]any{"key": "k"}))
		a, b := hs["a"], hs["b"]
		if a == nil || b == nil {
			t.Fatalf("holders = %v, want a and b", hs)
		}
		if a["memory_id"] != v1 || a["current"] != false || b["memory_id"] != v2 || b["current"] != true {
			t.Fatalf("a=%v b=%v, want a on v1 (stale), b on v2 (current)", a, b)
		}
		tasks, _ := a["active_tasks"].([]any)
		if len(tasks) != 1 || tasks[0] != id {
			t.Fatalf("a active_tasks = %v, want [%s]", a["active_tasks"], id)
		}
	})

	t.Run("WhoConsumedActiveOnly", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.setMem(t, "k", "v")
		f.boot("a")
		if _, err := f.conn.ExecContext(ctx, `UPDATE agents SET status = 'inactive' WHERE name = 'a'`); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		if hs := holders(t, f.whoConsumed(t, map[string]any{"key": "k"})); len(hs) != 0 {
			t.Fatalf("inactive agent listed by default: %v", hs)
		}
		if hs := holders(t, f.whoConsumed(t, map[string]any{"key": "k", "active_only": false})); hs["a"] == nil {
			t.Fatalf("active_only=false misses the inactive agent: %v", hs)
		}
		res, _ := f.h.HandleWhoConsumed(ctx, call(map[string]any{"project": "p1", "as": "a"}))
		expectError(t, res) // no key and no self
	})

	t.Run("WhoConsumedSelfReturnsOwnBasis", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.setMem(t, "k", "v1")
		f.boot("a")
		f.setMem(t, "k", "v2")
		out := f.whoConsumed(t, map[string]any{"self": true})
		head, _ := f.d.GetContextHead("p1", "a")
		if out["basis"] != head.Basis() || out["snapshot_id"] != head.SnapshotID {
			t.Fatalf("self basis = %v / %v, want %s", out["basis"], out["snapshot_id"], head.Basis())
		}
		mems, _ := out["memories"].([]any)
		if len(mems) != 1 || mems[0].(map[string]any)["newer_version_exists"] != true {
			t.Fatalf("self memories = %v, want k flagged newer_version_exists", mems)
		}
	})

	t.Run("GetMemoryNoWriteOnCall", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.setMem(t, "k", "v")
		before := f.dataVersion(t)
		for i := 0; i < 100; i++ {
			res, _ := f.h.HandleGetMemory(ctx, call(map[string]any{"project": "p1", "as": "a", "key": "k"}))
			parseJSON(t, res)
		}
		if after := f.dataVersion(t); after != before {
			t.Fatalf("get_memory wrote before the tick (data_version %d -> %d)", before, after)
		}
		f.h.flushRecalls()
		if f.dataVersion(t) == before {
			t.Fatal("the flush tick wrote nothing for the buffered recalls")
		}
	})

	t.Run("ToolsAreReadOnly", func(t *testing.T) {
		f := newConsFixture(t, "a")
		f.setMem(t, "k", "v")
		f.boot("a")
		before := f.dataVersion(t)
		for i := 0; i < 5; i++ {
			f.whoConsumed(t, map[string]any{"key": "k"})
			f.whoConsumed(t, map[string]any{"self": true})
		}
		if after := f.dataVersion(t); after != before {
			t.Fatalf("who_consumed wrote (data_version %d -> %d)", before, after)
		}
	})
}
