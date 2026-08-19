package db

import "time"

// wakePred returns the SQL fragment (on the given messages alias) that reproduces
// the guard-first wake rule from UnreadCountForAgent / DEC-relay-comms-discipline-1:
// a message is wake-eligible iff it is P0, OR an ask/dispatch type
// (question/user_question/task always wake), OR its effective action tag is not
// 'none'. COALESCE(action_required,'do') keeps a legacy/NULL-tag row waking.
// One source of truth so the metric can never silently drift from the live
// predicate — the exact anti-pattern the comms-discipline work exists to avoid.
// The alias is parameterized because the coalescing self-join needs m1/m2.
func wakePred(alias string) string {
	return `(` + alias + `.priority = 'P0' OR ` + alias + `.type IN ('question','user_question','task') OR COALESCE(` + alias + `.action_required, 'do') != 'none')`
}

// SenderActionDist is one sender's message breakdown by effective action tag.
// Total is the row sum; a NULL/legacy tag is folded into 'do' (the wake default),
// mirroring the runtime predicate so the distribution matches what actually woke.
type SenderActionDist struct {
	From   string `json:"from"`
	Ask    int64  `json:"ask"`
	Do     int64  `json:"do"`
	Decide int64  `json:"decide"`
	None   int64  `json:"none"`
	Other  int64  `json:"other"`
	Total  int64  `json:"total"`
}

// CommsMetrics is the read-only comms-discipline health snapshot behind
// GET /api/metrics/comms (comms S4). It answers: are we hitting the no-wake
// target, which senders drive wakes, how fast do agents actually read, and how
// many wakes could have been coalesced. Every field is best-effort — a failing
// sub-query leaves its slice/counter at zero, never 500s the scrape.
type CommsMetrics struct {
	Project          string  `json:"project"`
	WindowHours      float64 `json:"window_hours"`
	Since            string  `json:"since"`
	TargetNoWakeRate float64 `json:"target_no_wake_share"`

	// Deliveries is the wake-rate view. Total is every delivery in the window (the
	// naive "everything wakes" pre-discipline baseline); WouldWake is how many
	// still wake under the guard-first predicate; NoWake = Total-WouldWake, the
	// share routed silent. NoWakeShare is the headline the >=55% target tracks.
	Deliveries struct {
		Total       int64   `json:"total"`
		WouldWake   int64   `json:"would_wake"`
		NoWake      int64   `json:"no_wake"`
		NoWakeShare float64 `json:"no_wake_share"`
	} `json:"deliveries"`

	// ActionRequiredBySender is per-sender message counts by effective action tag
	// (one row per sender), so an over-waking sender is visible by name. Computed
	// over messages (a send decision), not deliveries (a fan-out).
	ActionRequiredBySender []SenderActionDist `json:"action_required_by_sender"`

	// ReadLatency buckets read delay = message_reads.read_at - messages.created_at
	// (the live per-agent read receipt; messages.read_at is the DEAD column and is
	// never touched here). Sampled is the count with a usable receipt.
	ReadLatency struct {
		Under1m int64 `json:"under_1m"`
		OneTo5m int64 `json:"one_to_5m"`
		Over5m  int64 `json:"over_5m"`
		Sampled int64 `json:"sampled"`
	} `json:"read_latency"`

	// CoalescingCandidates counts wake-eligible deliveries that landed within 5min
	// of a PRIOR wake-eligible delivery to the same recipient — an estimate of how
	// many wakes a batching window could have merged.
	CoalescingCandidates int64 `json:"coalescing_candidates"`

	// MsgLength summarises message CONTENT size (SQLite LENGTH = character count)
	// over the window — the comms-discipline analysis found verbosity is
	// substance-length, not preamble, so this is the metric that actually tracks
	// (ASCII dev chatter makes chars≈bytes). Avg is the mean;
	// P50/P90 are the median and 90th percentile; Sampled is the message count the
	// percentiles are computed over.
	MsgLength struct {
		Avg     float64 `json:"avg"`
		P50     int64   `json:"p50"`
		P90     int64   `json:"p90"`
		Sampled int64   `json:"sampled"`
	} `json:"msg_length"`
}

// CommsMetricsSnapshot computes a CommsMetrics for one project over the last
// `window`. Best-effort like MetricsSnapshot: each query independently degrades
// to zero rather than failing the whole snapshot. Read-only (RO pool); NO
// hot-path write — safe to scrape on a timer.
func (d *DB) CommsMetricsSnapshot(project string, window time.Duration) CommsMetrics {
	var m CommsMetrics
	m.Project = project
	m.WindowHours = window.Hours()
	m.TargetNoWakeRate = 0.55
	since := time.Now().UTC().Add(-window).Format(memoryTimeFmt)
	m.Since = since
	ro := d.ro()

	// Wake-rate over deliveries.
	_ = ro.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN `+wakePred("m")+` THEN 1 ELSE 0 END), 0)
		FROM deliveries d JOIN messages m ON d.message_id = m.id
		WHERE d.project = ? AND d.created_at >= ?`,
		project, since,
	).Scan(&m.Deliveries.Total, &m.Deliveries.WouldWake)
	m.Deliveries.NoWake = m.Deliveries.Total - m.Deliveries.WouldWake
	if m.Deliveries.Total > 0 {
		m.Deliveries.NoWakeShare = float64(m.Deliveries.NoWake) / float64(m.Deliveries.Total)
	}

	// Per-sender action-tag distribution over messages (the send decision).
	if rows, err := ro.Query(`
		SELECT from_agent, COALESCE(action_required, 'do') AS tag, COUNT(*)
		FROM messages
		WHERE project = ? AND created_at >= ?
		GROUP BY from_agent, tag`,
		project, since,
	); err == nil {
		bySender := map[string]*SenderActionDist{}
		var order []string
		for rows.Next() {
			var from, tag string
			var n int64
			if err := rows.Scan(&from, &tag, &n); err != nil {
				continue
			}
			s := bySender[from]
			if s == nil {
				s = &SenderActionDist{From: from}
				bySender[from] = s
				order = append(order, from)
			}
			switch tag {
			case "ask":
				s.Ask += n
			case "do":
				s.Do += n
			case "decide":
				s.Decide += n
			case "none":
				s.None += n
			default:
				s.Other += n
			}
			s.Total += n
		}
		_ = rows.Close()
		for _, from := range order {
			m.ActionRequiredBySender = append(m.ActionRequiredBySender, *bySender[from])
		}
	}

	// Read-latency buckets from the live message_reads receipt. julianday parses
	// both timestamp formats (RFC3339 receipt, microsecond message); a negative
	// delta (clock skew) is dropped by the outer WHERE.
	_ = ro.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN lat < 60 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN lat >= 60 AND lat < 300 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN lat >= 300 THEN 1 ELSE 0 END), 0),
		       COUNT(*)
		FROM (
			SELECT (julianday(mr.read_at) - julianday(m.created_at)) * 86400.0 AS lat
			FROM message_reads mr JOIN messages m ON mr.message_id = m.id
			WHERE m.project = ? AND m.created_at >= ? AND mr.read_at IS NOT NULL
		) WHERE lat >= 0`,
		project, since,
	).Scan(&m.ReadLatency.Under1m, &m.ReadLatency.OneTo5m, &m.ReadLatency.Over5m, &m.ReadLatency.Sampled)

	// Coalescing candidates: a wake-eligible delivery within 5min AFTER a prior
	// distinct wake-eligible delivery to the same recipient.
	_ = ro.QueryRow(`
		SELECT COUNT(*)
		FROM deliveries d2 JOIN messages m2 ON d2.message_id = m2.id
		WHERE d2.project = ? AND d2.created_at >= ?
		  AND `+wakePred("m2")+`
		  AND EXISTS (
			SELECT 1 FROM deliveries d1 JOIN messages m1 ON d1.message_id = m1.id
			WHERE d1.to_agent = d2.to_agent AND d1.project = d2.project
			  AND d1.message_id != d2.message_id
			  AND `+wakePred("m1")+`
			  AND d1.created_at < d2.created_at
			  AND (julianday(d2.created_at) - julianday(d1.created_at)) <= (300.0 / 86400.0)
		  )`,
		project, since,
	).Scan(&m.CoalescingCandidates)

	// Message-length distribution (content char count = verbosity proxy). Avg + count
	// in one pass; the p50/p90 are read off the ordered set by OFFSET (SQLite has
	// no PERCENTILE_CONT). Best-effort: a failing sub-query leaves zeros. Same
	// project/window scoping as the rest, so unscoped (project='') samples nothing.
	_ = ro.QueryRow(
		`SELECT COALESCE(AVG(LENGTH(content)), 0), COUNT(*)
		 FROM messages WHERE project = ? AND created_at >= ?`,
		project, since,
	).Scan(&m.MsgLength.Avg, &m.MsgLength.Sampled)
	if m.MsgLength.Sampled > 0 {
		pick := func(frac float64) int64 {
			off := int64(frac * float64(m.MsgLength.Sampled-1))
			var v int64
			_ = ro.QueryRow(
				`SELECT LENGTH(content) FROM messages
				 WHERE project = ? AND created_at >= ?
				 ORDER BY LENGTH(content) LIMIT 1 OFFSET ?`,
				project, since, off,
			).Scan(&v)
			return v
		}
		m.MsgLength.P50 = pick(0.5)
		m.MsgLength.P90 = pick(0.9)
	}

	return m
}
