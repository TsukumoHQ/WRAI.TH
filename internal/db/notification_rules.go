package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// deliveryLogCap bounds the notification_deliveries table. On each insert we
// prune the oldest rows beyond this many entries (global, newest-kept).
const deliveryLogCap = 1000

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CreateNotificationRule inserts a new rule and returns it.
func (d *DB) CreateNotificationRule(r *models.NotificationRule) (*models.NotificationRule, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	if r.Project == "" {
		r.Project = "default"
	}
	if r.Match == "" {
		r.Match = "{}"
	}
	if r.Opts == "" {
		r.Opts = "{}"
	}
	r.CreatedAt = now
	r.UpdatedAt = now

	_, err := d.conn.Exec(`
		INSERT INTO notification_rules (id, project, name, enabled, event, match_json, action, target, opts_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Project, r.Name, boolToInt(r.Enabled), r.Event, r.Match, r.Action, r.Target, r.Opts, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create notification rule: %w", err)
	}
	return r, nil
}

// ListNotificationRules returns all rules for a project ordered by creation.
func (d *DB) ListNotificationRules(project string) ([]models.NotificationRule, error) {
	rows, err := d.ro().Query(`
		SELECT id, project, name, enabled, event, match_json, action, target, opts_json, created_at, updated_at
		FROM notification_rules WHERE project = ? ORDER BY created_at`, project)
	if err != nil {
		return nil, fmt.Errorf("list notification rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanNotificationRules(rows)
}

// ListEnabledNotificationRulesForEvent returns enabled rules matching the event,
// across all projects (the evaluator filters by event project at fire time).
func (d *DB) ListEnabledNotificationRulesForEvent(event string) ([]models.NotificationRule, error) {
	rows, err := d.ro().Query(`
		SELECT id, project, name, enabled, event, match_json, action, target, opts_json, created_at, updated_at
		FROM notification_rules WHERE enabled = 1 AND event = ?`, event)
	if err != nil {
		return nil, fmt.Errorf("list enabled rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanNotificationRules(rows)
}

// ListAllEnabledNotificationRules returns every enabled rule (used by the digest
// scheduler to discover configured intervals).
func (d *DB) ListAllEnabledNotificationRules() ([]models.NotificationRule, error) {
	rows, err := d.ro().Query(`
		SELECT id, project, name, enabled, event, match_json, action, target, opts_json, created_at, updated_at
		FROM notification_rules WHERE enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("list all enabled rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanNotificationRules(rows)
}

func scanNotificationRules(rows *sql.Rows) ([]models.NotificationRule, error) {
	var out []models.NotificationRule
	for rows.Next() {
		var r models.NotificationRule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Project, &r.Name, &enabled, &r.Event, &r.Match, &r.Action, &r.Target, &r.Opts, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetNotificationRule returns a single rule by ID.
func (d *DB) GetNotificationRule(id string) (*models.NotificationRule, error) {
	var r models.NotificationRule
	var enabled int
	err := d.ro().QueryRow(`
		SELECT id, project, name, enabled, event, match_json, action, target, opts_json, created_at, updated_at
		FROM notification_rules WHERE id = ?`, id).
		Scan(&r.ID, &r.Project, &r.Name, &enabled, &r.Event, &r.Match, &r.Action, &r.Target, &r.Opts, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get notification rule: %w", err)
	}
	r.Enabled = enabled != 0
	return &r, nil
}

// PatchNotificationRule applies a partial update. Nil fields are left unchanged.
func (d *DB) PatchNotificationRule(id string, name, event, match, action, target, opts *string, enabled *bool) (*models.NotificationRule, error) {
	existing, err := d.GetNotificationRule(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("rule not found")
	}
	if name != nil {
		existing.Name = *name
	}
	if event != nil {
		existing.Event = *event
	}
	if match != nil {
		existing.Match = *match
	}
	if action != nil {
		existing.Action = *action
	}
	if target != nil {
		existing.Target = *target
	}
	if opts != nil {
		existing.Opts = *opts
	}
	if enabled != nil {
		existing.Enabled = *enabled
	}
	existing.UpdatedAt = time.Now().UTC().Format(memoryTimeFmt)

	_, err = d.conn.Exec(`
		UPDATE notification_rules
		SET name = ?, enabled = ?, event = ?, match_json = ?, action = ?, target = ?, opts_json = ?, updated_at = ?
		WHERE id = ?`,
		existing.Name, boolToInt(existing.Enabled), existing.Event, existing.Match, existing.Action, existing.Target, existing.Opts, existing.UpdatedAt, id)
	if err != nil {
		return nil, fmt.Errorf("patch notification rule: %w", err)
	}
	return existing, nil
}

// DeleteNotificationRule removes a rule by ID.
func (d *DB) DeleteNotificationRule(id string) error {
	_, err := d.conn.Exec("DELETE FROM notification_rules WHERE id = ?", id)
	return err
}

// CountNotificationRules returns the total number of rules (used for first-run seed).
func (d *DB) CountNotificationRules() (int, error) {
	var n int
	err := d.ro().QueryRow("SELECT COUNT(*) FROM notification_rules").Scan(&n)
	return n, err
}

// DigestStats holds the coalesced cycle digest counts for one project.
type DigestStats struct {
	Project    string `json:"project"`
	CycleName  string `json:"cycle_name"`
	Done       int    `json:"done"`
	Total      int    `json:"total"`
	Blocked    int    `json:"blocked"`
	InReview   int    `json:"in_review"`
	InProgress int    `json:"in_progress"`
}

// ComputeDigestStats computes digest counts for a project from the tasks mirror.
//
// Native mode has no "in-review" status; in-review is derived from started,
// not-yet-done tasks (the closest signal to "PR up / awaiting review"). When a
// single cycle is defined for the project its name is used as the digest label.
func (d *DB) ComputeDigestStats(project string) (*DigestStats, error) {
	s := &DigestStats{Project: project}
	rows, err := d.ro().Query(`
		SELECT status, COUNT(*)
		FROM tasks
		WHERE project = ? AND archived_at IS NULL AND status != 'cancelled'
		GROUP BY status`, project)
	if err != nil {
		return nil, fmt.Errorf("compute digest stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		s.Total += n
		switch status {
		case "done":
			s.Done += n
		case "blocked":
			s.Blocked += n
		case "in-progress":
			s.InProgress += n
			// in-progress doubles as the "in review" proxy in native mode.
			s.InReview += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Label: use the sole cycle name if exactly one exists for the project.
	cycles, _ := d.ListCycles(project)
	if len(cycles) == 1 {
		s.CycleName = cycles[0].Name
	}
	if s.CycleName == "" {
		s.CycleName = project
	}
	return s, nil
}

// ProjectsWithTasks returns the distinct project names that currently have any
// non-archived tasks (used by the digest scheduler to emit one event per project).
func (d *DB) ProjectsWithTasks() ([]string, error) {
	rows, err := d.ro().Query(`SELECT DISTINCT project FROM tasks WHERE archived_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LogNotificationDelivery records a delivery outcome and prunes the log to the cap.
func (d *DB) LogNotificationDelivery(rec *models.NotificationDelivery) error {
	if rec.ID == "" {
		rec.ID = uuid.New().String()
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(memoryTimeFmt)
	}
	if rec.Project == "" {
		rec.Project = "default"
	}
	_, err := d.conn.Exec(`
		INSERT INTO notification_deliveries (id, project, rule_id, rule_name, event, action, target, outcome, status_code, error, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Project, rec.RuleID, rec.RuleName, rec.Event, rec.Action, rec.Target, rec.Outcome, rec.StatusCode, rec.Error, rec.Payload, rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("log notification delivery: %w", err)
	}
	// Prune to cap: keep the newest deliveryLogCap rows.
	_, _ = d.conn.Exec(`
		DELETE FROM notification_deliveries
		WHERE id NOT IN (
			SELECT id FROM notification_deliveries ORDER BY created_at DESC LIMIT ?
		)`, deliveryLogCap)
	return nil
}

// ListNotificationDeliveries returns the most recent delivery-log entries.
func (d *DB) ListNotificationDeliveries(limit int) ([]models.NotificationDelivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.ro().Query(`
		SELECT id, project, rule_id, rule_name, event, action, target, outcome, status_code, error, payload, created_at
		FROM notification_deliveries ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list notification deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []models.NotificationDelivery
	for rows.Next() {
		var r models.NotificationDelivery
		if err := rows.Scan(&r.ID, &r.Project, &r.RuleID, &r.RuleName, &r.Event, &r.Action, &r.Target, &r.Outcome, &r.StatusCode, &r.Error, &r.Payload, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
