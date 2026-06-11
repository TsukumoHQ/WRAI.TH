package db

import (
	"time"

	"github.com/google/uuid"
)

// Trigger represents an event-driven spawn rule.
type Trigger struct {
	ID              string `json:"id"`
	Project         string `json:"project"`
	Event           string `json:"event"`        // task_pending, pr_opened, message_received
	MatchRules      string `json:"match_rules"`  // JSON: {"profile": "backend-lead", "age": ">30s"}
	ProfileSlug     string `json:"profile_slug"` // which profile to spawn
	Cycle           string `json:"cycle"`        // which cycle to run
	MaxDuration     string `json:"max_duration"`
	Enabled         bool   `json:"enabled"`
	CooldownSeconds int    `json:"cooldown_seconds"`
	LastFiredAt     string `json:"last_fired_at,omitempty"`
}

// TriggerFire records a single trigger firing event.
type TriggerFire struct {
	ID        string `json:"id"`
	TriggerID string `json:"trigger_id"`
	Project   string `json:"project"`
	Event     string `json:"event"`
	ChildID   string `json:"child_id,omitempty"`
	Error     string `json:"error,omitempty"`
	FiredAt   string `json:"fired_at"`
}

// UpsertTrigger creates or updates a trigger.
// cooldownSeconds: nil uses the default (60s); *0 means no cooldown.
func (d *DB) UpsertTrigger(project, event, matchRules, profileSlug, cycle, maxDuration string, cooldownSeconds *int) (*Trigger, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	id := uuid.New().String()
	if maxDuration == "" {
		maxDuration = "10m"
	}
	if matchRules == "" {
		matchRules = "{}"
	}
	cooldown := 60
	if cooldownSeconds != nil {
		cooldown = *cooldownSeconds
		if cooldown < 0 {
			cooldown = 0
		}
	}

	_, err := d.conn.Exec(`
		INSERT INTO triggers (id, project, event, match_rules, profile_slug, cycle, max_duration, cooldown_seconds, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, project, event, matchRules, profileSlug, cycle, maxDuration, cooldown, now, now)
	if err != nil {
		return nil, err
	}

	return &Trigger{
		ID:              id,
		Project:         project,
		Event:           event,
		MatchRules:      matchRules,
		ProfileSlug:     profileSlug,
		Cycle:           cycle,
		MaxDuration:     maxDuration,
		Enabled:         true,
		CooldownSeconds: cooldown,
	}, nil
}

// ListTriggers returns all enabled triggers for a project and event type.
func (d *DB) ListTriggers(project, event string) []Trigger {
	query := `SELECT id, project, event, match_rules, profile_slug, cycle, max_duration, enabled,
		COALESCE(cooldown_seconds, 60), COALESCE(last_fired_at, '')
		FROM triggers WHERE project = ? AND enabled = 1`
	args := []any{project}

	if event != "" {
		query += " AND event = ?"
		args = append(args, event)
	}
	query += " ORDER BY created_at LIMIT 500"

	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []Trigger
	for rows.Next() {
		var t Trigger
		if err := rows.Scan(&t.ID, &t.Project, &t.Event, &t.MatchRules, &t.ProfileSlug, &t.Cycle, &t.MaxDuration, &t.Enabled, &t.CooldownSeconds, &t.LastFiredAt); err != nil {
			continue
		}
		result = append(result, t)
	}
	return result
}

// ListAllTriggers returns all triggers for a project (including disabled).
func (d *DB) ListAllTriggers(project string) []Trigger {
	rows, err := d.ro().Query(`SELECT id, project, event, match_rules, profile_slug, cycle, max_duration, enabled,
		COALESCE(cooldown_seconds, 60), COALESCE(last_fired_at, '')
		FROM triggers WHERE project = ? ORDER BY event, created_at`, project)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []Trigger
	for rows.Next() {
		var t Trigger
		if err := rows.Scan(&t.ID, &t.Project, &t.Event, &t.MatchRules, &t.ProfileSlug, &t.Cycle, &t.MaxDuration, &t.Enabled, &t.CooldownSeconds, &t.LastFiredAt); err != nil {
			continue
		}
		result = append(result, t)
	}
	return result
}

// DeleteTrigger removes a trigger by ID.
func (d *DB) DeleteTrigger(id string) {
	_, _ = d.conn.Exec("DELETE FROM triggers WHERE id = ?", id)
}

// GetTrigger returns a single trigger by ID, or nil if not found.
func (d *DB) GetTrigger(id string) *Trigger {
	var t Trigger
	var enabled int
	err := d.ro().QueryRow(`SELECT id, project, event, match_rules, profile_slug, cycle, max_duration, enabled,
		COALESCE(cooldown_seconds, 60), COALESCE(last_fired_at, '')
		FROM triggers WHERE id = ?`, id).Scan(
		&t.ID, &t.Project, &t.Event, &t.MatchRules, &t.ProfileSlug, &t.Cycle, &t.MaxDuration, &enabled,
		&t.CooldownSeconds, &t.LastFiredAt,
	)
	if err != nil {
		return nil
	}
	t.Enabled = enabled == 1
	return &t
}

// UpdateTriggerFields applies a partial update. Any non-nil field overwrites the
// existing value. Used by PUT /api/triggers/{id}.
func (d *DB) UpdateTriggerFields(id string, matchRules, profileSlug, cycle, maxDuration *string, enabled *bool, cooldownSeconds *int) (*Trigger, error) {
	existing := d.GetTrigger(id)
	if existing == nil {
		return nil, nil
	}
	if matchRules != nil {
		existing.MatchRules = *matchRules
	}
	if profileSlug != nil {
		existing.ProfileSlug = *profileSlug
	}
	if cycle != nil {
		existing.Cycle = *cycle
	}
	if maxDuration != nil {
		existing.MaxDuration = *maxDuration
	}
	if enabled != nil {
		existing.Enabled = *enabled
	}
	if cooldownSeconds != nil {
		existing.CooldownSeconds = *cooldownSeconds
		if existing.CooldownSeconds < 0 {
			existing.CooldownSeconds = 0
		}
	}
	now := time.Now().UTC().Format(memoryTimeFmt)
	enabledInt := 0
	if existing.Enabled {
		enabledInt = 1
	}
	_, err := d.conn.Exec(
		`UPDATE triggers SET match_rules = ?, profile_slug = ?, cycle = ?, max_duration = ?, enabled = ?, cooldown_seconds = ?, updated_at = ? WHERE id = ?`,
		existing.MatchRules, existing.ProfileSlug, existing.Cycle, existing.MaxDuration, enabledInt, existing.CooldownSeconds, now, id,
	)
	if err != nil {
		return nil, err
	}
	return existing, nil
}

// ToggleTrigger enables or disables a trigger.
func (d *DB) ToggleTrigger(id string, enabled bool) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	e := 0
	if enabled {
		e = 1
	}
	_, _ = d.conn.Exec("UPDATE triggers SET enabled = ?, updated_at = ? WHERE id = ?", e, now, id)
}

// ClaimTriggerFire atomically reserves a fire slot for the given trigger.
// Returns true if the caller wins the cooldown race (last_fired_at is
// updated to now), false if another goroutine already claimed the slot
// within the cooldown window.
//
// Used by the dispatcher to prevent the burst-race where N concurrent
// goroutines all pass a stale in-memory cooldown check and fire N times.
func (d *DB) ClaimTriggerFire(triggerID string, cooldownSeconds int) bool {
	now := time.Now().UTC()
	nowStr := now.Format(memoryTimeFmt)
	threshold := now.Add(-time.Duration(cooldownSeconds) * time.Second).Format(memoryTimeFmt)
	// Only update if last_fired_at is NULL or older than the cooldown window.
	res, err := d.conn.Exec(
		`UPDATE triggers SET last_fired_at = ?
		 WHERE id = ?
		   AND (last_fired_at IS NULL OR last_fired_at = '' OR last_fired_at < ?)`,
		nowStr, triggerID, threshold,
	)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// RecordTriggerFire logs a trigger firing event and updates last_fired_at.
func (d *DB) RecordTriggerFire(triggerID, project, event, childID string, err error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	id := uuid.New().String()

	errStr := ""
	if err != nil {
		errStr = err.Error()
	}

	_, _ = d.conn.Exec(`INSERT INTO trigger_history (id, trigger_id, project, event, child_id, error, fired_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, triggerID, project, event, childID, errStr, now)

	// Update last_fired_at on the trigger
	_, _ = d.conn.Exec(`UPDATE triggers SET last_fired_at = ? WHERE id = ?`, now, triggerID)
}

// GetTriggerHistory returns recent trigger fire events for a project.
func (d *DB) GetTriggerHistory(project string, limit int) []TriggerFire {
	if limit <= 0 {
		limit = 50
	}

	rows, err := d.ro().Query(`SELECT id, trigger_id, project, event, COALESCE(child_id, ''), COALESCE(error, ''), fired_at
		FROM trigger_history WHERE project = ? ORDER BY fired_at DESC LIMIT ?`, project, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []TriggerFire
	for rows.Next() {
		var f TriggerFire
		if err := rows.Scan(&f.ID, &f.TriggerID, &f.Project, &f.Event, &f.ChildID, &f.Error, &f.FiredAt); err != nil {
			continue
		}
		result = append(result, f)
	}
	return result
}
