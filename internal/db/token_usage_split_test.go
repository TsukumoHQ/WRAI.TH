package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// tableExistsIn reports whether `table` exists in the given sqlite file, opened
// read-only through a fresh connection (no ATTACH), so it inspects exactly that
// one file's schema.
func tableExistsIn(t *testing.T, path, table string) bool {
	t.Helper()
	conn, err := sql.Open("sqlite3", path+"?mode=ro")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = conn.Close() }()
	var n int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&n); err != nil {
		t.Fatalf("query schema %s: %v", path, err)
	}
	return n > 0
}

// TestTokenUsageLivesInAnalyticsDB asserts the coordination DB carries no
// token_usage table, the sibling analytics DB does, and reads/writes still work
// transparently through the ATTACH.
func TestTokenUsageLivesInAnalyticsDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	d, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now().UTC().Format(time.RFC3339)
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Tool: "t", Input: 100, Output: 50, CreatedAt: now},
		{Project: "p", Agent: "b", Tool: "t", Bytes: 400, CreatedAt: now},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Read path resolves the ATTACHed table transparently.
	rows, err := d.GetTokenUsageByProject(time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 || rows[0].CallCount != 2 {
		t.Fatalf("expected 1 project row with 2 calls, got %+v", rows)
	}

	apath := analyticsDBPath(path)
	if !fileExists(apath) {
		t.Fatalf("analytics DB %s does not exist", apath)
	}
	if tableExistsIn(t, path, "token_usage") {
		t.Errorf("coordination DB still has a token_usage table")
	}
	if !tableExistsIn(t, apath, "token_usage") {
		t.Errorf("analytics DB is missing the token_usage table")
	}
}

// TestBackupExcludesTelemetry asserts VACUUM INTO snapshots of the coordination
// DB never carry the token_usage rows (they live in the un-vacuumed analytics DB).
func TestBackupExcludesTelemetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	d, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now().UTC().Format(time.RFC3339)
	if err := d.InsertTokenUsageBatch([]TokenRecord{{Project: "p", Agent: "a", Bytes: 400, CreatedAt: now}}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	snap, err := d.Backup(2)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if tableExistsIn(t, snap, "token_usage") {
		t.Errorf("coordination DB backup %s carries the token_usage table", snap)
	}
}

// TestLegacyTokenUsageBackfill asserts a token_usage table sitting in an existing
// coordination DB is moved into the analytics DB (rows preserved) and dropped from
// the coordination DB on the next open — non-destructive + idempotent.
func TestLegacyTokenUsageBackfill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")

	// Seed a legacy coordination DB with a token_usage table + rows, as an older
	// build would have left it (telemetry inside the coord DB).
	seed, err := sql.Open("sqlite3", path+"?_foreign_keys=ON")
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if _, err := seed.Exec(`CREATE TABLE token_usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL DEFAULT 'default',
		agent TEXT NOT NULL DEFAULT '',
		tool TEXT NOT NULL DEFAULT '',
		bytes INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
		model TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 3; i++ {
		if _, err := seed.Exec(`INSERT INTO token_usage (project, agent, bytes, created_at) VALUES ('p','a',400,?)`, now); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	_ = seed.Close()

	// Open through the real path: migrate should backfill + drop the legacy table.
	d, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	if tableExistsIn(t, path, "token_usage") {
		t.Errorf("legacy token_usage was not dropped from the coordination DB")
	}
	rows, err := d.GetTokenUsageByProject(time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("read after backfill: %v", err)
	}
	if len(rows) != 1 || rows[0].CallCount != 3 {
		t.Fatalf("expected 3 backfilled rows, got %+v", rows)
	}
}

// TestTokenUsageDailyRollup asserts the rollup aggregates raw rows and survives a
// raw purge, so shortened retention keeps aggregate history.
func TestTokenUsageDailyRollup(t *testing.T) {
	dir := t.TempDir()
	d, err := NewTestDB(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	// Rows aged 20 days: inside a 30-day rollup window but outside a 14-day raw
	// retention, so the purge below actually removes them.
	old := time.Now().UTC().AddDate(0, 0, -20)
	oldTS := old.Format(time.RFC3339)
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Input: 100, Output: 50, CreatedAt: oldTS},
		{Project: "p", Agent: "a", Input: 10, Output: 5, CreatedAt: oldTS},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := d.RollupTokenUsage(30); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	// Purge raw rows older than 14 days; the rollup must still report the aggregate.
	purged, err := d.PurgeOldTokenUsage(14 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 2 {
		t.Fatalf("expected 2 raw rows purged, got %d", purged)
	}
	agg, err := d.GetTokenUsageDailyByProject(old.Format("2006-01-02"))
	if err != nil {
		t.Fatalf("rollup read: %v", err)
	}
	if len(agg) != 1 || agg[0].CallCount != 2 || agg[0].Tokens != 165 {
		t.Fatalf("expected rollup 2 calls / 165 tokens, got %+v", agg)
	}
}
