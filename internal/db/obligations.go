package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Obligations engine, slice 1 (DEC-wraith-obligations-1, design 50b4b349).
// Norms are seeded rows whose predicates are closed Go enums: each value below
// maps to a hand-written, SQL-bounded predicate, never to a user expression.
// Slice 1 carries only the two ACK norms and reproduces the legacy ACK checker
// byte-for-byte, quirks Q1-Q4 included.

// Norm ids seeded by migrate.
const (
	NormAckEscalate = "ack.escalate"
	NormAckNotify   = "ack.notify"
)

// Closed enums (slice 1 values only; a new value is a code change + review).
// The engine runs a norm only when every one of its trigger / what / while /
// bearer values is one of these (ackNormKnown); any other row is inert.
const (
	SubjectTask               = "task"
	TriggerTaskPendingUnclaim = "task_pending_unclaimed"
	WhatTaskLeftPending       = "task_left_pending"
	WhileTaskPendingLive      = "task_pending_live"
	BearerAssigneeProfile     = "assignee_profile"
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

// ackMarkColumn maps an ACK norm to the legacy tasks column its transition
// CASes. Whitelist: the column name is spliced into SQL.
var ackMarkColumn = map[string]string{
	NormAckEscalate: "ack_escalated_at",
	NormAckNotify:   "ack_notified_at",
}

// ackCandidate is task_pending_unclaimed / task_pending_live: exactly the
// GetUnackedTasks predicate the legacy checker reads each tick.
const ackCandidate = `t.status = 'pending' AND t.archived_at IS NULL AND t.dispatched_at < ?
	AND (t.run_state IS NULL OR t.run_state = '')`

// TaskObligation is an active ACK obligation joined to the task fields the
// sweeper needs to decide and word its sanction.
type TaskObligation struct {
	ID               string
	NormID           string
	SanctionTemplate string
	TaskID           string
	Project          string
	Title            string
	ProfileSlug      string
	DispatchedBy     string
	DispatchedAt     string
}

func bindingsHash(normID, subjectKind, subjectID string, depth int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", normID, subjectKind, subjectID, depth)))
	return hex.EncodeToString(sum[:])
}

// InstantiateTaskAck opens the ACK obligations of every task the legacy
// checker would read at this cutoff (now - ack_notify_age) that has none yet.
// A norm whose legacy mark is already set opens pre-closed as unfulfilled, so
// historical marks never re-fire. Writes only when a task is newly eligible.
func (d *DB) InstantiateTaskAck(cutoff, now time.Time) (int, error) {
	rows, err := d.ro().Query(`SELECT n.id, n.version, n.bearer_kind, t.id, t.project, COALESCE(t.profile_slug, ''),
			t.ack_notified_at, t.ack_escalated_at
		FROM tasks t JOIN norms n ON `+ackNormKnown+` AND (n.project IS NULL OR n.project = t.project)
		WHERE `+ackCandidate+`
		  AND NOT EXISTS (SELECT 1 FROM obligations o WHERE o.norm_id = n.id AND o.subject_kind = ? AND o.subject_id = t.id)`,
		cutoff.UTC().Format(memoryTimeFmt), SubjectTask)
	if err != nil {
		return 0, fmt.Errorf("obligation candidates: %w", err)
	}
	type pending struct {
		normID, bearerKind, taskID, project, profile string
		version                                      int
		mark                                         sql.NullString
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var notified, escalated sql.NullString
		if err := rows.Scan(&p.normID, &p.version, &p.bearerKind, &p.taskID, &p.project, &p.profile, &notified, &escalated); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan obligation candidate: %w", err)
		}
		p.mark = notified
		if p.normID == NormAckEscalate {
			p.mark = escalated
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
		state, closedAt := ObligationActive, sql.NullString{}
		if p.mark.Valid {
			state, closedAt = ObligationUnfulfilled, p.mark
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO obligations
			(id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state, created_at, closed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), p.project, p.normID, p.version, bindingsHash(p.normID, SubjectTask, p.taskID, 0),
			SubjectTask, p.taskID, p.bearerKind, p.profile, state, ts, closedAt)
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
// legacy checker would read at this cutoff, per task in norm eval_order, so
// the sweeper can reproduce its if/else-if (one sanction per task per tick).
func (d *DB) ActiveTaskObligations(cutoff time.Time) ([]TaskObligation, error) {
	rows, err := d.ro().Query(`SELECT o.id, o.norm_id, n.sanction_template, t.id, t.project, COALESCE(t.title, ''),
			COALESCE(t.profile_slug, ''), t.dispatched_by, t.dispatched_at
		FROM obligations o
		JOIN norms n ON n.id = o.norm_id AND `+ackNormKnown+`
		JOIN tasks t ON t.id = o.subject_id
		WHERE o.state = ? AND o.subject_kind = ? AND `+ackCandidate+`
		ORDER BY t.dispatched_at, t.id, n.eval_order`,
		ObligationActive, SubjectTask, cutoff.UTC().Format(memoryTimeFmt))
	if err != nil {
		return nil, fmt.Errorf("active obligations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TaskObligation
	for rows.Next() {
		var o TaskObligation
		if err := rows.Scan(&o.ID, &o.NormID, &o.SanctionTemplate, &o.TaskID, &o.Project, &o.Title,
			&o.ProfileSlug, &o.DispatchedBy, &o.DispatchedAt); err != nil {
			return nil, fmt.Errorf("scan active obligation: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// TransitionTaskAck moves an active ACK obligation to unfulfilled and sets its
// legacy tasks mark, in one writer tx. Both are CASes: the obligation must
// still be active, and the task must pass the exact legacy guard (pending, not
// a run container, mark unset). If either matches 0 rows nothing lands and ok
// is false — the caller must not fire the sanction.
func (d *DB) TransitionTaskAck(obligationID, normID, taskID string, deadline, now time.Time) (ok bool, err error) {
	col, known := ackMarkColumn[normID]
	if !known {
		return false, fmt.Errorf("transition: %q is not an ACK norm", normID)
	}
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, fmt.Errorf("transition begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(memoryTimeFmt)
	res, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, deadline_at = ? WHERE id = ? AND state = ?`,
		ObligationUnfulfilled, ts, deadline.UTC().Format(memoryTimeFmt), obligationID, ObligationActive)
	if err != nil {
		return false, fmt.Errorf("transition obligation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	res, err = tx.Exec(`UPDATE tasks SET `+col+` = ? WHERE id = ? AND status = 'pending'
		AND (run_state IS NULL OR run_state = '') AND `+col+` IS NULL`, ts, taskID)
	if err != nil {
		return false, fmt.Errorf("transition legacy mark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
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
