package relay

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/server"
)

// DEC-wraith-memory-causal-1 T2 (task 1a031d43): set_memory takes based_on,
// auto-fills it from the caller's last get_memory, routes a sibling to the
// current author, and logs every write's causal outcome.

// memHandlersAt builds Handlers on a DB at a known path so a test can open a
// second handle (data_version) or build a second Handlers (restart).
func memHandlersAt(t *testing.T, database *db.DB) *Handlers {
	t.Helper()
	h := NewHandlers(database, NewSessionRegistry(server.NewMCPServer("test", "0.0.0")), nil, NewEventBus())
	t.Cleanup(h.Close)
	return h
}

func memDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mem.db")
	database, err := db.NewTestDB(path)
	if err != nil {
		t.Fatalf("new test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, path
}

func setMem(t *testing.T, h *Handlers, as string, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"project": "p1", "as": as, "key": "auth-policy"}
	for k, v := range extra {
		args[k] = v
	}
	res, _ := h.HandleSetMemory(ctx, call(args))
	return parseJSON(t, res)
}

func getMemID(t *testing.T, h *Handlers, as string) string {
	t.Helper()
	res, _ := h.HandleGetMemory(ctx, call(map[string]any{"project": "p1", "as": as, "key": "auth-policy"}))
	mems := parseJSON(t, res)["memories"].([]any)
	if len(mems) != 1 {
		t.Fatalf("get_memory returned %d rows, want 1", len(mems))
	}
	return mems[0].(map[string]any)["id"].(string)
}

func memID(res map[string]any) string {
	return res["memory"].(map[string]any)["id"].(string)
}

// conflictNotices returns the memory-conflict messages queued for agent,
// read from the table (the inbox projection omits action_required).
func conflictNotices(t *testing.T, path, agent string) []map[string]string {
	t.Helper()
	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query(`SELECT m.from_agent, m.type, m.priority, COALESCE(m.action_required, '')
		FROM messages m JOIN deliveries d ON d.message_id = m.id
		WHERE d.to_agent = ? AND m.subject LIKE 'memory conflict: %'`, agent)
	if err != nil {
		t.Fatalf("notices: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]string
	for rows.Next() {
		var from, typ, prio, action string
		if err := rows.Scan(&from, &typ, &prio, &action); err != nil {
			t.Fatalf("scan notice: %v", err)
		}
		out = append(out, map[string]string{"from": from, "type": typ, "priority": prio, "action": action})
	}
	return out
}

// lostUpdate plays the design §1.2 race: a reads v1, b overwrites it (no read,
// legacy), then a writes from its stale read. Returns a's result and b's id.
func lostUpdate(t *testing.T, h *Handlers, layer string, aArgs map[string]any) (map[string]any, string) {
	t.Helper()
	setMem(t, h, "a", map[string]any{"value": "sessions: cookie", "layer": layer})
	v1 := getMemID(t, h, "a")
	bID := memID(setMem(t, h, "b", map[string]any{"value": "JWT prohibited", "layer": layer}))
	args := map[string]any{"value": "sessions: cookie, 30d", "layer": layer}
	if aArgs != nil {
		for k, v := range aArgs {
			args[k] = v
		}
	} else {
		args["based_on"] = v1
	}
	return setMem(t, h, "a", args), bID
}

func TestSetMemoryCausal(t *testing.T) {
	t.Run("ExplicitMismatchSiblingAndNotice", func(t *testing.T) {
		d, path := memDB(t)
		h := memHandlersAt(t, d)
		res, bID := lostUpdate(t, h, "behavior", nil)
		if res["conflict"] != true || res["conflict_with"] != bID || res["current_author"] != "b" || res["causal"] != "arg" {
			t.Fatalf("sibling result = %v", res)
		}
		// b's write is still live next to a's sibling.
		got, _ := h.HandleGetMemory(ctx, call(map[string]any{"project": "p1", "as": "b", "key": "auth-policy"}))
		if n := len(parseJSON(t, got)["memories"].([]any)); n != 2 {
			t.Fatalf("live rows = %d, want 2 (nothing lost)", n)
		}
		notices := conflictNotices(t, path, "b")
		if len(notices) != 1 || notices[0]["from"] != "relay" || notices[0]["type"] != "fyi" || notices[0]["priority"] != "P2" || notices[0]["action"] != "none" {
			t.Fatalf("notice to b = %v, want one fyi P2 no-wake from relay", notices)
		}
	})

	t.Run("ConstraintsLayerNoticeIsP1", func(t *testing.T) {
		d, path := memDB(t)
		h := memHandlersAt(t, d)
		lostUpdate(t, h, "constraints", nil)
		notices := conflictNotices(t, path, "b")
		if len(notices) != 1 || notices[0]["priority"] != "P1" || notices[0]["type"] != "notification" || notices[0]["action"] != "decide" {
			t.Fatalf("constraints notice = %v, want one P1 decide message", notices)
		}
	})

	t.Run("SelfRaceNoNotice", func(t *testing.T) {
		d, path := memDB(t)
		h := memHandlersAt(t, d)
		v1 := memID(setMem(t, h, "a", map[string]any{"value": "one"}))
		setMem(t, h, "a", map[string]any{"value": "two"})
		res := setMem(t, h, "a", map[string]any{"value": "three", "based_on": v1})
		if res["conflict"] != true {
			t.Fatalf("stale based_on must still sibling: %v", res)
		}
		if n := len(conflictNotices(t, path, "a")); n != 0 {
			t.Fatalf("self-race sent %d notice(s)", n)
		}
	})

	t.Run("AgentScopeNoNotice", func(t *testing.T) {
		d, path := memDB(t)
		h := memHandlersAt(t, d)
		v1 := memID(setMem(t, h, "a", map[string]any{"value": "one", "scope": "agent"}))
		setMem(t, h, "a", map[string]any{"value": "two", "scope": "agent"})
		res := setMem(t, h, "a", map[string]any{"value": "three", "scope": "agent", "based_on": v1})
		if res["conflict"] != true {
			t.Fatalf("agent-scope stale write must sibling: %v", res)
		}
		if n := len(conflictNotices(t, path, "a")); n != 0 {
			t.Fatalf("agent scope sent %d notice(s)", n)
		}
	})

	t.Run("AutofillFromGetMemory", func(t *testing.T) {
		d, _ := memDB(t)
		h := memHandlersAt(t, d)
		res, bID := lostUpdate(t, h, "behavior", map[string]any{}) // no based_on arg
		if res["causal"] != "cache" || res["conflict"] != true || res["conflict_with"] != bID {
			t.Fatalf("auto-filled write = %v, want causal=cache sibling of b's row", res)
		}
	})

	t.Run("NoReadNoArgLegacy", func(t *testing.T) {
		d, _ := memDB(t)
		h := memHandlersAt(t, d)
		setMem(t, h, "a", map[string]any{"value": "one"})
		res := setMem(t, h, "b", map[string]any{"value": "two"})
		if res["causal"] != "none" || res["conflict"] != nil {
			t.Fatalf("context-free write = %v, want causal=none last-writer-wins", res)
		}
		got, _ := h.HandleGetMemory(ctx, call(map[string]any{"project": "p1", "as": "a", "key": "auth-policy"}))
		if n := len(parseJSON(t, got)["memories"].([]any)); n != 1 {
			t.Fatalf("live rows = %d, want 1 (legacy archive)", n)
		}
	})

	t.Run("GetMemoryDoesNoDBWrite", func(t *testing.T) {
		d, path := memDB(t)
		h := memHandlersAt(t, d)
		setMem(t, h, "a", map[string]any{"value": "one"})
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
			getMemID(t, h, "a")
		}
		if after := version(); after != before {
			t.Fatalf("get_memory committed a write (data_version %d -> %d)", before, after)
		}
	})

	t.Run("CacheBoundedAndTTL", func(t *testing.T) {
		now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		c := memoryReadCache{max: 2, ttl: time.Hour, now: func() time.Time { return now }}
		c.record("p", "a", "project", "k1", "id1")
		now = now.Add(time.Minute)
		c.record("p", "a", "project", "k2", "id2")
		now = now.Add(time.Minute)
		c.record("p", "a", "project", "k3", "id3") // full: evicts k1, the oldest
		if got := c.lookup("p", "a", "project", "k1"); got != "" {
			t.Fatalf("oldest entry not evicted: %q", got)
		}
		if got := c.lookup("p", "a", "project", "k3"); got != "id3" {
			t.Fatalf("k3 = %q, want id3", got)
		}
		if len(c.entries) != 2 {
			t.Fatalf("cache holds %d entries, max 2", len(c.entries))
		}
		now = now.Add(2 * time.Hour)
		if got := c.lookup("p", "a", "project", "k3"); got != "" {
			t.Fatalf("expired read still served: %q", got)
		}
	})

	t.Run("RestartDegradesToLegacyNotConflict", func(t *testing.T) {
		d, _ := memDB(t)
		h := memHandlersAt(t, d)
		setMem(t, h, "a", map[string]any{"value": "sessions: cookie"})
		getMemID(t, h, "a")
		setMem(t, h, "b", map[string]any{"value": "JWT prohibited"})
		restarted := memHandlersAt(t, d) // same DB, empty read cache
		res := setMem(t, restarted, "a", map[string]any{"value": "sessions: cookie, 30d"})
		if res["causal"] != "none" || res["conflict"] != nil {
			t.Fatalf("after restart = %v, want causal=none and no conflict", res)
		}
	})

	t.Run("CausalLogLine", func(t *testing.T) {
		d, _ := memDB(t)
		h := memHandlersAt(t, d)
		out := captureLinkageLog(t, func() {
			setMem(t, h, "a", map[string]any{"value": "one"})                   // fresh
			setMem(t, h, "a", map[string]any{"value": "one"})                   // noop
			v2 := memID(setMem(t, h, "a", map[string]any{"value": "two"}))      // fast-forward
			setMem(t, h, "b", map[string]any{"value": "three", "based_on": v2}) // fast-forward on the live row
		})
		for _, want := range []string{
			"[memory] causal=none outcome=fresh",
			"[memory] causal=none outcome=noop",
			"[memory] causal=none outcome=fast-forward",
			"[memory] causal=arg outcome=fast-forward",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q in:\n%s", want, out)
			}
		}
		if n := strings.Count(out, "[memory] causal="); n != 4 {
			t.Fatalf("%d [memory] lines for 4 writes", n)
		}
	})

	t.Run("ConflictEventEmitted", func(t *testing.T) {
		d, _ := memDB(t)
		h := memHandlersAt(t, d)
		res, bID := lostUpdate(t, h, "behavior", nil)
		for _, e := range h.events.Recent("p1", 100) {
			if e.Type == "event:memory-conflict" {
				return
			}
		}
		t.Fatalf("no event:memory-conflict after sibling %v (current %s)", res, bID)
	})

	t.Run("BasedOnWrongKeyRejected", func(t *testing.T) {
		d, _ := memDB(t)
		h := memHandlersAt(t, d)
		other := memID(setMem(t, h, "a", map[string]any{"key": "other-key", "value": "x"}))
		res, _ := h.HandleSetMemory(ctx, call(map[string]any{"project": "p1", "as": "a", "key": "auth-policy", "value": "y", "based_on": other}))
		if msg := expectError(t, res); !strings.Contains(msg, "based_on does not match key/scope") {
			t.Fatalf("error = %s", msg)
		}
	})
}
