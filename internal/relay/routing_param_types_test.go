package relay

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// wrappedTool returns the toolRegistry handler for name — the SAME wrapped
// handler the MCP server dispatches to in production (guardRoutingParamTypes
// applied). Calling a handler method directly (h.HandleSendMessage) would bypass
// the wrapper, so the routing-param guard must be exercised through this seam.
func wrappedTool(t *testing.T, h *Handlers, name string) server.ToolHandlerFunc {
	t.Helper()
	for _, rt := range h.toolRegistry() {
		if rt.Tool.Name == name {
			return rt.Handler
		}
	}
	t.Fatalf("tool %q not found in registry", name)
	return nil
}

// AC1: a tool call with `as` present as a non-string refuses INVALID_ARGUMENT
// naming 'as'; nothing executes — no message is sent (the refusal is before any
// write, so the intended recipient's inbox stays empty).
func TestRoutingParams_AsNonStringRefusedNothingSent(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "sender", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "recipient", "role": "dev"}))

	send := wrappedTool(t, h, "send_message")
	res, _ := send(ctx, call(map[string]any{
		"project": "p1",
		"as":      []any{"sender"}, // present but a native array, not a string
		"to":      "recipient",
		"content": "should never be delivered",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "as") {
		t.Errorf("refusal must name the param 'as', got: %s", msg)
	}
	if !strings.Contains(msg, CodeInvalidArgument) {
		t.Errorf("refusal must carry %s, got: %s", CodeInvalidArgument, msg)
	}

	// Nothing executed: the recipient's inbox is empty (no row written).
	inbox := wrappedTool(t, h, "get_inbox")
	inboxRes, _ := inbox(ctx, call(map[string]any{"project": "p1", "as": "recipient", "format": "json"}))
	got := parseJSON(t, inboxRes)
	if msgs, ok := got["messages"].([]any); ok && len(msgs) != 0 {
		t.Errorf("no message should have been sent, inbox has %d", len(msgs))
	}
}

// AC2: `project` present as a non-string refuses identically — the call never
// resolves to the default project namespace (GetString would coerce it to "" and
// resolveProject would fall through to the context/registration default).
func TestRoutingParams_ProjectNonStringRefused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "sender", "role": "dev"}))

	send := wrappedTool(t, h, "send_message")
	res, _ := send(ctx, call(map[string]any{
		"project": 42, // present but a number, not a string
		"as":      "sender",
		"to":      "recipient",
		"content": "misrouted?",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "project") {
		t.Errorf("refusal must name the param 'project', got: %s", msg)
	}
	if !strings.Contains(msg, CodeInvalidArgument) {
		t.Errorf("refusal must carry %s, got: %s", CodeInvalidArgument, msg)
	}
}

// AC3: absent as/project keep today's default-resolution behavior byte-identical
// — the guard only fires on a PRESENT non-string. A well-formed string as/project
// still reaches the handler and executes; omitting project entirely still lets
// resolveProject fall through to the caller's single-project registration.
func TestRoutingParams_AbsentAndValidUnchanged(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "sender", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "recipient", "role": "dev"}))

	send := wrappedTool(t, h, "send_message")

	// Valid string as/project: passes the guard and delivers.
	res, _ := send(ctx, call(map[string]any{
		"project": "p1", "as": "sender", "to": "recipient", "content": "hello",
	}))
	if res.IsError {
		t.Fatalf("valid string routing params must pass the guard, got: %s", expectError(t, res))
	}

	// project ABSENT: resolveProject falls through to sender's sole registration
	// (p1) exactly as before — the guard must not refuse an absent param.
	res2, _ := send(ctx, call(map[string]any{
		"as": "sender", "to": "recipient", "content": "hello again",
	}))
	if res2.IsError {
		t.Fatalf("absent project must keep default-resolution, got: %s", expectError(t, res2))
	}
	inbox := wrappedTool(t, h, "get_inbox")
	inboxRes, _ := inbox(ctx, call(map[string]any{"project": "p1", "as": "recipient", "format": "json"}))
	got := parseJSON(t, inboxRes)
	if msgs, ok := got["messages"].([]any); !ok || len(msgs) != 2 {
		t.Errorf("both valid + absent-project sends should deliver (want 2), got: %v", got["messages"])
	}
}

// AC4: task_id present as a non-string on a task tool refuses naming 'task_id'
// instead of degrading into a NOT_FOUND / empty-id lookup (GetString would coerce
// it to "" and the handler would report task_id required or not found).
func TestRoutingParams_TaskIDNonStringRefused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))

	get := wrappedTool(t, h, "get_task")
	res, _ := get(ctx, call(map[string]any{
		"project": "p1",
		"as":      "dev-a",
		"task_id": []any{"abc123"}, // present but a native array, not a string
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "task_id") {
		t.Errorf("refusal must name the param 'task_id', got: %s", msg)
	}
	if !strings.Contains(msg, CodeInvalidArgument) {
		t.Errorf("refusal must carry %s (not NOT_FOUND / empty-id), got: %s", CodeInvalidArgument, msg)
	}
	if strings.Contains(msg, CodeNotFound) {
		t.Errorf("a non-string task_id must refuse before the lookup, not report %s: %s", CodeNotFound, msg)
	}
}
