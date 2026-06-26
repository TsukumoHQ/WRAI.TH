package db

import (
	"math"
	"testing"
)

// TestGetCostByAgent covers TSU-53 slice-A: per-agent $ rollup priced per model
// tier, with cache-read billed at 0.1× input (not flat), and a default-tier
// fallback when the row has no model.
func TestGetCostByAgent(t *testing.T) {
	d := testDB(t)
	const project = "p1"
	const ts = "2026-06-26T00:00:00.000000Z"
	const since = "2020-01-01T00:00:00.000000Z"

	err := d.InsertTokenUsageBatch([]TokenRecord{
		// opus: 1M input ($5) + 1M output ($25) + 1M cache_read (0.1×5 = $0.50) = $30.50
		{Project: project, Agent: "alice", Model: "claude-opus-4-8", Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000, CreatedAt: ts},
		// sonnet: 1M input ($3)
		{Project: project, Agent: "bob", Model: "claude-sonnet-4-6", Input: 1_000_000, CreatedAt: ts},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	costs, err := d.GetCostByAgent(project, since)
	if err != nil {
		t.Fatalf("cost: %v", err)
	}
	if len(costs) != 2 {
		t.Fatalf("want 2 agents, got %d", len(costs))
	}
	// Sorted by $ desc → alice first.
	if costs[0].Agent != "alice" {
		t.Fatalf("want alice first (higher $), got %q", costs[0].Agent)
	}
	if math.Abs(costs[0].USD-30.50) > 1e-6 {
		t.Fatalf("alice cost = %.4f, want 30.50", costs[0].USD)
	}
	if math.Abs(costs[1].USD-3.0) > 1e-6 {
		t.Fatalf("bob cost = %.4f, want 3.00", costs[1].USD)
	}

	// Default-tier fallback: a model-less row prices at the configured default.
	d.SetSetting("cost_default_model", "haiku")
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: project, Agent: "carol", Model: "", Input: 1_000_000, CreatedAt: ts}, // haiku input $1
	}); err != nil {
		t.Fatalf("insert carol: %v", err)
	}
	costs, _ = d.GetCostByAgent(project, since)
	var carol float64 = -1
	for _, c := range costs {
		if c.Agent == "carol" {
			carol = c.USD
		}
	}
	if math.Abs(carol-1.0) > 1e-6 {
		t.Fatalf("carol (model-less, default=haiku) cost = %.4f, want 1.00", carol)
	}
}
