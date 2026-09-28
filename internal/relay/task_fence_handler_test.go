package relay

import (
	"encoding/json"
	"testing"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestTerminalToolsFencedByLeaseGeneration (S3 0b980988): complete_task,
// block_task and review_task surface TASK_LEASE_FENCED (non-retryable) to a
// non-holder and to a stale lease_generation; the holder at the generation
// claim_task returned publishes.
func TestTerminalToolsFencedByLeaseGeneration(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "bot-a", nil)
	registerActive(t, h, "p1", "bot-b", nil)

	disp, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "lead", "profile": "dev", "title": "fence me",
	}))
	if disp.IsError {
		t.Fatalf("dispatch failed: %s", expectError(t, disp))
	}
	var dbody map[string]any
	_ = json.Unmarshal([]byte(disp.Content[0].(mcp.TextContent).Text), &dbody)
	task, _ := dbody["task"].(map[string]any)
	taskID, _ := task["id"].(string)

	claim, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID}))
	if claim.IsError {
		t.Fatalf("claim: %s", expectError(t, claim))
	}
	var cbody map[string]any
	_ = json.Unmarshal([]byte(claim.Content[0].(mcp.TextContent).Text), &cbody)
	current, ok := cbody["lease_generation"].(float64)
	if !ok || current < 1 {
		t.Fatalf("claim result must carry lease_generation >= 1, got %v", cbody["lease_generation"])
	}

	if res, _ := h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID})); res.IsError {
		t.Fatalf("start: %s", expectError(t, res))
	}

	fenced := func(res *mcp.CallToolResult, what string) {
		t.Helper()
		body := decodeToolError(t, res)
		if body["code"] != db.CodeTaskLeaseFenced || body["isRetryable"] != false {
			t.Fatalf("%s: want non-retryable %s, got %v", what, db.CodeTaskLeaseFenced, body)
		}
	}
	res, _ := h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "bot-b", "task_id": taskID}))
	fenced(res, "complete by non-holder")
	res, _ = h.HandleBlockTask(ctx, call(map[string]any{"project": "p1", "as": "bot-b", "task_id": taskID, "reason": "x"}))
	fenced(res, "block by non-holder")
	res, _ = h.HandleReviewTask(ctx, call(map[string]any{"project": "p1", "as": "bot-b", "task_id": taskID}))
	fenced(res, "review by non-holder")
	res, _ = h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID, "lease_generation": current - 1}))
	fenced(res, "complete with stale generation")

	res, _ = h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID, "lease_generation": "one"}))
	if body := decodeToolError(t, res); body["code"] != CodeInvalidArgument {
		t.Fatalf("non-integer lease_generation: want %s, got %v", CodeInvalidArgument, body)
	}

	res, _ = h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID, "lease_generation": current}))
	if res.IsError {
		t.Fatalf("holder complete at current generation: %s", expectError(t, res))
	}
}
