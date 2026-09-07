package db

import (
	"fmt"
	"strings"
	"time"
)

// TokenRecord represents a single token-usage measurement. Either the legacy
// Bytes estimate (relay tool payloads) or the real per-turn counts read from the
// Claude Code transcript by the Stop hook — never both in the same row.
type TokenRecord struct {
	Project       string
	Agent         string
	Tool          string
	Bytes         int
	Input         int
	Output        int
	CacheRead     int
	CacheCreation int
	Model         string
	CreatedAt     string
}

// tokenSum is the per-row token count used by all reporting: the real transcript
// usage when present, else the legacy bytes/4 estimate. Old and new rows coexist
// without double-counting.
const tokenSum = `SUM(CASE WHEN (input_tokens+output_tokens+cache_read_tokens+cache_creation_tokens) > 0
		THEN input_tokens+output_tokens+cache_read_tokens+cache_creation_tokens
		ELSE bytes/4 END)`

// TokenUsageSummary aggregates token usage by a grouping key.
type TokenUsageSummary struct {
	Key       string `json:"key"`
	Bytes     int64  `json:"bytes"`
	Tokens    int64  `json:"tokens"`
	CallCount int64  `json:"call_count"`
}

// InsertTokenUsageBatch inserts multiple token usage records in a single transaction.
func (d *DB) InsertTokenUsageBatch(records []TokenRecord) error {
	if len(records) == 0 {
		return nil
	}

	tx, err := d.beginWriterTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare("INSERT INTO token_usage (project, agent, tool, bytes, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, model, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range records {
		if _, err := stmt.Exec(r.Project, r.Agent, r.Tool, r.Bytes, r.Input, r.Output, r.CacheRead, r.CacheCreation, r.Model, r.CreatedAt); err != nil {
			return fmt.Errorf("insert token usage: %w", err)
		}
	}

	return tx.Commit()
}

// GetTokenUsageByProject returns token usage aggregated by project since the given time.
func (d *DB) GetTokenUsageByProject(since string) ([]TokenUsageSummary, error) {
	rows, err := d.ro().Query(`
		SELECT project, SUM(bytes), `+tokenSum+`, COUNT(*)
		FROM token_usage
		WHERE created_at >= ?
		GROUP BY project
		ORDER BY SUM(bytes) DESC
	`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []TokenUsageSummary
	for rows.Next() {
		var s TokenUsageSummary
		if err := rows.Scan(&s.Key, &s.Bytes, &s.Tokens, &s.CallCount); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

// GetTokenUsageByAgent returns token usage aggregated by agent within a project.
func (d *DB) GetTokenUsageByAgent(project, since string) ([]TokenUsageSummary, error) {
	rows, err := d.ro().Query(`
		SELECT agent, SUM(bytes), `+tokenSum+`, COUNT(*)
		FROM token_usage
		WHERE project = ? AND created_at >= ?
		GROUP BY agent
		ORDER BY SUM(bytes) DESC
	`, project, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []TokenUsageSummary
	for rows.Next() {
		var s TokenUsageSummary
		if err := rows.Scan(&s.Key, &s.Bytes, &s.Tokens, &s.CallCount); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

// GetTokenUsageByTool returns token usage aggregated by tool for a specific agent.
func (d *DB) GetTokenUsageByTool(project, agent, since string) ([]TokenUsageSummary, error) {
	q := `SELECT tool, SUM(bytes), ` + tokenSum + `, COUNT(*) FROM token_usage WHERE project = ? AND created_at >= ?`
	args := []any{project, since}

	if agent != "" {
		q += " AND agent = ?"
		args = append(args, agent)
	}

	q += " GROUP BY tool ORDER BY SUM(bytes) DESC"

	rows, err := d.ro().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []TokenUsageSummary
	for rows.Next() {
		var s TokenUsageSummary
		if err := rows.Scan(&s.Key, &s.Bytes, &s.Tokens, &s.CallCount); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

// PurgeOldTokenUsage removes token usage records older than the given duration.
func (d *DB) PurgeOldTokenUsage(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
	res, err := d.writerExec("DELETE FROM token_usage WHERE created_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// rollupRow is one aggregated (day, project, agent, model) group carried from the
// read phase (reader pool) to the apply phase (short writer tx).
type rollupRow struct {
	day, project, agent, model string
	bytes, tokens, calls       int64
}

// rollupReadHook is a test-only seam (nil in production): it fires at the start of
// the read phase so a test can pause the aggregate and prove the single coord writer
// is NOT held while the rollup reads.
var rollupReadHook func()

// RollupTokenUsage refreshes the daily aggregate table (analytics.token_usage_daily)
// from raw token_usage, summing ONLY calendar days whose rows are all still present
// in raw — i.e. strictly newer than the raw-retention purge boundary (now minus
// retentionDays). The single calendar day that straddles that instant is skipped:
// its raw rows are being purged piecemeal, so re-summing it would overwrite an
// already-complete aggregate with a shrinking partial sum and then freeze that
// undercount once the day fully purges. Each day was captured in full on earlier
// ticks while it sat wholly inside retention, and a fully-purged day yields no
// SELECT row (so its stored aggregate is never overwritten) — together that keeps
// the rollup a faithful, non-shrinking history. Idempotent.
//
// WRITER DISCIPLINE (task a0663508): the aggregate SELECT — a full GROUP BY over the
// raw retention window (a TEMP B-TREE over hundreds of thousands of rows, seconds
// under CPU load) — runs on the READER pool; the single coord writer is taken only
// for the apply tx (beginWriterTx), whose statements are pure INSERT ... ON CONFLICT
// DO UPDATE with explicit values (milliseconds). Running the aggregate on the writer
// conn (one shared INSERT ... SELECT) was pinning that connection for its full
// duration and starving every other relay write into writerTimeout.
//
// This is the FULL-window rollup: it re-sums every fully-retained day (since = the
// first midnight after the purge boundary, byte-identical to the pre-split rollup).
// The cleanup loop runs it once per UTC day as the catch-up that folds in rows which
// arrived for older days after the cheap routine tick's window had passed them; the
// frequent 5-min tick uses RollupTokenUsageRoutine instead.
//
// retentionDays MUST match the raw-retention window (cleanup.TokenUsageRetention);
// passing a larger value would try to sum days already purged from raw.
func (d *DB) RollupTokenUsage(retentionDays int) error {
	if retentionDays < 1 {
		retentionDays = 14
	}
	// The calendar day containing the purge instant (now - retentionDays) is only
	// partially retained; the first FULLY-retained day is the midnight after it.
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	boundaryDay := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	since := boundaryDay.AddDate(0, 0, 1).Format(time.RFC3339)
	return d.rollupTokenUsageSince(since)
}

// RollupTokenUsageRoutine is the cheap 5-min tick: it re-sums only the days that can
// still change — yesterday 00:00Z UTC onward — so the common tick touches two
// calendar days instead of TEMP-B-TREE-ing the whole retention window. Late-arriving
// rows for OLDER days are not corrected here; the once-per-day RollupTokenUsage
// catch-up folds those in. Same read-then-apply path, so it never holds the writer
// for the aggregate.
func (d *DB) RollupTokenUsageRoutine() error {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	since := today.AddDate(0, 0, -1).Format(time.RFC3339) // yesterday 00:00Z UTC
	return d.rollupTokenUsageSince(since)
}

// rollupTokenUsageSince is the read-then-apply core shared by both rollup ticks.
// READ PHASE: aggregate raw token_usage for created_at >= since on the reader pool
// (holds no writer). APPLY PHASE: one short writer tx that upserts the in-memory
// groups. A day with no raw rows (fully purged) yields no group, so its stored
// aggregate is never overwritten — the non-shrinking-history invariant above holds
// for both the routine and the full-catch-up caller.
func (d *DB) rollupTokenUsageSince(since string) error {
	if rollupReadHook != nil {
		rollupReadHook()
	}
	// READ PHASE — reader pool; the GROUP BY never touches the coord writer.
	rows, err := d.ro().Query(`
		SELECT strftime('%Y-%m-%d', created_at) AS day, project, agent, COALESCE(model, ''),
		       SUM(bytes), `+tokenSum+`, COUNT(*)
		FROM token_usage
		WHERE created_at >= ?
		GROUP BY day, project, agent, model`, since)
	if err != nil {
		return err
	}
	var groups []rollupRow
	for rows.Next() {
		var r rollupRow
		if err := rows.Scan(&r.day, &r.project, &r.agent, &r.model, &r.bytes, &r.tokens, &r.calls); err != nil {
			_ = rows.Close()
			return err
		}
		groups = append(groups, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if len(groups) == 0 {
		return nil // nothing to upsert; purged days keep their stored aggregate
	}

	// APPLY PHASE — one short writer tx (the ONLY writer use in the rollup). Explicit
	// values only; no SELECT ... FROM token_usage inside the tx.
	tx, err := d.beginWriterTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`
		INSERT INTO token_usage_daily (day, project, agent, model, bytes, tokens, call_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, project, agent, model) DO UPDATE SET
			bytes=excluded.bytes, tokens=excluded.tokens, call_count=excluded.call_count`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range groups {
		if _, err := stmt.Exec(r.day, r.project, r.agent, r.model, r.bytes, r.tokens, r.calls); err != nil {
			return fmt.Errorf("rollup apply: %w", err)
		}
	}
	return tx.Commit()
}

// GetTokenUsageDailyByProject returns per-project totals from the daily rollup
// since a UTC day (YYYY-MM-DD) — the read for dashboard windows wider than the
// raw retention. Scans the pre-aggregated rollup, not the raw row table.
func (d *DB) GetTokenUsageDailyByProject(sinceDay string) ([]TokenUsageSummary, error) {
	rows, err := d.ro().Query(`
		SELECT project, SUM(bytes), SUM(tokens), SUM(call_count)
		FROM token_usage_daily
		WHERE day >= ?
		GROUP BY project
		ORDER BY SUM(bytes) DESC
	`, sinceDay)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []TokenUsageSummary
	for rows.Next() {
		var s TokenUsageSummary
		if err := rows.Scan(&s.Key, &s.Bytes, &s.Tokens, &s.CallCount); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

// GetProjectTokens24h returns total tokens (bytes/4) per project for the last 24 hours.
// Returns a map of project name → tokens.
func (d *DB) GetProjectTokens24h() (map[string]int64, error) {
	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	rows, err := d.ro().Query(`
		SELECT project, `+tokenSum+`
		FROM token_usage
		WHERE created_at >= ?
		GROUP BY project
	`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int64)
	for rows.Next() {
		var project string
		var tokens int64
		if err := rows.Scan(&project, &tokens); err != nil {
			return nil, err
		}
		result[project] = tokens
	}
	return result, rows.Err()
}

// TokenTimeBucket represents a single time bucket for sparkline data.
type TokenTimeBucket struct {
	Bucket    string `json:"bucket"`
	Tokens    int64  `json:"tokens"`
	CallCount int64  `json:"call_count"`
}

// GetTokenTimeSeries returns token usage bucketed by time intervals.
// For periods <= 24h, buckets are hourly. For 7d/30d, buckets are daily.
func (d *DB) GetTokenTimeSeries(project, agent, since, bucket string) ([]TokenTimeBucket, error) {
	// bucket should be "hour" or "day"
	var truncFmt string
	switch bucket {
	case "day":
		truncFmt = "%Y-%m-%d"
	default:
		truncFmt = "%Y-%m-%dT%H:00"
	}

	q := `SELECT strftime('` + truncFmt + `', created_at) AS bucket,
		` + tokenSum + `, COUNT(*)
		FROM token_usage
		WHERE project = ? AND created_at >= ?`
	args := []any{project, since}

	if agent != "" {
		q += " AND agent = ?"
		args = append(args, agent)
	}

	q += " GROUP BY bucket ORDER BY bucket ASC"

	rows, err := d.ro().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []TokenTimeBucket
	for rows.Next() {
		var b TokenTimeBucket
		if err := rows.Scan(&b.Bucket, &b.Tokens, &b.CallCount); err != nil {
			return nil, err
		}
		results = append(results, b)
	}
	return results, rows.Err()
}

// PeriodToBucket returns the appropriate bucket size for a period.
func PeriodToBucket(period string) string {
	switch period {
	case "7d", "30d":
		return "day"
	default:
		return "hour"
	}
}

// PeriodToSince converts a period string to a since timestamp.
// Supported: 1h, 6h, 12h, 24h, 7d, 30d (default: 24h).
func PeriodToSince(period string) string {
	period = strings.TrimSpace(period)
	var d time.Duration
	switch period {
	case "1h":
		d = time.Hour
	case "6h":
		d = 6 * time.Hour
	case "12h":
		d = 12 * time.Hour
	case "7d":
		d = 7 * 24 * time.Hour
	case "30d":
		d = 30 * 24 * time.Hour
	default:
		d = 24 * time.Hour
	}
	return time.Now().UTC().Add(-d).Format(time.RFC3339)
}
