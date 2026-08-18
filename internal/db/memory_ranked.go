package db

import (
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"agent-relay/internal/models"
)

// MemPalace slice 2 — ranked recall.
//
// Default search_memory orders by FTS bm25 alone, so a highly-relevant-but-ancient
// note outranks a slightly-less-textual but load-bearing recent decision. Ranked
// recall composites three signals over the FTS candidate set — relevance (bm25
// order), recency decay, and derived importance (slice 1) — and re-orders in Go.
// It is ADDITIVE: exposed as rank="mempalace" on search_memory; the default order
// is untouched, so anything depending on pure-FTS order keeps working.
//
// Relevance is taken from the candidate's POSITION in the bm25-ordered fetch, not
// a raw bm25 value. That keeps scanMemory (and the memorySelectCols↔scanMemory
// lockstep) untouched — no extra SELECT column — while still ranking monotonically
// in bm25. We over-fetch a candidate window (topK), re-rank, then return `limit`.

// RankedMemory pairs a memory with its composite rank score so callers can show
// why an entry surfaced. RankScore is transient (never stored).
type RankedMemory struct {
	models.Memory
	RankScore float64 `json:"rank_score"`
}

// rankWeights tunes the three ranked-recall signals. Each sub-score is in [0,1];
// the composite is their weighted average, so RankScore is [0,1] for non-negative
// weights. Recency deliberately appears here AND inside importance — the standalone
// term lets ranked recall favor freshness more strongly than raw importance does.
type rankWeights struct {
	relevance  float64 // RELAY_MEM_RANK_W_RELEVANCE
	recency    float64 // RELAY_MEM_RANK_W_RECENCY
	importance float64 // RELAY_MEM_RANK_W_IMPORTANCE
}

func defaultRankWeights() rankWeights {
	return rankWeights{relevance: 0.5, recency: 0.2, importance: 0.3}
}

var (
	rankWeightsOnce   sync.Once
	rankWeightsCached rankWeights
)

func activeRankWeights() rankWeights {
	rankWeightsOnce.Do(func() {
		rankWeightsCached = resolveRankWeights(os.Getenv)
	})
	return rankWeightsCached
}

// resolveRankWeights overlays env onto defaults; garbage/negative values keep the
// default. Split from activeRankWeights (which memoizes) for deterministic tests.
func resolveRankWeights(getenv func(string) string) rankWeights {
	w := defaultRankWeights()
	overlay := func(key string, dst *float64) {
		v := getenv(key)
		if v == "" {
			return
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			*dst = f
		}
	}
	overlay("RELAY_MEM_RANK_W_RELEVANCE", &w.relevance)
	overlay("RELAY_MEM_RANK_W_RECENCY", &w.recency)
	overlay("RELAY_MEM_RANK_W_IMPORTANCE", &w.importance)
	return w
}

// rankedCandidateWindow is how many bm25-ordered candidates to over-fetch before
// re-ranking. Bounded so the in-Go sort stays cheap and the single sqlite read
// stays small — no hot-path cost beyond the existing FTS query.
func rankedCandidateWindow(limit int) int {
	k := limit * 4
	if k < 40 {
		k = 40
	}
	if k > 200 {
		k = 200
	}
	return k
}

// SearchMemoryRanked runs the same scope/tag/validity-filtered FTS search as
// SearchMemory but re-ranks the candidate window by the composite (relevance +
// recency + importance) and returns the top `limit`. Non-mutating additive path.
func (d *DB) SearchMemoryRanked(project, agentName, query string, tags []string, scope string, limit int, includeStale ...bool) ([]RankedMemory, error) {
	if limit <= 0 {
		limit = 20
	}
	stale := len(includeStale) > 0 && includeStale[0]
	topK := rankedCandidateWindow(limit)

	sql, args := d.buildMemorySearchQuery(project, agentName, query, tags, scope, topK, stale)
	candidates, err := d.queryMemories(sql, args...)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(memoryTimeFmt)
	w := activeRankWeights()
	hl := activeImportanceWeights().halfLifeH
	n := len(candidates)

	ranked := make([]RankedMemory, n)
	for i, m := range candidates {
		ranked[i] = RankedMemory{
			Memory:    m,
			RankScore: scoreRank(&candidates[i], i, n, now, hl, w),
		}
	}

	// Stable sort by score desc so ties keep the bm25 (relevance) order.
	sort.SliceStable(ranked, func(a, b int) bool {
		return ranked[a].RankScore > ranked[b].RankScore
	})

	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked, nil
}

// scoreRank composites the three normalized signals for one candidate at bm25
// position `pos` within a window of `total`. Pure and deterministic given now/hl.
func scoreRank(m *models.Memory, pos, total int, now string, halfLifeH float64, w rankWeights) float64 {
	sum := w.relevance + w.recency + w.importance
	if sum <= 0 {
		return 0
	}
	rel := relevanceFromPosition(pos, total)
	rec := recencySalience(m.UpdatedAt, now, halfLifeH)
	imp := m.Importance // stamped by scanMemory (slice 1)
	score := (w.relevance*rel + w.recency*rec + w.importance*imp) / sum
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

// relevanceFromPosition maps a 0-based bm25 rank position to [0,1]: the top hit
// scores 1.0, the last of the window scores ~0, monotonically decreasing. A
// single-item window scores 1.0.
func relevanceFromPosition(pos, total int) float64 {
	if total <= 1 {
		return 1.0
	}
	return float64(total-1-pos) / float64(total-1)
}
