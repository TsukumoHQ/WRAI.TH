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

	tx, err := d.conn.Begin()
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
	res, err := d.conn.Exec("DELETE FROM token_usage WHERE created_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RollupTokenUsage refreshes the daily aggregate table (analytics.token_usage_daily)
// from raw token_usage, summing ONLY calendar days whose rows are all still present
// in raw — i.e. strictly newer than the raw-retention purge boundary (now minus
// retentionDays). The single calendar day that straddles that instant is skipped:
// its raw rows are being purged piecemeal, so re-summing it would overwrite an
// already-complete aggregate with a shrinking partial sum and then freeze that
// undercount once the day fully purges. Each day was captured in full on earlier
// ticks while it sat wholly inside retention, and a fully-purged day yields no
// SELECT row (so its stored aggregate is never overwritten) — together that keeps
// the rollup a faithful, non-shrinking history. Idempotent; writer path.
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
	_, err := d.conn.Exec(`
		INSERT INTO token_usage_daily (day, project, agent, model, bytes, tokens, call_count)
		SELECT strftime('%Y-%m-%d', created_at) AS day, project, agent, COALESCE(model, ''),
		       SUM(bytes), `+tokenSum+`, COUNT(*)
		FROM token_usage
		WHERE created_at >= ?
		GROUP BY day, project, agent, model
		ON CONFLICT(day, project, agent, model) DO UPDATE SET
			bytes=excluded.bytes, tokens=excluded.tokens, call_count=excluded.call_count`,
		since)
	return err
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
