package relay

import (
	"encoding/json"
	"testing"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// decodeToolError extracts and validates the canonical typed-error envelope from
// a tool result: it MUST be an error carrying all four machine fields with the
// right types, so a caller can branch without string-matching.
func decodeToolError(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatal("expected an error result, got success/nil")
	}
	raw := res.Content[0].(mcp.TextContent).Text
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("error is not structured JSON: %v\nraw: %s", err, raw)
	}
	for _, k := range []string{"code", "errorCategory", "isRetryable", "message"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("error envelope missing %q: %s", k, raw)
		}
	}
	if _, ok := body["code"].(string); !ok {
		t.Fatalf("code must be a string: %s", raw)
	}
	cat, _ := body["errorCategory"].(string)
	switch cat {
	case CategoryTransient, CategoryValidation, CategoryPermission:
	default:
		t.Fatalf("errorCategory %q not in taxonomy: %s", cat, raw)
	}
	if _, ok := body["isRetryable"].(bool); !ok {
		t.Fatalf("isRetryable must be a bool: %s", raw)
	}
	if _, ok := body["message"].(string); !ok {
		t.Fatalf("message must be a string: %s", raw)
	}
	return body
}

func TestClassifyMessageTaxonomy(t *testing.T) {
	cases := []struct {
		msg      string
		code     string
		category string
		retry    bool
	}{
		{"title is required", CodeInvalidArgument, CategoryValidation, false},
		{"invalid priority value", CodeInvalidArgument, CategoryValidation, false},
		{"board not found", CodeNotFound, CategoryValidation, false},
		{"task already claimed by another agent", CodeInvalidArgument, CategoryValidation, false},
		{"status changed before claim could apply", CodeInvalidArgument, CategoryValidation, false},
		{"agent is not a member of this team", CodeForbidden, CategoryPermission, false},
		{"broadcast is not allowed for non-admins", CodeForbidden, CategoryPermission, false},
		{"failed to dispatch task: disk error", CodeInternal, CategoryTransient, true},
		{"something unexpected happened", CodeInternal, CategoryTransient, true},
	}
	for _, c := range cases {
		code, cat, retry := classifyMessage(c.msg)
		if code != c.code || cat != c.category || retry != c.retry {
			t.Fatalf("classify(%q) = (%s,%s,%v), want (%s,%s,%v)",
				c.msg, code, cat, retry, c.code, c.category, c.retry)
		}
	}
}

func TestToolResultErrorEnvelope(t *testing.T) {
	body := decodeToolError(t, toolResultError("name is required"))
	if body["code"] != CodeInvalidArgument || body["errorCategory"] != CategoryValidation {
		t.Fatalf("required-arg should be validation/INVALID_ARGUMENT: %v", body)
	}
	if body["isRetryable"] != false {
		t.Fatalf("a validation error must not be retryable: %v", body)
	}
}

func TestTypedConstructorsEnvelope(t *testing.T) {
	decodeToolError(t, validationError("X", "bad"))
	decodeToolError(t, permissionError("Y", "nope"))
	decodeToolError(t, transientError("Z", "later"))
}

func TestSenderInactiveEnvelope(t *testing.T) {
	body := decodeToolError(t, senderInactiveError("bot", "proj", "inactive"))
	if body["code"] != "SENDER_INACTIVE" || body["errorCategory"] != CategoryPermission {
		t.Fatalf("SENDER_INACTIVE should be permission-category: %v", body)
	}
	if body["reason"] != "inactive" {
		t.Fatalf("SENDER_INACTIVE must preserve reason extra: %v", body)
	}
}

func TestTypedTaskErrorEnvelope(t *testing.T) {
	// Every task conflict is NON-retryable (park, don't hot-loop). LEASE_HELD is
	// transient-category (lease lapses) but still isRetryable=false — a live
	// holder owns it, so hot-looping the same reclaim only spins.
	held := decodeToolError(t, typedTaskError(&db.TaskError{Code: db.CodeTaskLeaseHeld, Msg: "held"}))
	if held["errorCategory"] != CategoryTransient || held["isRetryable"] != false {
		t.Fatalf("TASK_LEASE_HELD should be transient but non-retryable (park): %v", held)
	}
	if held["code"] != db.CodeTaskLeaseHeld || held["error"] != db.CodeTaskLeaseHeld {
		t.Fatalf("task error must keep code + legacy error alias: %v", held)
	}
	conflict := decodeToolError(t, typedTaskError(&db.TaskError{Code: db.CodeTaskStateConflict, Msg: "raced"}))
	if conflict["errorCategory"] != CategoryValidation || conflict["isRetryable"] != false {
		t.Fatalf("TASK_STATE_CONFLICT should be validation/non-retryable: %v", conflict)
	}
}

// A representative real handler path: dispatch without profile returns the
// uniform envelope, not a bare string.
func TestHandlerErrorPathUsesEnvelope(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "title": "x"}))
	body := decodeToolError(t, res)
	if body["errorCategory"] != CategoryValidation {
		t.Fatalf("missing-profile dispatch should be a validation error: %v", body)
	}
}

// Regression (review round 1): the LOSER of a double-claim race must get a
// TASK_STATE_CONFLICT that is validation / NON-retryable — otherwise the caller
// hot-loops re-claiming a task it already lost. Before taskOpError, the conflict
// was wrapped as "failed to claim task: %v" and misclassified retryable=true.
func TestDoubleClaimConflictIsNotRetryable(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "bot-a", nil)
	registerActive(t, h, "p1", "bot-b", nil)

	disp, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "race me",
	}))
	if disp.IsError {
		t.Fatalf("dispatch failed: %s", expectError(t, disp))
	}
	var dbody map[string]any
	_ = json.Unmarshal([]byte(disp.Content[0].(mcp.TextContent).Text), &dbody)
	task, _ := dbody["task"].(map[string]any)
	taskID, _ := task["id"].(string)
	if taskID == "" {
		t.Fatalf("no task id in dispatch result: %v", dbody)
	}

	// First claim wins.
	if r, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID})); r.IsError {
		t.Fatalf("first claim should win: %s", expectError(t, r))
	}
	// Second claim loses the race → typed conflict, NOT retryable.
	res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-b", "task_id": taskID}))
	body := decodeToolError(t, res)
	if body["code"] != db.CodeTaskStateConflict {
		t.Fatalf("double-claim loser should get TASK_STATE_CONFLICT, got %v", body)
	}
	if body["errorCategory"] != CategoryValidation || body["isRetryable"] != false {
		t.Fatalf("conflict must be validation/non-retryable (no hot-loop): %v", body)
	}

	// Reclaiming a task whose holder (bot-a) is still LIVE must refuse with
	// TASK_LEASE_HELD and isRetryable=false — park, don't hot-loop against a live
	// holder (round-2 regression: reclaim reported this retryable).
	rec, _ := h.HandleReclaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-b", "task_id": taskID}))
	rbody := decodeToolError(t, rec)
	if rbody["code"] != db.CodeTaskLeaseHeld {
		t.Fatalf("reclaim of a live-held task should get TASK_LEASE_HELD, got %v", rbody)
	}
	if rbody["isRetryable"] != false {
		t.Fatalf("TASK_LEASE_HELD must be non-retryable (park): %v", rbody)
	}
}
