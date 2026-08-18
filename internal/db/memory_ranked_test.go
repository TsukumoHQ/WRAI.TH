package db

import (
	"sort"
	"testing"

	"agent-relay/internal/models"
)

func TestRelevanceFromPositionMonotone(t *testing.T) {
	if relevanceFromPosition(0, 1) != 1.0 {
		t.Fatalf("single-item window should score 1.0")
	}
	if relevanceFromPosition(0, 10) != 1.0 {
		t.Fatalf("top hit should score 1.0")
	}
	if relevanceFromPosition(9, 10) != 0.0 {
		t.Fatalf("last hit should score 0.0")
	}
	if relevanceFromPosition(2, 10) <= relevanceFromPosition(5, 10) {
		t.Fatalf("earlier position must score higher")
	}
}

func TestScoreRankInUnitRange(t *testing.T) {
	w := defaultRankWeights()
	m := &models.Memory{Layer: "constraints", Confidence: "observed", Version: 3, UpdatedAt: testNow, Importance: 0.8}
	for pos := 0; pos < 5; pos++ {
		got := scoreRank(m, pos, 5, testNow, 168, w)
		if got < 0 || got > 1 {
			t.Fatalf("rank score %v out of [0,1] at pos %d", got, pos)
		}
	}
}

func TestScoreRankFavorsImportanceAtEqualPosition(t *testing.T) {
	w := defaultRankWeights()
	hi := &models.Memory{UpdatedAt: testNow, Importance: 0.9}
	lo := &models.Memory{UpdatedAt: testNow, Importance: 0.1}
	// same position + same recency → the more important memory scores higher
	if scoreRank(hi, 0, 5, testNow, 168, w) <= scoreRank(lo, 0, 5, testNow, 168, w) {
		t.Fatalf("higher importance should win at equal position/recency")
	}
}

func TestScoreRankZeroWeightsZero(t *testing.T) {
	if got := scoreRank(&models.Memory{Importance: 1}, 0, 5, testNow, 168, rankWeights{}); got != 0 {
		t.Fatalf("zero weights should score 0, got %v", got)
	}
}

func TestResolveRankWeightsEnvOverlay(t *testing.T) {
	env := map[string]string{
		"RELAY_MEM_RANK_W_RELEVANCE":  "0.7",
		"RELAY_MEM_RANK_W_RECENCY":    "-1",   // invalid → default
		"RELAY_MEM_RANK_W_IMPORTANCE": "junk", // invalid → default
	}
	w := resolveRankWeights(func(k string) string { return env[k] })
	if w.relevance != 0.7 {
		t.Fatalf("relevance override failed: %v", w.relevance)
	}
	def := defaultRankWeights()
	if w.recency != def.recency || w.importance != def.importance {
		t.Fatalf("invalid overrides should keep defaults, got %+v", w)
	}
}

func TestRankedCandidateWindowBounds(t *testing.T) {
	if rankedCandidateWindow(1) != 40 {
		t.Fatalf("small limit should floor at 40")
	}
	if rankedCandidateWindow(100) != 200 {
		t.Fatalf("large limit should cap at 200")
	}
	if rankedCandidateWindow(15) != 60 {
		t.Fatalf("mid limit should be 4x = 60")
	}
}

// Ranked recall is ADDITIVE: it returns the same candidate SET as the default
// FTS search (just reordered), never a smaller or different set. Verifies the
// shared filter path and RankScore population without asserting a brittle order.
func TestSearchMemoryRankedIsAdditive(t *testing.T) {
	d := testDB(t)
	for _, v := range []string{"widget alpha note", "widget beta decision", "widget gamma context"} {
		if _, err := d.SetMemory("p1", "bot-a", v[:12], v, "[]", "project", "observed", "behavior"); err != nil {
			t.Fatalf("set memory: %v", err)
		}
	}

	def, err := d.SearchMemory("p1", "bot-a", "widget", nil, "project", 20)
	if err != nil {
		t.Fatalf("default search: %v", err)
	}
	ranked, err := d.SearchMemoryRanked("p1", "bot-a", "widget", nil, "project", 20)
	if err != nil {
		t.Fatalf("ranked search: %v", err)
	}
	if len(ranked) != len(def) {
		t.Fatalf("ranked set size %d != default %d — additivity broken", len(ranked), len(def))
	}

	idset := func(ids []string) []string { sort.Strings(ids); return ids }
	var defIDs, rIDs []string
	for _, m := range def {
		defIDs = append(defIDs, m.ID)
	}
	for i := range ranked {
		rIDs = append(rIDs, ranked[i].ID)
		if ranked[i].RankScore < 0 || ranked[i].RankScore > 1 {
			t.Fatalf("rank_score out of [0,1]: %v", ranked[i].RankScore)
		}
	}
	dl, rl := idset(defIDs), idset(rIDs)
	for i := range dl {
		if dl[i] != rl[i] {
			t.Fatalf("ranked returned a different id set than default search")
		}
	}

	// Scores must be sorted descending.
	for i := 1; i < len(ranked); i++ {
		if ranked[i-1].RankScore < ranked[i].RankScore {
			t.Fatalf("ranked results not sorted by score desc")
		}
	}
}
