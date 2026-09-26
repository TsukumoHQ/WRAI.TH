package relay

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// ladderFixture is a DB with class_budget_mode=on, three blocked dead-lane
// tasks dispatched by dispatcher (the S1 producer path), and one open
// systemic exception attributed from them.
type ladderFixture struct {
	t    *testing.T
	db   *db.DB
	raw  *sql.DB
	rec  *recordingNotifier
	sys  string
	task []string
}

func newLadderFixture(t *testing.T, dispatcher, ladderJSON string, agents ...string) *ladderFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.NewTestDB(dbPath)
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	raw, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	f := &ladderFixture{t: t, db: database, raw: raw, rec: &recordingNotifier{}}
	// agents: "project/name/status/profile/reports_to/exec"
	for _, spec := range agents {
		p := strings.Split(spec, "/")
		exec := 0
		if p[5] == "1" {
			exec = 1
		}
		f.exec(`INSERT INTO agents (id, name, role, registered_at, last_seen, project, status, profile_slug, reports_to, is_executive)
			VALUES (?, ?, '', '2026-01-01T00:00:00Z', '2026-09-26T00:00:00Z', ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
			"ag-"+p[0]+"-"+p[1], p[1], p[0], p[2], p[3], p[4], exec)
	}
	database.SetSetting("budget_epoch", "2000-01-01T00:00:00.000000Z")
	database.SetSetting(db.SettingClassBudgetMode, db.ClassBudgetModeOn)
	if ladderJSON != "" {
		f.exec(`UPDATE class_budgets SET ladder_json = ? WHERE kind = 'routing' AND reason_code = '*'`, ladderJSON)
	}
	reason := "B2 dead-lane disposition: daemon-side cancel+archive"
	for i := 0; i < 3; i++ {
		// Distinct assignees and profiles: the dispatcher is the only
		// concentrated dimension, so it is the attributed doctrine owner.
		dev := fmt.Sprintf("dev-%d", i)
		task, err := database.DispatchTask("p", "prof-"+dev, dispatcher, fmt.Sprintf("t%d", i), "", "P2", nil, nil, db.TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if _, err := database.StartTask(task.ID, dev, "p"); err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, err := database.BlockTask(task.ID, dev, "p", &reason); err != nil {
			t.Fatalf("block: %v", err)
		}
		f.task = append(f.task, task.ID)
	}
	opened, err := database.EvaluateClassBudgets(time.Now())
	if err != nil || len(opened) != 1 {
		t.Fatalf("systemic: %+v %v", opened, err)
	}
	f.sys = opened[0].ID
	return f
}

func (f *ladderFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.raw.Exec(q, args...); err != nil {
		f.t.Fatalf("exec %s: %v", q, err)
	}
}

func (f *ladderFixture) tick(at time.Time) { evaluateExceptionLadders(f.db, f.rec, at) }

// rung returns the current rung name ("" when the systemic is resolved) and
// the systemic's status / resolved_by.
func (f *ladderFixture) state() (rung, status, by string) {
	f.t.Helper()
	var idx sql.NullInt64
	var snap sql.NullString
	var resolvedBy sql.NullString
	if err := f.raw.QueryRow(`SELECT rung, ladder_snapshot_json, status, resolved_by FROM exceptions WHERE id = ?`, f.sys).
		Scan(&idx, &snap, &status, &resolvedBy); err != nil {
		f.t.Fatal(err)
	}
	if rs, _ := f.db.ActiveRungs(); len(rs) == 1 {
		rung = rs[0].Name()
	}
	return rung, status, resolvedBy.String
}

// until ticks until the current rung is want (max 12 ticks).
func (f *ladderFixture) until(want string) {
	f.t.Helper()
	for i := 0; i < 12; i++ {
		if r, _, _ := f.state(); r == want {
			return
		}
		f.tick(time.Now())
	}
	r, s, _ := f.state()
	f.t.Fatalf("never reached rung %s (at %q, status %s)", want, r, s)
}

func (f *ladderFixture) actionRef() string {
	f.t.Helper()
	rs, _ := f.db.ActiveRungs()
	if len(rs) != 1 {
		f.t.Fatalf("no active rung")
	}
	return rs[0].ActionRef()
}

func (f *ladderFixture) reply(from, to, metadata string) {
	f.t.Helper()
	if _, _, err := f.db.InsertMessageWithDeliveries("p", from, "relay", "notification", "re", "answer", metadata,
		"P2", -1, &to, nil, []string{"relay"}, ""); err != nil {
		f.t.Fatal(err)
	}
}

func TestExceptionLadderRelay(t *testing.T) {
	t.Run("ShadowAndOffDoNothing", func(t *testing.T) {
		for _, mode := range []string{db.ClassBudgetModeShadow, db.ClassBudgetModeOff} {
			f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0")
			f.db.SetSetting(db.SettingClassBudgetMode, mode)
			f.tick(time.Now())
			var started int
			_ = f.raw.QueryRow(`SELECT COUNT(*) FROM exceptions WHERE rung IS NOT NULL`).Scan(&started)
			if started != 0 || len(f.rec.drain()) != 0 {
				t.Fatalf("mode %s ran the ladder", mode)
			}
		}
	})

	t.Run("RouteSpecialistDoneResolves", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0")
		f.until(db.RungRouteSpecialist)
		f.tick(time.Now()) // dispatch
		ref := f.actionRef()
		task, err := f.db.GetTask(ref, "p")
		if err != nil || task == nil || task.ProfileSlug != "cmo-lane" || task.DispatchedBy != "relay-sweeper" ||
			!strings.HasPrefix(task.Title, db.LadderTag(f.sys, 3)) {
			t.Fatalf("rung task: %+v %v", task, err)
		}
		if _, err := f.db.StartTask(ref, "cmo", "p"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.CompleteTask(ref, "cmo", "p", nil); err != nil {
			t.Fatal(err)
		}
		f.tick(time.Now())
		if r, status, by := f.state(); status != "resolved" || by != "peer" || r != "" {
			t.Fatalf("after done: rung %q status %s by %s", r, status, by)
		}
		var reason string
		_ = f.raw.QueryRow(`SELECT resolution_reason FROM exceptions WHERE id = ?`, f.sys).Scan(&reason)
		if reason != "fixed" {
			t.Fatalf("resolution_reason = %q", reason)
		}
	})

	t.Run("RouteSpecialistCancelledAdvances", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0", "p/boss/active///1")
		f.until(db.RungRouteSpecialist)
		f.tick(time.Now())
		if _, err := f.db.CancelTask(f.actionRef(), "cmo", "p", nil); err != nil {
			t.Fatal(err)
		}
		f.tick(time.Now())
		if r, _, _ := f.state(); r != db.RungSupervisorAgent {
			t.Fatalf("cancelled specialist: rung %q, want supervisor_agent", r)
		}
	})

	t.Run("ActionIdempotentAfterCrash", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0")
		f.until(db.RungRouteSpecialist)
		// A crash after the dispatch but before action_ref was stored.
		pre, err := f.db.DispatchTask("p", "cmo-lane", "relay-sweeper", db.LadderTag(f.sys, 3)+" pre-crash", "", "P2", nil, nil, db.TypedTicket{}, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.tick(time.Now())
		if ref := f.actionRef(); ref != pre.ID {
			t.Fatalf("rung re-dispatched: action_ref %s, want the existing %s", ref, pre.ID)
		}
		var n int
		_ = f.raw.QueryRow(`SELECT COUNT(*) FROM tasks WHERE title LIKE ?`, db.LadderTag(f.sys, 3)+"%").Scan(&n)
		if n != 1 {
			t.Fatalf("%d tasks carry the rung tag, want 1", n)
		}
	})

	t.Run("AskSourceFixedResolves", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0", "p/dev-0/active/dev//0")
		f.until(db.RungAskSource)
		f.tick(time.Now()) // ask
		ref := f.actionRef()
		if notices := f.rec.drain(); len(notices) != 1 || !strings.Contains(notices[0], "|dev-0|") {
			t.Fatalf("ask_source notice: %v", notices)
		}
		f.reply("dev-0", ref, `{"verdict":"fixed"}`)
		f.tick(time.Now())
		if _, status, by := f.state(); status != "resolved" || by != "source" {
			t.Fatalf("ask_source fixed: %s by %s", status, by)
		}
	})

	t.Run("UpstreamReviewerProfileNeverTargeted", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0", "p/boss/active///1")
		f.exec(`UPDATE exceptions SET evidence_json = '{"upstream":{"rounds":1,"reviewer_profiles":["cmo-lane"]}}' WHERE cause_id = ?`, f.sys)
		f.until(db.RungSupervisorAgent)
		var skipped string
		_ = f.raw.QueryRow(`SELECT json_extract(discharge_evidence, '$.skipped') FROM obligations WHERE subject_id = ? AND norm_id = 'exc.route_specialist'`, f.sys).Scan(&skipped)
		if skipped != "profile_already_reviewed_upstream" {
			t.Fatalf("route_specialist targeted a reviewer profile Niwa already used: skipped=%q", skipped)
		}
		var tasks int
		_ = f.raw.QueryRow(`SELECT COUNT(*) FROM tasks WHERE dispatched_by = 'relay-sweeper'`).Scan(&tasks)
		if tasks != 0 {
			t.Fatalf("%d rung tasks dispatched", tasks)
		}
	})

	t.Run("HardStopRoutesToSupervisor", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0", "p/boss/active///1")
		f.exec(`UPDATE exceptions SET evidence_json = '{"upstream":{"rounds":13}}' WHERE cause_id = ?`, f.sys)
		f.tick(time.Now()) // start
		f.tick(time.Now()) // hard stop
		if r, _, _ := f.state(); r != db.RungSupervisorAgent {
			t.Fatalf("hard stop: rung %q, want supervisor_agent", r)
		}
	})

	t.Run("OwnerNoneRoutesToLiveSupervisor", func(t *testing.T) {
		// Dead project: the dispatcher is not an agent, nobody is active in p,
		// an inactive executive exists in p; the only live executive is elsewhere.
		f := newLadderFixture(t, "linear", `[{"rung":"supervisor_agent"},{"rung":"human"}]`,
			"p/old-cto/inactive///1", "other/cto-x/active///1")
		var owner string
		_ = f.raw.QueryRow(`SELECT COALESCE(owner, '') FROM exceptions WHERE id = ?`, f.sys).Scan(&owner)
		if owner != "" {
			t.Fatalf("dead project resolved an owner: %q", owner)
		}
		f.tick(time.Now()) // start
		f.tick(time.Now()) // supervisor message
		notices := f.rec.drain()
		if len(notices) != 1 || !strings.Contains(notices[0], "|cto-x|") {
			t.Fatalf("supervisor rung went to %v, want the live cto-x", notices)
		}
		for _, n := range notices {
			if strings.Contains(n, "|user|") || strings.Contains(n, "|old-cto|") {
				t.Fatalf("dead-project systemic targeted %s", n)
			}
		}
	})

	t.Run("HumanSchemaValidated", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", `[{"rung":"consult_knowledge"},{"rung":"human"}]`, "p/cmo/active/cmo-lane//0")
		f.until(db.RungHuman)
		f.tick(time.Now()) // decide → user
		first := f.actionRef()
		if notices := f.rec.drain(); len(notices) != 1 || !strings.Contains(notices[0], "|user|") {
			t.Fatalf("human notice: %v", notices)
		}
		f.reply("user", first, `{"decision":"accept_known","scope":"project","generalize":false}`) // no expires_in
		f.tick(time.Now())
		second := f.actionRef()
		if second == first || len(f.rec.drain()) != 1 {
			t.Fatal("invalid answer did not get exactly one re-ask")
		}
		f.reply("user", second, `{"decision":"accept_known","scope":"project","generalize":true,"expires_in":"30d"}`)
		f.tick(time.Now())
		if _, status, by := f.state(); status != "resolved" || by != "human" {
			t.Fatalf("valid answer: %s by %s", status, by)
		}
		var until string
		_ = f.raw.QueryRow(`SELECT COALESCE(suppressed_until, '') FROM class_budgets WHERE kind = 'routing' AND reason_code = 'dead_lane'`).Scan(&until)
		if until == "" {
			t.Fatal("accept_known did not set suppressed_until")
		}
	})

	t.Run("HumanTTLExpires", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", `[{"rung":"consult_knowledge"},{"rung":"human"}]`, "p/cmo/active/cmo-lane//0")
		f.until(db.RungHuman)
		f.tick(time.Now())
		f.tick(time.Now().Add(80 * time.Hour))
		if _, status, by := f.state(); status != "resolved" || by != "expired" {
			t.Fatalf("TTL: %s by %s, want resolved by expired", status, by)
		}
	})
}
