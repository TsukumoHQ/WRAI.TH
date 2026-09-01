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

// tokenUsageSchema is the coordination/analytics token_usage DDL, used by tests
// to pre-seed a raw file before migrate runs.
const tokenUsageSchema = `CREATE TABLE token_usage (
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
)`

// seedTokenUsage inserts a token_usage row with an explicit id into an already-open
// sqlite connection (used to hand-build legacy coordination + partial analytics DBs).
func seedTokenUsage(t *testing.T, conn *sql.DB, id int, agent, createdAt string) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO token_usage (id, project, agent, tool, bytes, created_at) VALUES (?, 'p', ?, 't', 400, ?)`,
		id, agent, createdAt,
	); err != nil {
		t.Fatalf("seed token_usage id=%d: %v", id, err)
	}
}

// TestBackfillIdempotentPartialMigration reproduces the synx-prod crash-loop: a
// legacy token_usage table still sits in the coordination DB while the analytics
// DB already holds SOME of the same ids (a migration that copied rows then crashed
// before the DROP). The old plain INSERT re-copied those ids and aborted init on
// `UNIQUE constraint failed: token_usage.id`, crash-looping the relay. Init must
// now boot clean, migrate the remaining rows, lose nothing, and be idempotent
// across repeated opens.
func TestBackfillIdempotentPartialMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	now := time.Now().UTC().Format(time.RFC3339)

	// Legacy coordination DB: token_usage with ids 1,2,3 still present.
	seed, err := sql.Open("sqlite3", path+"?_foreign_keys=ON")
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if _, err := seed.Exec(tokenUsageSchema); err != nil {
		t.Fatalf("seed coord schema: %v", err)
	}
	for i := 1; i <= 3; i++ {
		seedTokenUsage(t, seed, i, "a", now)
	}
	_ = seed.Close()

	// Partially-migrated analytics DB: ids 1,2 ALREADY copied (identical content),
	// as a crashed prior migration would have left it.
	apath := analyticsDBPath(path)
	aseed, err := sql.Open("sqlite3", apath+"?_foreign_keys=ON")
	if err != nil {
		t.Fatalf("open analytics seed: %v", err)
	}
	if _, err := aseed.Exec(tokenUsageSchema); err != nil {
		t.Fatalf("seed analytics schema: %v", err)
	}
	for i := 1; i <= 2; i++ {
		seedTokenUsage(t, aseed, i, "a", now)
	}
	_ = aseed.Close()

	// First open: must NOT crash on the duplicate ids; must migrate id 3 and drop.
	d, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("first open must not crash-loop on duplicate ids: %v", err)
	}
	rows, err := d.GetTokenUsageByProject(time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("read after backfill: %v", err)
	}
	if len(rows) != 1 || rows[0].CallCount != 3 {
		t.Fatalf("no data loss expected: want 3 rows, got %+v", rows)
	}
	if tableExistsIn(t, path, "token_usage") {
		t.Errorf("legacy token_usage should be dropped once all rows are in analytics")
	}
	_ = d.Close()

	// Second open (init runs again): idempotent — clean boot, still exactly 3 rows.
	d2, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("second open must be idempotent, got: %v", err)
	}
	defer func() { _ = d2.Close() }()
	rows2, err := d2.GetTokenUsageByProject(time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("read after second open: %v", err)
	}
	if len(rows2) != 1 || rows2[0].CallCount != 3 {
		t.Fatalf("idempotent re-open changed the data: got %+v", rows2)
	}
}

// TestBackfillKeepsLegacyOnIdConflict asserts that when analytics already holds a
// DIFFERENT row under an id that also exists in the legacy table (id-space overlap
// with content mismatch), init still boots clean AND never drops the legacy table —
// so the legacy row is preserved, not silently clobbered. Zero data loss.
func TestBackfillKeepsLegacyOnIdConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	now := time.Now().UTC().Format(time.RFC3339)

	// Legacy coordination DB: id 1 = agent "legacy", id 2 = agent "legacy".
	seed, err := sql.Open("sqlite3", path+"?_foreign_keys=ON")
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if _, err := seed.Exec(tokenUsageSchema); err != nil {
		t.Fatalf("seed coord schema: %v", err)
	}
	seedTokenUsage(t, seed, 1, "legacy", now)
	seedTokenUsage(t, seed, 2, "legacy", now)
	_ = seed.Close()

	// Analytics DB already holds id 1 with DIFFERENT content (a live-written row).
	apath := analyticsDBPath(path)
	aseed, err := sql.Open("sqlite3", apath+"?_foreign_keys=ON")
	if err != nil {
		t.Fatalf("open analytics seed: %v", err)
	}
	if _, err := aseed.Exec(tokenUsageSchema); err != nil {
		t.Fatalf("seed analytics schema: %v", err)
	}
	seedTokenUsage(t, aseed, 1, "live", now)
	_ = aseed.Close()

	d, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("open must not crash on id conflict: %v", err)
	}
	defer func() { _ = d.Close() }()

	// Legacy table must be KEPT (its id-1 row conflicts and must not be lost).
	if !tableExistsIn(t, path, "token_usage") {
		t.Errorf("legacy table must be kept when an id conflicts with different analytics content")
	}
	// The legacy row is intact in the coordination DB.
	var legacyAgent string
	if err := d.conn.QueryRow(`SELECT agent FROM main.token_usage WHERE id=1`).Scan(&legacyAgent); err != nil {
		t.Fatalf("legacy id=1 lost: %v", err)
	}
	if legacyAgent != "legacy" {
		t.Errorf("legacy id=1 clobbered: got agent %q", legacyAgent)
	}
	// Analytics keeps its own id-1 row and gained the non-conflicting id-2 row.
	var liveAgent string
	if err := d.conn.QueryRow(`SELECT agent FROM analytics.token_usage WHERE id=1`).Scan(&liveAgent); err != nil {
		t.Fatalf("analytics id=1 lost: %v", err)
	}
	if liveAgent != "live" {
		t.Errorf("analytics id=1 overwritten: got agent %q", liveAgent)
	}
	var id2 int
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM analytics.token_usage WHERE id=2`).Scan(&id2); err != nil {
		t.Fatalf("query analytics id=2: %v", err)
	}
	if id2 != 1 {
		t.Errorf("non-conflicting id=2 should have migrated to analytics, got count %d", id2)
	}
}

// TestTokenUsageDailyRollup asserts the rollup aggregates raw rows and survives a
// raw purge, so shortened retention keeps aggregate history.
// rollupDayCount returns how many token_usage_daily rows exist for a UTC day.
func rollupDayCount(t *testing.T, d *DB, day string) int {
	t.Helper()
	var n int
	if err := d.ro().QueryRow(`SELECT COUNT(*) FROM token_usage_daily WHERE day=?`, day).Scan(&n); err != nil {
		t.Fatalf("rollup count %s: %v", day, err)
	}
	return n
}

// TestRollupSkipsPartialBoundaryDay asserts RollupTokenUsage summarizes only
// fully-retained calendar days and SKIPS the day straddling the purge boundary
// (now - retention). Rolling that partial day up would overwrite a complete
// aggregate with a shrinking partial and freeze the undercount once it purges —
// the boundary-day bug this fix closes (review-de1ee790 finding).
func TestRollupSkipsPartialBoundaryDay(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now().UTC()
	full := now.AddDate(0, 0, -10) // wholly inside a 14d retention
	// Last second of the calendar day that straddles the purge boundary (now-14d):
	// deterministically inside that day and before the first fully-retained
	// midnight, so the fix must exclude it regardless of the current time-of-day.
	cutoff := now.AddDate(0, 0, -14)
	bDay := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	boundary := bDay.AddDate(0, 0, 1).Add(-time.Second)
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Input: 100, Output: 50, CreatedAt: full.Format(time.RFC3339)},
		{Project: "p", Agent: "a", Input: 9, Output: 1, CreatedAt: boundary.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := d.RollupTokenUsage(14); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	if got := rollupDayCount(t, d, full.Format("2006-01-02")); got != 1 {
		t.Errorf("fully-retained day: expected 1 rollup row, got %d", got)
	}
	if got := rollupDayCount(t, d, boundary.Format("2006-01-02")); got != 0 {
		t.Errorf("partial boundary day must be skipped, but got %d rollup row(s)", got)
	}
}

// TestRollupPreservesPurgedDayAggregate asserts a day that has fully purged from
// raw keeps its stored aggregate: with no raw rows the SELECT yields nothing, so
// the ON CONFLICT never overwrites the prior full value (non-shrinking history).
func TestRollupPreservesPurgedDayAggregate(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now().UTC()
	day := now.AddDate(0, 0, -5)
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Input: 100, Output: 50, CreatedAt: day.Format(time.RFC3339)},
		{Project: "p", Agent: "a", Input: 10, Output: 5, CreatedAt: day.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := d.RollupTokenUsage(14); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	// Simulate that day (and all raw) having fully purged, then roll up again.
	if _, err := d.conn.Exec(`DELETE FROM token_usage`); err != nil {
		t.Fatalf("purge raw: %v", err)
	}
	if err := d.RollupTokenUsage(14); err != nil {
		t.Fatalf("rollup after purge: %v", err)
	}
	agg, err := d.GetTokenUsageDailyByProject(day.Format("2006-01-02"))
	if err != nil {
		t.Fatalf("rollup read: %v", err)
	}
	if len(agg) != 1 || agg[0].CallCount != 2 || agg[0].Tokens != 165 {
		t.Fatalf("purged day aggregate must survive intact: got %+v", agg)
	}
}
