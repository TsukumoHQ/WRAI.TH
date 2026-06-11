package db

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"agent-relay/internal/models"
)

// agentStatsTimeFmt matches the auto-stamped overlay timestamps (memoryTimeFmt).
// We also tolerate RFC3339 just in case mirror fields use it.
const agentStatsTimeFmt = "2006-01-02T15:04:05.000000Z"

// --- Public response shape (all durations in SECONDS; UI formats) ---

// AgentStats is the full aggregation payload for GET /api/stats.
type AgentStats struct {
	Cycle       CycleScope       `json:"cycle"`
	Cycles      []CycleOption    `json:"cycles"`     // selectable cycles (most recent first)
	Agents      []string         `json:"agents"`     // distinct agent names in scope
	CycleTime   CycleTimeStats   `json:"cycle_time"` // claim→in_review / claim→done
	TimeInState TimeInStateStats `json:"time_in_state"`
	Blocked     BlockedStats     `json:"blocked"`
	Throughput  ThroughputStats  `json:"throughput"`
	Load        []AgentLoad      `json:"load"` // current load per agent
	Burndown    BurndownStats    `json:"burndown"`
	GeneratedAt string           `json:"generated_at"` // RFC3339 when computed
	Empty       bool             `json:"empty"`        // true when no tasks in scope
}

// CycleScope describes the cycle being viewed.
type CycleScope struct {
	ID          string `json:"id"`   // cycle id, or "all"
	Name        string `json:"name"` // human label
	Start       string `json:"start,omitempty"`
	End         string `json:"end,omitempty"`
	Active      bool   `json:"active"`      // true if today is within [start,end]
	TotalTasks  int    `json:"total_tasks"` // scope size
	TotalPoints int    `json:"total_points"`
}

// CycleOption is a selectable cycle for the dimension picker.
type CycleOption struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Start  string `json:"start,omitempty"`
	End    string `json:"end,omitempty"`
	Active bool   `json:"active"`
}

// DurationStat holds median + p90 for a duration (seconds), plus the sample count.
type DurationStat struct {
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	Avg    float64 `json:"avg"`
	Count  int     `json:"count"`
}

// CycleTimeStats: claim→in_review and claim→done, overall + per agent.
type CycleTimeStats struct {
	Overall  AgentCycleTime   `json:"overall"`
	PerAgent []AgentCycleTime `json:"per_agent"`
}

// AgentCycleTime holds the two cycle-time measures for one agent (or overall).
type AgentCycleTime struct {
	Agent         string       `json:"agent"` // "" for overall
	ClaimToReview DurationStat `json:"claim_to_review"`
	ClaimToDone   DurationStat `json:"claim_to_done"`
}

// TimeInStateStats: average/median seconds spent in each lifecycle state.
type TimeInStateStats struct {
	Todo       DurationStat `json:"todo"`        // dispatched→claimed
	InProgress DurationStat `json:"in_progress"` // claimed→in_review (minus blocked)
	InReview   DurationStat `json:"in_review"`   // in_review→done
	Bottleneck string       `json:"bottleneck"`  // state with highest median ("" if none)
}

// BlockedStats: total + per-agent blocked time and current blockers.
type BlockedStats struct {
	TotalSeconds     float64        `json:"total_seconds"`
	EpisodeCount     int            `json:"episode_count"`
	PerAgent         []AgentBlocked `json:"per_agent"`
	CurrentlyBlocked []BlockedTask  `json:"currently_blocked"`
}

// AgentBlocked is blocked time attributed to one agent.
type AgentBlocked struct {
	Agent        string  `json:"agent"`
	TotalSeconds float64 `json:"total_seconds"`
	AvgSeconds   float64 `json:"avg_seconds"`
	EpisodeCount int     `json:"episode_count"`
}

// BlockedTask is a task currently in an open blocked window.
type BlockedTask struct {
	TaskID       string  `json:"task_id"`
	Title        string  `json:"title"`
	Agent        string  `json:"agent"`
	LinearKey    *string `json:"linear_key,omitempty"`
	SinceSeconds float64 `json:"since_seconds"` // how long it's been blocked
}

// ThroughputStats: tasks + points done, per cycle (here single scope) and per agent,
// plus a cumulative daily series for the line chart.
type ThroughputStats struct {
	TasksDone  int                `json:"tasks_done"`
	PointsDone int                `json:"points_done"`
	PerAgent   []AgentThroughput  `json:"per_agent"`
	Series     []ThroughputBucket `json:"series"` // cumulative, per day
}

// AgentThroughput is completed work attributed to one agent.
type AgentThroughput struct {
	Agent      string `json:"agent"`
	TasksDone  int    `json:"tasks_done"`
	PointsDone int    `json:"points_done"`
}

// ThroughputBucket is one day of cumulative completion.
type ThroughputBucket struct {
	Date             string `json:"date"` // YYYY-MM-DD
	CumulativeTasks  int    `json:"cumulative_tasks"`
	CumulativePoints int    `json:"cumulative_points"`
}

// AgentLoad is the live load for one agent (from the overlay snapshot).
type AgentLoad struct {
	Agent   string `json:"agent"`
	Claimed int    `json:"claimed"` // accepted/in-progress/in-review, not done
	Blocked int    `json:"blocked"` // currently in an open blocked window
	Idle    bool   `json:"idle"`    // no active claims
}

// BurndownStats: cycle scope vs remaining over time (per day).
type BurndownStats struct {
	TotalPoints     int              `json:"total_points"`
	TotalTasks      int              `json:"total_tasks"`
	RemainingPoints int              `json:"remaining_points"`
	RemainingTasks  int              `json:"remaining_tasks"`
	Series          []BurndownBucket `json:"series"`
}

// BurndownBucket is remaining points/tasks at end of one day.
type BurndownBucket struct {
	Date            string `json:"date"`
	RemainingPoints int    `json:"remaining_points"`
	RemainingTasks  int    `json:"remaining_tasks"`
	Ideal           int    `json:"ideal"` // ideal-line remaining points for the day
}

// blockedWindow is the parsed {start,end} window from blocked_periods JSON.
type blockedWindow struct {
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
}

// --- Aggregation entrypoint ---

// GetAgentStats computes the full agentic analytics payload for a project,
// scoped to a cycle (id, or "all") and optionally filtered to a single agent.
// Reads tasks only — zero Linear round-trips.
func (d *DB) GetAgentStats(project, cycleID, agentFilter string) (*AgentStats, error) {
	tasks, err := d.statsTasks(project)
	if err != nil {
		return nil, err
	}
	cycles := collectCycles(tasks)
	scope := resolveCycle(cycleID, cycles, tasks)

	// Filter tasks to the chosen cycle scope.
	scoped := filterByCycle(tasks, scope.ID)

	// Build the agent dimension list BEFORE applying the agent filter, so the
	// UI can still offer the full agent dropdown for the cycle.
	agents := collectAgents(scoped)

	// Apply agent filter to the metric computation (load is always all-agents).
	metricTasks := scoped
	if agentFilter != "" {
		metricTasks = filterByAgent(scoped, agentFilter)
	}

	out := &AgentStats{
		Cycle:       scope,
		Cycles:      cycles,
		Agents:      agents,
		CycleTime:   computeCycleTime(metricTasks),
		TimeInState: computeTimeInState(metricTasks),
		Blocked:     computeBlocked(metricTasks),
		Throughput:  computeThroughput(metricTasks, scope),
		Load:        computeLoad(scoped, agentFilter),
		Burndown:    computeBurndown(scoped, scope),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Empty:       len(scoped) == 0,
	}
	return out, nil
}

// statsTasks loads the columns needed for stats from non-archived tasks.
func (d *DB) statsTasks(project string) ([]models.Task, error) {
	rows, err := d.ro().Query(
		"SELECT "+taskColumns+" FROM tasks WHERE project = ? AND archived_at IS NULL",
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("stats tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan stats task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- Cycle resolution ---

func collectCycles(tasks []models.Task) []CycleOption {
	seen := map[string]CycleOption{}
	for _, t := range tasks {
		if t.CycleID == nil || *t.CycleID == "" {
			continue
		}
		id := *t.CycleID
		if _, ok := seen[id]; ok {
			continue
		}
		opt := CycleOption{ID: id}
		if t.CycleName != nil {
			opt.Name = *t.CycleName
		} else {
			opt.Name = id
		}
		if t.CycleStart != nil {
			opt.Start = *t.CycleStart
		}
		if t.CycleEnd != nil {
			opt.End = *t.CycleEnd
		}
		opt.Active = isActiveCycle(opt.Start, opt.End, time.Now().UTC())
		seen[id] = opt
	}
	out := make([]CycleOption, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	// Most recent first (by start desc, falling back to name).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start > out[j].Start
		}
		return out[i].Name > out[j].Name
	})
	return out
}

// resolveCycle picks the scope: explicit id if valid, else the active cycle,
// else "all" (native mode / no cycles).
func resolveCycle(cycleID string, cycles []CycleOption, tasks []models.Task) CycleScope {
	allScope := func() CycleScope {
		s := CycleScope{ID: "all", Name: "All time", Active: false}
		s.TotalTasks, s.TotalPoints = scopeTotals(tasks)
		return s
	}

	if cycleID == "all" {
		return allScope()
	}

	// Explicit cycle id.
	if cycleID != "" {
		for _, c := range cycles {
			if c.ID == cycleID {
				return cycleScopeFrom(c, tasks)
			}
		}
		// Unknown id → fall back to all.
		return allScope()
	}

	// Default: the active cycle (today within [start,end]).
	for _, c := range cycles {
		if c.Active {
			return cycleScopeFrom(c, tasks)
		}
	}

	// No active cycle: most recent cycle if any, else all-time.
	if len(cycles) > 0 {
		return cycleScopeFrom(cycles[0], tasks)
	}
	return allScope()
}

func cycleScopeFrom(c CycleOption, tasks []models.Task) CycleScope {
	s := CycleScope{ID: c.ID, Name: c.Name, Start: c.Start, End: c.End, Active: c.Active}
	scoped := filterByCycle(tasks, c.ID)
	s.TotalTasks, s.TotalPoints = scopeTotals(scoped)
	return s
}

func scopeTotals(tasks []models.Task) (int, int) {
	pts := 0
	for _, t := range tasks {
		if t.Points != nil {
			pts += *t.Points
		}
	}
	return len(tasks), pts
}

func isActiveCycle(start, end string, now time.Time) bool {
	if start == "" || end == "" {
		return false
	}
	s, okS := parseStatsTime(start)
	e, okE := parseStatsTime(end)
	if !okS || !okE {
		return false
	}
	// Inclusive on both ends; end is bumped to end-of-day if it's a bare date.
	return !now.Before(s) && !now.After(e)
}

func filterByCycle(tasks []models.Task, cycleID string) []models.Task {
	if cycleID == "all" || cycleID == "" {
		return tasks
	}
	out := make([]models.Task, 0, len(tasks))
	for _, t := range tasks {
		if t.CycleID != nil && *t.CycleID == cycleID {
			out = append(out, t)
		}
	}
	return out
}

func filterByAgent(tasks []models.Task, agent string) []models.Task {
	out := make([]models.Task, 0, len(tasks))
	for _, t := range tasks {
		if taskAgent(t) == agent {
			out = append(out, t)
		}
	}
	return out
}

func collectAgents(tasks []models.Task) []string {
	seen := map[string]struct{}{}
	for _, t := range tasks {
		a := taskAgent(t)
		if a != "" {
			seen[a] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// taskAgent returns the agent attributed to a task: claimed_by, else assignee,
// else assigned_to.
func taskAgent(t models.Task) string {
	if t.ClaimedBy != nil && *t.ClaimedBy != "" {
		return *t.ClaimedBy
	}
	if t.Assignee != nil && *t.Assignee != "" {
		return *t.Assignee
	}
	if t.AssignedTo != nil && *t.AssignedTo != "" {
		return *t.AssignedTo
	}
	return ""
}

// --- Metric: cycle time ---

func computeCycleTime(tasks []models.Task) CycleTimeStats {
	var overallReview, overallDone []float64
	perReview := map[string][]float64{}
	perDone := map[string][]float64{}

	for _, t := range tasks {
		agent := taskAgent(t)
		if d, ok := claimToReview(t); ok {
			overallReview = append(overallReview, d)
			if agent != "" {
				perReview[agent] = append(perReview[agent], d)
			}
		}
		if d, ok := claimToDone(t); ok {
			overallDone = append(overallDone, d)
			if agent != "" {
				perDone[agent] = append(perDone[agent], d)
			}
		}
	}

	stats := CycleTimeStats{
		Overall: AgentCycleTime{
			Agent:         "",
			ClaimToReview: durationStat(overallReview),
			ClaimToDone:   durationStat(overallDone),
		},
	}

	agents := map[string]struct{}{}
	for a := range perReview {
		agents[a] = struct{}{}
	}
	for a := range perDone {
		agents[a] = struct{}{}
	}
	names := make([]string, 0, len(agents))
	for a := range agents {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		stats.PerAgent = append(stats.PerAgent, AgentCycleTime{
			Agent:         a,
			ClaimToReview: durationStat(perReview[a]),
			ClaimToDone:   durationStat(perDone[a]),
		})
	}
	return stats
}

func claimToReview(t models.Task) (float64, bool) {
	return spanSeconds(t.ClaimedAt, t.InReviewAt)
}

func claimToDone(t models.Task) (float64, bool) {
	return spanSeconds(t.ClaimedAt, doneTimestamp(t))
}

// doneTimestamp returns done_at, falling back to completed_at for done tasks.
func doneTimestamp(t models.Task) *string {
	if t.DoneAt != nil && *t.DoneAt != "" {
		return t.DoneAt
	}
	if t.Status == "done" && t.CompletedAt != nil && *t.CompletedAt != "" {
		return t.CompletedAt
	}
	return nil
}

// --- Metric: time in state ---

func computeTimeInState(tasks []models.Task) TimeInStateStats {
	var todo, inProgress, inReview []float64

	for _, t := range tasks {
		// Todo: dispatched → claimed
		if d, ok := spanSeconds(ptr(t.DispatchedAt), t.ClaimedAt); ok {
			todo = append(todo, d)
		}
		// In Progress: claimed → in_review, minus blocked time inside that window.
		if d, ok := spanSeconds(t.ClaimedAt, t.InReviewAt); ok {
			d -= blockedSecondsWithin(t, t.ClaimedAt, t.InReviewAt)
			if d < 0 {
				d = 0
			}
			inProgress = append(inProgress, d)
		}
		// In Review: in_review → done
		if d, ok := spanSeconds(t.InReviewAt, doneTimestamp(t)); ok {
			inReview = append(inReview, d)
		}
	}

	s := TimeInStateStats{
		Todo:       durationStat(todo),
		InProgress: durationStat(inProgress),
		InReview:   durationStat(inReview),
	}
	s.Bottleneck = bottleneck(s)
	return s
}

func bottleneck(s TimeInStateStats) string {
	type cand struct {
		name string
		med  float64
		n    int
	}
	cands := []cand{
		{"todo", s.Todo.Median, s.Todo.Count},
		{"in_progress", s.InProgress.Median, s.InProgress.Count},
		{"in_review", s.InReview.Median, s.InReview.Count},
	}
	best := ""
	var bestMed float64 = -1
	for _, c := range cands {
		if c.n == 0 {
			continue
		}
		if c.med > bestMed {
			bestMed = c.med
			best = c.name
		}
	}
	return best
}

// --- Metric: blocked time ---

func computeBlocked(tasks []models.Task) BlockedStats {
	now := time.Now().UTC()
	out := BlockedStats{}
	perAgent := map[string]*AgentBlocked{}

	for _, t := range tasks {
		windows := parseBlockedWindows(t.BlockedPeriods)
		agent := taskAgent(t)
		for _, w := range windows {
			secs, openWindow := windowSeconds(w, now)
			if secs <= 0 {
				continue
			}
			out.TotalSeconds += secs
			out.EpisodeCount++
			if agent != "" {
				ab := perAgent[agent]
				if ab == nil {
					ab = &AgentBlocked{Agent: agent}
					perAgent[agent] = ab
				}
				ab.TotalSeconds += secs
				ab.EpisodeCount++
			}
			if openWindow {
				bt := BlockedTask{
					TaskID:       t.ID,
					Title:        t.Title,
					Agent:        agent,
					LinearKey:    t.LinearKey,
					SinceSeconds: secs,
				}
				out.CurrentlyBlocked = append(out.CurrentlyBlocked, bt)
			}
		}
	}

	names := make([]string, 0, len(perAgent))
	for a := range perAgent {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		ab := perAgent[a]
		if ab.EpisodeCount > 0 {
			ab.AvgSeconds = ab.TotalSeconds / float64(ab.EpisodeCount)
		}
		out.PerAgent = append(out.PerAgent, *ab)
	}
	sort.Slice(out.CurrentlyBlocked, func(i, j int) bool {
		return out.CurrentlyBlocked[i].SinceSeconds > out.CurrentlyBlocked[j].SinceSeconds
	})
	return out
}

// blockedSecondsWithin sums blocked time that overlaps [from,to]. Open windows
// are clamped to `to` (or now if to is nil).
func blockedSecondsWithin(t models.Task, from, to *string) float64 {
	fromT, okF := parseStatsTimePtr(from)
	if !okF {
		return 0
	}
	var toT time.Time
	if to != nil {
		if v, ok := parseStatsTime(*to); ok {
			toT = v
		} else {
			toT = time.Now().UTC()
		}
	} else {
		toT = time.Now().UTC()
	}

	total := 0.0
	for _, w := range parseBlockedWindows(t.BlockedPeriods) {
		ws, okWS := parseStatsTime(w.Start)
		if !okWS {
			continue
		}
		var we time.Time
		if w.End == "" {
			we = toT // open window clamps to the state boundary
		} else if v, ok := parseStatsTime(w.End); ok {
			we = v
		} else {
			continue
		}
		// Clip to [from,to].
		s := maxTime(ws, fromT)
		e := minTime(we, toT)
		if e.After(s) {
			total += e.Sub(s).Seconds()
		}
	}
	return total
}

// --- Metric: throughput ---

func computeThroughput(tasks []models.Task, scope CycleScope) ThroughputStats {
	out := ThroughputStats{}
	perAgent := map[string]*AgentThroughput{}

	// Collect (date, points) for each done task.
	var events []struct {
		date   string
		points int
	}

	for _, t := range tasks {
		dt := doneTimestamp(t)
		if dt == nil {
			continue
		}
		ts, ok := parseStatsTime(*dt)
		if !ok {
			continue
		}
		pts := 0
		if t.Points != nil {
			pts = *t.Points
		}
		out.TasksDone++
		out.PointsDone += pts
		events = append(events, struct {
			date   string
			points int
		}{date: ts.UTC().Format("2006-01-02"), points: pts})

		agent := taskAgent(t)
		if agent != "" {
			at := perAgent[agent]
			if at == nil {
				at = &AgentThroughput{Agent: agent}
				perAgent[agent] = at
			}
			at.TasksDone++
			at.PointsDone += pts
		}
	}

	names := make([]string, 0, len(perAgent))
	for a := range perAgent {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		out.PerAgent = append(out.PerAgent, *perAgent[a])
	}

	out.Series = cumulativeSeries(events, scope)
	return out
}

// cumulativeSeries buckets done events per day and produces a cumulative series
// spanning the cycle window (or min→max done date in all-time mode).
func cumulativeSeries(events []struct {
	date   string
	points int
}, scope CycleScope) []ThroughputBucket {
	if len(events) == 0 {
		return []ThroughputBucket{}
	}
	// Per-day totals.
	tasksByDay := map[string]int{}
	pointsByDay := map[string]int{}
	for _, e := range events {
		tasksByDay[e.date]++
		pointsByDay[e.date] += e.points
	}

	days := bucketDays(scope, events)
	out := make([]ThroughputBucket, 0, len(days))
	cumT, cumP := 0, 0
	for _, d := range days {
		cumT += tasksByDay[d]
		cumP += pointsByDay[d]
		out = append(out, ThroughputBucket{
			Date:             d,
			CumulativeTasks:  cumT,
			CumulativePoints: cumP,
		})
	}
	return out
}

// --- Metric: load (live overlay snapshot) ---

func computeLoad(tasks []models.Task, agentFilter string) []AgentLoad {
	now := time.Now().UTC()
	loads := map[string]*AgentLoad{}

	ensure := func(a string) *AgentLoad {
		l := loads[a]
		if l == nil {
			l = &AgentLoad{Agent: a, Idle: true}
			loads[a] = l
		}
		return l
	}

	for _, t := range tasks {
		agent := taskAgent(t)
		if agent == "" {
			continue
		}
		if agentFilter != "" && agent != agentFilter {
			continue
		}
		// Active claim: claimed and not done/cancelled.
		active := t.Status == "accepted" || t.Status == "in-progress" || t.Status == "in-review" || t.Status == "blocked"
		if active {
			l := ensure(agent)
			l.Claimed++
			l.Idle = false
		}
		// Currently blocked: open blocked window.
		for _, w := range parseBlockedWindows(t.BlockedPeriods) {
			if w.End == "" {
				_, open := windowSeconds(w, now)
				if open {
					l := ensure(agent)
					l.Blocked++
					l.Idle = false
				}
			}
		}
	}

	names := make([]string, 0, len(loads))
	for a := range loads {
		names = append(names, a)
	}
	sort.Strings(names)
	out := make([]AgentLoad, 0, len(names))
	for _, a := range names {
		out = append(out, *loads[a])
	}
	return out
}

// --- Metric: burndown ---

func computeBurndown(tasks []models.Task, scope CycleScope) BurndownStats {
	out := BurndownStats{}
	out.TotalTasks, out.TotalPoints = scopeTotals(tasks)

	// done events for the burndown.
	type doneEvent struct {
		date   string
		points int
	}
	var events []doneEvent
	remainingTasks := out.TotalTasks
	remainingPoints := out.TotalPoints
	for _, t := range tasks {
		dt := doneTimestamp(t)
		if dt == nil {
			continue
		}
		ts, ok := parseStatsTime(*dt)
		if !ok {
			continue
		}
		pts := 0
		if t.Points != nil {
			pts = *t.Points
		}
		events = append(events, doneEvent{date: ts.UTC().Format("2006-01-02"), points: pts})
		remainingTasks--
		remainingPoints -= pts
	}
	out.RemainingTasks = remainingTasks
	out.RemainingPoints = remainingPoints

	if out.TotalTasks == 0 {
		out.Series = []BurndownBucket{}
		return out
	}

	tasksByDay := map[string]int{}
	pointsByDay := map[string]int{}
	for _, e := range events {
		tasksByDay[e.date]++
		pointsByDay[e.date] += e.points
	}

	evGeneric := make([]struct {
		date   string
		points int
	}, len(events))
	for i, e := range events {
		evGeneric[i] = struct {
			date   string
			points int
		}{e.date, e.points}
	}
	days := bucketDays(scope, evGeneric)
	ideals := idealLine(out.TotalPoints, len(days))

	remT := out.TotalTasks
	remP := out.TotalPoints
	series := make([]BurndownBucket, 0, len(days))
	for i, d := range days {
		remT -= tasksByDay[d]
		remP -= pointsByDay[d]
		ideal := 0
		if i < len(ideals) {
			ideal = ideals[i]
		}
		series = append(series, BurndownBucket{
			Date:            d,
			RemainingPoints: remP,
			RemainingTasks:  remT,
			Ideal:           ideal,
		})
	}
	out.Series = series
	return out
}

// idealLine returns the ideal-burndown remaining-points value at the end of each
// of n days, decreasing linearly from total to 0.
func idealLine(total, n int) []int {
	out := make([]int, n)
	if n == 0 {
		return out
	}
	for i := 0; i < n; i++ {
		// remaining at end of day i (1-indexed step over n days)
		rem := float64(total) * (1 - float64(i+1)/float64(n))
		out[i] = int(math.Round(rem))
		if out[i] < 0 {
			out[i] = 0
		}
	}
	return out
}

// --- Day bucketing shared by throughput + burndown ---

// bucketDays returns the ordered list of YYYY-MM-DD day strings spanning the
// cycle window when available, else the min→max of the done events.
func bucketDays(scope CycleScope, events []struct {
	date   string
	points int
}) []string {
	var start, end time.Time
	haveRange := false

	if s, ok := parseStatsTime(scope.Start); ok {
		if e, ok2 := parseStatsTime(scope.End); ok2 {
			start, end = dayFloor(s), dayFloor(e)
			haveRange = true
		}
	}

	if !haveRange {
		// Derive from events.
		var minD, maxD string
		for _, e := range events {
			if minD == "" || e.date < minD {
				minD = e.date
			}
			if maxD == "" || e.date > maxD {
				maxD = e.date
			}
		}
		if minD == "" {
			return []string{}
		}
		s, _ := time.Parse("2006-01-02", minD)
		e, _ := time.Parse("2006-01-02", maxD)
		start, end = s, e
	}

	// Cap the cycle end at today so we don't draw flat future days.
	today := dayFloor(time.Now().UTC())
	if end.After(today) {
		end = today
	}
	if end.Before(start) {
		end = start
	}

	var days []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format("2006-01-02"))
	}
	return days
}

// --- Generic helpers ---

func parseBlockedWindows(raw string) []blockedWindow {
	if raw == "" || raw == "[]" {
		return nil
	}
	var ws []blockedWindow
	if err := json.Unmarshal([]byte(raw), &ws); err != nil {
		return nil
	}
	return ws
}

// windowSeconds returns the duration of a blocked window (open windows extend to
// now) and whether it is open.
func windowSeconds(w blockedWindow, now time.Time) (float64, bool) {
	s, ok := parseStatsTime(w.Start)
	if !ok {
		return 0, false
	}
	if w.End == "" {
		if now.Before(s) {
			return 0, true
		}
		return now.Sub(s).Seconds(), true
	}
	e, ok := parseStatsTime(w.End)
	if !ok {
		return 0, false
	}
	if e.Before(s) {
		return 0, false
	}
	return e.Sub(s).Seconds(), false
}

// spanSeconds returns to-from in seconds when both timestamps parse and to>=from.
func spanSeconds(from, to *string) (float64, bool) {
	f, okF := parseStatsTimePtr(from)
	if !okF {
		return 0, false
	}
	t, okT := parseStatsTimePtr(to)
	if !okT {
		return 0, false
	}
	if t.Before(f) {
		return 0, false
	}
	return t.Sub(f).Seconds(), true
}

func durationStat(vals []float64) DurationStat {
	n := len(vals)
	if n == 0 {
		return DurationStat{}
	}
	sorted := make([]float64, n)
	copy(sorted, vals)
	sort.Float64s(sorted)
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	return DurationStat{
		Median: percentile(sorted, 50),
		P90:    percentile(sorted, 90),
		Avg:    sum / float64(n),
		Count:  n,
	}
}

// percentile computes the p-th percentile of a pre-sorted slice using linear
// interpolation between closest ranks. p in [0,100].
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := (p / 100) * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// parseStatsTime parses an overlay timestamp. Tolerates the microsecond format,
// RFC3339, and bare dates (YYYY-MM-DD → end-of-day for inclusive cycle ends is
// handled by the caller; here a bare date parses to 00:00 UTC, then we bump
// end-of-day only for cycle-end comparisons via parseCycleEnd).
func parseStatsTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(agentStatsTimeFmt, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		// Bare date: treat as end-of-day so an inclusive cycle end covers the
		// whole final day.
		return t.UTC().Add(24*time.Hour - time.Second), true
	}
	return time.Time{}, false
}

func parseStatsTimePtr(s *string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	return parseStatsTime(*s)
}

func ptr(s string) *string { return &s }

func dayFloor(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
