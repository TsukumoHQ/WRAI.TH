package relay

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// statsCacheTTL is the short in-memory cache window for the aggregation endpoint.
// Live updates re-fetch (debounced) on the UI side; this just absorbs bursts.
const statsCacheTTL = 30 * time.Second

type statsCacheEntry struct {
	payload   []byte
	expiresAt time.Time
}

// statsCache is a tiny process-wide cache keyed by project+cycle+agent.
var statsCache = struct {
	mu      sync.Mutex
	entries map[string]statsCacheEntry
}{entries: map[string]statsCacheEntry{}}

// apiGetAgentStats serves GET /api/stats?cycle=<id|all>&agent=<name>&project=<p>.
// Computes agentic analytics from the local task overlay/mirror only.
func (r *Relay) apiGetAgentStats(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	cycle := req.URL.Query().Get("cycle")
	agent := req.URL.Query().Get("agent")

	key := project + "\x00" + cycle + "\x00" + agent

	// Cache hit.
	statsCache.mu.Lock()
	if e, ok := statsCache.entries[key]; ok && time.Now().Before(e.expiresAt) {
		body := e.payload
		statsCache.mu.Unlock()
		w.Header().Set("X-Cache", "hit")
		_, _ = w.Write(body)
		return
	}
	statsCache.mu.Unlock()

	stats, err := r.DB.GetAgentStats(project, cycle, agent)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to compute stats", err)
		return
	}

	// Marshal once, cache the bytes.
	body, err := json.Marshal(stats)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to encode stats", err)
		return
	}
	statsCache.mu.Lock()
	statsCache.entries[key] = statsCacheEntry{payload: body, expiresAt: time.Now().Add(statsCacheTTL)}
	// Opportunistic prune of expired entries to bound memory.
	if len(statsCache.entries) > 256 {
		now := time.Now()
		for k, e := range statsCache.entries {
			if now.After(e.expiresAt) {
				delete(statsCache.entries, k)
			}
		}
	}
	statsCache.mu.Unlock()

	w.Header().Set("X-Cache", "miss")
	_, _ = w.Write(body)
}
