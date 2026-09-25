package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Obligations engine, slice 1 (DEC-wraith-obligations-1, design 50b4b349).
// Norms are seeded rows whose predicates are closed Go enums: each value below
// maps to a hand-written, SQL-bounded predicate, never to a user expression.
// Slice 1 carries only the two ACK norms and reproduces the legacy ACK checker
// byte-for-byte, quirks Q1-Q4 included.

// Norm ids seeded by migrate: the ACK escalation chain, rungs 0..3
// (DEC-wraith-obligations-1 slice 2a, max_depth 3).
const (
	NormAckNotify   = "ack.notify"   // rung 0: dispatcher, fyi P2 no-wake
	NormAckEscalate = "ack.escalate" // rung 1: dispatcher, P1
	NormAckManager  = "ack.manager"  // rung 2: reports_to / executive / founder
	NormAckHuman    = "ack.human"    // rung 3: the human, last rung only
)

// AckNormDepth is each ACK norm's escalation depth. Firing a rung closes every
// lower rung still active (Q1 retired: no notify after an escalate).
var AckNormDepth = map[string]int{NormAckNotify: 0, NormAckEscalate: 1, NormAckManager: 2, NormAckHuman: 3}

// Closed enums (slice 1 values only; a new value is a code change + review).
// The engine runs a norm only when every one of its trigger / what / while /
// bearer values is one of these (ackNormKnown); any other row is inert.
const (
	SubjectTask               = "task"
	TriggerTaskPendingUnclaim = "task_pending_unclaimed"
	WhatTaskLeftPending       = "task_left_pending"
	WhileTaskPendingLive      = "task_pending_live"
	BearerAssigneeProfile     = "assignee_profile"
	BearerProfilePool         = "profile_pool" // unassigned: any agent of the profile discharges
)

// ackNormKnown restricts a query on norms n to the closed-enum ACK shape.
const ackNormKnown = "n.subject_kind = '" + SubjectTask + "' AND n.trigger = '" + TriggerTaskPendingUnclaim +
	"' AND n.what = '" + WhatTaskLeftPending + "' AND n.while_pred = '" + WhileTaskPendingLive +
	"' AND n.bearer_kind = '" + BearerAssigneeProfile + "' AND n.enabled = 1"

// Obligation states (NPL lifecycle, design §3.3).
const (
	ObligationActive      = "active"
	ObligationFulfilled   = "fulfilled"
	ObligationUnfulfilled = "unfulfilled"
	ObligationInactive    = "inactive"
)

// ackMarkColumn maps the rung-0/1 ACK norms to the legacy tasks column their
// transition also CASes (kept for UI/back-compat). Whitelist: the column name
// is spliced into SQL. Rungs 2/3 have no legacy column.
var ackMarkColumn = map[string]string{
	NormAckEscalate: "ack_escalated_at",
	NormAckNotify:   "ack_notified_at",
}

// ackClock is when a task's ACK clock started: the last time it entered
// 'pending' (task c933b2f1), falling back to dispatched_at for rows written
// without pending_since (linear mirror, pre-migration binaries).
const ackClock = `COALESCE(t.pending_since, t.dispatched_at)`

// ackCandidate is task_pending_unclaimed / task_pending_live: exactly the
// GetUnackedTasks predicate the legacy checker reads each tick.
const ackCandidate = `t.status = 'pending' AND t.archived_at IS NULL AND ` + ackClock + ` < ?
	AND (t.run_state IS NULL OR t.run_state = '')`

// TaskObligation is an active ACK obligation joined to the task fields the
// sweeper needs to decide and word its sanction.
type TaskObligation struct {
	ID               string
	NormID           string
	Depth            int
	SanctionTemplate string
	TaskID           string
	Project          string
	Title            string
	ProfileSlug      string
	DispatchedBy     string
	DispatchedAt     string // the ACK clock (ackClock), not always tasks.dispatched_at

	// Set by TaskObligationByID only (the tools need them; the sweeper doesn't).
	State      string
	BearerKind string
	Bearer     string
	AssignedTo string
	What       string
}

// IsBearer reports whether agent (with profile) owes this obligation: its
// profile pool, or the assignee of the task for an assignee_profile bearer.
func (o TaskObligation) IsBearer(agent, profile string) bool {
	if o.BearerKind == BearerProfilePool {
		return o.Bearer == agent || (profile != "" && o.Bearer == profile)
	}
	return o.BearerKind == BearerAssigneeProfile && o.AssignedTo == agent
}

func bindingsHash(normID, subjectKind, subjectID string, depth int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", normID, subjectKind, subjectID, depth)))
	return hex.EncodeToString(sum[:])
}

// InstantiateTaskAck opens the ACK obligations of every task the legacy
// checker would read at this cutoff (now - ack_notify_age) that has none yet.
// A norm whose legacy mark is already set opens pre-closed as unfulfilled, so
// historical marks never re-fire; a chain rung (depth >= 2) of a task that was
// already escalated opens pre-closed as inactive, so tasks escalated before the
// chain existed never mass-fire it. The bearer is the assignee profile, or the
// profile pool when nobody is assigned. Writes only when a task is newly
// eligible.
func (d *DB) InstantiateTaskAck(cutoff, now time.Time) (int, error) {
	rows, err := d.ro().Query(`SELECT n.id, n.version, t.id, t.project, COALESCE(t.profile_slug, ''),
			COALESCE(t.assigned_to, '') <> '', t.ack_notified_at, t.ack_escalated_at,
			COALESCE((SELECT group_concat(e.norm_id) FROM obligations e
				WHERE e.subject_kind = ? AND e.subject_id = t.id AND e.state = ?), '')
		FROM tasks t JOIN norms n ON `+ackNormKnown+` AND (n.project IS NULL OR n.project = t.project)
		WHERE `+ackCandidate+`
		  AND NOT EXISTS (SELECT 1 FROM obligations o WHERE o.norm_id = n.id AND o.subject_kind = ? AND o.subject_id = t.id)`,
		SubjectTask, ObligationUnfulfilled, cutoff.UTC().Format(memoryTimeFmt), SubjectTask)
	if err != nil {
		return 0, fmt.Errorf("obligation candidates: %w", err)
	}
	type pending struct {
		normID, bearerKind, taskID, project, profile string
		version                                      int
		preState, preReason                          string
		preClosedAt                                  sql.NullString
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var assigned bool
		var notified, escalated sql.NullString
		var fired string // norms of this task already fired (unfulfilled)
		if err := rows.Scan(&p.normID, &p.version, &p.taskID, &p.project, &p.profile, &assigned,
			&notified, &escalated, &fired); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan obligation candidate: %w", err)
		}
		p.bearerKind = BearerProfilePool
		if assigned {
			p.bearerKind = BearerAssigneeProfile
		}
		highestFired := -1
		for _, n := range strings.Split(fired, ",") {
			if d, ok := AckNormDepth[n]; ok && d > highestFired {
				highestFired = d
			}
		}
		depth := AckNormDepth[p.normID]
		switch {
		case p.normID == NormAckNotify && notified.Valid:
			p.preState, p.preClosedAt = ObligationUnfulfilled, notified
		case p.normID == NormAckEscalate && escalated.Valid:
			p.preState, p.preClosedAt = ObligationUnfulfilled, escalated
		case depth >= 2 && (escalated.Valid || highestFired >= AckNormDepth[NormAckEscalate]):
			p.preState, p.preReason = ObligationInactive, "escalated before chain"
		case highestFired > depth:
			p.preState, p.preReason = ObligationInactive, "superseded by a fired higher rung"
		}
		todo = append(todo, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(todo) == 0 {
		return 0, err
	}

	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, fmt.Errorf("instantiate begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(memoryTimeFmt)
	opened := 0
	for _, p := range todo {
		state, closedAt, evidence := ObligationActive, p.preClosedAt, sql.NullString{}
		if p.preState != "" {
			state = p.preState
		}
		if state == ObligationInactive {
			closedAt = sql.NullString{String: ts, Valid: true}
			evidence = sql.NullString{String: fmt.Sprintf(`{"reason":%q}`, p.preReason), Valid: true}
		}
		depth := AckNormDepth[p.normID]
		res, err := tx.Exec(`INSERT OR IGNORE INTO obligations
			(id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state, created_at, closed_at,
			 escalation_depth, discharge_evidence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), p.project, p.normID, p.version, bindingsHash(p.normID, SubjectTask, p.taskID, depth),
			SubjectTask, p.taskID, p.bearerKind, p.profile, state, ts, closedAt, depth, evidence)
		if err != nil {
			return 0, fmt.Errorf("insert obligation: %w", err)
		}
		n, _ := res.RowsAffected()
		opened += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("instantiate commit: %w", err)
	}
	return opened, nil
}

// ActiveTaskObligations lists the active ACK obligations of the tasks the
// legacy checker would read at this cutoff, per task highest rung first (norm
// eval_order), so the sweeper fires at most one sanction per task per tick.
func (d *DB) ActiveTaskObligations(cutoff time.Time) ([]TaskObligation, error) {
	rows, err := d.ro().Query(`SELECT o.id, o.norm_id, o.escalation_depth, n.sanction_template, t.id, t.project, COALESCE(t.title, ''),
			COALESCE(t.profile_slug, ''), t.dispatched_by, `+ackClock+`
		FROM obligations o
		JOIN norms n ON n.id = o.norm_id AND `+ackNormKnown+`
		JOIN tasks t ON t.id = o.subject_id
		WHERE o.state = ? AND o.subject_kind = ? AND `+ackCandidate+`
		ORDER BY `+ackClock+`, t.id, n.eval_order`,
		ObligationActive, SubjectTask, cutoff.UTC().Format(memoryTimeFmt))
	if err != nil {
		return nil, fmt.Errorf("active obligations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TaskObligation
	for rows.Next() {
		var o TaskObligation
		if err := rows.Scan(&o.ID, &o.NormID, &o.Depth, &o.SanctionTemplate, &o.TaskID, &o.Project, &o.Title,
			&o.ProfileSlug, &o.DispatchedBy, &o.DispatchedAt); err != nil {
			return nil, fmt.Errorf("scan active obligation: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// TransitionTaskAck fires one ACK rung, in one writer tx: the obligation CAS
// (active -> unfulfilled), the task guard (still pending, not a run container;
// for rungs 0/1 the exact legacy tasks.ack_*_at CAS), then every lower rung of
// the task still active closes inactive (Q1 retired), as do the alsoClose norms
// (rung 2 landing on the founder closes rung 3). If a CAS matches 0 rows
// nothing lands and ok is false — the caller must not fire the sanction.
func (d *DB) TransitionTaskAck(obligationID, normID, taskID string, deadline, now time.Time, alsoClose ...string) (ok bool, err error) {
	return d.transitionTaskAck(obligationID, normID, taskID, "", deadline, now, alsoClose...)
}

// DeclineTaskAck is the bearer's early breach (slice 2b): the same transition
// as a due rung firing — unfulfilled, task guard, lower rungs closed — with the
// decline reason class recorded. The caller then fires the rung's sanction.
func (d *DB) DeclineTaskAck(obligationID, normID, taskID, reasonClass string, now time.Time, alsoClose ...string) (ok bool, err error) {
	if !DeclineReasons[reasonClass] {
		return false, fmt.Errorf("decline: %q is not a reason class", reasonClass)
	}
	return d.transitionTaskAck(obligationID, normID, taskID, reasonClass, now, now, alsoClose...)
}

// DeclineReasons is the closed enum of obligation_decline reason classes.
var DeclineReasons = map[string]bool{"not_mine": true, "cannot": true, "blocked_by": true, "duplicate": true}

func (d *DB) transitionTaskAck(obligationID, normID, taskID, declineReason string, deadline, now time.Time, alsoClose ...string) (ok bool, err error) {
	depth, known := AckNormDepth[normID]
	if !known {
		return false, fmt.Errorf("transition: %q is not an ACK norm", normID)
	}
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, fmt.Errorf("transition begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(memoryTimeFmt)
	res, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, deadline_at = ?, decline_reason_class = NULLIF(?, '')
		WHERE id = ? AND state = ?`,
		ObligationUnfulfilled, ts, deadline.UTC().Format(memoryTimeFmt), declineReason, obligationID, ObligationActive)
	if err != nil {
		return false, fmt.Errorf("transition obligation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if col, legacy := ackMarkColumn[normID]; legacy {
		res, err = tx.Exec(`UPDATE tasks SET `+col+` = ? WHERE id = ? AND status = 'pending'
			AND (run_state IS NULL OR run_state = '') AND `+col+` IS NULL`, ts, taskID)
		if err != nil {
			return false, fmt.Errorf("transition legacy mark: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, nil
		}
	} else {
		// No legacy column: re-check the task under the writer lock instead.
		var live int
		err := tx.QueryRow(`SELECT 1 FROM tasks WHERE id = ? AND status = 'pending'
			AND (run_state IS NULL OR run_state = '') AND archived_at IS NULL`, taskID).Scan(&live)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("transition task guard: %w", err)
		}
	}
	evidence := fmt.Sprintf(`{"superseded_by":%q}`, normID)
	if _, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = ?
		WHERE subject_kind = ? AND subject_id = ? AND state = ? AND escalation_depth < ?`,
		ObligationInactive, ts, evidence, SubjectTask, taskID, ObligationActive, depth); err != nil {
		return false, fmt.Errorf("close lower rungs: %w", err)
	}
	for _, other := range alsoClose {
		if _, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = ?
			WHERE subject_kind = ? AND subject_id = ? AND norm_id = ? AND state = ?`,
			ObligationInactive, ts, evidence, SubjectTask, taskID, other, ObligationActive); err != nil {
			return false, fmt.Errorf("close %s: %w", other, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("transition commit: %w", err)
	}
	return true, nil
}

// CloseMootTaskObligations closes active task obligations whose task left the
// candidate set for good: fulfilled when the task moved on (claimed, started,
// done…), inactive when the reason is gone (cancelled, archived, became a run
// container, deleted). Fulfilled beats inactive. No sanction fires. Writes
// only when something closes.
func (d *DB) CloseMootTaskObligations(now time.Time) (int, error) {
	rows, err := d.ro().Query(`SELECT o.id, COALESCE(t.status, ''),
			t.id IS NULL OR t.status = 'cancelled' OR t.archived_at IS NOT NULL OR (t.run_state IS NOT NULL AND t.run_state <> '')
		FROM obligations o LEFT JOIN tasks t ON t.id = o.subject_id
		WHERE o.state = ? AND o.subject_kind = ?
		  AND (t.id IS NULL OR t.status <> 'pending' OR t.archived_at IS NOT NULL OR (t.run_state IS NOT NULL AND t.run_state <> ''))`,
		ObligationActive, SubjectTask)
	if err != nil {
		return 0, fmt.Errorf("moot obligations: %w", err)
	}
	type closing struct {
		id, status string
		moot       bool
	}
	var todo []closing
	for rows.Next() {
		var c closing
		if err := rows.Scan(&c.id, &c.status, &c.moot); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan moot obligation: %w", err)
		}
		todo = append(todo, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(todo) == 0 {
		return 0, err
	}

	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, fmt.Errorf("close begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(memoryTimeFmt)
	closed := 0
	for _, c := range todo {
		state := ObligationInactive
		if !c.moot || (c.status != "" && c.status != "pending" && c.status != "cancelled") {
			state = ObligationFulfilled
		}
		evidence, _ := json.Marshal(map[string]string{"task_status": c.status})
		res, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = ? WHERE id = ? AND state = ?`,
			state, ts, string(evidence), c.id, ObligationActive)
		if err != nil {
			return 0, fmt.Errorf("close obligation: %w", err)
		}
		n, _ := res.RowsAffected()
		closed += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("close commit: %w", err)
	}
	return closed, nil
}

// StampLateDone records late-done (design §3.4): an unfulfilled task
// obligation whose task was later taken up (left pending, not cancelled) gets
// done_at and late_by_ms = done_at - deadline, once.
func (d *DB) StampLateDone(now time.Time) (int, error) {
	rows, err := d.ro().Query(`SELECT o.id, COALESCE(o.deadline_at, '')
		FROM obligations o JOIN tasks t ON t.id = o.subject_id
		WHERE o.state = ? AND o.done_at IS NULL AND o.subject_kind = ?
		  AND t.status NOT IN ('pending', 'cancelled')`,
		ObligationUnfulfilled, SubjectTask)
	if err != nil {
		return 0, fmt.Errorf("late-done candidates: %w", err)
	}
	type late struct{ id, deadline string }
	var todo []late
	for rows.Next() {
		var l late
		if err := rows.Scan(&l.id, &l.deadline); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan late-done: %w", err)
		}
		todo = append(todo, l)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(todo) == 0 {
		return 0, err
	}

	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, fmt.Errorf("late-done begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(memoryTimeFmt)
	stamped := 0
	for _, l := range todo {
		var lateBy sql.NullInt64
		if deadline, err := time.Parse(memoryTimeFmt, l.deadline); err == nil {
			lateBy = sql.NullInt64{Int64: now.Sub(deadline).Milliseconds(), Valid: true}
		}
		res, err := tx.Exec(`UPDATE obligations SET done_at = ?, late_by_ms = ? WHERE id = ? AND state = ? AND done_at IS NULL`,
			ts, lateBy, l.id, ObligationUnfulfilled)
		if err != nil {
			return 0, fmt.Errorf("stamp late-done: %w", err)
		}
		n, _ := res.RowsAffected()
		stamped += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("late-done commit: %w", err)
	}
	return stamped, nil
}

// TaskObligationByID reads one task obligation with its task and norm fields,
// whatever its state. (nil, nil) when unknown or not in project.
func (d *DB) TaskObligationByID(project, id string) (*TaskObligation, error) {
	var o TaskObligation
	err := d.ro().QueryRow(`SELECT o.id, o.norm_id, o.escalation_depth, n.sanction_template, n.what, t.id, t.project,
			COALESCE(t.title, ''), COALESCE(t.profile_slug, ''), t.dispatched_by, `+ackClock+`,
			o.state, o.bearer_kind, o.bearer, COALESCE(t.assigned_to, '')
		FROM obligations o JOIN norms n ON n.id = o.norm_id JOIN tasks t ON t.id = o.subject_id
		WHERE o.id = ? AND o.project = ? AND o.subject_kind = ?`, id, project, SubjectTask).
		Scan(&o.ID, &o.NormID, &o.Depth, &o.SanctionTemplate, &o.What, &o.TaskID, &o.Project, &o.Title, &o.ProfileSlug,
			&o.DispatchedBy, &o.DispatchedAt, &o.State, &o.BearerKind, &o.Bearer, &o.AssignedTo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("obligation %s: %w", id, err)
	}
	return &o, nil
}

// ObligationView is one obligation as obligations_mine shows it.
type ObligationView struct {
	ID          string `json:"id"`
	Norm        string `json:"norm"`
	What        string `json:"what"`
	SubjectKind string `json:"subject_kind"`
	SubjectID   string `json:"subject_id"`
	Subject     string `json:"subject,omitempty"`
	BearerKind  string `json:"bearer_kind"`
	Depth       int    `json:"escalation_depth"`
	Deadline    string `json:"deadline,omitempty"`
}

// MyTaskObligations lists the active task obligations agent owes in project:
// its profile pool (bearer = its name or profile) or, for an assignee_profile
// bearer, the tasks assigned to it. The deadline is the norm's current setting
// from the ACK clock (ackClock), as the sweeper computes it. Read-only.
func (d *DB) MyTaskObligations(project, agent, profile string) ([]ObligationView, error) {
	rows, err := d.ro().Query(`SELECT o.id, o.norm_id, n.what, o.subject_kind, o.subject_id, COALESCE(t.title, ''),
			o.bearer_kind, o.escalation_depth, `+ackClock+`, COALESCE(n.deadline_setting, ''),
			COALESCE(n.deadline_default_s, 0), COALESCE(n.deadline_min_s, 0), COALESCE(n.deadline_max_s, 0)
		FROM obligations o JOIN norms n ON n.id = o.norm_id JOIN tasks t ON t.id = o.subject_id
		WHERE o.project = ? AND o.state = ? AND o.subject_kind = ?
		  AND ((o.bearer_kind = ? AND o.bearer IN (?, ?)) OR (o.bearer_kind = ? AND t.assigned_to = ?))
		ORDER BY `+ackClock+`, o.escalation_depth`,
		project, ObligationActive, SubjectTask, BearerProfilePool, agent, profile, BearerAssigneeProfile, agent)
	if err != nil {
		return nil, fmt.Errorf("my obligations: %w", err)
	}
	type row struct {
		v                 ObligationView
		dispatchedAt, key string
		def, minS, maxS   int64
	}
	var raw []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.v.ID, &r.v.Norm, &r.v.What, &r.v.SubjectKind, &r.v.SubjectID, &r.v.Subject,
			&r.v.BearerKind, &r.v.Depth, &r.dispatchedAt, &r.key, &r.def, &r.minS, &r.maxS); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan my obligation: %w", err)
		}
		raw = append(raw, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ObligationView, 0, len(raw))
	for _, r := range raw {
		if at, err := time.Parse(memoryTimeFmt, r.dispatchedAt); err == nil && r.key != "" {
			age := d.SettingDuration(r.key, time.Duration(r.def)*time.Second, time.Duration(r.minS)*time.Second, time.Duration(r.maxS)*time.Second)
			r.v.Deadline = at.Add(age).UTC().Format(memoryTimeFmt)
		}
		out = append(out, r.v)
	}
	return out, nil
}

// DischargeTaskObligation moves an active obligation to fulfilled only if the
// relay itself sees its what hold — for task_left_pending, the task was taken
// up (left pending, not cancelled). The claim alone is never trusted: when the
// predicate is false nothing changes and reason says why.
func (d *DB) DischargeTaskObligation(project, id, by, evidence string, now time.Time) (ok bool, reason string, err error) {
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, "", fmt.Errorf("discharge begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var state, what string
	var status sql.NullString
	err = tx.QueryRow(`SELECT o.state, n.what, t.status FROM obligations o JOIN norms n ON n.id = o.norm_id
		LEFT JOIN tasks t ON t.id = o.subject_id WHERE o.id = ? AND o.project = ? AND o.subject_kind = ?`,
		id, project, SubjectTask).Scan(&state, &what, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "unknown obligation", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("discharge read: %w", err)
	}
	switch {
	case state != ObligationActive:
		return false, "obligation is " + state + ", not active", nil
	case what != WhatTaskLeftPending:
		return false, "the relay cannot verify " + what, nil
	case !status.Valid || status.String == "pending" || status.String == "cancelled":
		return false, "predicate task_left_pending is false: the task is " + status.String, nil
	}
	ev, _ := json.Marshal(map[string]string{"by": by, "evidence": evidence, "task_status": status.String})
	res, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = ? WHERE id = ? AND state = ?`,
		ObligationFulfilled, now.UTC().Format(memoryTimeFmt), string(ev), id, ObligationActive)
	if err != nil {
		return false, "", fmt.Errorf("discharge: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, "obligation moved concurrently", nil
	}
	if err := tx.Commit(); err != nil {
		return false, "", fmt.Errorf("discharge commit: %w", err)
	}
	return true, "", nil
}

// --- Answer obligations (roadmap item 10, slice A, task 044a4876) ---------
//
// A direct or team message tagged ask or decide opens answer.reply on each
// recipient (subject_kind message). A reply from the bearer whose reply_to
// chain reaches the ask fulfils it; the bearer can also decline it. The
// deadline breach, the role child and the sanction are slice B. Every query
// here is keyed on subject_kind = 'message', so the ACK (task) paths never see
// these rows.

// Answer chain norm ids and their closed enum values.
const (
	NormAnswerReply = "answer.reply" // depth 0: the recipient
	NormAnswerRole  = "answer.role"  // depth 1: the recipient's role (slice B)
	NormAnswerHuman = "answer.human" // depth 2 = max_depth: the human (slice B)

	SubjectMessage      = "message"
	TriggerMessageAsk   = "message_ask"
	WhatMessageAnswered = "message_answered"
	WhileMessageOpen    = "message_open"
	BearerRecipient     = "recipient"
)

// AnswerActions are the action_required tags that open an answer obligation.
var AnswerActions = map[string]bool{"ask": true, "decide": true}

// answerReplyHops bounds the reply_to walk from a reply up to the ask.
const answerReplyHops = 8

// answerNormKnown restricts a query on norms n to the answer chain.
const answerNormKnown = "n.subject_kind = '" + SubjectMessage + "' AND n.trigger = '" + TriggerMessageAsk +
	"' AND n.what = '" + WhatMessageAnswered + "' AND n.while_pred = '" + WhileMessageOpen + "' AND n.enabled = 1"

// OpenAnswerObligations opens answer.reply on each recipient of a message
// whose action_required is ask or decide. Sentinel principals (user, cron,
// linear) never bear one: the human is only the last rung. Idempotent
// (UNIQUE norm_id + bindings_hash per message and recipient). Returns how many
// opened; writes nothing for any other tag.
func (d *DB) OpenAnswerObligations(project, messageID, actionRequired string, recipients []string, now time.Time) (int, error) {
	if !AnswerActions[actionRequired] || messageID == "" {
		return 0, nil
	}
	var bearers []string
	seen := map[string]bool{}
	for _, r := range recipients {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || r == "*" || integritySentinels[r] || seen[r] {
			continue
		}
		seen[r] = true
		bearers = append(bearers, r)
	}
	if len(bearers) == 0 {
		return 0, nil
	}
	var version int
	var key sql.NullString
	var def, minS, maxS sql.NullInt64
	err := d.ro().QueryRow(`SELECT n.version, n.deadline_setting, n.deadline_default_s, n.deadline_min_s, n.deadline_max_s
		FROM norms n WHERE n.id = ? AND `+answerNormKnown, NormAnswerReply).Scan(&version, &key, &def, &minS, &maxS)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // norm disabled or absent: nothing to open
	}
	if err != nil {
		return 0, fmt.Errorf("answer norm: %w", err)
	}
	age := d.SettingDuration(key.String, time.Duration(def.Int64)*time.Second,
		time.Duration(minS.Int64)*time.Second, time.Duration(maxS.Int64)*time.Second)
	ts := now.UTC().Format(memoryTimeFmt)
	deadline := now.Add(age).UTC().Format(memoryTimeFmt)

	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, fmt.Errorf("answer open begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	opened := 0
	for _, b := range bearers {
		res, err := tx.Exec(`INSERT OR IGNORE INTO obligations
			(id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state,
			 created_at, deadline_at, escalation_depth)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			uuid.New().String(), project, NormAnswerReply, version,
			bindingsHash(NormAnswerReply, SubjectMessage, messageID+"|"+b, 0),
			SubjectMessage, messageID, BearerRecipient, b, ObligationActive, ts, deadline)
		if err != nil {
			return 0, fmt.Errorf("answer open: %w", err)
		}
		n, _ := res.RowsAffected()
		opened += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("answer open commit: %w", err)
	}
	return opened, nil
}

// answerChain returns the message ids a reply answers: its reply_to, then that
// message's reply_to, and so on, read from messages and, for a purged link,
// from message_tombstones. At most answerReplyHops ids; stops at a cycle or an
// unknown id.
func (d *DB) answerChain(project, replyTo string) []string {
	var ids []string
	seen := map[string]bool{}
	for cur := replyTo; cur != "" && len(ids) < answerReplyHops && !seen[cur]; {
		seen[cur] = true
		ids = append(ids, cur)
		var parent sql.NullString
		err := d.ro().QueryRow(`SELECT reply_to FROM messages WHERE id = ? AND project = ?`, cur, project).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			err = d.ro().QueryRow(`SELECT reply_to FROM message_tombstones WHERE id = ? AND project = ?`, cur, project).Scan(&parent)
		}
		if err != nil {
			break
		}
		cur = parent.String
	}
	return ids
}

// FulfilAnswerObligations fulfils the active answer obligations borne by from
// on any message its reply answers (the reply_to chain, tombstones included).
// Reads first on the RO pool and opens a writer tx only when something is
// active, so an ordinary reply costs no write.
func (d *DB) FulfilAnswerObligations(project, from, replyTo, replyID string, now time.Time) (int, error) {
	if replyTo == "" || from == "" {
		return 0, nil
	}
	chain := d.answerChain(project, replyTo)
	if len(chain) == 0 {
		return 0, nil
	}
	ph, args := inPlaceholders(chain)
	q := `SELECT id FROM obligations WHERE project = ? AND subject_kind = ? AND state = ? AND bearer = ?
		AND subject_id IN (` + ph + `)`
	rows, err := d.ro().Query(q, append([]interface{}{project, SubjectMessage, ObligationActive, strings.ToLower(from)}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("answer candidates: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan answer candidate: %w", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}

	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, fmt.Errorf("answer fulfil begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(memoryTimeFmt)
	ev, _ := json.Marshal(map[string]string{"reply": replyID, "by": from})
	fulfilled := 0
	for _, id := range ids {
		res, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = ? WHERE id = ? AND state = ?`,
			ObligationFulfilled, ts, string(ev), id, ObligationActive)
		if err != nil {
			return 0, fmt.Errorf("answer fulfil: %w", err)
		}
		n, _ := res.RowsAffected()
		fulfilled += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("answer fulfil commit: %w", err)
	}
	return fulfilled, nil
}

// AnswerObligation is one message obligation as the obligation tools read it.
type AnswerObligation struct {
	ID, NormID, MessageID, Bearer, State, CreatedAt string
}

// AnswerObligationByID reads one answer obligation in project, whatever its
// state. (nil, nil) when unknown, in another project, or not a message one.
func (d *DB) AnswerObligationByID(project, id string) (*AnswerObligation, error) {
	var o AnswerObligation
	err := d.ro().QueryRow(`SELECT id, norm_id, subject_id, bearer, state, created_at FROM obligations
		WHERE id = ? AND project = ? AND subject_kind = ?`, id, project, SubjectMessage).
		Scan(&o.ID, &o.NormID, &o.MessageID, &o.Bearer, &o.State, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("answer obligation %s: %w", id, err)
	}
	return &o, nil
}

// answeredBy reports whether from sent a message after since whose reply_to
// chain reaches messageID: the predicate message_answered, re-checked by the
// relay on discharge instead of trusting the claim.
func (d *DB) answeredBy(project, from, messageID, since string) (string, bool) {
	rows, err := d.ro().Query(`SELECT id, reply_to FROM messages
		WHERE project = ? AND from_agent = ? AND reply_to IS NOT NULL AND reply_to <> '' AND created_at >= ?
		ORDER BY created_at`, project, from, since)
	if err != nil {
		return "", false
	}
	type reply struct{ id, replyTo string }
	var replies []reply
	for rows.Next() {
		var r reply
		if rows.Scan(&r.id, &r.replyTo) == nil {
			replies = append(replies, r)
		}
	}
	_ = rows.Close()
	for _, r := range replies {
		for _, id := range d.answerChain(project, r.replyTo) {
			if id == messageID {
				return r.id, true
			}
		}
	}
	return "", false
}

// DischargeAnswerObligation fulfils an active answer obligation only if its
// bearer's reply to the message exists; otherwise nothing changes and reason
// says why.
func (d *DB) DischargeAnswerObligation(project, id, by, evidence string, now time.Time) (ok bool, reason string, err error) {
	o, err := d.AnswerObligationByID(project, id)
	if err != nil {
		return false, "", err
	}
	switch {
	case o == nil:
		return false, "unknown obligation", nil
	case o.State != ObligationActive:
		return false, "obligation is " + o.State + ", not active", nil
	case !strings.EqualFold(o.Bearer, by):
		return false, "only the obligation's bearer can discharge it", nil
	}
	replyID, answered := d.answeredBy(project, o.Bearer, o.MessageID, o.CreatedAt)
	if !answered {
		return false, "predicate message_answered is false: no reply from " + o.Bearer + " reaches message " + o.MessageID, nil
	}
	ev, _ := json.Marshal(map[string]string{"by": by, "evidence": evidence, "reply": replyID})
	res, err := d.writerExec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = ? WHERE id = ? AND state = ?`,
		ObligationFulfilled, now.UTC().Format(memoryTimeFmt), string(ev), id, ObligationActive)
	if err != nil {
		return false, "", fmt.Errorf("answer discharge: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, "obligation moved concurrently", nil
	}
	return true, "", nil
}

// DeclineAnswerObligation is the bearer's early breach: active -> unfulfilled
// with the reason class stored. The escalation it triggers is slice B.
func (d *DB) DeclineAnswerObligation(project, id, reasonClass string, now time.Time) (bool, error) {
	if !DeclineReasons[reasonClass] {
		return false, fmt.Errorf("decline: %q is not a reason class", reasonClass)
	}
	res, err := d.writerExec(`UPDATE obligations SET state = ?, closed_at = ?, decline_reason_class = ?
		WHERE id = ? AND project = ? AND subject_kind = ? AND state = ?`,
		ObligationUnfulfilled, now.UTC().Format(memoryTimeFmt), reasonClass, id, project, SubjectMessage, ObligationActive)
	if err != nil {
		return false, fmt.Errorf("answer decline: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MyAnswerObligations lists the active answer obligations agent bears in
// project, for obligations_mine. Read-only.
func (d *DB) MyAnswerObligations(project, agent string) ([]ObligationView, error) {
	rows, err := d.ro().Query(`SELECT o.id, o.norm_id, n.what, o.subject_kind, o.subject_id,
			COALESCE(m.subject, ''), o.bearer_kind, o.escalation_depth, COALESCE(o.deadline_at, '')
		FROM obligations o JOIN norms n ON n.id = o.norm_id LEFT JOIN messages m ON m.id = o.subject_id
		WHERE o.project = ? AND o.state = ? AND o.subject_kind = ? AND o.bearer = ?
		ORDER BY o.created_at, o.id`, project, ObligationActive, SubjectMessage, strings.ToLower(agent))
	if err != nil {
		return nil, fmt.Errorf("my answer obligations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ObligationView
	for rows.Next() {
		var v ObligationView
		if err := rows.Scan(&v.ID, &v.Norm, &v.What, &v.SubjectKind, &v.SubjectID, &v.Subject, &v.BearerKind, &v.Depth, &v.Deadline); err != nil {
			return nil, fmt.Errorf("scan answer obligation: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
