package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// seedReviewer inserts an agent with an explicit status + deactivated_at so the
// reviewer-purge tests can craft dead / live / recently-inactive identities.
func seedReviewer(t *testing.T, conn *sql.DB, project, name, status string, deactivatedAt string) {
	t.Helper()
	var deact interface{}
	if deactivatedAt != "" {
		deact = deactivatedAt
	}
	_, err := conn.Exec(
		`INSERT INTO agents (id, name, role, description, registered_at, last_seen, project, reports_to, profile_slug, status, deactivated_at, is_executive, interest_tags, max_context_bytes, is_service, cwd)
		 VALUES (?, ?, '', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', ?, NULL, NULL, ?, ?, 0, '[]', 16384, 0, '')`,
		"ag-"+project+"-"+name, name, project, status, deact,
	)
	if err != nil {
		t.Fatalf("seed reviewer %s/%s: %v", project, name, err)
	}
}

func agentStatus(t *testing.T, d *DB, project, name string) string {
	t.Helper()
	var s string
	err := d.ro().QueryRow(`SELECT status FROM agents WHERE project=? AND name=?`, project, name).Scan(&s)
	if err != nil {
		t.Fatalf("read status %s/%s: %v", project, name, err)
	}
	return s
}

// TestPurgeStaleReviewersOneShot asserts the one-shot purge (olderThan=0)
// soft-deletes every inactive review-* across projects while HARD-SKIPPING an
// active reviewer, a reviewer holding a live task, and non-review agents — and is
// idempotent on re-run.
func TestPurgeStaleReviewersOneShot(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	seedReviewer(t, d.conn, "p1", "review-aaa", "inactive", "2026-01-01T00:00:00Z")
	seedReviewer(t, d.conn, "p2", "review-bbb", "inactive", "2026-01-01T00:00:00Z")
	seedReviewer(t, d.conn, "p1", "review-live", "active", "") // must be skipped
	seedReviewer(t, d.conn, "p3", "review-busy", "inactive", "2026-01-01T00:00:00Z")
	seedReviewer(t, d.conn, "p1", "backend", "inactive", "2026-01-01T00:00:00Z") // not a reviewer

	// review-busy holds a live (in-review) task → must be skipped.
	seedTask(t, d.conn, "task-live", "p3", "in-review", "cto", "", "review-busy", "", "", "", false)
	// review-aaa's only task is done → NOT live → still purged.
	seedTask(t, d.conn, "task-done", "p1", "done", "cto", "", "review-aaa", "", "", "", false)

	res, err := d.PurgeStaleReviewers(false, 0)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if res.Purged != 2 {
		t.Fatalf("expected 2 purged (review-aaa, review-bbb), got %d (candidates %v)", res.Purged, res.Candidates)
	}
	if got := agentStatus(t, d, "p1", "review-aaa"); got != "deleted" {
		t.Errorf("review-aaa should be soft-deleted, got %q", got)
	}
	if got := agentStatus(t, d, "p2", "review-bbb"); got != "deleted" {
		t.Errorf("review-bbb should be soft-deleted, got %q", got)
	}
	if got := agentStatus(t, d, "p1", "review-live"); got != "active" {
		t.Errorf("active reviewer must be untouched, got %q", got)
	}
	if got := agentStatus(t, d, "p3", "review-busy"); got != "inactive" {
		t.Errorf("reviewer holding a live task must be skipped, got %q", got)
	}
	if got := agentStatus(t, d, "p1", "backend"); got != "inactive" {
		t.Errorf("non-review agent must be untouched, got %q", got)
	}

	// Idempotent: a second run finds nothing (the rows are no longer 'inactive').
	res2, err := d.PurgeStaleReviewers(false, 0)
	if err != nil {
		t.Fatalf("purge 2: %v", err)
	}
	if res2.Purged != 0 || len(res2.Candidates) != 0 {
		t.Errorf("purge not idempotent: second run purged=%d candidates=%v", res2.Purged, res2.Candidates)
	}
}

// TestPurgeStaleReviewersDryRun asserts a dry run reports candidates but mutates
// nothing.
func TestPurgeStaleReviewersDryRun(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	seedReviewer(t, d.conn, "p1", "review-aaa", "inactive", "2026-01-01T00:00:00Z")

	res, err := d.PurgeStaleReviewers(true, 0)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !res.DryRun || res.Purged != 0 || len(res.Candidates) != 1 {
		t.Fatalf("dry run should report 1 candidate + 0 purged, got %+v", res)
	}
	if got := agentStatus(t, d, "p1", "review-aaa"); got != "inactive" {
		t.Errorf("dry run must not mutate, status now %q", got)
	}
}

// TestPurgeStaleReviewersTTLBoundary asserts the TTL reaper (olderThan>0) purges
// a reviewer inactive past the window but keeps one that went inactive recently.
func TestPurgeStaleReviewersTTLBoundary(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now().UTC()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * 24 * time.Hour).Format(time.RFC3339)
	seedReviewer(t, d.conn, "p1", "review-old", "inactive", old)
	seedReviewer(t, d.conn, "p1", "review-recent", "inactive", recent)

	res, err := d.PurgeStaleReviewers(false, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if res.Purged != 1 {
		t.Fatalf("expected only the aged reviewer purged, got %d (%v)", res.Purged, res.Candidates)
	}
	if got := agentStatus(t, d, "p1", "review-old"); got != "deleted" {
		t.Errorf("aged reviewer should be purged, got %q", got)
	}
	if got := agentStatus(t, d, "p1", "review-recent"); got != "inactive" {
		t.Errorf("recently-inactive reviewer must be kept, got %q", got)
	}
}
