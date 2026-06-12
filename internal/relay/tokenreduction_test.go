package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// resultBytes returns the size of a tool result's text payload — what the
// model actually pays in context tokens (≈ bytes/4).
func resultBytes(t *testing.T, res *mcp.CallToolResult) int {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content[0].(mcp.TextContent).Text)
	}
	return len(res.Content[0].(mcp.TextContent).Text)
}

// seedColony populates a project with a realistic mid-sprint load:
// 10 agents, 30 messages to one inbox, 25 tasks, 15 memories.
func seedColony(t *testing.T, h *Handlers, project string) {
	t.Helper()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{
			"project": project,
			"name":    fmt.Sprintf("worker-%d", i),
			"role":    "Backend developer on the payments squad",
			"description": "Implementing the refund pipeline: webhook ingestion, ledger entries, " +
				"reconciliation job, and the operator dashboard for manual overrides.",
		}))
	}
	for i := 0; i < 30; i++ {
		_, _ = h.HandleSendMessage(ctx, call(map[string]any{
			"project": project, "as": fmt.Sprintf("worker-%d", i%9+1),
			"to": "worker-0", "type": "notification", "priority": "P2",
			"subject": fmt.Sprintf("Refund pipeline status update %d", i),
			"content": "Webhook ingestion is deployed to staging. The ledger writer is behind a " +
				"feature flag; reconciliation runs hourly. Next: operator dashboard wiring and the " +
				"alerting rules for failed refunds over the threshold.",
		}))
	}
	for i := 0; i < 25; i++ {
		_, _ = h.HandleDispatchTask(ctx, call(map[string]any{
			"project": project, "as": "worker-0", "profile": "backend",
			"title": fmt.Sprintf("Implement reconciliation step %d", i),
			"description": "Compare ledger entries against provider settlement reports, flag " +
				"mismatches above 0.01 CHF, and emit a daily summary to the operators channel.",
			"priority": "P2",
		}))
	}
	for i := 0; i < 15; i++ {
		_, _ = h.HandleSetMemory(ctx, call(map[string]any{
			"project": project, "as": "worker-0", "scope": "project",
			"key": fmt.Sprintf("refund-rule-%d", i),
			"value": "Refunds above 500 CHF require a second operator approval and are queued in " +
				"the manual-review board; below that they auto-execute after the fraud check passes.",
			"tags": []any{"payments", "refunds"},
		}))
	}
}

// TestTokenReductionTableVsJSON measures the real payload shrink of the
// markdown-table default against format=json on seeded data, per tool.
func TestTokenReductionTableVsJSON(t *testing.T) {
	h := testHandlers(t)
	seedColony(t, h, "bench")
	ctx := context.Background()

	cases := []struct {
		tool    string
		handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
		args    map[string]any
		minGain float64 // fraction of bytes that must disappear in table mode
	}{
		// unread_only=false: fetching the inbox flips read state, so the second
		// fetch would be empty and the comparison meaningless.
		{"get_inbox", h.HandleGetInbox, map[string]any{"project": "bench", "as": "worker-0", "limit": 30, "unread_only": false}, 0.20},
		{"list_tasks", h.HandleListTasks, map[string]any{"project": "bench"}, 0.30},
		{"list_agents", h.HandleListAgents, map[string]any{"project": "bench"}, 0.15},
		{"list_memories", h.HandleListMemories, map[string]any{"project": "bench"}, 0.25},
	}

	for _, tc := range cases {
		jsonArgs := make(map[string]any, len(tc.args)+1)
		for k, v := range tc.args {
			jsonArgs[k] = v
		}
		jsonArgs["format"] = "json"
		jsonRes, err := tc.handler(ctx, call(jsonArgs))
		if err != nil {
			t.Fatalf("%s json: %v", tc.tool, err)
		}
		jsonSize := resultBytes(t, jsonRes)

		tableArgs := make(map[string]any, len(tc.args)+1)
		for k, v := range tc.args {
			tableArgs[k] = v
		}
		tableArgs["format"] = "md"
		tableRes, err := tc.handler(ctx, call(tableArgs))
		if err != nil {
			t.Fatalf("%s table: %v", tc.tool, err)
		}
		tableSize := resultBytes(t, tableRes)

		gain := 1 - float64(tableSize)/float64(jsonSize)
		t.Logf("%-14s json=%6dB (~%4d tok)  md=%6dB (~%4d tok)  saved %.0f%%",
			tc.tool, jsonSize, jsonSize/4, tableSize, tableSize/4, gain*100)
		if gain < tc.minGain {
			t.Errorf("%s: md saves only %.0f%% (expected ≥ %.0f%%)", tc.tool, gain*100, tc.minGain*100)
		}
	}
}

// TestTokenReductionDiscoveryVsFull measures session-init schema cost:
// full exposure vs the ?tools=discovery pair, and the worst follow-up cost
// (discovery pair + the largest single category).
func TestTokenReductionDiscoveryVsFull(t *testing.T) {
	h := testHandlers(t)

	fullSize := 0
	for _, rt := range h.toolRegistry() {
		b, _ := json.Marshal(rt.Tool)
		fullSize += len(b)
	}

	pairSize := 0
	for _, tool := range []mcp.Tool{discoverToolsTool(), callToolTool()} {
		b, _ := json.Marshal(tool)
		pairSize += len(b)
	}

	largestCategory := 0
	for _, c := range toolCategories {
		res, err := h.HandleDiscoverTools(context.Background(), call(map[string]any{"category": c.name}))
		if err != nil {
			t.Fatalf("discover %s: %v", c.name, err)
		}
		if size := resultBytes(t, res); size > largestCategory {
			largestCategory = size
		}
	}

	initGain := 1 - float64(pairSize)/float64(fullSize)
	worstCase := pairSize + largestCategory
	worstGain := 1 - float64(worstCase)/float64(fullSize)

	t.Logf("session init: full=%dB (~%d tok)  discovery=%dB (~%d tok)  saved %.0f%%",
		fullSize, fullSize/4, pairSize, pairSize/4, initGain*100)
	t.Logf("worst case (pair + largest category payload): %dB (~%d tok)  saved %.0f%%",
		worstCase, worstCase/4, worstGain*100)

	if initGain < 0.90 {
		t.Errorf("discovery init saves only %.0f%% (expected ≥ 90%%)", initGain*100)
	}
	if worstGain < 0.50 {
		t.Errorf("discovery worst case saves only %.0f%% (expected ≥ 50%%)", worstGain*100)
	}
}
