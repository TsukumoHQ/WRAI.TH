package db

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Token-usage rollup off the single coord writer (task a0663508). The 5-min rollup
// used to run its full GROUP-BY aggregate as one INSERT...SELECT on the writer conn,
// pinning it for the aggregate's whole duration (15s under load) and starving every
// other relay write into writerTimeout. The aggregate now runs on the reader pool;
// the writer is taken only for a short apply tx. The routine tick re-sums only
// yesterday-onward; a once-per-day full catch-up folds in late rows for older days.

func newRollupDB(t *testing.T) *DB {
	t.Helper()
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// dailyOneRow returns the single token_usage_daily aggregate for a UTC day (tests
// seed one project/agent/model so a day maps to exactly one row).
func dailyOneRow(t *testing.T, d *DB, day string) (bytes, tokens, calls int64) {
	t.Helper()
	err := d.ro().QueryRow(
		`SELECT bytes, tokens, call_count FROM token_usage_daily WHERE day=?`, day,
	).Scan(&bytes, &tokens, &calls)
	if err != nil {
		t.Fatalf("read daily %s: %v", day, err)
	}
	return
}

// TestRollupAggregateReadsOffWriter (AC1): while the aggregate read is in progress
// (paused via rollupReadHook), a concurrent writerExec from another goroutine still
// completes well under 100ms — proof the aggregate no longer holds the coord writer.
// Revert-check: run the aggregate on the writer conn (the old INSERT...SELECT) and
// this concurrent write would wait out the read instead.
func TestRollupAggregateReadsOffWriter(t *testing.T) {
	d := newRollupDB(t)
	now := time.Now().UTC()
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Bytes: 400, CreatedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	started := make(chan struct{})
	rollupReadHook = func() {
		close(started)
		time.Sleep(300 * time.Millisecond) // hold the read phase open
	}
	defer func() { rollupReadHook = nil }()

	go func() { _ = d.RollupTokenUsageRoutine() }()

	<-started // read phase now in progress (writer NOT held)
	t0 := time.Now()
	if _, err := d.writerExec("UPDATE agents SET last_seen = ? WHERE name = ?", "x", "nobody"); err != nil {
		t.Fatalf("concurrent writerExec: %v", err)
	}
	if el := time.Since(t0); el > 100*time.Millisecond {
		t.Fatalf("concurrent write waited %s during the rollup read — the aggregate must not hold the writer", el)
	}
}

// TestRollupByteIdenticalAcrossDays (AC2): rollup output matches the exact per-day
// sums on fixtures spanning >=3 days, a fully-purged day's stored aggregate is never
// overwritten, and re-running after a new raw row ON CONFLICT-updates the day.
func TestRollupByteIdenticalAcrossDays(t *testing.T) {
	d := newRollupDB(t)
	now := time.Now().UTC()
	dayA := now.AddDate(0, 0, -3)
	dayB := now.AddDate(0, 0, -6)
	dayC := now.AddDate(0, 0, -9) // will be "purged" (no raw rows)

	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Bytes: 400, Input: 100, Output: 50, CreatedAt: dayA.Format(time.RFC3339)},
		{Project: "p", Agent: "a", Bytes: 200, Input: 10, Output: 5, CreatedAt: dayA.Format(time.RFC3339)},
		{Project: "p", Agent: "a", Bytes: 100, Input: 20, CreatedAt: dayB.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Pre-existing aggregate for a day with NO raw rows (already purged).
	if _, err := d.conn.Exec(
		`INSERT INTO token_usage_daily (day, project, agent, model, bytes, tokens, call_count)
		 VALUES (?, 'p', 'a', '', 9999, 8888, 77)`, dayC.Format("2006-01-02")); err != nil {
		t.Fatalf("seed purged day: %v", err)
	}

	if err := d.RollupTokenUsage(14); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	if b, tk, c := dailyOneRow(t, d, dayA.Format("2006-01-02")); b != 600 || tk != 165 || c != 2 {
		t.Fatalf("day A = (%d,%d,%d), want (600,165,2)", b, tk, c)
	}
	if b, tk, c := dailyOneRow(t, d, dayB.Format("2006-01-02")); b != 100 || tk != 20 || c != 1 {
		t.Fatalf("day B = (%d,%d,%d), want (100,20,1)", b, tk, c)
	}
	// Fully-purged day: stored aggregate untouched (non-shrinking history).
	if b, tk, c := dailyOneRow(t, d, dayC.Format("2006-01-02")); b != 9999 || tk != 8888 || c != 77 {
		t.Fatalf("purged day C overwritten: (%d,%d,%d), want (9999,8888,77)", b, tk, c)
	}

	// ON CONFLICT update: a new raw row for day A re-sums it (bytes/4 estimate, since
	// this row carries no transcript token counts: 1000/4 = 250 tokens).
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Bytes: 1000, CreatedAt: dayA.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed update: %v", err)
	}
	if err := d.RollupTokenUsage(14); err != nil {
		t.Fatalf("rollup 2: %v", err)
	}
	if b, tk, c := dailyOneRow(t, d, dayA.Format("2006-01-02")); b != 1600 || tk != 415 || c != 3 {
		t.Fatalf("day A after ON CONFLICT = (%d,%d,%d), want (1600,415,3)", b, tk, c)
	}
}

// TestRollupRoutineLogsNoWriterWait (AC3): with writerSlowWait shrunk to 20ms and a
// slow aggregate read, the rollup logs no `writer wait:` line — the apply tx alone is
// under threshold, and the slow aggregate runs off the writer.
func TestRollupRoutineLogsNoWriterWait(t *testing.T) {
	d := newRollupDB(t)
	now := time.Now().UTC()
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Bytes: 400, CreatedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	origW := writerSlowWait
	writerSlowWait = 20 * time.Millisecond
	defer func() { writerSlowWait = origW }()
	rollupReadHook = func() { time.Sleep(60 * time.Millisecond) } // slow aggregate, off the writer
	defer func() { rollupReadHook = nil }()

	out := captureLog(t, func() {
		if err := d.RollupTokenUsageRoutine(); err != nil {
			t.Fatalf("rollup: %v", err)
		}
	})
	if strings.Contains(out, "writer wait:") {
		t.Fatalf("rollup must log no writer-wait line (aggregate is off the writer), got:\n%s", out)
	}
}

// TestRollupRoutineTickWindowExcludesOlderDays (AC4): the routine tick re-sums only
// yesterday-onward, so a row for an older day (day-3) inserted late is NOT summed by
// the routine tick.
func TestRollupRoutineTickWindowExcludesOlderDays(t *testing.T) {
	d := newRollupDB(t)
	now := time.Now().UTC()
	day3 := now.AddDate(0, 0, -3)
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Bytes: 400, CreatedAt: day3.Format(time.RFC3339)},
		{Project: "p", Agent: "a", Bytes: 400, CreatedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := d.RollupTokenUsageRoutine(); err != nil {
		t.Fatalf("routine rollup: %v", err)
	}
	if got := rollupDayCount(t, d, day3.Format("2006-01-02")); got != 0 {
		t.Fatalf("routine tick must not sum the older day-3 row, got %d rollup row(s)", got)
	}
	if got := rollupDayCount(t, d, now.Format("2006-01-02")); got != 1 {
		t.Fatalf("routine tick must sum today, got %d rollup row(s)", got)
	}
}

// TestRollupDailyCatchupFixesLateRows (AC5): the once-per-day full catch-up re-sums
// the whole retention window and fixes the late day-3 row the routine tick skipped.
func TestRollupDailyCatchupFixesLateRows(t *testing.T) {
	d := newRollupDB(t)
	now := time.Now().UTC()
	day3 := now.AddDate(0, 0, -3)
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Bytes: 400, Input: 30, Output: 10, CreatedAt: day3.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Routine tick leaves the older day uncorrected...
	if err := d.RollupTokenUsageRoutine(); err != nil {
		t.Fatalf("routine rollup: %v", err)
	}
	if got := rollupDayCount(t, d, day3.Format("2006-01-02")); got != 0 {
		t.Fatalf("precondition: routine must skip day-3, got %d", got)
	}
	// ...the full catch-up folds it in.
	if err := d.RollupTokenUsage(14); err != nil {
		t.Fatalf("catch-up rollup: %v", err)
	}
	if b, tk, c := dailyOneRow(t, d, day3.Format("2006-01-02")); b != 400 || tk != 40 || c != 1 {
		t.Fatalf("catch-up day-3 = (%d,%d,%d), want (400,40,1)", b, tk, c)
	}
}
