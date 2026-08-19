package relay

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseCronErrors(t *testing.T) {
	bad := []string{
		"",              // empty
		"* * * * *",     // 5 fields, not 6
		"0 0 0 0 * *",   // dom 0 out of range [1,31]
		"0 0 24 * * *",  // hour 24 out of range
		"0 0 * * 13 *",  // month 13 out of range
		"0 0 * * * 9",   // dow 9 out of range
		"0 0 * * * a",   // non-numeric
		"0 0 * * * */0", // zero step
		"0 0 5-3 * * *", // inverted range
	}
	for _, expr := range bad {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("expected error for %q", expr)
		}
	}
}

func TestCronMatches(t *testing.T) {
	mustSpec := func(expr string) cronSpec {
		s, err := parseCron(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		return s
	}
	// 2026-08-19 is a Wednesday (weekday 3).
	wed0930 := time.Date(2026, 8, 19, 9, 30, 0, 0, time.UTC)
	tue0930 := time.Date(2026, 8, 18, 9, 30, 0, 0, time.UTC)

	cases := []struct {
		expr string
		t    time.Time
		want bool
	}{
		{"0 30 9 * * *", wed0930, true},   // daily 09:30
		{"0 31 9 * * *", wed0930, false},  // wrong minute
		{"0 30 9 * * 3", wed0930, true},   // Wednesday match
		{"0 30 9 * * 1", wed0930, false},  // Monday only
		{"0 */30 * * * *", wed0930, true}, // every 30 min (min 30)
		{"0 0-30 9 * * *", wed0930, true}, // range covers 30
		{"0 30 9 19 8 *", wed0930, true},  // dom 19, month 8
		{"0 30 9 20 8 3", wed0930, true},  // dom OR dow: dom=20 no, dow=Wed yes → match
		{"0 30 9 20 8 1", wed0930, false}, // dom=20 no, dow=Mon no → no match
		{"0 30 9 19 8 *", tue0930, false}, // dom 19 but t is the 18th
	}
	for _, c := range cases {
		if got := mustSpec(c.expr).matches(c.t); got != c.want {
			t.Errorf("matches(%q, %s) = %v, want %v", c.expr, c.t.Format(time.RFC3339), got, c.want)
		}
	}
}

// TestCronTickFiresAndIsIdempotent drives a schedule that matches the tick time
// through runCronTick and asserts exactly one ticket is created, and a second
// tick in the same minute creates no duplicate.
func TestCronTickFiresAndIsIdempotent(t *testing.T) {
	r := testRelay(t)
	const project = "p1"

	schedules := []CronSchedule{{
		ID: "nightly-triage", Cron: "0 30 9 * * *", Project: project,
		Profile: "triage", Title: "overnight triage", Priority: "P2",
		Goal: "triage the backlog", AcceptanceCriteria: []string{"reviewed"}, Dod: "backlog triaged",
		Enabled: true,
	}}
	blob, _ := json.Marshal(schedules)
	r.DB.SetSetting(cronSettingKey, string(blob))

	fireAt := time.Date(2026, 8, 19, 9, 30, 15, 0, time.UTC) // sec 15 → truncated to 09:30:00
	r.runCronTick(fireAt)

	tasks, err := r.DB.ListTasks(project, "", "", "", "", "", 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 cron ticket, got %d", len(tasks))
	}
	if tasks[0].Title != "overnight triage" || tasks[0].DispatchedBy != cronDispatchedBy {
		t.Fatalf("unexpected ticket: title=%q by=%q", tasks[0].Title, tasks[0].DispatchedBy)
	}
	if tasks[0].Goal != "triage the backlog" {
		t.Fatalf("typed ticket goal missing: %q", tasks[0].Goal)
	}

	// Second tick in the SAME minute → no duplicate.
	r.runCronTick(fireAt.Add(20 * time.Second))
	tasks, _ = r.DB.ListTasks(project, "", "", "", "", "", 50, false)
	if len(tasks) != 1 {
		t.Fatalf("idempotency broken: %d tickets after second tick in same minute", len(tasks))
	}

	// A tick in a non-matching minute → still no new ticket.
	r.runCronTick(time.Date(2026, 8, 19, 9, 31, 0, 0, time.UTC))
	tasks, _ = r.DB.ListTasks(project, "", "", "", "", "", 50, false)
	if len(tasks) != 1 {
		t.Fatalf("non-matching minute must not fire, got %d tickets", len(tasks))
	}
}

// TestCronDisabledAndEmpty: a disabled schedule and an empty setting both fire
// nothing.
func TestCronDisabledAndEmpty(t *testing.T) {
	r := testRelay(t)
	const project = "p1"

	// Empty setting → no-op.
	r.runCronTick(time.Date(2026, 8, 19, 9, 30, 0, 0, time.UTC))

	disabled := []CronSchedule{{
		ID: "off", Cron: "0 30 9 * * *", Project: project, Profile: "x",
		Title: "disabled", Enabled: false,
	}}
	blob, _ := json.Marshal(disabled)
	r.DB.SetSetting(cronSettingKey, string(blob))
	r.runCronTick(time.Date(2026, 8, 19, 9, 30, 0, 0, time.UTC))

	tasks, _ := r.DB.ListTasks(project, "", "", "", "", "", 50, false)
	if len(tasks) != 0 {
		t.Fatalf("disabled/empty schedules must create nothing, got %d", len(tasks))
	}
}
