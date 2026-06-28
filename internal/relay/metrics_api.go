package relay

import (
	"net/http"
	"os"
	"time"
)

// apiGetMetrics serves GET /api/metrics — a lightweight JSON ops snapshot so a
// monitor can scrape relay health without shelling in (TSU-141). Sits behind the
// auth chain (loopback-exempt): a local scraper needs no token, a remote one
// needs RELAY_API_KEY. Read-only + best-effort: never 500s on one bad count.
func (r *Relay) apiGetMetrics(w http.ResponseWriter, _ *http.Request) {
	m := r.DB.MetricsSnapshot()
	out := map[string]any{
		"version":        r.Version,
		"uptime_seconds": int64(time.Since(r.StartedAt).Seconds()),
		"agents": map[string]any{
			"active": m.AgentsActive,
			"total":  m.AgentsTotal,
		},
		"messages": map[string]any{
			"total":              m.MessagesTotal,
			"last_minute":        m.MessagesLastMinute,
			"last_hour":          m.MessagesLastHour,
			"rate_per_minute":    m.MessagesLastMinute,
			"expired_pending_gc": m.MessagesExpiredPendingGC,
		},
		"deliveries": map[string]any{"unacked": m.DeliveriesUnacked},
		"tasks":      map[string]any{"open": m.TasksOpen},
		"tables": map[string]any{
			"audit_log": m.AuditLogRows,
			"memories":  m.MemoriesRows,
		},
		"db": dbFileMetrics(r.DB.Path()),
	}
	// Linear connector freshness (last reconcile/webhook, writer failures) when active.
	if c := r.LinearConnector(); c != nil {
		out["linear"] = c.Status()
	}
	writeJSON(w, out)
}

// dbFileMetrics reports the live DB file size and the freshness of the newest
// rotated snapshot (.bak.0) — surfaces backup/GC health (TSU-137/TSU-127) to
// monitoring. Missing files are simply omitted.
func dbFileMetrics(path string) map[string]any {
	out := map[string]any{}
	if fi, err := os.Stat(path); err == nil {
		out["size_bytes"] = fi.Size()
	}
	if fi, err := os.Stat(path + ".bak.0"); err == nil {
		out["snapshot_size_bytes"] = fi.Size()
		out["snapshot_age_seconds"] = int64(time.Since(fi.ModTime()).Seconds())
	}
	return out
}
