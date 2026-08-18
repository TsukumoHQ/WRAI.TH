package db

import (
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"agent-relay/internal/models"
)

// MemPalace slice 1 — derived importance.
//
// Importance is a salience score in [0,1] computed at READ time from columns the
// store already has: layer, confidence, recency (updated_at) and version. It is
// a pure function — no schema change, no write, no hot-path cost — stamped onto
// every Memory by scanMemory. Ranked recall (slice 2) and capacity eviction
// (slice 3) all share THIS one definition so "which memory matters" means the
// same thing everywhere.
//
// Deliberately excluded: access-frequency signals (access_count /
// last_accessed_at). Those need a write on every recall, which violates the
// single-writer lock discipline; skipped until ranking proves recency+curation
// is insufficient (see design doc 937dd2da).

// importanceWeights tunes the four sub-scores. Each sub-score is normalized to
// [0,1]; the composite is their weighted average (so importance itself is [0,1]
// whenever the weights are non-negative). Defaults are sane; every field is
// overridable via env for fleet-tuning without a redeploy.
type importanceWeights struct {
	layer      float64 // RELAY_MEM_W_LAYER
	confidence float64 // RELAY_MEM_W_CONFIDENCE
	recency    float64 // RELAY_MEM_W_RECENCY
	version    float64 // RELAY_MEM_W_VERSION
	halfLifeH  float64 // RELAY_MEM_RECENCY_HALFLIFE_HOURS — recency decay half-life
}

// defaultImportanceWeights are the shipped defaults: curation (layer) leads,
// recency second, confidence third, version a light tie-breaker. Sum is 1.0 so
// the raw weighted average lands in [0,1] out of the box.
func defaultImportanceWeights() importanceWeights {
	return importanceWeights{
		layer:      0.40,
		confidence: 0.20,
		recency:    0.30,
		version:    0.10,
		halfLifeH:  168, // 7 days
	}
}

var (
	importanceWeightsOnce   sync.Once
	importanceWeightsCached importanceWeights
)

// activeImportanceWeights returns the env-resolved weights, parsed once. Env is
// read a single time (not per row) so scanMemory stays cheap on large result
// sets; an invalid or absent env value keeps the default.
func activeImportanceWeights() importanceWeights {
	importanceWeightsOnce.Do(func() {
		importanceWeightsCached = resolveImportanceWeights(os.Getenv)
	})
	return importanceWeightsCached
}

// resolveImportanceWeights overlays env values onto the defaults. Split out from
// activeImportanceWeights (which memoizes) so tests can exercise env parsing
// deterministically without the sync.Once cache.
func resolveImportanceWeights(getenv func(string) string) importanceWeights {
	w := defaultImportanceWeights()
	overlay := func(key string, dst *float64, mustBePositive bool) {
		v := getenv(key)
		if v == "" {
			return
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || (mustBePositive && f == 0) {
			return // ignore garbage / negative / zero-halflife; keep default
		}
		*dst = f
	}
	overlay("RELAY_MEM_W_LAYER", &w.layer, false)
	overlay("RELAY_MEM_W_CONFIDENCE", &w.confidence, false)
	overlay("RELAY_MEM_W_RECENCY", &w.recency, false)
	overlay("RELAY_MEM_W_VERSION", &w.version, false)
	overlay("RELAY_MEM_RECENCY_HALFLIFE_HOURS", &w.halfLifeH, true)
	return w
}

// layerSalience ranks the curation tiers: a hard constraint outweighs a settled
// decision, which outweighs an adaptable behavior, which outweighs ephemeral
// context. Unknown layers score as context (lowest) rather than 0 so a future
// layer name never reads as "maximally unimportant" by accident.
func layerSalience(layer string) float64 {
	switch layer {
	case "constraints":
		return 1.0
	case "decision":
		return 0.75
	case "behavior":
		return 0.50
	default: // "context" and anything unrecognized
		return 0.25
	}
}

// confidenceSalience ranks how the knowledge was obtained: observed > inferred >
// stated. Unknown confidence scores as stated (lowest).
func confidenceSalience(confidence string) float64 {
	switch confidence {
	case "observed":
		return 1.0
	case "inferred":
		return 0.6
	default: // "stated" and anything unrecognized
		return 0.3
	}
}

// versionCap bounds the version sub-score: past this many revisions a memory is
// "well-curated" and further edits stop raising importance.
const versionCap = 5

// versionSalience rewards curation depth with diminishing returns, capped so a
// runaway version counter cannot dominate the composite.
func versionSalience(version int) float64 {
	if version <= 1 {
		return 0
	}
	if version >= versionCap {
		return 1.0
	}
	return float64(version-1) / float64(versionCap-1)
}

// recencySalience applies exponential decay with a configurable half-life: a
// memory updated "now" scores 1.0, one a half-life old scores 0.5, and so on.
// A memory with a future or unparseable updated_at scores 1.0 (treat as fresh
// rather than penalize a clock skew).
func recencySalience(updatedAt, now string, halfLifeH float64) float64 {
	if halfLifeH <= 0 {
		return 1.0
	}
	u, ok := parseMemoryTime(updatedAt)
	if !ok {
		return 1.0
	}
	n, ok := parseMemoryTime(now)
	if !ok {
		return 1.0
	}
	tu, err1 := time.Parse(memoryTimeFmt, u)
	tn, err2 := time.Parse(memoryTimeFmt, n)
	if err1 != nil || err2 != nil {
		return 1.0
	}
	ageH := tn.Sub(tu).Hours()
	if ageH <= 0 {
		return 1.0
	}
	// 0.5 ^ (age / halfLife) == 2 ** (-age/halfLife)
	return math.Exp2(-ageH / halfLifeH)
}

// scoreImportance is the pure composite: a weighted average of the four
// normalized sub-scores, clamped to [0,1]. It takes explicit weights and `now`
// so it is fully deterministic and unit-testable without env or the clock.
func scoreImportance(m *models.Memory, now string, w importanceWeights) float64 {
	sum := w.layer + w.confidence + w.recency + w.version
	if sum <= 0 {
		return 0
	}
	weighted := w.layer*layerSalience(m.Layer) +
		w.confidence*confidenceSalience(m.Confidence) +
		w.recency*recencySalience(m.UpdatedAt, now, w.halfLifeH) +
		w.version*versionSalience(m.Version)
	score := weighted / sum
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}
