package db

import (
	"testing"
	"time"
)

func TestClassifyHealth(t *testing.T) {
	cases := []struct {
		name         string
		idle         time.Duration
		tokensRecent int64
		want         string
	}{
		{"fresh + spending = working", 1 * time.Minute, 5000, "working"},
		{"fresh + quiet = idle", 2 * time.Minute, 0, "idle"},
		{"stale = dead (even if it spent earlier)", 45 * time.Minute, 9999, "dead"},
		{"just past the dead line", healthDeadAfter + time.Second, 0, "dead"},
		{"right at the recent edge, spending", healthRecentWindow, 1, "working"},
	}
	for _, c := range cases {
		if got := classifyHealth(c.idle, c.tokensRecent); got != c.want {
			t.Fatalf("%s: classifyHealth(%v, %d) = %q, want %q", c.name, c.idle, c.tokensRecent, got, c.want)
		}
	}
}

// TestGetAgentHealth_Integration: a freshly-touched agent that spent tokens reads
// "working"; the snapshot carries recent + 24h token totals.
func TestGetAgentHealth_Integration(t *testing.T) {
	d := testDB(t)
	const project = "p1"

	now := time.Now().UTC().Format(memoryTimeFmt)
	if _, err := d.conn.Exec(
		`INSERT INTO agents (id, name, role, registered_at, last_seen, project, status) VALUES (?, ?, 'eng', ?, ?, ?, 'active')`,
		"agent-worker-1", "worker", now, now, project,
	); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: project, Agent: "worker", Input: 1000, Output: 500, CreatedAt: now},
	}); err != nil {
		t.Fatalf("tokens: %v", err)
	}

	hs, err := d.GetAgentHealth(project)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	var w *AgentHealth
	for i := range hs {
		if hs[i].Agent == "worker" {
			w = &hs[i]
		}
	}
	if w == nil {
		t.Fatal("worker missing from health")
	}
	if w.Status != "working" {
		t.Fatalf("status = %q, want working (fresh + spending)", w.Status)
	}
	if w.TokensRecent != 1500 {
		t.Fatalf("tokens_recent = %d, want 1500", w.TokensRecent)
	}
}
