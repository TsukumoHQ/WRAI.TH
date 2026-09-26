package db

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Graph S1 (design d523e74e §2, §3, §6 slice 1; ruling b3a6ab43).

func edgeTask(t *testing.T, d *DB, ticket TypedTicket) string {
	t.Helper()
	task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, ticket, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return task.ID
}

func edgeCode(err error) string {
	var te *TaskError
	if errors.As(err, &te) {
		return te.Code
	}
	return ""
}

func edgeCount(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM org_edges`).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	return n
}

func holdState(t *testing.T, d *DB, taskID string) (held bool, released bool, flag string) {
	t.Helper()
	var rel, fl *string
	err := d.conn.QueryRow(`SELECT released_at, flagged FROM task_holds WHERE task_id = ?`, taskID).Scan(&rel, &fl)
	if err != nil {
		return false, false, ""
	}
	if fl != nil {
		flag = *fl
	}
	return true, rel != nil, flag
}

func finish(t *testing.T, d *DB, id string) *[]string {
	t.Helper()
	task, err := d.CompleteTask(id, "human", "p1", nil)
	if err != nil {
		t.Fatalf("complete %s: %v", id, err)
	}
	return &task.Released
}

func sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func TestOrgEdges(t *testing.T) {
	t.Run("UnregisteredTypeRejected", func(t *testing.T) {
		d := testDB(t)
		a, b := edgeTask(t, d, TypedTicket{}), edgeTask(t, d, TypedTicket{})
		err := d.AddEdge("p1", EdgeInput{SrcKind: "task", SrcID: a, Type: "relates_to", DstKind: "task", DstID: b, CreatedBy: "cto"})
		if edgeCode(err) != CodeEdgeInvalidArgument {
			t.Fatalf("unregistered type: %v, want INVALID_ARGUMENT", err)
		}
		err = d.AddEdge("p1", EdgeInput{SrcKind: "memory", SrcID: a, Type: EdgeBlockedBy, DstKind: "task", DstID: b, CreatedBy: "cto"})
		if edgeCode(err) != CodeEdgeInvalidArgument {
			t.Fatalf("disallowed kind pair: %v, want INVALID_ARGUMENT", err)
		}
		if _, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{BlockedBy: []string{b + "@soon"}}, false, nil); edgeCode(err) != CodeEdgeInvalidArgument {
			t.Fatalf("bad until suffix: %v, want INVALID_ARGUMENT", err)
		}
		if n := edgeCount(t, d); n != 0 {
			t.Fatalf("rejected edges wrote %d rows", n)
		}
	})

	t.Run("CycleRejected", func(t *testing.T) {
		d := testDB(t)
		a := edgeTask(t, d, TypedTicket{})
		b := edgeTask(t, d, TypedTicket{BlockedBy: []string{a}})
		err := d.AddEdge("p1", EdgeInput{SrcKind: "task", SrcID: a, Type: EdgeBlockedBy, DstKind: "task", DstID: b, CreatedBy: "cto"})
		if edgeCode(err) != CodeEdgeCycle || !strings.Contains(err.Error(), a) {
			t.Fatalf("a->b->a: %v, want EDGE_CYCLE naming the path", err)
		}
		// A chain longer than the bound: the closing edge is refused, not guessed.
		chain := []string{edgeTask(t, d, TypedTicket{})}
		for i := 0; i < cycleCheckLimit+10; i++ {
			chain = append(chain, edgeTask(t, d, TypedTicket{BlockedBy: []string{chain[len(chain)-1]}}))
		}
		err = d.AddEdge("p1", EdgeInput{SrcKind: "task", SrcID: chain[0], Type: EdgeBlockedBy, DstKind: "task", DstID: chain[len(chain)-1], CreatedBy: "cto"})
		if edgeCode(err) != CodeEdgeCheckLimit {
			t.Fatalf("over-bound chain: %v, want EDGE_CHECK_LIMIT", err)
		}
	})

	t.Run("LinearSrcBlockingRefused", func(t *testing.T) {
		d := testDB(t)
		lin, nat := edgeTask(t, d, TypedTicket{}), edgeTask(t, d, TypedTicket{})
		if _, err := d.conn.Exec(`UPDATE tasks SET source = 'linear' WHERE id = ?`, lin); err != nil {
			t.Fatal(err)
		}
		err := d.AddEdge("p1", EdgeInput{SrcKind: "task", SrcID: lin, Type: EdgeBlockedBy, DstKind: "task", DstID: nat, CreatedBy: "cto"})
		if edgeCode(err) != CodeLinearReadOnly {
			t.Fatalf("linear src: %v, want LINEAR_READ_ONLY", err)
		}
		if err := d.AddEdge("p1", EdgeInput{SrcKind: "task", SrcID: nat, Type: EdgeBlockedBy, DstKind: "task", DstID: lin, CreatedBy: "cto"}); err != nil {
			t.Fatalf("native blocked_by linear: %v, want accepted", err)
		}
		// Display-only edges are not blocking: allowed out of a mirrored task.
		if err := d.AddEdge("p1", EdgeInput{SrcKind: "task", SrcID: lin, Type: EdgeDiscoveredFrom, DstKind: "task", DstID: nat, CreatedBy: "cto"}); err != nil {
			t.Fatalf("discovered_from out of linear: %v", err)
		}
	})

	t.Run("TaskColumnsUntouched", func(t *testing.T) {
		d := testDB(t)
		a := edgeTask(t, d, TypedTicket{})
		b := edgeTask(t, d, TypedTicket{BlockedBy: []string{a}})
		if _, err := scanTask(d.conn.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, b)); err != nil {
			t.Fatalf("scanTask over taskColumns: %v", err)
		}
		for _, c := range []string{"ready", "depends_on", "blocked_by", "edge"} {
			if strings.Contains(taskColumns, c) {
				t.Fatalf("taskColumns gained %q; readiness must stay derived and side-tabled", c)
			}
		}
	})

	t.Run("OldDBMigrates", func(t *testing.T) {
		d := testDB(t)
		for _, stmt := range []string{`DROP TABLE org_edges`, `DROP TABLE edge_semantics`, `DROP TABLE task_holds`} {
			if _, err := d.conn.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		for i := 0; i < 2; i++ {
			if err := migrate(d.conn); err != nil {
				t.Fatalf("migrate #%d: %v", i+1, err)
			}
		}
		var n int
		_ = d.conn.QueryRow(`SELECT COUNT(*) FROM edge_semantics`).Scan(&n)
		if n != 2 {
			t.Fatalf("registry rows = %d, want 2 (seed idempotent)", n)
		}
		if edgeCount(t, d) != 0 {
			t.Fatal("migration invented edges (no backfill)")
		}
	})

	t.Run("ReadyPredicateSharedByListAndClaim", func(t *testing.T) {
		d := testDB(t)
		mk := func() string { return edgeTask(t, d, TypedTicket{}) }
		done1, done2, review, cancelled, pendingPre := mk(), mk(), mk(), mk(), mk()
		finish(t, d, done1)
		finish(t, d, done2)
		if _, err := d.transitionTask(review, "human", "p1", "in-review", nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := d.CancelTask(cancelled, "human", "p1", nil); err != nil {
			t.Fatal(err)
		}
		bt := func(b ...string) string { return edgeTask(t, d, TypedTicket{BlockedBy: b}) }
		want := map[string]bool{}
		want[bt(done1)] = true               // done: ready
		want[bt(done1, done2)] = true        // both done
		want[bt(review+"@in-review")] = true // in-review satisfies until=in-review
		bt(review)                           // default until=done: not ready
		bt(cancelled)                        // cancelled never satisfies
		bt(pendingPre)                       // unfinished
		removed := bt(pendingPre)            // edge removed below: ready
		expired := bt(pendingPre)            // edge expired below: ready
		if _, err := d.RemoveEdge("p1", removed, EdgeBlockedBy, pendingPre, "cto"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.conn.Exec(`UPDATE org_edges SET valid_until = '2000-01-01T00:00:00.000000Z' WHERE src_id = ?`, expired); err != nil {
			t.Fatal(err)
		}
		want[removed], want[expired] = true, true
		want[pendingPre] = true // no edges of its own
		for _, id := range []string{mk()} {
			want[id] = true
		}
		listed, err := d.ListReadyTasks("p1", "dev", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		var listIDs, claimed []string
		for _, x := range listed {
			listIDs = append(listIDs, x.ID)
		}
		for {
			c, err := d.ClaimNextTask("p1", "dev-a", "dev", "", SortPriority)
			if err != nil {
				t.Fatal(err)
			}
			if c == nil {
				break
			}
			claimed = append(claimed, c.ID)
		}
		var wantIDs []string
		for id := range want {
			wantIDs = append(wantIDs, id)
		}
		if a, b, c := sortedIDs(listIDs), sortedIDs(claimed), sortedIDs(wantIDs); strings.Join(a, ",") != strings.Join(b, ",") || strings.Join(a, ",") != strings.Join(c, ",") {
			t.Fatalf("ready listing %v\nclaim drain  %v\nexpected     %v", a, b, c)
		}
	})

	t.Run("HeldUntilReadyExactlyOnce", func(t *testing.T) {
		d := testDB(t)
		a := edgeTask(t, d, TypedTicket{})
		b := edgeTask(t, d, TypedTicket{BlockedBy: []string{a}})
		if held, rel, _ := holdState(t, d, b); !held || rel || !d.TaskHeld("p1", b) {
			t.Fatalf("b not held at dispatch (held=%v released=%v)", held, rel)
		}
		if ready, refs, _ := d.TaskReadiness("p1", b); ready || len(refs) != 1 || refs[0].ID != a {
			t.Fatalf("readiness = %v %v, want not ready, blocked by a", ready, refs)
		}
		if released := finish(t, d, a); strings.Join(*released, ",") != b {
			t.Fatalf("completing a released %v, want [b]", *released)
		}
		if again, err := d.ReleaseReadyHolds(time.Now()); err != nil || len(again) != 0 {
			t.Fatalf("sweep re-released %v (%v)", again, err)
		}
		// A task with no blocker is never held.
		c := edgeTask(t, d, TypedTicket{BlockedBy: []string{a}})
		if held, _, _ := holdState(t, d, c); held {
			t.Fatal("task blocked by a done prerequisite was held")
		}
	})

	t.Run("InReviewSatisfiesUntilInReview", func(t *testing.T) {
		d := testDB(t)
		a := edgeTask(t, d, TypedTicket{})
		early := edgeTask(t, d, TypedTicket{BlockedBy: []string{a + "@in-review"}})
		late := edgeTask(t, d, TypedTicket{BlockedBy: []string{a}})
		task, err := d.transitionTask(a, "human", "p1", "in-review", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(task.Released, ",") != early {
			t.Fatalf("in-review released %v, want only the until=in-review dependent", task.Released)
		}
		if _, rel, _ := holdState(t, d, late); rel {
			t.Fatal("default until=done dependent released at in-review")
		}
	})

	t.Run("PrerequisiteCancelledFlagsNotReleases", func(t *testing.T) {
		d := testDB(t)
		a := edgeTask(t, d, TypedTicket{})
		b := edgeTask(t, d, TypedTicket{BlockedBy: []string{a}})
		task, err := d.CancelTask(a, "human", "p1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(task.Released) != 0 {
			t.Fatalf("cancel released %v", task.Released)
		}
		if held, rel, flag := holdState(t, d, b); !held || rel || flag != HoldFlagPrerequisiteCancelled {
			t.Fatalf("b hold = held %v released %v flag %q, want held + prerequisite_cancelled", held, rel, flag)
		}
		if bt, _ := d.GetTask(b, "p1"); bt.Status != "pending" {
			t.Fatalf("b auto-changed to %s", bt.Status)
		}
		released, err := d.RemoveEdge("p1", b, EdgeBlockedBy, a, "cto")
		if err != nil || strings.Join(released, ",") != b {
			t.Fatalf("remove edge released %v (%v), want [b]", released, err)
		}
	})

	t.Run("SettleBounded", func(t *testing.T) {
		d := testDB(t)
		a := edgeTask(t, d, TypedTicket{})
		var deps []string
		for i := 0; i < 100; i++ {
			deps = append(deps, edgeTask(t, d, TypedTicket{BlockedBy: []string{a}}))
		}
		first := *finish(t, d, a)
		if len(first) != settleLimit {
			t.Fatalf("one transition released %d, want %d", len(first), settleLimit)
		}
		rest, err := d.ReleaseReadyHolds(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		all := map[string]int{}
		for _, id := range first {
			all[id]++
		}
		for _, h := range rest {
			all[h.TaskID]++
		}
		if len(all) != 100 {
			t.Fatalf("released %d distinct of 100", len(all))
		}
		for id, n := range all {
			if n != 1 {
				t.Fatalf("%s released %d times", id, n)
			}
		}
	})

	t.Run("ClaimNextWalksPastRace", func(t *testing.T) {
		d := testDB(t)
		edgeTask(t, d, TypedTicket{})
		edgeTask(t, d, TypedTicket{})
		var wg sync.WaitGroup
		got := make([]string, 2)
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c, err := d.ClaimNextTask("p1", fmt.Sprintf("dev-%d", i), "dev", "", SortPriority)
				errs[i] = err
				if c != nil {
					got[i] = c.ID
				}
			}(i)
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || got[0] == "" || got[1] == "" || got[0] == got[1] {
			t.Fatalf("concurrent claim-next = %v (%v), want 2 distinct tasks", got, errs)
		}
		if c, err := d.ClaimNextTask("p1", "dev-3", "dev", "", SortPriority); c != nil || err != nil {
			t.Fatalf("nothing left: got %v (%v)", c, err)
		}
	})

	t.Run("UnblockImpactOrders", func(t *testing.T) {
		d := testDB(t)
		lone := edgeTask(t, d, TypedTicket{})
		hub := edgeTask(t, d, TypedTicket{})
		mid := edgeTask(t, d, TypedTicket{BlockedBy: []string{hub}})
		edgeTask(t, d, TypedTicket{BlockedBy: []string{mid}})
		if n := d.UnblockImpact("p1", hub); n != 2 {
			t.Fatalf("hub impact = %d, want 2 (transitive)", n)
		}
		c, err := d.ClaimNextTask("p1", "dev-a", "dev", "", SortUnblockImpact)
		if err != nil || c == nil || c.ID != hub {
			t.Fatalf("unblock_impact picked %v (%v), want hub over %s", c, err, lone)
		}
	})

	t.Run("DiscoveredFromInherits", func(t *testing.T) {
		d := testDB(t)
		b1, err := d.CreateBoard("p1", "Board one", "board-one", "", "cto")
		if err != nil {
			t.Fatalf("board: %v", err)
		}
		origin, err := d.DispatchTask("p1", "backend", "cto", "origin", "", "P2", nil, &b1.ID, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.ClaimTask(origin.ID, "dev-a", "p1"); err != nil {
			t.Fatal(err)
		}
		child, err := d.DispatchTask("p1", "", "dev-a", "found a bug", "", "P2", nil, nil, TypedTicket{DiscoveredFrom: origin.ID}, false, nil)
		if err != nil {
			t.Fatalf("discovered dispatch: %v", err)
		}
		if child.ProfileSlug != "backend" || child.BoardID == nil || *child.BoardID != b1.ID ||
			child.TraceID == nil || *child.TraceID != *origin.TraceID || child.ParentTaskID != nil {
			t.Fatalf("inherited profile %q board %v trace %v parent %v", child.ProfileSlug, child.BoardID, child.TraceID, child.ParentTaskID)
		}
		var meta string
		if err := d.conn.QueryRow(`SELECT metadata FROM org_edges WHERE src_id = ? AND type = 'discovered_from'`, child.ID).Scan(&meta); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(meta, `"discovered_by":"dev-a"`) || !strings.Contains(meta, `"origin_holder":"dev-a"`) {
			t.Fatalf("metadata = %s, want discovered_by + origin_holder", meta)
		}
		if held, _, _ := holdState(t, d, child.ID); held {
			t.Fatal("display-only discovered_from held the task")
		}
		// Passed values win over inheritance.
		tr := "0123456789abcdef0123456789abcdef"
		own, err := d.DispatchTask("p1", "frontend", "dev-a", "mine", "", "P2", nil, &b1.ID, TypedTicket{DiscoveredFrom: origin.ID}, false, &tr)
		if err != nil {
			t.Fatal(err)
		}
		if own.ProfileSlug != "frontend" || *own.TraceID != tr {
			t.Fatalf("passed values overridden: profile %q trace %v", own.ProfileSlug, *own.TraceID)
		}
	})
}
