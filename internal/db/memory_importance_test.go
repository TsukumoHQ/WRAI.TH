package db

import (
	"testing"
	"time"

	"agent-relay/internal/models"
)

const testNow = "2026-08-18T12:00:00.000000Z"

func mem(layer, confidence string, version int, updatedAt string) *models.Memory {
	return &models.Memory{
		Layer:      layer,
		Confidence: confidence,
		Version:    version,
		UpdatedAt:  updatedAt,
	}
}

func TestScoreImportanceInUnitRange(t *testing.T) {
	w := defaultImportanceWeights()
	cases := []*models.Memory{
		mem("constraints", "observed", 9, testNow),
		mem("context", "stated", 1, "2020-01-01T00:00:00.000000Z"),
		mem("", "", 0, ""),
		mem("behavior", "inferred", 3, "2026-08-11T12:00:00.000000Z"),
	}
	for _, m := range cases {
		got := scoreImportance(m, testNow, w)
		if got < 0 || got > 1 {
			t.Fatalf("importance %v out of [0,1] for %+v", got, m)
		}
	}
}

func TestConstraintOutranksContext(t *testing.T) {
	w := defaultImportanceWeights()
	hi := scoreImportance(mem("constraints", "observed", 5, testNow), testNow, w)
	lo := scoreImportance(mem("context", "stated", 1, testNow), testNow, w)
	if hi <= lo {
		t.Fatalf("constraints/observed (%v) should outrank context/stated (%v)", hi, lo)
	}
}

func TestRecencyDecayHalvesAtHalfLife(t *testing.T) {
	w := importanceWeights{recency: 1, halfLifeH: 168} // recency-only
	fresh := scoreImportance(mem("context", "stated", 1, testNow), testNow, w)
	// one half-life (7 days) earlier
	old := scoreImportance(mem("context", "stated", 1, "2026-08-11T12:00:00.000000Z"), testNow, w)
	if fresh <= old {
		t.Fatalf("fresh (%v) should exceed one-half-life-old (%v)", fresh, old)
	}
	// at exactly one half-life recency sub-score is 0.5 → with recency-only weights, importance ≈ 0.5
	if old < 0.45 || old > 0.55 {
		t.Fatalf("half-life-old recency importance = %v, want ~0.5", old)
	}
}

func TestFutureTimestampTreatedAsFresh(t *testing.T) {
	w := importanceWeights{recency: 1, halfLifeH: 168}
	future := time.Now().Add(48 * time.Hour).UTC().Format(memoryTimeFmt)
	got := scoreImportance(mem("context", "stated", 1, future), testNow, w)
	if got < 0.99 {
		t.Fatalf("future updated_at should score ~1.0 (fresh), got %v", got)
	}
}

func TestZeroWeightsScoreZero(t *testing.T) {
	w := importanceWeights{}
	if got := scoreImportance(mem("constraints", "observed", 9, testNow), testNow, w); got != 0 {
		t.Fatalf("all-zero weights should score 0, got %v", got)
	}
}

func TestVersionSalienceMonotoneAndCapped(t *testing.T) {
	if versionSalience(1) != 0 {
		t.Fatalf("version 1 should score 0")
	}
	if versionSalience(versionCap) != 1.0 {
		t.Fatalf("version at cap should score 1.0")
	}
	if versionSalience(versionCap+10) != 1.0 {
		t.Fatalf("version past cap should stay 1.0")
	}
	if versionSalience(3) <= versionSalience(2) {
		t.Fatalf("version salience should increase with version")
	}
}

func TestResolveImportanceWeightsEnvOverlay(t *testing.T) {
	env := map[string]string{
		"RELAY_MEM_W_LAYER":                "0.9",
		"RELAY_MEM_W_VERSION":              "not-a-number", // ignored → default
		"RELAY_MEM_RECENCY_HALFLIFE_HOURS": "-5",           // invalid → default
	}
	w := resolveImportanceWeights(func(k string) string { return env[k] })
	if w.layer != 0.9 {
		t.Fatalf("layer weight override failed: %v", w.layer)
	}
	def := defaultImportanceWeights()
	if w.version != def.version {
		t.Fatalf("garbage version weight should keep default %v, got %v", def.version, w.version)
	}
	if w.halfLifeH != def.halfLifeH {
		t.Fatalf("negative half-life should keep default %v, got %v", def.halfLifeH, w.halfLifeH)
	}
}
