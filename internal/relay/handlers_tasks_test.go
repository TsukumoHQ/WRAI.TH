package relay

import (
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// Typed edges over MCP (design d523e74e slice 2, ruling b3a6ab43): a held
// dispatch is announced exactly once when released, by the handler or the
// sweeper; claim next=true pulls the next ready task; a claim of a task with
// unmet prerequisites succeeds and names them; task_edge rejects unregistered
// types and cycles.
func TestTaskGraph(t *testing.T) {
	setup := func(t *testing.T) *Handlers {
		t.Helper()
		h := testHandlers(t)
		prof := "dev"
		for _, name := range []string{"w1", "w2"} {
			if _, _, err := h.db.RegisterAgent("p1", name, "worker", "", nil, &prof, false, nil, "[]", 0, db.RegisterOptions{ProfileSlugSet: true}); err != nil {
				t.Fatalf("register %s: %v", name, err)
			}
		}
		return h
	}
	dispatch := func(t *testing.T, h *Handlers, args map[string]any) string {
		t.Helper()
		base := map[string]any{"project": "p1", "as": "cto", "profile": "dev"}
		for k, v := range args {
			base[k] = v
		}
		res, _ := h.HandleDispatchTask(ctx, call(base))
		return parseJSON(t, res)["task"].(map[string]any)["id"].(string)
	}
	dispatched := func(h *Handlers, taskID string) int {
		n := 0
		for _, ev := range h.events.Recent("p1", 0) {
			if ev.Type == "task.dispatched" && ev.Semantic["task_id"] == taskID {
				n++
			}
		}
		return n
	}
	unread := func(t *testing.T, h *Handlers, agent string) int {
		t.Helper()
		n, err := h.db.UnreadCountForAgent("p1", agent)
		if err != nil {
			t.Fatalf("unread %s: %v", agent, err)
		}
		return n
	}

	t.Run("DispatchBlockedByHoldsDelivery", func(t *testing.T) {
		h := setup(t)
		a := dispatch(t, h, map[string]any{"title": "prereq A"})
		before1, before2 := unread(t, h, "w1"), unread(t, h, "w2")
		b := dispatch(t, h, map[string]any{"title": "dependent B", "blocked_by": []any{a[:8]}})
		if unread(t, h, "w1") != before1 || unread(t, h, "w2") != before2 {
			t.Fatal("a held dispatch must deliver nothing")
		}
		if n := dispatched(h, b); n != 0 {
			t.Fatalf("held B emitted %d task.dispatched, want 0", n)
		}
		if !h.db.TaskHeld("p1", b) {
			t.Fatal("B is not held")
		}

		parseJSON(t, result(h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "task_id": a}))))
		parseJSON(t, result(h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "task_id": a}))))
		if n := dispatched(h, b); n != 1 {
			t.Fatalf("released B emitted %d task.dispatched, want 1", n)
		}
		if got1, got2 := unread(t, h, "w1")-before1, unread(t, h, "w2")-before2; got1 != 1 || got2 != 1 {
			t.Fatalf("release deliveries w1=%d w2=%d, want one per profile agent", got1, got2)
		}
		// The sweeper finds nothing left to announce.
		if n := releaseHeldTasks(h, time.Now().Add(time.Hour)); n != 0 {
			t.Fatalf("sweeper re-announced %d after an inline announce", n)
		}
		if n := dispatched(h, b); n != 1 {
			t.Fatalf("B task.dispatched = %d after sweep, want 1", n)
		}
	})

	t.Run("SweeperAnnouncesMissedRelease", func(t *testing.T) {
		h := setup(t)
		a := dispatch(t, h, map[string]any{"title": "prereq A"})
		b := dispatch(t, h, map[string]any{"title": "dependent B", "blocked_by": []any{a}})
		before := unread(t, h, "w1")
		// A status write that bypasses the handlers (the Linear sync path):
		// the hold is released in the transition tx, but nobody announces.
		if _, err := h.db.CompleteTask(a, "linear-sync", "p1", nil); err != nil {
			t.Fatalf("complete A: %v", err)
		}
		if n := dispatched(h, b); n != 0 {
			t.Fatalf("B announced %d times before the sweep", n)
		}
		// Inside the grace window the sweeper leaves it to an in-flight handler.
		if n := releaseHeldTasks(h, time.Now()); n != 0 {
			t.Fatalf("sweeper announced %d inside the grace window", n)
		}
		if n := releaseHeldTasks(h, time.Now().Add(2*releaseAnnounceGrace)); n != 1 {
			t.Fatalf("sweeper announced %d, want 1", n)
		}
		if n := releaseHeldTasks(h, time.Now().Add(2*releaseAnnounceGrace)); n != 0 {
			t.Fatalf("second sweep announced %d, want 0", n)
		}
		if n := dispatched(h, b); n != 1 {
			t.Fatalf("B task.dispatched = %d, want 1", n)
		}
		if got := unread(t, h, "w1") - before; got != 1 {
			t.Fatalf("w1 got %d deliveries, want 1", got)
		}
	})

	t.Run("InlineAndSweeperRaceAnnounceOnce", func(t *testing.T) {
		h := setup(t)
		for i := 0; i < 20; i++ {
			a := dispatch(t, h, map[string]any{"title": "prereq"})
			b := dispatch(t, h, map[string]any{"title": "dependent", "blocked_by": []any{a}})
			done, err := h.db.CompleteTask(a, "w1", "p1", nil)
			if err != nil {
				t.Fatalf("complete A: %v", err)
			}
			if len(done.Released) != 1 || done.Released[0] != b {
				t.Fatalf("Released = %v, want [%s]", done.Released, b)
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); h.announceReleased("p1", done.Released) }()
			go func() { defer wg.Done(); releaseHeldTasks(h, time.Now().Add(time.Hour)) }()
			wg.Wait()
			if n := dispatched(h, b); n != 1 {
				t.Fatalf("round %d: B task.dispatched = %d, want exactly 1", i, n)
			}
		}
	})

	t.Run("ClaimNextOverMCP", func(t *testing.T) {
		h := setup(t)
		urgent := dispatch(t, h, map[string]any{"title": "urgent leaf", "priority": "P0"})
		hub := dispatch(t, h, map[string]any{"title": "unblocks two", "priority": "P3"})
		dispatch(t, h, map[string]any{"title": "needs hub 1", "blocked_by": []any{hub}})
		dispatch(t, h, map[string]any{"title": "needs hub 2", "blocked_by": []any{hub}})

		list := parseJSON(t, result(h.HandleListTasks(ctx, call(map[string]any{"project": "p1", "ready": true, "format": "json"}))))
		if list["count"].(float64) != 2 {
			t.Fatalf("ready count = %v, want 2 (urgent, hub)", list["count"])
		}

		got := parseJSON(t, result(h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "next": true, "sort": "unblock_impact"}))))
		if got["id"] != hub {
			t.Fatalf("unblock_impact claimed %v, want hub %s", got["id"], hub)
		}
		got = parseJSON(t, result(h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "w2", "next": true}))))
		if got["id"] != urgent {
			t.Fatalf("priority claimed %v, want urgent %s", got["id"], urgent)
		}
		got = parseJSON(t, result(h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "next": true}))))
		if v, ok := got["task"]; !ok || v != nil || got["ready_count"].(float64) != 0 {
			t.Fatalf("nothing ready: got %v, want {task:null, ready_count:0}", got)
		}
		if res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "next": true, "sort": "random"})); !res.IsError {
			t.Fatal("unknown sort accepted")
		}
	})

	t.Run("ClaimNotReadyWarnsWithBlockers", func(t *testing.T) {
		h := setup(t)
		a := dispatch(t, h, map[string]any{"title": "prereq A"})
		b := dispatch(t, h, map[string]any{"title": "dependent B", "blocked_by": []any{a + "@in-review"}})
		got := parseJSON(t, result(h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "task_id": b}))))
		if got["status"] != "accepted" {
			t.Fatalf("claim of a held task: status %v, want accepted", got["status"])
		}
		r, ok := got["readiness"].(map[string]any)
		if !ok || r["ready"] != false {
			t.Fatalf("readiness = %v, want ready:false", got["readiness"])
		}
		bl := r["blockers"].([]any)
		if len(bl) != 1 {
			t.Fatalf("blockers = %v, want 1", bl)
		}
		first := bl[0].(map[string]any)
		if first["id"] != a || first["title"] != "prereq A" || first["until"] != "in-review" || first["status"] != "pending" {
			t.Fatalf("blocker = %v, want id/title/status/until of A", first)
		}
		// A ready task carries no readiness warning.
		c := dispatch(t, h, map[string]any{"title": "free C"})
		if got := parseJSON(t, result(h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "w2", "task_id": c})))); got["readiness"] != nil {
			t.Fatalf("ready task carried readiness %v", got["readiness"])
		}
	})

	t.Run("EdgeToolRegistryErrors", func(t *testing.T) {
		h := setup(t)
		a := dispatch(t, h, map[string]any{"title": "A"})
		b := dispatch(t, h, map[string]any{"title": "B"})
		edge := func(args map[string]any) *mcp.CallToolResult {
			base := map[string]any{"project": "p1", "as": "cto"}
			for k, v := range args {
				base[k] = v
			}
			res, _ := h.HandleTaskEdge(ctx, call(base))
			return res
		}
		msg := expectError(t, edge(map[string]any{"op": "add", "task_id": a, "type": "relates_to", "target_id": b}))
		if !strings.Contains(msg, db.CodeEdgeInvalidArgument) {
			t.Fatalf("unknown type: %s, want INVALID_ARGUMENT", msg)
		}
		msg = expectError(t, edge(map[string]any{"op": "link", "task_id": a, "type": "blocked_by", "target_id": b}))
		if !strings.Contains(msg, "INVALID_ARGUMENT") {
			t.Fatalf("unknown op: %s, want INVALID_ARGUMENT", msg)
		}

		// a blocked_by b holds a (already announced at dispatch: no un-send).
		got := parseJSON(t, edge(map[string]any{"op": "add", "task_id": a, "type": "blocked_by", "target_id": b[:8]}))
		if got["ready"] != false {
			t.Fatalf("a blocked_by b: ready = %v, want false", got["ready"])
		}
		msg = expectError(t, edge(map[string]any{"op": "add", "task_id": b, "type": "blocked_by", "target_id": a}))
		if !strings.Contains(msg, db.CodeEdgeCycle) || !strings.Contains(msg, a) || !strings.Contains(msg, b) {
			t.Fatalf("cycle: %s, want EDGE_CYCLE naming the path", msg)
		}

		// Removing the edge releases a and announces it once.
		before := dispatched(h, a)
		got = parseJSON(t, edge(map[string]any{"op": "remove", "task_id": a, "type": "blocked_by", "target_id": b}))
		if rel, _ := got["released"].([]any); len(rel) != 1 || rel[0] != a || got["ready"] != true {
			t.Fatalf("remove: %v, want a released and ready", got)
		}
		if n := dispatched(h, a) - before; n != 1 {
			t.Fatalf("remove announced a %d times, want 1", n)
		}
	})

	t.Run("HeldTaskSkipsACKLadder", func(t *testing.T) {
		d, path := memDB(t)
		h := memHandlersAt(t, d)
		raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		prof := "dev"
		if _, _, err := d.RegisterAgent("p1", "w1", "worker", "", nil, &prof, false, nil, "[]", 0, db.RegisterOptions{ProfileSlugSet: true}); err != nil {
			t.Fatalf("register: %v", err)
		}
		a := dispatch(t, h, map[string]any{"title": "prereq A"})
		b := dispatch(t, h, map[string]any{"title": "dependent B", "blocked_by": []any{a}})
		if _, err := raw.Exec(`UPDATE tasks SET dispatched_at = ?, pending_since = ? WHERE id IN (?, ?)`, ago(20*time.Minute), ago(20*time.Minute), a, b); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		acked := func(id string) bool {
			task, _ := d.GetTask(id, "p1")
			return task != nil && task.AckNotifiedAt != nil
		}
		evaluateObligations(d, h.registry, time.Now().UTC())
		if !acked(a) {
			t.Fatal("control: the unclaimed ready task A got no ACK rung")
		}
		if acked(b) {
			t.Fatal("held task B entered the ACK ladder")
		}
		// Release restarts B's ACK clock: time held never reads as unacked.
		result(h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "task_id": a})))
		result(h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "task_id": a})))
		evaluateObligations(d, h.registry, time.Now().UTC())
		if acked(b) {
			t.Fatal("released task B was escalated on the time it spent held")
		}
	})

	t.Run("DiscoveredFromInheritsProfile", func(t *testing.T) {
		h := setup(t)
		origin := dispatch(t, h, map[string]any{"title": "origin"})
		res, _ := h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "w1", "title": "found while working", "discovered_from": origin}))
		task := parseJSON(t, res)["task"].(map[string]any)
		if task["profile_slug"] != "dev" {
			t.Fatalf("profile = %v, want dev inherited from origin", task["profile_slug"])
		}
		if n := dispatched(h, task["id"].(string)); n != 1 {
			t.Fatalf("discovered task announced %d times, want 1 (display-only edge never holds)", n)
		}
	})
}

// result drops a handler's (always nil) error so a call nests in parseJSON.
func result(res *mcp.CallToolResult, _ error) *mcp.CallToolResult { return res }
