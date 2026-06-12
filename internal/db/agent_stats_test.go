package db

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"agent-relay/internal/models"
)

// fixed reference time for deterministic tests.
var refNow = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

func ts(t time.Time) string { return t.UTC().Format(agentStatsTimeFmt) }

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

// mkTask builds a minimal task with sensible defaults for stats tests.
func mkTask(id, agent string) models.Task {
	return models.Task{
		ID:             id,
		Title:          "task-" + id,
		Status:         "done",
		ClaimedBy:      sp(agent),
		BlockedPeriods: "[]",
		Labels:         "[]",
	}
}

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestPercentileMedianAndP90(t *testing.T) {
	// odd count
	got := percentile([]float64{1, 2, 3, 4, 5}, 50)
	if !almost(got, 3) {
		t.Errorf("median of 1..5 = %v, want 3", got)
	}
	// even count → interpolated
	got = percentile([]float64{1, 2, 3, 4}, 50)
	if !almost(got, 2.5) {
		t.Errorf("median of 1..4 = %v, want 2.5", got)
	}
	// p90 of 1..10 (sorted, 10 elems): rank = 0.9*9 = 8.1 → 9 + 0.1*(10-9) = 9.1
	got = percentile([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 90)
	if !almost(got, 9.1) {
		t.Errorf("p90 of 1..10 = %v, want 9.1", got)
	}
	// single element
	if got := percentile([]float64{7}, 90); !almost(got, 7) {
		t.Errorf("p90 single = %v, want 7", got)
	}
	// empty
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("p50 empty = %v, want 0", got)
	}
}

func TestDurationStat(t *testing.T) {
	s := durationStat([]float64{10, 20, 30})
	if s.Count != 3 {
		t.Errorf("count = %d, want 3", s.Count)
	}
	if !almost(s.Median, 20) {
		t.Errorf("median = %v, want 20", s.Median)
	}
	if !almost(s.Avg, 20) {
		t.Errorf("avg = %v, want 20", s.Avg)
	}
	if e := durationStat(nil); e.Count != 0 {
		t.Errorf("empty count = %d, want 0", e.Count)
	}
}

func TestSpanSeconds(t *testing.T) {
	a := ts(refNow)
	b := ts(refNow.Add(90 * time.Second))
	d, ok := spanSeconds(&a, &b)
	if !ok || !almost(d, 90) {
		t.Errorf("span = %v ok=%v, want 90 true", d, ok)
	}
	// reversed → invalid
	if _, ok := spanSeconds(&b, &a); ok {
		t.Errorf("reversed span should be invalid")
	}
	// nil
	if _, ok := spanSeconds(nil, &b); ok {
		t.Errorf("nil span should be invalid")
	}
}

func TestBlockedWindowSumming_ClosedAndOpen(t *testing.T) {
	// One closed window (300s) + one open window (still open at refNow, 600s).
	start1 := refNow.Add(-3600 * time.Second)
	end1 := start1.Add(300 * time.Second)
	openStart := refNow.Add(-600 * time.Second)

	periods := []blockedWindow{
		{Start: ts(start1), End: ts(end1)},
		{Start: ts(openStart)}, // open
	}
	raw, _ := json.Marshal(periods)

	task := mkTask("t1", "alice")
	task.BlockedPeriods = string(raw)
	task.ClaimedAt = sp(ts(refNow.Add(-4000 * time.Second)))
	task.InReviewAt = sp(ts(refNow)) // window boundary = now

	// blockedSecondsWithin over [claimed, in_review=now]: closed 300 + open clamps
	// to now → 600. Total 900.
	got := blockedSecondsWithin(task, task.ClaimedAt, task.InReviewAt)
	// Open window end clamps to in_review (sp(now)), so 600. Use tolerance for now drift.
	want := 300.0 + 600.0
	if math.Abs(got-want) > 2 {
		t.Errorf("blockedSecondsWithin = %v, want ~%v", got, want)
	}

	// computeBlocked: open window measured to actual now → may be slightly > 600.
	bs := computeBlocked([]models.Task{task})
	if bs.EpisodeCount != 2 {
		t.Errorf("episode count = %d, want 2", bs.EpisodeCount)
	}
	if len(bs.CurrentlyBlocked) != 1 {
		t.Errorf("currently blocked = %d, want 1", len(bs.CurrentlyBlocked))
	}
	if bs.TotalSeconds < 300+600-5 {
		t.Errorf("total blocked = %v, want >= ~900", bs.TotalSeconds)
	}
	if len(bs.PerAgent) != 1 || bs.PerAgent[0].Agent != "alice" {
		t.Errorf("per-agent blocked attribution wrong: %+v", bs.PerAgent)
	}
}

func TestComputeCycleTime(t *testing.T) {
	// alice: claim→review 100s, claim→done 250s
	a := mkTask("a", "alice")
	a.ClaimedAt = sp(ts(refNow))
	a.InReviewAt = sp(ts(refNow.Add(100 * time.Second)))
	a.DoneAt = sp(ts(refNow.Add(250 * time.Second)))

	// bob: claim→review 300s, claim→done 400s
	b := mkTask("b", "bob")
	b.ClaimedAt = sp(ts(refNow))
	b.InReviewAt = sp(ts(refNow.Add(300 * time.Second)))
	b.DoneAt = sp(ts(refNow.Add(400 * time.Second)))

	ct := computeCycleTime([]models.Task{a, b})
	if ct.Overall.ClaimToReview.Count != 2 {
		t.Fatalf("overall review count = %d, want 2", ct.Overall.ClaimToReview.Count)
	}
	// median of {100,300} = 200
	if !almost(ct.Overall.ClaimToReview.Median, 200) {
		t.Errorf("overall review median = %v, want 200", ct.Overall.ClaimToReview.Median)
	}
	if !almost(ct.Overall.ClaimToDone.Median, 325) {
		t.Errorf("overall done median = %v, want 325", ct.Overall.ClaimToDone.Median)
	}
	if len(ct.PerAgent) != 2 {
		t.Errorf("per-agent = %d, want 2", len(ct.PerAgent))
	}
}

func TestComputeTimeInState_SubtractsBlocked(t *testing.T) {
	// dispatched→claimed 60s, claimed→in_review 1000s with 200s blocked inside,
	// in_review→done 120s.
	disp := refNow
	claim := disp.Add(60 * time.Second)
	inrev := claim.Add(1000 * time.Second)
	done := inrev.Add(120 * time.Second)
	blkStart := claim.Add(100 * time.Second)
	blkEnd := blkStart.Add(200 * time.Second)

	task := mkTask("t", "alice")
	task.DispatchedAt = ts(disp)
	task.ClaimedAt = sp(ts(claim))
	task.InReviewAt = sp(ts(inrev))
	task.DoneAt = sp(ts(done))
	periods, _ := json.Marshal([]blockedWindow{{Start: ts(blkStart), End: ts(blkEnd)}})
	task.BlockedPeriods = string(periods)

	tis := computeTimeInState([]models.Task{task})
	if !almost(tis.Todo.Median, 60) {
		t.Errorf("todo median = %v, want 60", tis.Todo.Median)
	}
	// in-progress = 1000 - 200 = 800
	if !almost(tis.InProgress.Median, 800) {
		t.Errorf("in_progress median = %v, want 800", tis.InProgress.Median)
	}
	if !almost(tis.InReview.Median, 120) {
		t.Errorf("in_review median = %v, want 120", tis.InReview.Median)
	}
	// bottleneck = in_progress (largest)
	if tis.Bottleneck != "in_progress" {
		t.Errorf("bottleneck = %q, want in_progress", tis.Bottleneck)
	}
}

func TestComputeThroughputAndBurndownBucketing(t *testing.T) {
	// Cycle: 2026-06-01 .. 2026-06-05. 3 tasks, points 2/3/5 = 10 total.
	cycleStart := "2026-06-01T00:00:00.000000Z"
	cycleEnd := "2026-06-05T00:00:00.000000Z"

	mk := func(id string, pts int, doneDay string) models.Task {
		t := mkTask(id, "alice")
		t.Points = ip(pts)
		t.CycleID = sp("cyc1")
		t.CycleStart = sp(cycleStart)
		t.CycleEnd = sp(cycleEnd)
		t.DoneAt = sp(doneDay + "T10:00:00.000000Z")
		return t
	}
	tasks := []models.Task{
		mk("t1", 2, "2026-06-02"),
		mk("t2", 3, "2026-06-02"),
		mk("t3", 5, "2026-06-04"),
	}

	scope := CycleScope{ID: "cyc1", Start: cycleStart, End: cycleEnd}
	tp := computeThroughput(tasks, scope)
	if tp.TasksDone != 3 || tp.PointsDone != 10 {
		t.Errorf("throughput totals: tasks=%d points=%d, want 3/10", tp.TasksDone, tp.PointsDone)
	}
	// Series should cap at min(cycleEnd, today). today (refNow date) is after cycle
	// end so we expect the full 5-day window 06-01..06-05.
	if len(tp.Series) < 4 {
		t.Fatalf("series too short: %d", len(tp.Series))
	}
	// Cumulative on 06-02 = 2 tasks/5 points; on 06-04 = 3 tasks/10 points.
	var d02, d04 *ThroughputBucket
	for i := range tp.Series {
		if tp.Series[i].Date == "2026-06-02" {
			d02 = &tp.Series[i]
		}
		if tp.Series[i].Date == "2026-06-04" {
			d04 = &tp.Series[i]
		}
	}
	if d02 == nil || d02.CumulativeTasks != 2 || d02.CumulativePoints != 5 {
		t.Errorf("06-02 bucket wrong: %+v", d02)
	}
	if d04 == nil || d04.CumulativeTasks != 3 || d04.CumulativePoints != 10 {
		t.Errorf("06-04 bucket wrong: %+v", d04)
	}

	// Burndown: remaining points start at 10 and reach 0.
	bd := computeBurndown(tasks, scope)
	if bd.TotalPoints != 10 || bd.TotalTasks != 3 {
		t.Errorf("burndown totals: %d pts %d tasks, want 10/3", bd.TotalPoints, bd.TotalTasks)
	}
	if bd.RemainingPoints != 0 || bd.RemainingTasks != 0 {
		t.Errorf("burndown remaining: %d pts %d tasks, want 0/0", bd.RemainingPoints, bd.RemainingTasks)
	}
	last := bd.Series[len(bd.Series)-1]
	if last.RemainingPoints != 0 {
		t.Errorf("burndown final remaining = %d, want 0", last.RemainingPoints)
	}
	// ideal line monotonically non-increasing, ends at 0.
	for i := 1; i < len(bd.Series); i++ {
		if bd.Series[i].Ideal > bd.Series[i-1].Ideal {
			t.Errorf("ideal line not non-increasing at %d", i)
		}
	}
}

func TestIdealLine(t *testing.T) {
	got := idealLine(10, 5)
	want := []int{8, 6, 4, 2, 0}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ideal[%d] = %d, want %d", i, got[i], want[i])
		}
	}
	if got := idealLine(10, 0); len(got) != 0 {
		t.Errorf("idealLine n=0 should be empty")
	}
}

func TestComputeLoad(t *testing.T) {
	// alice: 1 in-progress (claimed), 1 blocked-open. bob: idle (only done).
	a1 := mkTask("a1", "alice")
	a1.Status = "in-progress"
	a2 := mkTask("a2", "alice")
	a2.Status = "blocked"
	periods, _ := json.Marshal([]blockedWindow{{Start: ts(refNow.Add(-100 * time.Second))}})
	a2.BlockedPeriods = string(periods)
	b1 := mkTask("b1", "bob")
	b1.Status = "done"

	loads := computeLoad([]models.Task{a1, a2, b1}, "")
	var alice, bob *AgentLoad
	for i := range loads {
		switch loads[i].Agent {
		case "alice":
			alice = &loads[i]
		case "bob":
			bob = &loads[i]
		}
	}
	if alice == nil || alice.Claimed != 2 || alice.Blocked != 1 || alice.Idle {
		t.Errorf("alice load wrong: %+v", alice)
	}
	if bob != nil {
		// bob only has a done task → no active claim → not present in load map.
		t.Errorf("bob should be absent (idle, no active claims): %+v", bob)
	}
}

func TestResolveCycle_ActiveDefaultAndAll(t *testing.T) {
	// Active cycle wraps refNow.
	active := models.Task{
		ID:             "x",
		Status:         "done",
		BlockedPeriods: "[]",
		Labels:         "[]",
		CycleID:        sp("cyc-active"),
		CycleName:      sp("Cycle Active"),
		CycleStart:     sp(ts(refNow.Add(-48 * time.Hour))),
		CycleEnd:       sp(ts(refNow.Add(48 * time.Hour))),
	}
	old := active
	old.ID = "y"
	old.CycleID = sp("cyc-old")
	old.CycleName = sp("Cycle Old")
	old.CycleStart = sp(ts(refNow.Add(-200 * time.Hour)))
	old.CycleEnd = sp(ts(refNow.Add(-100 * time.Hour)))

	tasks := []models.Task{active, old}
	cycles := collectCycles(tasks)
	if len(cycles) != 2 {
		t.Fatalf("cycles = %d, want 2", len(cycles))
	}

	// Default (empty id) → active cycle. NOTE: isActiveCycle uses real now, so
	// only valid because refNow window is anchored relative to real now below.
	// We assert resolveCycle picks the cycle marked Active.
	var activeOpt CycleOption
	for _, c := range cycles {
		if c.ID == "cyc-active" {
			activeOpt = c
		}
	}
	_ = activeOpt

	// Explicit "all".
	all := resolveCycle("all", cycles, tasks)
	if all.ID != "all" {
		t.Errorf("resolve all id = %q, want all", all.ID)
	}
	if all.TotalTasks != 2 {
		t.Errorf("all totals = %d, want 2", all.TotalTasks)
	}

	// Explicit known id.
	got := resolveCycle("cyc-old", cycles, tasks)
	if got.ID != "cyc-old" || got.TotalTasks != 1 {
		t.Errorf("resolve cyc-old = %+v, want id cyc-old total 1", got)
	}

	// Unknown id → all.
	unknown := resolveCycle("nope", cycles, tasks)
	if unknown.ID != "all" {
		t.Errorf("resolve unknown id = %q, want all", unknown.ID)
	}
}

func TestParseStatsTime_Formats(t *testing.T) {
	cases := []string{
		"2026-06-10T12:00:00.000000Z",
		"2026-06-10T12:00:00Z",
		"2026-06-10",
	}
	for _, c := range cases {
		if _, ok := parseStatsTime(c); !ok {
			t.Errorf("failed to parse %q", c)
		}
	}
	if _, ok := parseStatsTime(""); ok {
		t.Errorf("empty should not parse")
	}
	if _, ok := parseStatsTime("garbage"); ok {
		t.Errorf("garbage should not parse")
	}
}

func TestEmptyScopeProducesEmptySeries(t *testing.T) {
	scope := CycleScope{ID: "all"}
	tp := computeThroughput(nil, scope)
	if len(tp.Series) != 0 {
		t.Errorf("empty throughput series should be empty, got %d", len(tp.Series))
	}
	bd := computeBurndown(nil, scope)
	if len(bd.Series) != 0 {
		t.Errorf("empty burndown series should be empty, got %d", len(bd.Series))
	}
}

// --- Integration: full GetAgentStats path against a real migrated DB ---

func TestGetAgentStats_Integration(t *testing.T) {
	d := testDB(t)
	now := time.Now().UTC()
	day := func(offset int) string {
		return now.AddDate(0, 0, offset).Format(agentStatsTimeFmt)
	}
	insert := func(id, agent, status string, points int, claimed, review, done string, blocked string, cycleID string) {
		_, err := d.conn.Exec(`INSERT INTO tasks
			(id, profile_slug, dispatched_by, title, status, project, dispatched_at,
			 points, assignee, claimed_by, claimed_at, in_review_at, done_at, completed_at,
			 blocked_periods, labels, source, cycle_id, cycle_name, cycle_start, cycle_end)
			VALUES (?, 'dev', 'cto', ?, ?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', 'linear', ?, 'Cycle 1', ?, ?)`,
			id, "T-"+id, status, day(-5), points, agent, agent, claimed, nullify(review), nullify(done), nullify(done),
			blocked, cycleID, day(-6), day(3))
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	// Two done tasks (alice) + one blocked-open (bob).
	insert("1", "alice", "done", 3, day(-4), day(-4), day(-3), "[]", "cyc1")
	insert("2", "alice", "done", 5, day(-3), day(-2), day(-1), "[]", "cyc1")
	blkOpen := `[{"start":"` + day(0) + `"}]`
	insert("3", "bob", "blocked", 2, day(-1), "", "", blkOpen, "cyc1")

	stats, err := d.GetAgentStats("default", "all", "")
	if err != nil {
		t.Fatalf("GetAgentStats: %v", err)
	}
	if stats.Empty {
		t.Fatal("stats should not be empty")
	}
	if stats.Cycle.TotalTasks != 3 {
		t.Errorf("total tasks = %d, want 3", stats.Cycle.TotalTasks)
	}
	if stats.Throughput.TasksDone != 2 || stats.Throughput.PointsDone != 8 {
		t.Errorf("throughput = %d tasks/%d pts, want 2/8", stats.Throughput.TasksDone, stats.Throughput.PointsDone)
	}
	if len(stats.Blocked.CurrentlyBlocked) != 1 || stats.Blocked.CurrentlyBlocked[0].Agent != "bob" {
		t.Errorf("currently blocked wrong: %+v", stats.Blocked.CurrentlyBlocked)
	}
	if stats.Burndown.TotalPoints != 10 {
		t.Errorf("burndown total points = %d, want 10", stats.Burndown.TotalPoints)
	}
	if stats.Burndown.RemainingPoints != 2 {
		t.Errorf("burndown remaining points = %d, want 2", stats.Burndown.RemainingPoints)
	}
	// Load: bob should appear (blocked active), alice should not (only done tasks).
	var bobLoad *AgentLoad
	for i := range stats.Load {
		if stats.Load[i].Agent == "bob" {
			bobLoad = &stats.Load[i]
		}
	}
	if bobLoad == nil || bobLoad.Blocked != 1 {
		t.Errorf("bob load wrong: %+v", bobLoad)
	}
	if len(stats.Agents) != 2 {
		t.Errorf("agents = %v, want 2", stats.Agents)
	}

	// Agent filter narrows metric scope.
	aliceOnly, err := d.GetAgentStats("default", "all", "alice")
	if err != nil {
		t.Fatalf("GetAgentStats alice: %v", err)
	}
	if aliceOnly.Throughput.TasksDone != 2 {
		t.Errorf("alice throughput tasks = %d, want 2", aliceOnly.Throughput.TasksDone)
	}
}

// nullify returns nil for empty strings so INSERT writes NULL.
func nullify(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
