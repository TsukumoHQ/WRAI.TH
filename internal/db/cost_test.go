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

// TestGetCostByAgent_BytesFallback covers the legacy path: a bytes-only row (no
// real token counts) is estimated at bytes/4 tokens priced at the input rate.
func TestGetCostByAgent_BytesFallback(t *testing.T) {
	d := testDB(t)
	const project = "p2"
	const since = "2020-01-01T00:00:00.000000Z"
	d.SetSetting("cost_default_model", "opus")

	// 4M bytes → 1M estimated tokens at opus input ($5/MTok) = $5.00.
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: project, Agent: "dave", Tool: "send_message", Bytes: 4_000_000, CreatedAt: "2026-06-26T00:00:00.000000Z"},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	costs, err := d.GetCostByAgent(project, since)
	if err != nil || len(costs) != 1 {
		t.Fatalf("cost: len=%d err=%v", len(costs), err)
	}
	if costs[0].Tokens != 1_000_000 {
		t.Fatalf("est tokens = %d, want 1_000_000 (bytes/4)", costs[0].Tokens)
	}
	if math.Abs(costs[0].USD-5.0) > 1e-6 {
		t.Fatalf("bytes-fallback cost = %.4f, want 5.00", costs[0].USD)
	}
}

// TestGetCostByDay covers TSU-153 "spend by day": real-token usage rolled up to
// $ per UTC day, priced per model tier, sorted oldest→newest.
func TestGetCostByDay(t *testing.T) {
	d := testDB(t)
	const project = "p1"
	const since = "2020-01-01T00:00:00.000000Z"

	if err := d.InsertTokenUsageBatch([]TokenRecord{
		// 2026-06-26: opus 1M output = $25
		{Project: project, Agent: "alice", Model: "claude-opus-4-8", Output: 1_000_000, CreatedAt: "2026-06-26T09:00:00.000000Z"},
		// 2026-06-26: sonnet 1M input = $3 (same day, different agent/model → coalesces into the day)
		{Project: project, Agent: "bob", Model: "claude-sonnet-4-6", Input: 1_000_000, CreatedAt: "2026-06-26T20:00:00.000000Z"},
		// 2026-06-27: sonnet 1M output = $15
		{Project: project, Agent: "alice", Model: "claude-sonnet-4-6", Output: 1_000_000, CreatedAt: "2026-06-27T10:00:00.000000Z"},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	days, err := d.GetCostByDay(project, since)
	if err != nil {
		t.Fatalf("cost by day: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("want 2 day buckets, got %d: %+v", len(days), days)
	}
	// Ascending by day.
	if days[0].Day != "2026-06-26" || days[1].Day != "2026-06-27" {
		t.Fatalf("days not sorted ascending: %q, %q", days[0].Day, days[1].Day)
	}
	if math.Abs(days[0].USD-28.0) > 1e-6 { // $25 + $3
		t.Fatalf("2026-06-26 cost = %.4f, want 28.00", days[0].USD)
	}
	if math.Abs(days[1].USD-15.0) > 1e-6 {
		t.Fatalf("2026-06-27 cost = %.4f, want 15.00", days[1].USD)
	}
}
