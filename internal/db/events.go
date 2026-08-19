package db

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Event is one row of the durable event outbox / replay log (TSU-52).
type Event struct {
	ID          string  `json:"id"`
	DeliveryID  string  `json:"delivery_id"`
	Project     string  `json:"project"`
	EventType   string  `json:"event_type"`
	Agent       string  `json:"agent"`
	Payload     string  `json:"payload"` // JSON object
	CreatedAt   string  `json:"created_at"`
	DeliveredAt *string `json:"delivered_at,omitempty"`
	Attempts    int     `json:"attempts"`
	LastError   string  `json:"last_error,omitempty"`
}

// InsertEvent appends an event to the outbox, deduping on delivery_id via INSERT
// OR IGNORE. deliveryID is the idempotency key — pass a stable one for
// at-least-once sources (e.g. a webhook's delivery GUID) so a retry is a no-op;
// pass "" for internally-generated events and a fresh UUID is used (always
// inserts). Returns (eventID, inserted): inserted=false means a duplicate
// delivery_id was ignored. Best-effort persistence must never block the caller's
// hot path — callers log and continue on error.
func (d *DB) InsertEvent(deliveryID, project, eventType, agent, payloadJSON string) (string, bool, error) {
	if eventType == "" {
		return "", false, fmt.Errorf("insert event: empty event_type")
	}
	if project == "" {
		project = "default"
	}
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	id := uuid.New().String()
	if deliveryID == "" {
		deliveryID = id
	}
	now := time.Now().UTC().Format(memoryTimeFmt)

	res, err := d.conn.Exec(
		`INSERT OR IGNORE INTO events (id, delivery_id, project, event_type, agent, payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, deliveryID, project, eventType, agent, payloadJSON, now,
	)
	if err != nil {
		return "", false, fmt.Errorf("insert event: %w", err)
	}
	n, _ := res.RowsAffected()
	return id, n > 0, nil
}

// RecentEvents returns the newest events (newest first) for replay / inspection.
func (d *DB) RecentEvents(project string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, delivery_id, project, event_type, agent, payload, created_at, delivered_at, attempts, last_error
	      FROM events`
	args := []any{}
	if project != "" {
		q += ` WHERE project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.ro().Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanEvents(rows)
}

// UndeliveredEvents returns events the sweeper (slice-B) hasn't processed yet,
// oldest first, capped. delivered_at IS NULL is the work queue.
func (d *DB) UndeliveredEvents(limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.ro().Query(
		`SELECT id, delivery_id, project, event_type, agent, payload, created_at, delivered_at, attempts, last_error
		 FROM events WHERE delivered_at IS NULL ORDER BY id ASC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("undelivered events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanEvents(rows)
}

// MarkEventDelivered stamps delivered_at so the sweeper won't reprocess the row.
func (d *DB) MarkEventDelivered(id string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec(`UPDATE events SET delivered_at = ? WHERE id = ?`, now, id)
	return err
}

// IncrementEventAttempt bumps the retry counter and records the last error,
// leaving delivered_at NULL so the sweeper retries the row next tick.
func (d *DB) IncrementEventAttempt(id, lastErr string) error {
	_, err := d.conn.Exec(`UPDATE events SET attempts = attempts + 1, last_error = ? WHERE id = ?`, lastErr, id)
	return err
}

// MarkEventDead dead-letters an event: stamps delivered_at (so it leaves the
// sweeper queue) and records why. attempts is bumped for the audit trail.
func (d *DB) MarkEventDead(id, lastErr string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec(`UPDATE events SET delivered_at = ?, attempts = attempts + 1, last_error = ? WHERE id = ?`, now, lastErr, id)
	return err
}

// DeleteEventByDelivery removes the event row for a (project, delivery_id). It is
// the compensating action for a record-then-act webhook: if the work triggered
// by an accepted signal fails, deleting the dedup row lets an at-least-once
// redelivery retry instead of deduping to a silent no-op. Best-effort.
func (d *DB) DeleteEventByDelivery(project, deliveryID string) {
	if deliveryID == "" {
		return
	}
	_, _ = d.conn.Exec(`DELETE FROM events WHERE project = ? AND delivery_id = ?`, project, deliveryID)
}

// PruneDeliveredEvents caps the replay log: keeps the newest `keep` delivered
// rows, deletes older delivered ones. Undelivered (queued) rows are never
// pruned. Best-effort maintenance against unbounded growth.
func (d *DB) PruneDeliveredEvents(keep int) {
	if keep < 1 {
		keep = 1000
	}
	_, _ = d.conn.Exec(
		`DELETE FROM events WHERE delivered_at IS NOT NULL AND id NOT IN (
		   SELECT id FROM events WHERE delivered_at IS NOT NULL ORDER BY id DESC LIMIT ?
		 )`,
		keep,
	)
}

func scanEvents(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.DeliveryID, &e.Project, &e.EventType, &e.Agent,
			&e.Payload, &e.CreatedAt, &e.DeliveredAt, &e.Attempts, &e.LastError); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
