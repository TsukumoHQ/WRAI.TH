package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"agent-relay/internal/models"
)

// Parked tasks and founder gates (task d43d844e, relay half of c373a9ad).
//
// A parked task is a pending task held on purpose: it keeps an open task_holds
// row (reason 'parked'), so the ACK ladder skips it like any held task and no
// rung (ack.notify / escalate / manager / human) ever fires on it. Parked until
// a task = a blocked_by edge tagged parked plus the hold: the existing settle
// releases it (and restarts its ACK clock) when that task is done. Parked until
// the founder = a hold nothing releases but unpark (or the task leaving
// pending); readyPredicate treats it as not ready, so claim next skips it.
//
// A founder gate is a pending task waiting on the human: profile human/user, or
// parked until the founder. Each entry into a gate opens one founder_gates row
// and alerts the founder exactly once (alerted_at CAS); the ACK ladder never
// runs on it.

// HoldReasonParked is task_holds.reason for a parked task (vs 'blocked_by').
const HoldReasonParked = "parked"

// ParkUntilFounder is the park_until value of a task parked on the founder.
const ParkUntilFounder = "founder"

// CodeTaskNotParked — unpark on a task that has no open park.
const CodeTaskNotParked = "TASK_NOT_PARKED"

// CodeTaskNotPending — park on a task that is not pending.
const CodeTaskNotPending = "TASK_NOT_PENDING"

// founderGateTask is true for tasks row t sitting in a founder gate.
const founderGateTask = `t.status = 'pending' AND t.archived_at IS NULL AND (LOWER(COALESCE(t.profile_slug, '')) IN ('human', 'user')
	OR EXISTS (SELECT 1 FROM task_holds fh WHERE fh.task_id = t.id AND fh.released_at IS NULL AND fh.park_until = '` + ParkUntilFounder + `'))`

func migrateParking(conn *sql.DB) {
	ensureColumns(conn, "task_holds", map[string]string{
		"park_until":  "TEXT",
		"park_reason": "TEXT",
		"parked_by":   "TEXT",
	})
	_, missing := conn.Exec(`SELECT 1 FROM founder_gates LIMIT 0`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS founder_gates (
		id         TEXT PRIMARY KEY,
		task_id    TEXT NOT NULL,
		project    TEXT NOT NULL,
		reason     TEXT NOT NULL,
		entered_at TEXT NOT NULL,
		alerted_at TEXT,
		closed_at  TEXT
	)`)
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_founder_gates_open ON founder_gates(task_id) WHERE closed_at IS NULL`)
	if missing != nil {
		// Human tasks already waiting when the table lands were paged by the
		// ladder before: seed their gates as alerted so the first sweep does
		// not burst one founder alert per old task (the digest still lists them).
		_, _ = conn.Exec(`INSERT OR IGNORE INTO founder_gates (id, task_id, project, reason, entered_at, alerted_at)
			SELECT lower(hex(randomblob(16))), t.id, t.project, 'profile ' || t.profile_slug,
				COALESCE(t.pending_since, t.dispatched_at), COALESCE(t.pending_since, t.dispatched_at)
			FROM tasks t WHERE ` + founderGateTask)
	}
}

// ParkTask parks a pending task until the founder (until == "founder") or
// until another task of the project reaches untilStatus (until = its full id;
// untilStatus "in-review", else done). reason is required. Parking an already
// parked task re-parks it with the new until. fresh is false only on a re-park
// with the same reason (task fea65594: that one is not announced again).
func (d *DB) ParkTask(project, taskID, by, reason, until, untilStatus string) (fresh bool, err error) {
	if reason == "" || until == "" {
		return false, newTaskError(CodeEdgeInvalidArgument, "park needs a reason and until (founder or a task id)")
	}
	if untilStatus != "" && untilStatus != "done" && untilStatus != "in-review" {
		return false, newTaskError(CodeEdgeInvalidArgument, "park until status must be done or in-review, got %q", untilStatus)
	}
	now := d.Now().UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	err = tx.QueryRow(`SELECT status FROM tasks WHERE id = ? AND project = ? AND archived_at IS NULL`, taskID, project).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, newTaskError(CodeTaskNotFound, "task %s not found in project %s", taskID, project)
	}
	if err != nil {
		return false, err
	}
	if status != "pending" {
		return false, newTaskError(CodeTaskNotPending, "only a pending task can be parked (task %s is %s)", taskID, status)
	}
	var priorReason string
	switch err := tx.QueryRow(`SELECT COALESCE(park_reason, '') FROM task_holds WHERE task_id = ? AND released_at IS NULL AND reason = ?`,
		taskID, HoldReasonParked).Scan(&priorReason); {
	case errors.Is(err, sql.ErrNoRows):
		fresh = true
	case err != nil:
		return false, err
	default:
		fresh = priorReason != reason
	}
	// A re-park drops the previous park's edge first, so until is exactly one thing.
	if _, err := tx.Exec(`UPDATE org_edges SET removed_at = ?, removed_by = ? WHERE src_kind = 'task' AND src_id = ?
		AND removed_at IS NULL AND json_extract(metadata, '$.parked') = 1`, now, by, taskID); err != nil {
		return false, err
	}
	// Parked work pages nobody again: its open ACK obligations close inactive
	// in this same tx (ruling wraith-park-ruling, P2).
	if _, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = '{"parked":true}'
		WHERE subject_kind = ? AND subject_id = ? AND state = ? AND norm_id LIKE 'ack.%'`,
		ObligationInactive, now, SubjectTask, taskID, ObligationActive); err != nil {
		return false, err
	}
	if until == ParkUntilFounder {
		_, err = tx.Exec(`INSERT INTO task_holds (task_id, project, reason, held_at, park_until, park_reason, parked_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(task_id) DO UPDATE SET reason = excluded.reason, park_until = excluded.park_until,
				park_reason = excluded.park_reason, parked_by = excluded.parked_by,
				held_at = CASE WHEN task_holds.released_at IS NULL THEN task_holds.held_at ELSE excluded.held_at END,
				released_at = NULL, flagged = NULL, announced_at = NULL`,
			taskID, project, HoldReasonParked, now, ParkUntilFounder, reason, by)
		if err != nil {
			return false, err
		}
		return fresh, tx.Commit()
	}
	var untilState string
	err = tx.QueryRow(`SELECT status FROM tasks WHERE id = ? AND project = ?`, until, project).Scan(&untilState)
	if errors.Is(err, sql.ErrNoRows) {
		return false, newTaskError(CodeTaskNotFound, "until task %s not found in project %s", until, project)
	}
	if err != nil {
		return false, err
	}
	if untilState == "done" || (untilStatus == "in-review" && untilState == "in-review") {
		return false, newTaskError(CodeEdgeInvalidArgument, "task %s is already %s: nothing to park %s on", until, untilState, taskID)
	}
	// Until a task: the founder hold (if any) stops counting, then the
	// blocked_by edge opens (or keeps) the hold.
	if _, err := tx.Exec(`UPDATE task_holds SET park_until = NULL WHERE task_id = ? AND released_at IS NULL`, taskID); err != nil {
		return false, err
	}
	meta := map[string]any{"parked": true}
	if untilStatus == "in-review" {
		meta["until"] = untilStatus
	}
	if err := d.addEdgeTx(tx, project, EdgeInput{SrcKind: "task", SrcID: taskID, Type: EdgeBlockedBy, DstKind: "task", DstID: until,
		Metadata: meta, CreatedBy: by}, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE task_holds SET reason = ?, park_until = ?, park_reason = ?, parked_by = ?
		WHERE task_id = ? AND released_at IS NULL`, HoldReasonParked, until, reason, by, taskID); err != nil {
		return false, err
	}
	return fresh, tx.Commit()
}

// UnparkTask lifts the park of a task: its park edge is removed and the hold
// settles like any other (released, ACK clock restarted, when no other
// prerequisite still blocks it). released reports whether the hold released
// now, so the caller announces it.
func (d *DB) UnparkTask(project, taskID, by string) (released bool, err error) {
	now := d.Now().UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE task_holds SET reason = 'blocked_by', park_until = NULL, park_reason = NULL, parked_by = NULL
		WHERE task_id = ? AND project = ? AND released_at IS NULL AND reason = ?`, taskID, project, HoldReasonParked)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, newTaskError(CodeTaskNotParked, "task %s is not parked", taskID)
	}
	if _, err := tx.Exec(`UPDATE org_edges SET removed_at = ?, removed_by = ? WHERE src_kind = 'task' AND src_id = ?
		AND removed_at IS NULL AND json_extract(metadata, '$.parked') = 1`, now, by, taskID); err != nil {
		return false, err
	}
	if released, err = settleTaskTx(tx, project, taskID, now); err != nil {
		return false, err
	}
	return released, tx.Commit()
}

// TaskParked reports whether the task has an open park.
func (d *DB) TaskParked(project, taskID string) bool {
	var one int
	return d.ro().QueryRow(`SELECT 1 FROM task_holds WHERE task_id = ? AND project = ? AND released_at IS NULL AND reason = ?`,
		taskID, project, HoldReasonParked).Scan(&one) == nil
}

// AttachParks fills Park on every task of the slice that has an open park.
func (d *DB) AttachParks(project string, tasks []models.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	rows, err := d.ro().Query(`SELECT task_id, COALESCE(park_until, ''), COALESCE(park_reason, ''), COALESCE(parked_by, ''), held_at
		FROM task_holds WHERE project = ? AND released_at IS NULL AND reason = ?`, project, HoldReasonParked)
	if err != nil {
		return fmt.Errorf("parks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	parks := map[string]*models.TaskPark{}
	for rows.Next() {
		var id string
		var p models.TaskPark
		if err := rows.Scan(&id, &p.Until, &p.Reason, &p.ParkedBy, &p.Since); err != nil {
			return err
		}
		parks[id] = &p
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range tasks {
		tasks[i].Park = parks[tasks[i].ID]
	}
	return nil
}

// FounderGate is one open founder gate, for the alert and the digest.
type FounderGate struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	Project    string `json:"project"`
	Title      string `json:"title"`
	Reason     string `json:"reason"`
	EnteredAt  string `json:"entered_at"`
	AgeSeconds int64  `json:"age_seconds"`
	alerted    bool
}

// SyncFounderGates closes the gates whose task left them and opens one gate
// per task that entered one, then returns every open gate not yet alerted.
func (d *DB) SyncFounderGates(now time.Time) ([]FounderGate, error) {
	ts := now.UTC().Format(memoryTimeFmt)
	// Read first, write only when a gate closes (single writer, one tick a minute).
	var leaving int
	if err := d.ro().QueryRow(`SELECT COUNT(*) FROM founder_gates g WHERE g.closed_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = g.task_id AND ` + founderGateTask + `)`).Scan(&leaving); err != nil {
		return nil, fmt.Errorf("founder gates leaving: %w", err)
	}
	if leaving > 0 {
		if _, err := d.writerExec(`UPDATE founder_gates SET closed_at = ? WHERE closed_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = founder_gates.task_id AND `+founderGateTask+`)`, ts); err != nil {
			return nil, fmt.Errorf("close founder gates: %w", err)
		}
	}
	rows, err := d.ro().Query(`SELECT t.id, t.project, COALESCE((SELECT h.park_reason FROM task_holds h
			WHERE h.task_id = t.id AND h.released_at IS NULL AND h.park_until = ?), 'profile ' || t.profile_slug)
		FROM tasks t WHERE `+founderGateTask+`
		  AND NOT EXISTS (SELECT 1 FROM founder_gates g WHERE g.task_id = t.id AND g.closed_at IS NULL)`, ParkUntilFounder)
	if err != nil {
		return nil, fmt.Errorf("founder gate candidates: %w", err)
	}
	var entering [][3]string
	for rows.Next() {
		var e [3]string
		if err := rows.Scan(&e[0], &e[1], &e[2]); err != nil {
			_ = rows.Close()
			return nil, err
		}
		entering = append(entering, e)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, e := range entering {
		// The partial unique index makes a concurrent double-open a no-op.
		if _, err := d.writerExec(`INSERT OR IGNORE INTO founder_gates (id, task_id, project, reason, entered_at) VALUES (?, ?, ?, ?, ?)`,
			uuid.NewString(), e[0], e[1], e[2], ts); err != nil {
			return nil, fmt.Errorf("open founder gate: %w", err)
		}
	}
	gates, err := d.founderGates("", now)
	if err != nil {
		return nil, err
	}
	var unalerted []FounderGate
	for _, g := range gates {
		if !g.alerted {
			unalerted = append(unalerted, g)
		}
	}
	return unalerted, nil
}

// MarkFounderGateAlerted claims a gate's single alert: true exactly once.
func (d *DB) MarkFounderGateAlerted(id string, now time.Time) (bool, error) {
	res, err := d.writerExec(`UPDATE founder_gates SET alerted_at = ? WHERE id = ? AND alerted_at IS NULL AND closed_at IS NULL`,
		now.UTC().Format(memoryTimeFmt), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// OpenFounderGates is the digest: the project's open founder gates, oldest
// first, with their age at now.
func (d *DB) OpenFounderGates(project string, now time.Time) ([]FounderGate, error) {
	return d.founderGates(project, now)
}

func (d *DB) founderGates(project string, now time.Time) ([]FounderGate, error) {
	rows, err := d.ro().Query(`SELECT g.id, g.task_id, g.project, COALESCE(t.title, ''), g.reason, g.entered_at, g.alerted_at IS NOT NULL
		FROM founder_gates g LEFT JOIN tasks t ON t.id = g.task_id
		WHERE g.closed_at IS NULL AND (? = '' OR g.project = ?) ORDER BY g.entered_at, g.task_id`, project, project)
	if err != nil {
		return nil, fmt.Errorf("founder gates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FounderGate
	for rows.Next() {
		var g FounderGate
		if err := rows.Scan(&g.ID, &g.TaskID, &g.Project, &g.Title, &g.Reason, &g.EnteredAt, &g.alerted); err != nil {
			return nil, err
		}
		if at, err := time.Parse(memoryTimeFmt, g.EnteredAt); err == nil {
			g.AgeSeconds = int64(now.Sub(at).Seconds())
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
