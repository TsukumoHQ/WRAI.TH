package relay

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"agent-relay/internal/db"
)

// Compiled guards, slice S2a (task 1c56b1c3): the one guard(op=...) tool over
// the S1 db layer. Exceptions come from the real producer path (a dead-lane
// block), so every open runs the open-point guard evaluation.

type guardFixture struct {
	t   *testing.T
	h   *Handlers
	raw *sql.DB
	n   int
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	d, path := memDB(t)
	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &guardFixture{t: t, h: memHandlersAt(t, d), raw: raw}
}

// block opens one dead-lane block exception raised by dev; resolve=true lets
// dev resume the task, so the exception is resolved by self (resolver = dev).
func (f *guardFixture) block(dev string, resolve bool) string {
	f.t.Helper()
	f.n++
	reason := "B2 dead-lane disposition: daemon-side cancel+archive"
	task, err := f.h.db.DispatchTask("p1", "prof-"+dev, "cto", fmt.Sprintf("t%d", f.n), "", "P2", nil, nil, db.TypedTicket{}, false, nil)
	if err != nil {
		f.t.Fatalf("dispatch: %v", err)
	}
	if _, err := f.h.db.StartTask(task.ID, dev, "p1"); err != nil {
		f.t.Fatalf("start: %v", err)
	}
	if _, err := f.h.db.BlockTask(task.ID, dev, "p1", &reason); err != nil {
		f.t.Fatalf("block: %v", err)
	}
	if resolve {
		if _, err := f.h.db.StartTask(task.ID, dev, "p1"); err != nil {
			f.t.Fatalf("resume: %v", err)
		}
	}
	var id string
	if err := f.raw.QueryRow(`SELECT id FROM exceptions WHERE task_id = ?`, task.ID).Scan(&id); err != nil {
		f.t.Fatalf("exception of %s: %v", task.ID, err)
	}
	return id
}

func (f *guardFixture) guard(args map[string]any) map[string]any {
	f.t.Helper()
	args["project"] = "p1"
	res, _ := f.h.HandleGuard(ctx, call(args))
	return parseJSON(f.t, res)
}

// refused asserts the guard call fails with code.
func (f *guardFixture) refused(code string, args map[string]any) {
	f.t.Helper()
	if _, ok := args["project"]; !ok {
		args["project"] = "p1"
	}
	res, _ := f.h.HandleGuard(ctx, call(args))
	if msg := expectError(f.t, res); !strings.Contains(msg, `"code":"`+code+`"`) {
		f.t.Fatalf("guard %v: %s, want code %s", args, msg, code)
	}
}

func (f *guardFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.raw.QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// compiled compiles a 30-day suppress guard from a fresh resolved origin by
// its resolver dev-a, after one earlier resolved sibling (the replay's match).
func (f *guardFixture) compiled() map[string]any {
	f.t.Helper()
	f.block("dev-a", true)
	origin := f.block("dev-a", true)
	return f.guard(map[string]any{"as": "dev-a", "op": "compile", "id": origin, "action": "suppress", "days": 30})
}

// promoted is compiled() plus 3 shadow hits, promoted by "third".
func (f *guardFixture) promoted() string {
	f.t.Helper()
	id := f.compiled()["id"].(string)
	for i := 0; i < 3; i++ {
		f.block(fmt.Sprintf("dev-%d", i), false)
	}
	if g := f.guard(map[string]any{"as": "third", "op": "promote", "id": id}); g["mode"] != db.GuardModeActive {
		f.t.Fatalf("promote: %v", g)
	}
	return id
}

func TestGuardTool(t *testing.T) {
	t.Run("CompileReturnsReplay", func(t *testing.T) {
		f := newGuardFixture(t)
		g := f.compiled()
		if g["mode"] != db.GuardModeShadow || g["point"] != db.GuardPointOpen || g["created_by"] != "dev-a" {
			t.Fatalf("compiled guard: %v", g)
		}
		r := g["replay"].(map[string]any)
		if r["window_days"] != 90.0 || r["matched"] != 1.0 || r["agree"] != 1.0 || r["conflicts"] != 0.0 || r["undecided"] != 0.0 {
			t.Fatalf("replay = %v, want 90d matched 1 agree 1", r)
		}
		if got := f.guard(map[string]any{"as": "anyone", "op": "get", "id": g["id"]}); got["id"] != g["id"] {
			t.Fatalf("get: %v", got)
		}
		f.refused(db.GuardErrBadAction, map[string]any{"as": "dev-a", "op": "compile", "id": g["from_exception_id"], "action": "deny", "days": 30})
	})

	t.Run("CompileRefusedThirdParty", func(t *testing.T) {
		f := newGuardFixture(t)
		origin := f.block("dev-a", true)
		f.refused(db.GuardErrForbidden, map[string]any{"as": "mallory", "op": "compile", "id": origin, "action": "suppress", "days": 30})
		if n := f.count(`SELECT COUNT(*) FROM compiled_guards`); n != 0 {
			t.Fatalf("compiled_guards = %d after a third-party compile, want 0", n)
		}
	})

	t.Run("PromoteRefusedSameAgent", func(t *testing.T) {
		f := newGuardFixture(t)
		id := f.compiled()["id"].(string)
		for i := 0; i < 3; i++ {
			f.block(fmt.Sprintf("dev-%d", i), false)
		}
		// dev-a is both created_by and the origin's resolver.
		f.refused(db.GuardErrPromoteRefused, map[string]any{"as": "dev-a", "op": "promote", "id": id})
		g := f.guard(map[string]any{"as": "third", "op": "promote", "id": id})
		if g["mode"] != db.GuardModeActive || g["challenged_by"] != "third" || g["shadow_hits"] != 3.0 {
			t.Fatalf("promoted by third: %v", g)
		}
	})

	t.Run("RenewNeedsLiveHitsAndOtherAgent", func(t *testing.T) {
		f := newGuardFixture(t)
		id := f.promoted()
		f.refused(db.GuardErrRenewRefused, map[string]any{"as": "third", "op": "renew", "id": id, "days": 60})
		f.block("dev-9", false)
		f.refused(db.GuardErrRenewRefused, map[string]any{"as": "dev-a", "op": "renew", "id": id, "days": 60})
		before := f.guard(map[string]any{"as": "third", "op": "get", "id": id})["expires_at"]
		g := f.guard(map[string]any{"as": "third", "op": "renew", "id": id, "days": 60})
		if g["live_hits"] != 1.0 || g["expires_at"] == before {
			t.Fatalf("renewed: %v (expires before %v)", g, before)
		}
	})

	t.Run("WithdrawStopsMatching", func(t *testing.T) {
		f := newGuardFixture(t)
		id := f.promoted()
		f.block("dev-8", false) // one live hit while active
		f.refused(db.GuardErrNotFound, map[string]any{"as": "dev-a", "op": "withdraw", "id": id, "project": "other"})
		f.refused(db.GuardErrForbidden, map[string]any{"as": "mallory", "op": "withdraw", "id": id})
		g := f.guard(map[string]any{"as": "dev-a", "op": "withdraw", "id": id})
		if g["mode"] != db.GuardModeRetired || g["ended_reason"] != "withdrawn" {
			t.Fatalf("withdrawn: %v", g)
		}
		hits := f.count(`SELECT COUNT(*) FROM guard_hits WHERE guard_id = ?`, id)
		f.block("dev-9", false)
		if n := f.count(`SELECT COUNT(*) FROM guard_hits WHERE guard_id = ?`, id); n != hits {
			t.Fatalf("guard_hits %d -> %d after withdraw, want no new hit", hits, n)
		}
		if g := f.guard(map[string]any{"as": "dev-a", "op": "get", "id": id}); g["live_hits"] != 1.0 {
			t.Fatalf("withdrawn guard still acted: %v", g)
		}
		f.refused(db.GuardErrWithdrawRefused, map[string]any{"as": "dev-a", "op": "withdraw", "id": id})
	})
}
