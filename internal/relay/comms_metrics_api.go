package relay

import (
	"net/http"
	"strconv"
	"time"
)

// apiGetCommsMetrics serves GET /api/metrics/comms?project=<p>&hours=<h> — the
// read-only comms-discipline health view (comms S4). It reports the no-wake
// share against the >=55% target, per-sender action_required distribution,
// read-latency buckets, and coalescing candidates. Sits behind the same auth
// chain as /api/metrics (loopback-exempt) and never writes — safe to scrape.
func (r *Relay) apiGetCommsMetrics(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	window := parseWindowHours(req.URL.Query().Get("hours"))
	writeJSON(w, r.DB.CommsMetricsSnapshot(project, window))
}

// parseWindowHours reads the ?hours= window, defaulting to 168h (7 days) and
// clamping to (0, 8760h] (1 year) so a bad/huge value can't turn the scan into
// an unbounded or empty query.
func parseWindowHours(raw string) time.Duration {
	const def = 168.0
	h := def
	if raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			h = v
		}
	}
	if h > 8760 {
		h = 8760
	}
	return time.Duration(h * float64(time.Hour))
}
