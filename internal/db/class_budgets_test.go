package db

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// budgetDB is a test DB whose budget_epoch predates every seeded instance.
func budgetDB(t *testing.T) *DB {
	t.Helper()
	d := testDB(t)
	d.SetSetting(settingBudgetEpoch, "2000-01-01T00:00:00.000000Z")
	return d
}

// seedInstance writes one instance exception (the producer path of S1) with a
// source-declared class, opened ago before now.
func seedInstance(t *testing.T, d *DB, project, code, kind, retry, taskID string, ago time.Duration) string {
	t.Helper()
	tx, err := d.beginWriterTx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	ref := taskID
	if ref == "" {
		ref = uuid.New().String()
	}
	id, err := openExceptionTx(tx, exceptionOpen{
		Project: project, SourceKind: excSourceTaskBlock, SourceRef: ref, RaisedBy: "dev", TaskID: taskID,
		Text: "reason " + code, Code: code, Kind: kind, Retry: retry,
		At: time.Now().Add(-ago).UTC().Format(memoryTimeFmt),
	})
	if err != nil {
		t.Fatalf("open exception: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

func seedDeadLane(t *testing.T, d *DB, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, seedInstance(t, d, "p", "dead_lane", "routing", "non_retryable", "", time.Duration(n-i)*time.Hour))
	}
	return ids
}

func countSystemic(t *testing.T, d *DB, status string) int {
	t.Helper()
	var n int
	if err := d.ro().QueryRow(`SELECT COUNT(*) FROM exceptions WHERE source_kind = 'class_budget' AND status = ?`, status).Scan(&n); err != nil {
		t.Fatalf("count systemic: %v", err)
	}
	return n
}

func causeOf(t *testing.T, d *DB, id string) string {
	t.Helper()
	var c string
	if err := d.ro().QueryRow(`SELECT COALESCE(cause_id, '') FROM exceptions WHERE id = ?`, id).Scan(&c); err != nil {
		t.Fatalf("cause of %s: %v", id, err)
	}
	return c
}

func TestClassBudgets(t *testing.T) {
	t.Run("BreachOpensOneSystemic", func(t *testing.T) {
		d := budgetDB(t)
		ids := seedDeadLane(t, d, 2)
		if got, err := d.EvaluateClassBudgets(time.Now()); err != nil || len(got) != 0 {
			t.Fatalf("2 instances must not breach: %v %v", got, err)
		}
		ids = append(ids, seedDeadLane(t, d, 1)...)
		got, err := d.EvaluateClassBudgets(time.Now())
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if len(got) != 1 || got[0].Instances != 3 || got[0].ReasonCode != "dead_lane" {
			t.Fatalf("want one systemic over 3 instances, got %+v", got)
		}
		if n := countSystemic(t, d, "open"); n != 1 {
			t.Fatalf("open systemic = %d, want 1", n)
		}
		for _, id := range ids {
			if c := causeOf(t, d, id); c != got[0].ID {
				t.Fatalf("instance %s cause_id = %q, want %s", id, c, got[0].ID)
			}
		}
	})

	t.Run("FourthInstanceLinksNotReopens", func(t *testing.T) {
		d := budgetDB(t)
		seedDeadLane(t, d, 3)
		first, _ := d.EvaluateClassBudgets(time.Now())
		fourth := seedInstance(t, d, "p", "dead_lane", "routing", "non_retryable", "", time.Minute)
		got, err := d.EvaluateClassBudgets(time.Now())
		if err != nil || len(got) != 0 {
			t.Fatalf("4th instance reopened: %+v %v", got, err)
		}
		if n := countSystemic(t, d, "open"); n != 1 {
			t.Fatalf("open systemic = %d, want 1", n)
		}
		if c := causeOf(t, d, fourth); c != first[0].ID {
			t.Fatalf("4th instance not linked: %q", c)
		}
	})

	t.Run("ConcurrentTicksOneSystemic", func(t *testing.T) {
		d := budgetDB(t)
		ids := seedDeadLane(t, d, 3)
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := d.EvaluateClassBudgets(time.Now()); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent tick surfaced an error: %v", err)
		}
		if n := countSystemic(t, d, "open"); n != 1 {
			t.Fatalf("open systemic = %d, want 1", n)
		}
		for _, id := range ids {
			if causeOf(t, d, id) == "" {
				t.Fatalf("instance %s not linked", id)
			}
		}
	})

	t.Run("RegressionOfSet", func(t *testing.T) {
		d := budgetDB(t)
		seedDeadLane(t, d, 3)
		first, _ := d.EvaluateClassBudgets(time.Now())
		if _, err := d.writerExec(`UPDATE exceptions SET status = 'resolved', resolved_at = ? WHERE id = ?`,
			time.Now().UTC().Format(memoryTimeFmt), first[0].ID); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		for i := 0; i < 3; i++ {
			seedInstance(t, d, "p", "dead_lane", "routing", "non_retryable", "", time.Duration(3-i)*time.Second)
		}
		got, err := d.EvaluateClassBudgets(time.Now())
		if err != nil || len(got) != 1 {
			t.Fatalf("want a new systemic, got %+v %v", got, err)
		}
		if got[0].RegressionOf != first[0].ID {
			t.Fatalf("regression_of = %q, want %s", got[0].RegressionOf, first[0].ID)
		}
		var ev string
		_ = d.ro().QueryRow(`SELECT evidence_json FROM exceptions WHERE id = ?`, got[0].ID).Scan(&ev)
		var m map[string]any
		if json.Unmarshal([]byte(ev), &m) != nil || m["regression_of"] != first[0].ID {
			t.Fatalf("evidence regression_of missing: %s", ev)
		}
	})

	t.Run("BenignAndUnclassifiedNeverBreach", func(t *testing.T) {
		d := budgetDB(t)
		for i := 0; i < 10; i++ {
			seedInstance(t, d, "p", "superseded", "plan_change", "benign", "", time.Duration(i+1)*time.Minute)
			seedInstance(t, d, "p", excUnclassified, "routing", "unknown", "", time.Duration(i+1)*time.Minute)
		}
		if got, err := d.EvaluateClassBudgets(time.Now()); err != nil || len(got) != 0 {
			t.Fatalf("benign/unclassified breached: %+v %v", got, err)
		}
		if n := countSystemic(t, d, "open"); n != 0 {
			t.Fatalf("open systemic = %d, want 0", n)
		}
	})

	t.Run("BackfillBeforeEpochIgnored", func(t *testing.T) {
		d := testDB(t) // epoch = migrate time
		d.SetSetting(settingBudgetEpoch, time.Now().UTC().Format(memoryTimeFmt))
		seedDeadLane(t, d, 5) // all opened hours before the epoch
		if got, err := d.EvaluateClassBudgets(time.Now()); err != nil || len(got) != 0 {
			t.Fatalf("pre-epoch backfill breached: %+v %v", got, err)
		}
	})

	t.Run("OutsidePeriodIgnored", func(t *testing.T) {
		d := budgetDB(t)
		for i := 0; i < 3; i++ {
			seedInstance(t, d, "p", "dead_lane", "routing", "non_retryable", "", 8*24*time.Hour+time.Duration(i)*time.Hour)
		}
		if got, _ := d.EvaluateClassBudgets(time.Now()); len(got) != 0 {
			t.Fatalf("instances older than 7d breached: %+v", got)
		}
	})

	t.Run("Attribution", func(t *testing.T) {
		d := budgetDB(t)
		seedAgent(t, d.conn, "p", "cmo", "active", "", "", 0)
		seedAgent(t, d.conn, "p", "fs1", "active", "fullstack-lead", "lead-a", 0)
		seedAgent(t, d.conn, "p", "fs2", "active", "fullstack-lead", "lead-a", 0)
		seedAgent(t, d.conn, "p", "lead-a", "active", "", "", 0)
		seedAgent(t, d.conn, "p", "olddisp", "inactive", "", "boss", 0)
		seedAgent(t, d.conn, "p", "boss", "active", "", "", 0)
		seedAgent(t, d.conn, "p", "cto", "active", "", "", 0)
		if _, err := d.conn.Exec(`UPDATE agents SET is_executive = 1 WHERE name = 'cto'`); err != nil {
			t.Fatal(err)
		}
		in := func(assignee, profile, dispatcher string) budgetInstance {
			return budgetInstance{Assignee: assignee, Profile: profile, Dispatcher: dispatcher}
		}
		cases := []struct {
			name       string
			instances  []budgetInstance
			dim, owner string
			rule       string
		}{
			{"dispatcher100", []budgetInstance{in("a1", "p1", "cmo"), in("a2", "p2", "cmo"), in("a3", "p3", "cmo")},
				"dispatcher", "cmo", "direct"},
			{"linearSkippedLaneLead", []budgetInstance{in("a1", "fullstack-lead", "linear"), in("a2", "fullstack-lead", "linear"),
				in("a3", "fullstack-lead", "linear"), in("a4", "other", "linear")}, "profile", "lead-a", "lane_lead"},
			{"noDimensionProcessExecutive", []budgetInstance{in("a1", "p1", "cmo"), in("a2", "p2", "boss"), in("a3", "p3", "cto")},
				"process", "cto", "executive"},
			{"inactiveOwnerClimbs", []budgetInstance{in("a1", "p1", "olddisp"), in("a2", "p2", "olddisp"), in("a3", "p3", "olddisp")},
				"dispatcher", "boss", "reports_to"},
			{"founderNeverOwner", []budgetInstance{in("a1", "p1", "user"), in("a2", "p2", "user"), in("a3", "p3", "user")},
				"process", "cto", "executive"},
		}
		for _, c := range cases {
			a := d.attributeClass("p", c.instances)
			if a.Dimension != c.dim || a.Owner != c.owner || a.OwnerRule != c.rule {
				t.Errorf("%s: got %+v, want dim=%s owner=%s rule=%s", c.name, a, c.dim, c.owner, c.rule)
			}
			if a.Owner == "user" {
				t.Errorf("%s: founder attributed as owner", c.name)
			}
		}
		// No executive and only the founder: nobody, never the founder.
		solo := budgetDB(t)
		a := solo.attributeClass("p", []budgetInstance{in("a1", "p1", "user"), in("a2", "p2", "user"), in("a3", "p3", "user")})
		if a.Owner != "" || a.OwnerRule != "none" {
			t.Fatalf("no executive: got %+v, want no owner", a)
		}
	})

	t.Run("EndToEndAttributionFromTasks", func(t *testing.T) {
		d := budgetDB(t)
		seedAgent(t, d.conn, "p", "cmo", "active", "", "", 0)
		for i := 0; i < 3; i++ {
			id := fmt.Sprintf("task-%d", i)
			seedTask(t, d.conn, id, "p", "blocked", "cmo", fmt.Sprintf("dev-%d", i), "", fmt.Sprintf("prof-%d", i), "", "", false)
			seedInstance(t, d, "p", "dead_lane", "routing", "non_retryable", id, time.Duration(3-i)*time.Hour)
		}
		got, err := d.EvaluateClassBudgets(time.Now())
		if err != nil || len(got) != 1 {
			t.Fatalf("evaluate: %+v %v", got, err)
		}
		if got[0].Attribution.Owner != "cmo" || got[0].Attribution.Dimension != "dispatcher" {
			t.Fatalf("attribution = %+v, want dispatcher cmo", got[0].Attribution)
		}
		var owner string
		_ = d.ro().QueryRow(`SELECT COALESCE(owner, '') FROM exceptions WHERE id = ?`, got[0].ID).Scan(&owner)
		if owner != "cmo" {
			t.Fatalf("exceptions.owner = %q, want cmo", owner)
		}
	})

	t.Run("MigrateIdempotent", func(t *testing.T) {
		d := testDB(t)
		epoch := d.GetSetting(settingBudgetEpoch)
		if epoch == "" {
			t.Fatal("budget_epoch not set at migrate")
		}
		if _, err := d.writerExec(`UPDATE class_budgets SET intensity = 7 WHERE kind = 'routing' AND reason_code = '*'`); err != nil {
			t.Fatal(err)
		}
		migrateExceptions(d.conn)
		var intensity, norms, rows int
		_ = d.ro().QueryRow(`SELECT intensity FROM class_budgets WHERE kind = 'routing' AND reason_code = '*'`).Scan(&intensity)
		_ = d.ro().QueryRow(`SELECT COUNT(*) FROM norms WHERE id LIKE 'exc.%' AND enabled = 0`).Scan(&norms)
		_ = d.ro().QueryRow(`SELECT COUNT(*) FROM class_budgets`).Scan(&rows)
		if intensity != 7 {
			t.Fatalf("edited class_budgets row reset by re-migrate: %d", intensity)
		}
		if norms != len(excLadderRungs) || rows != 10 {
			t.Fatalf("seed drift: exc norms %d (want %d), class_budgets rows %d (want 10)", norms, len(excLadderRungs), rows)
		}
		if got := d.GetSetting(settingBudgetEpoch); got != epoch {
			t.Fatalf("budget_epoch moved on re-migrate: %s -> %s", epoch, got)
		}
		if _, err := d.ro().Exec(`SELECT exception_id, rung, strategy, outcome FROM exception_attempts LIMIT 1`); err != nil {
			t.Fatalf("exception_attempts view: %v", err)
		}
	})
}
