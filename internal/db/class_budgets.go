package db

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Class budgets, slice T1 (design 1111292b, ruling af6e3a5f). OTP restart
// intensity applied to exception classes: when a (project, reason_code) exceeds
// intensity instances within period_s, ONE systemic exception opens, its
// instances are linked through cause_id, and the owner of the responsible
// doctrine is attributed. T1 is shadow only: no rung, no obligation, no
// message. The ladder (exc.* norms, exceptions.rung / ladder_snapshot_json)
// is seeded here so the whole schema lands in one migration, and stays inert
// until T2.

const (
	excSourceClassBudget = "class_budget"
	excKindSystemic      = "systemic"
	excSweeper           = "relay-sweeper"

	// SettingClassBudgetMode is off | shadow | on (default shadow).
	SettingClassBudgetMode = "class_budget_mode"
	// settingBudgetEpoch is set once at first migrate: instances opened before
	// it (the 220f4f3d backfill) never count, so a deploy cannot open every
	// historical breach on day 1.
	settingBudgetEpoch = "budget_epoch"
	// settingAttributionShare is the minimum share a dimension needs to be
	// blamed (default 0.6).
	settingAttributionShare = "attribution_share"

	defaultAttributionShare = 0.6
	attributionMinInstances = 3
	systemicLinkedMax       = 20 // instance ids kept in evidence_json
	ownerClimbMax           = 5  // reports_to hops before the executive fallback
)

// ClassBudgetModeOff etc. are the class_budget_mode values.
const (
	ClassBudgetModeOff    = "off"
	ClassBudgetModeShadow = "shadow"
	ClassBudgetModeOn     = "on"
)

// excLadderRungs is the rung vocabulary (design §4.1), seeded as inert norms.
var excLadderRungs = []struct {
	rung, bearer, deadlineSetting string
	deadlineS                     int
}{
	{"match_precedent", "relay", "exc_match_precedent_age", 0},
	{"consult_knowledge", "relay", "exc_consult_knowledge_age", 0},
	{"ask_source", "source", "exc_ask_source_age", 14400},
	{"route_specialist", "owner", "exc_route_specialist_age", 172800},
	{"adversarial_review", "reviewer", "exc_adversarial_review_age", 86400},
	{"reversible_action", "owner", "exc_reversible_action_age", 86400},
	{"wait_if_volume", "relay", "exc_wait_if_volume_age", 604800},
	{"supervisor_agent", "supervisor", "exc_supervisor_agent_age", 86400},
	{"human", "human", "exc_human_ttl", 259200},
}

// nonAgentDispatchers are dispatched_by values that are sources, not agents:
// never blamed as a dispatcher (design §3.2).
var nonAgentDispatchers = map[string]bool{
	"": true, "user": true, "human": true, "founder": true, "relay": true, excSweeper: true,
	"anonymous": true, "linear": true, "system": true,
}

func migrateClassBudgets(conn *sql.DB) {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS class_budgets (
		kind                 TEXT NOT NULL,
		reason_code          TEXT NOT NULL DEFAULT '*',
		intensity            INTEGER NOT NULL,
		period_s             INTEGER NOT NULL,
		total_attempt_budget INTEGER NOT NULL DEFAULT 6,
		reversibility        TEXT NOT NULL DEFAULT 'none',
		ladder_json          TEXT,
		suppressed_until     TEXT,
		enabled              INTEGER NOT NULL DEFAULT 1,
		PRIMARY KEY (kind, reason_code)
	)`)
	// INSERT OR IGNORE: an operator-edited row survives every re-migrate.
	_, _ = conn.Exec(`INSERT OR IGNORE INTO class_budgets (kind, reason_code, intensity, period_s, enabled) VALUES
		('blocker', '*', 3, 604800, 1),
		('gate_exhausted', '*', 3, 604800, 1),
		('limbo', '*', 3, 604800, 1),
		('stall', '*', 3, 604800, 1),
		('routing', '*', 3, 604800, 1),
		('lease_expired', '*', 5, 86400, 1),
		('delivery', '*', 10, 86400, 0),
		('plan_change', '*', 0, 604800, 1),
		('integrity', '*', 0, 604800, 1),
		('systemic', '*', 0, 604800, 1)`)
	ensureColumns(conn, "exceptions", map[string]string{
		"cause_id":             "TEXT",
		"rung":                 "INTEGER",
		"ladder_snapshot_json": "TEXT",
		"owner":                "TEXT",
	})
	// "Escalates once" is a database fact: one open systemic per class.
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_exceptions_systemic_open ON exceptions(project, reason_code)
		WHERE source_kind = 'class_budget' AND status = 'open'`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_exceptions_cause ON exceptions(cause_id) WHERE cause_id IS NOT NULL`)
	// Ladder rungs (T2). enabled=0 and subject_kind 'exception' match none of
	// the engine's known-norm guards (ackNormKnown / answerNormKnown): inert.
	for i, r := range excLadderRungs {
		_, _ = conn.Exec(`INSERT OR IGNORE INTO norms (id, subject_kind, trigger, what, while_pred, deadline_kind,
			deadline_anchor, deadline_setting, deadline_default_s, deadline_min_s, deadline_max_s, bearer_kind, sanction,
			sanction_template, max_depth, eval_order, enabled)
			VALUES (?, 'exception', 'class_budget_breach', 'exception_resolved', 'exception_open', 'time',
			'rung_opened_at', ?, ?, 0, 2592000, ?, 'open_child', '', ?, ?, 0)`,
			"exc."+r.rung, r.deadlineSetting, r.deadlineS, r.bearer, len(excLadderRungs)-1, i)
	}
	// exception_attempts is a view over the obligations the ladder opens, not a
	// second ledger (design §4.5).
	_, _ = conn.Exec(`CREATE VIEW IF NOT EXISTS exception_attempts AS
		SELECT o.subject_id AS exception_id, o.escalation_depth AS rung, substr(o.norm_id, 5) AS strategy,
		       o.bearer AS actor, o.created_at AS started_at, o.closed_at AS ended_at,
		       CASE o.state WHEN 'fulfilled' THEN 'resolved_or_done' WHEN 'unfulfilled' THEN 'failed'
		                    WHEN 'inactive' THEN 'skipped' ELSE 'running' END AS outcome,
		       json_extract(o.discharge_evidence, '$.action_ref') AS action_ref,
		       json_extract(o.discharge_evidence, '$.cost_tokens') AS cost_tokens
		FROM obligations o WHERE o.subject_kind = 'exception'`)
	_, _ = conn.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`,
		settingBudgetEpoch, time.Now().UTC().Format(memoryTimeFmt))
}

// classBudget is the effective budget of one (kind, reason_code).
type classBudget struct {
	Intensity, PeriodS int
	Enabled            bool
	SuppressedUntil    string
}

// loadClassBudgets returns every class_budgets row keyed by kind|reason_code.
func (d *DB) loadClassBudgets() (map[string]classBudget, error) {
	rows, err := d.ro().Query(`SELECT kind, reason_code, intensity, period_s, enabled, COALESCE(suppressed_until, '') FROM class_budgets`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]classBudget{}
	for rows.Next() {
		var kind, code string
		var b classBudget
		var enabled int
		if err := rows.Scan(&kind, &code, &b.Intensity, &b.PeriodS, &enabled, &b.SuppressedUntil); err != nil {
			return nil, err
		}
		b.Enabled = enabled == 1
		out[kind+"|"+code] = b
	}
	return out, rows.Err()
}

// budgetFor resolves the (kind, code) row, else the (kind, '*') default. ok is
// false when the class is unbudgeted (no row, disabled, or intensity 0).
func budgetFor(budgets map[string]classBudget, kind, code string) (classBudget, bool) {
	b, found := budgets[kind+"|"+code]
	if !found {
		b, found = budgets[kind+"|*"]
	}
	if !found || !b.Enabled || b.Intensity <= 0 || b.PeriodS <= 0 {
		return classBudget{}, false
	}
	return b, true
}

// Attribution is who must repair the doctrine behind a breached class.
type Attribution struct {
	Dimension string  `json:"dimension"` // assignee | profile | dispatcher | process
	Value     string  `json:"value,omitempty"`
	Share     float64 `json:"share"`
	N         int     `json:"n"`
	Owner     string  `json:"owner,omitempty"`
	OwnerRule string  `json:"owner_rule"` // direct | reports_to | lane_lead | executive | none
}

// SystemicOpened reports one systemic exception opened by a tick.
type SystemicOpened struct {
	ID, Project, Kind, ReasonCode string
	Instances                     int
	RegressionOf                  string
	Attribution                   Attribution
}

type budgetInstance struct {
	ID, TaskID, Assignee, Profile, Dispatcher string
}

// EvaluateClassBudgets is one class-budget tick: it links new instances to
// their open systemic exception, then opens one systemic exception per
// (project, reason_code) whose unlinked instances inside its period reach the
// intensity. Unclassified and benign instances never count, nor anything
// opened before budget_epoch. One writer tx per group that needs a write; a
// quiet tick writes nothing.
func (d *DB) EvaluateClassBudgets(now time.Time) ([]SystemicOpened, error) {
	epoch := d.GetSetting(settingBudgetEpoch)
	if epoch == "" {
		return nil, nil
	}
	budgets, err := d.loadClassBudgets()
	if err != nil {
		return nil, fmt.Errorf("class budgets: %w", err)
	}
	ts := now.UTC().Format(memoryTimeFmt)
	if err := d.linkOpenSystemics(epoch); err != nil {
		return nil, err
	}
	maxPeriod := 0
	for _, b := range budgets {
		if b.PeriodS > maxPeriod {
			maxPeriod = b.PeriodS
		}
	}
	if maxPeriod == 0 {
		return nil, nil
	}
	from := laterTS(epoch, now.Add(-time.Duration(maxPeriod)*time.Second).UTC().Format(memoryTimeFmt))
	// An instance an active suppress guard acted on never counts (4d2e57a3 §4.1).
	rows, err := d.ro().Query(`SELECT project, kind, reason_code, opened_at FROM exceptions e
		WHERE cause_id IS NULL AND source_kind <> ? AND retry_class <> 'benign' AND reason_code <> ?
		  AND opened_at >= ? AND NOT `+fmt.Sprintf(guardSuppressedSQL, "e")+` ORDER BY project, kind, reason_code, opened_at`,
		excSourceClassBudget, excUnclassified, from)
	if err != nil {
		return nil, fmt.Errorf("class budgets scan: %w", err)
	}
	type group struct{ project, kind, code string }
	times := map[group][]string{}
	var order []group
	for rows.Next() {
		var g group
		var at string
		if err := rows.Scan(&g.project, &g.kind, &g.code, &at); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if _, seen := times[g]; !seen {
			order = append(order, g)
		}
		times[g] = append(times[g], at)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var opened []SystemicOpened
	for _, g := range order {
		b, ok := budgetFor(budgets, g.kind, g.code)
		if !ok || (b.SuppressedUntil != "" && b.SuppressedUntil > ts) {
			continue
		}
		windowStart := laterTS(epoch, now.Add(-time.Duration(b.PeriodS)*time.Second).UTC().Format(memoryTimeFmt))
		n := 0
		for _, at := range times[g] {
			if at >= windowStart {
				n++
			}
		}
		if n < b.Intensity {
			continue
		}
		s, ok, err := d.openSystemic(g.project, g.kind, g.code, windowStart, b, ts)
		if err != nil {
			return opened, err
		}
		if ok {
			opened = append(opened, s)
		}
	}
	return opened, nil
}

// linkOpenSystemics attaches unlinked instances to the open systemic of their
// (project, reason_code) when they fall inside its window. Writes only when
// something is to be linked.
func (d *DB) linkOpenSystemics(epoch string) error {
	const pending = `FROM exceptions i JOIN exceptions s
		ON s.source_kind = 'class_budget' AND s.status = 'open' AND s.project = i.project AND s.reason_code = i.reason_code
		WHERE i.cause_id IS NULL AND i.source_kind <> 'class_budget' AND i.opened_at >= ?
		  AND i.opened_at >= json_extract(s.evidence_json, '$.window_start')`
	var n int
	pendingSQL := `SELECT COUNT(*) ` + pending + ` AND NOT ` + fmt.Sprintf(guardSuppressedSQL, "i")
	if err := d.ro().QueryRow(pendingSQL, epoch).Scan(&n); err != nil {
		return fmt.Errorf("class budgets link check: %w", err)
	}
	if n == 0 {
		return nil
	}
	tx, err := d.beginWriterTx()
	if err != nil {
		return fmt.Errorf("class budgets link begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(linkInstancesSQL+` AND exceptions.opened_at >= ?`, epoch); err != nil {
		return fmt.Errorf("class budgets link: %w", err)
	}
	return tx.Commit()
}

// linkInstancesSQL sets cause_id on every unlinked instance of an open
// systemic's (project, reason_code) inside that systemic's window. The
// correlated form works whichever writer opened the systemic.
// A suppressed instance is never linked: it was not counted.
var linkInstancesSQL = `UPDATE exceptions SET cause_id = (
		SELECT s.id FROM exceptions s WHERE s.source_kind = 'class_budget' AND s.status = 'open'
		  AND s.project = exceptions.project AND s.reason_code = exceptions.reason_code)
	WHERE cause_id IS NULL AND source_kind <> 'class_budget'
	  AND NOT ` + fmt.Sprintf(guardSuppressedSQL, "exceptions") + `
	  AND EXISTS (SELECT 1 FROM exceptions s WHERE s.source_kind = 'class_budget' AND s.status = 'open'
	    AND s.project = exceptions.project AND s.reason_code = exceptions.reason_code
	    AND exceptions.opened_at >= json_extract(s.evidence_json, '$.window_start'))`

// openSystemic opens one systemic exception for a breached group and links its
// instances, in one writer tx. ok is false when another writer already holds
// the open systemic (the partial unique index): the instances are linked to
// that one instead.
func (d *DB) openSystemic(project, kind, code, windowStart string, b classBudget, now string) (SystemicOpened, bool, error) {
	instances, err := d.budgetInstances(project, code, windowStart)
	if err != nil {
		return SystemicOpened{}, false, err
	}
	attr := d.attributeClass(project, instances)
	var regressionOf sql.NullString
	err = d.ro().QueryRow(`SELECT id FROM exceptions WHERE source_kind = ? AND project = ? AND reason_code = ? AND status = 'resolved'
		ORDER BY resolved_at DESC LIMIT 1`, excSourceClassBudget, project, code).Scan(&regressionOf)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SystemicOpened{}, false, fmt.Errorf("regression lookup: %w", err)
	}
	ids := make([]string, 0, len(instances))
	for i, in := range instances {
		if i == systemicLinkedMax {
			break
		}
		ids = append(ids, in.ID)
	}
	evidence := map[string]any{
		"window_start": windowStart, "window_end": now, "intensity": b.Intensity, "period_s": b.PeriodS,
		"n": len(instances), "instance_ids": ids, "attribution": attr,
	}
	if regressionOf.Valid {
		evidence["regression_of"] = regressionOf.String
	}
	ev, _ := json.Marshal(evidence)

	sum := sha1.Sum([]byte(excKindSystemic + "|" + kind + "|" + code + "|" + strconv.Itoa(exceptionGroupingVersion)))
	fingerprint := hex.EncodeToString(sum[:])[:16]
	template := fmt.Sprintf("systemic: %s/%s over budget", kind, code)

	tx, err := d.beginWriterTx()
	if err != nil {
		return SystemicOpened{}, false, fmt.Errorf("systemic begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO exception_classes
		(id, kind, reason_code, fingerprint, template, grouping_version, matched_by, occurrences, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, 'new', 0, ?, ?)`,
		uuid.New().String(), excKindSystemic, code, fingerprint, template, exceptionGroupingVersion, now, now); err != nil {
		return SystemicOpened{}, false, fmt.Errorf("systemic class: %w", err)
	}
	var classID string
	if err := tx.QueryRow(`SELECT id FROM exception_classes WHERE fingerprint = ? AND grouping_version = ?`,
		fingerprint, exceptionGroupingVersion).Scan(&classID); err != nil {
		return SystemicOpened{}, false, fmt.Errorf("systemic class id: %w", err)
	}
	id := uuid.New().String()
	var owner any
	if attr.Owner != "" {
		owner = attr.Owner
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO exceptions
		(id, project, class_id, source_kind, source_ref, kind, reason_code, retry_class, fingerprint, matched_by,
		 raised_by, evidence_json, status, opened_at, owner)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'non_retryable', ?, 'new', ?, ?, 'open', ?, ?)`,
		id, project, classID, excSourceClassBudget, project+"|"+code, excKindSystemic, code, fingerprint,
		excSweeper, string(ev), now, owner)
	if err != nil {
		return SystemicOpened{}, false, fmt.Errorf("systemic insert: %w", err)
	}
	inserted, _ := res.RowsAffected()
	if inserted == 1 {
		if err := bumpClassTx(tx, classID, template, now); err != nil {
			return SystemicOpened{}, false, err
		}
	}
	if _, err := tx.Exec(linkInstancesSQL+` AND exceptions.project = ? AND exceptions.reason_code = ?`, project, code); err != nil {
		return SystemicOpened{}, false, fmt.Errorf("systemic link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SystemicOpened{}, false, fmt.Errorf("systemic commit: %w", err)
	}
	if inserted == 0 {
		return SystemicOpened{}, false, nil
	}
	return SystemicOpened{ID: id, Project: project, Kind: kind, ReasonCode: code, Instances: len(instances),
		RegressionOf: regressionOf.String, Attribution: attr}, true, nil
}

// budgetInstances lists the unlinked instances of a group inside its window,
// with the task columns attribution needs.
func (d *DB) budgetInstances(project, code, windowStart string) ([]budgetInstance, error) {
	rows, err := d.ro().Query(`SELECT e.id, COALESCE(e.task_id, ''), COALESCE(t.assigned_to, ''),
			COALESCE(t.profile_slug, ''), COALESCE(t.dispatched_by, '')
		FROM exceptions e LEFT JOIN tasks t ON t.id = e.task_id
		WHERE e.project = ? AND e.reason_code = ? AND e.cause_id IS NULL AND e.source_kind <> ?
		  AND e.retry_class <> 'benign' AND e.opened_at >= ? AND NOT `+fmt.Sprintf(guardSuppressedSQL, "e")+`
		ORDER BY e.opened_at`, project, code, excSourceClassBudget, windowStart)
	if err != nil {
		return nil, fmt.Errorf("budget instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []budgetInstance
	for rows.Next() {
		var in budgetInstance
		if err := rows.Scan(&in.ID, &in.TaskID, &in.Assignee, &in.Profile, &in.Dispatcher); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// attributeClass picks the doctrine owner of a breached class (design §3.2):
// the dimension (assignee > profile > dispatcher on ties) whose top value holds
// at least attribution_share of the instances, else "process" (the project
// executive). Non-agent dispatchers are skipped; the founder is never owner.
func (d *DB) attributeClass(project string, instances []budgetInstance) Attribution {
	n := len(instances)
	share := defaultAttributionShare
	if raw := d.GetSetting(settingAttributionShare); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 && v <= 1 {
			share = v
		}
	}
	best := Attribution{Dimension: "process", N: n}
	if n >= attributionMinInstances {
		dims := []struct {
			name string
			get  func(budgetInstance) string
		}{
			{"assignee", func(i budgetInstance) string { return i.Assignee }},
			{"profile", func(i budgetInstance) string { return i.Profile }},
			{"dispatcher", func(i budgetInstance) string {
				if nonAgentDispatchers[strings.ToLower(i.Dispatcher)] || !d.isRegisteredAgent(project, i.Dispatcher) {
					return ""
				}
				return i.Dispatcher
			}},
		}
		for _, dim := range dims {
			value, count := topValue(instances, dim.get)
			if value == "" {
				continue
			}
			s := float64(count) / float64(n)
			if s >= share && s > best.Share {
				best = Attribution{Dimension: dim.name, Value: value, Share: s, N: n}
			}
		}
	}
	best.Owner, best.OwnerRule = d.resolveDoctrineOwner(project, best)
	return best
}

// topValue returns the most frequent non-empty value (ties: lexical order, so
// the choice is deterministic) and its count.
func topValue(instances []budgetInstance, get func(budgetInstance) string) (string, int) {
	counts := map[string]int{}
	for _, in := range instances {
		if v := get(in); v != "" {
			counts[v]++
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	top, max := "", 0
	for _, k := range keys {
		if counts[k] > max {
			top, max = k, counts[k]
		}
	}
	return top, max
}

func (d *DB) isRegisteredAgent(project, name string) bool {
	if name == "" {
		return false
	}
	a, err := d.GetAgent(project, name)
	return err == nil && a != nil
}

// resolveDoctrineOwner turns an attribution into an active agent: assignee →
// its reports_to; profile → the lane lead (most common reports_to of the
// profile's active agents); dispatcher → itself; process → the executive. An
// inactive candidate climbs reports_to, then falls back to the project's
// first active executive. Never the founder; "" when nobody resolves.
func (d *DB) resolveDoctrineOwner(project string, a Attribution) (string, string) {
	candidate, rule := "", "direct"
	switch a.Dimension {
	case "assignee":
		if ag, err := d.GetAgent(project, a.Value); err == nil && ag != nil && ag.ReportsTo != nil {
			candidate, rule = *ag.ReportsTo, "reports_to"
		}
	case "profile":
		candidate, rule = d.laneLead(project, a.Value), "lane_lead"
	case "dispatcher":
		candidate = a.Value
	}
	for hops := 0; candidate != "" && hops <= ownerClimbMax; hops++ {
		if nonAgentDispatchers[strings.ToLower(candidate)] {
			break
		}
		ag, err := d.GetAgent(project, candidate)
		if err != nil || ag == nil {
			break
		}
		if ag.Status == "active" {
			return ag.Name, rule
		}
		if ag.ReportsTo == nil || *ag.ReportsTo == "" {
			break
		}
		candidate, rule = *ag.ReportsTo, "reports_to"
	}
	if agents, err := d.ListAgents(project); err == nil {
		for _, ag := range agents {
			if ag.IsExecutive && ag.Status == "active" && !nonAgentDispatchers[strings.ToLower(ag.Name)] {
				return ag.Name, "executive"
			}
		}
	}
	return "", "none"
}

// laneLead is the most common reports_to among the project's active agents
// running profile ("" when none has one).
func (d *DB) laneLead(project, profile string) string {
	agents, err := d.ListAgents(project)
	if err != nil {
		return ""
	}
	counts := map[string]int{}
	for _, ag := range agents {
		if ag.Status == "active" && ag.ProfileSlug != nil && *ag.ProfileSlug == profile && ag.ReportsTo != nil && *ag.ReportsTo != "" {
			counts[*ag.ReportsTo]++
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lead, max := "", 0
	for _, k := range keys {
		if counts[k] > max {
			lead, max = k, counts[k]
		}
	}
	return lead
}

// laterTS returns the later of two memoryTimeFmt timestamps.
func laterTS(a, b string) string {
	if a > b {
		return a
	}
	return b
}
