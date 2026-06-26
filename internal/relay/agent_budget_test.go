package relay

import (
	"path/filepath"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// TestCheckBudgets covers TSU-53 slice-C: an agent over its per-day token quota
// fires a budget-exceeded event (once/hour, deduped), and the quota measures
// real tokens (tokenSum), not raw bytes.
func TestCheckBudgets(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.NewTestDB(dbPath)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	events := NewEventBus()
	h := NewHandlers(database, NewSessionRegistry(nil), nil, events)

	const project, agent = "p1", "greedy"
	if err := database.SetAgentQuota(project, agent, 1000, 0, 0, 0); err != nil { // 1000 tokens/day
		t.Fatalf("quota: %v", err)
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	if err := database.InsertTokenUsageBatch([]db.TokenRecord{
		{Project: project, Agent: agent, Input: 1500, Output: 600, CreatedAt: now}, // 2100 > 1000
	}); err != nil {
		t.Fatalf("tokens: %v", err)
	}

	// Quota measures real tokens now (tokenSum), so it's over.
	allowed, used, limit := database.CheckQuota(project, agent, "tokens")
	if allowed || used != 2100 || limit != 1000 {
		t.Fatalf("CheckQuota = (allowed=%v used=%d limit=%d), want (false, 2100, 1000)", allowed, used, limit)
	}

	sub := events.Subscribe()
	defer events.Unsubscribe(sub)

	h.checkBudgets([]db.TokenRecord{{Project: project, Agent: agent}})
	select {
	case e := <-sub:
		if e.Type != "event:budget-exceeded" || e.Semantic["agent"] != agent {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected budget-exceeded event, none fired")
	}

	// Dedup: a second check within the hour fires nothing.
	h.checkBudgets([]db.TokenRecord{{Project: project, Agent: agent}})
	select {
	case e := <-sub:
		t.Fatalf("duplicate budget event within the hour: %+v", e)
	case <-time.After(300 * time.Millisecond):
		// good — deduped
	}
}
