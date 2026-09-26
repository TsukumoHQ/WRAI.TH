package relay

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Knowledge S2 (design af783f93 §6 slice 2, task 1edb1377): set_memory takes a
// declared change_class, knowledge_delta reads the log since a revision, and
// the cleanup tick compacts. (session_context knowledge_rev: follow-up.)

func knowledgeDelta(t *testing.T, h *Handlers, since int) map[string]any {
	t.Helper()
	res, _ := h.HandleKnowledgeDelta(ctx, call(map[string]any{"project": "p1", "as": "a", "since_rev": float64(since)}))
	return parseJSON(t, res)
}

func deltaChanges(d map[string]any) []map[string]any {
	var out []map[string]any
	for _, c := range d["changes"].([]any) {
		out = append(out, c.(map[string]any))
	}
	return out
}

func setKey(t *testing.T, h *Handlers, key, value string, extra map[string]any) {
	t.Helper()
	args := map[string]any{"project": "p1", "as": "a", "key": key, "value": value}
	for k, v := range extra {
		args[k] = v
	}
	res, _ := h.HandleSetMemory(ctx, call(args))
	parseJSON(t, res)
}

// knowledgeHandlers builds Handlers with the writer agent registered in p1, as
// every real set_memory caller is (compaction enumerates known projects).
func knowledgeHandlers(t *testing.T) (*Handlers, *db.DB, string) {
	t.Helper()
	d, path := memDB(t)
	if _, _, err := d.RegisterAgent("p1", "a", "test", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return memHandlersAt(t, d), d, path
}

func TestKnowledgeTools(t *testing.T) {
	t.Run("SetMemoryChangeClassStoredAsDeclared", func(t *testing.T) {
		h, _, _ := knowledgeHandlers(t)
		setKey(t, h, "k1", "v1", map[string]any{"change_class": "breaking"})
		ch := deltaChanges(knowledgeDelta(t, h, 0))
		if len(ch) != 1 || ch[0]["key"] != "k1" || ch[0]["declared_class"] != "breaking" || ch[0]["change_class"] != "breaking" {
			t.Fatalf("changes = %v, want one k1 row declared+effective breaking", ch)
		}
		// Undeclared stays undeclared (the relay's floor decides the class).
		setKey(t, h, "k2", "v", nil)
		ch = deltaChanges(knowledgeDelta(t, h, 0))
		if last := ch[len(ch)-1]; last["key"] != "k2" || last["declared_class"] != nil {
			t.Fatalf("undeclared write: %v", last)
		}
	})

	t.Run("InvalidChangeClassRejectedNothingWritten", func(t *testing.T) {
		h, d, _ := knowledgeHandlers(t)
		res, _ := h.HandleSetMemory(ctx, call(map[string]any{"project": "p1", "as": "a", "key": "k1", "value": "v", "change_class": "retraction"}))
		if msg := expectError(t, res); !strings.Contains(msg, "INVALID_ARGUMENT") {
			t.Fatalf("error = %s, want INVALID_ARGUMENT", msg)
		}
		if ch := deltaChanges(knowledgeDelta(t, h, 0)); len(ch) != 0 {
			t.Fatalf("rejected write logged %d rows", len(ch))
		}
		if mem, _ := d.GetMemory("p1", "a", "k1", "project"); len(mem) != 0 {
			t.Fatalf("rejected write stored %d memories", len(mem))
		}
	})

	t.Run("DeltaReturnsNonEditorialSinceRev", func(t *testing.T) {
		h, _, _ := knowledgeHandlers(t)
		setKey(t, h, "k1", "v1", nil)
		first := int(knowledgeDelta(t, h, 0)["head_rev"].(float64))
		setKey(t, h, "k1", "v1", nil) // convergent write: a touch, editorial
		setKey(t, h, "k2", "v2", nil)
		all := knowledgeDelta(t, h, 0)
		if ch := deltaChanges(all); len(ch) != 2 || ch[0]["key"] != "k1" || ch[1]["key"] != "k2" {
			t.Fatalf("since 0: %v, want k1 then k2 (the touch is editorial)", ch)
		}
		since := deltaChanges(knowledgeDelta(t, h, first))
		if len(since) != 1 || since[0]["key"] != "k2" {
			t.Fatalf("since %d: %v, want only k2", first, since)
		}
		if int(all["head_rev"].(float64)) <= first || all["compacted"] != false {
			t.Fatalf("head_rev/compacted = %v/%v", all["head_rev"], all["compacted"])
		}
		res, _ := h.HandleKnowledgeDelta(ctx, call(map[string]any{"project": "p1", "as": "a", "since_rev": float64(-1)}))
		expectError(t, res)
	})

	t.Run("DeltaCompactedTrueBelowWatermark", func(t *testing.T) {
		h, d, _ := knowledgeHandlers(t)
		setKey(t, h, "k1", "v1", nil)
		setKey(t, h, "k1", "v2", nil) // supersedes: the v1 row becomes compactable
		head := int(knowledgeDelta(t, h, 0)["head_rev"].(float64))
		if n := compactKnowledgeLogs(d, time.Now().Add(60*24*time.Hour)); n != 1 {
			t.Fatalf("compaction removed %d rows, want 1", n)
		}
		below := knowledgeDelta(t, h, 0)
		if below["compacted"] != true {
			t.Fatalf("since 0 below the watermark: compacted = %v", below["compacted"])
		}
		if ch := deltaChanges(below); len(ch) != 1 || ch[0]["key"] != "k1" {
			t.Fatalf("state-complete delta = %v, want the latest k1 row", ch)
		}
		if at := knowledgeDelta(t, h, head); at["compacted"] != false {
			t.Fatalf("since head: compacted = %v", at["compacted"])
		}
	})

	t.Run("DeltaDoesNoDBWrite", func(t *testing.T) {
		h, _, path := knowledgeHandlers(t)
		setKey(t, h, "k1", "v1", nil)
		raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		defer func() { _ = raw.Close() }()
		conn, err := raw.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		defer func() { _ = conn.Close() }()
		version := func() int {
			var v int
			if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&v); err != nil {
				t.Fatalf("data_version: %v", err)
			}
			return v
		}
		before := version()
		for i := 0; i < 3; i++ {
			knowledgeDelta(t, h, i)
		}
		if after := version(); after != before {
			t.Fatalf("knowledge_delta committed a write (data_version %d -> %d)", before, after)
		}
	})

	t.Run("TickCompactsAndIsIdempotent", func(t *testing.T) {
		h, d, _ := knowledgeHandlers(t)
		for _, v := range []string{"v1", "v2", "v3"} {
			setKey(t, h, "k1", v, nil)
		}
		setKey(t, h, "g1", "v1", map[string]any{"scope": "global"})
		setKey(t, h, "g1", "v2", map[string]any{"scope": "global"})
		later := time.Now().Add(60 * 24 * time.Hour)
		// Inside the default 30 d lag nothing is eligible.
		if n := compactKnowledgeLogs(d, time.Now()); n != 0 {
			t.Fatalf("compaction inside the lag removed %d rows", n)
		}
		if n := compactKnowledgeLogs(d, later); n != 3 {
			t.Fatalf("first pass removed %d rows, want 3 (two k1, one global g1)", n)
		}
		if n := compactKnowledgeLogs(d, later); n != 0 {
			t.Fatalf("second pass removed %d rows, want 0", n)
		}
		// The tick runs it and stamps its throttle.
		st := &cleanupState{lastBackup: time.Now(), lastRollupCatchup: time.Now().UTC().Format("2006-01-02")}
		runCleanupTick(d, st)
		if st.lastKnowledgeCompact.IsZero() {
			t.Fatal("cleanup tick did not run knowledge compaction")
		}
	})
}
