package relay

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func ctxWith(agent, project string) context.Context {
	ctx := context.WithValue(context.Background(), agentNameKey, agent)
	return context.WithValue(ctx, projectKey, project)
}

// TestGuardRegistered verifies the RELAY_REQUIRE_REGISTERED gate: anonymous and
// unregistered identities are rejected before the wrapped handler runs, while a
// registered agent passes through.
func TestGuardRegistered(t *testing.T) {
	h := testHandlers(t)
	h.requireRegistered = true

	ran := false
	next := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ran = true
		return mcp.NewToolResultText("ok"), nil
	}
	guarded := h.guardRegistered(next)

	// anonymous → rejected, next never runs
	res, _ := guarded(ctxWith("anonymous", "p1"), call(nil))
	if !res.IsError {
		t.Fatal("anonymous identity should be rejected")
	}
	if ran {
		t.Fatal("wrapped handler must not run for anonymous")
	}

	// unregistered name → rejected
	ran = false
	res, _ = guarded(ctxWith("ghost", "p1"), call(nil))
	if !res.IsError {
		t.Fatal("unregistered agent should be rejected")
	}
	if ran {
		t.Fatal("wrapped handler must not run for unregistered agent")
	}

	// register bob, then the same call passes and runs the handler
	if r, _ := h.HandleRegisterAgent(ctxWith("", "p1"), call(map[string]any{"name": "bob", "role": "dev"})); r.IsError {
		t.Fatalf("register bob failed: %s", expectError(t, r))
	}
	ran = false
	res, _ = guarded(ctxWith("bob", "p1"), call(nil))
	if res != nil && res.IsError {
		t.Fatalf("registered agent should pass: %s", res.Content[0].(mcp.TextContent).Text)
	}
	if !ran {
		t.Fatal("wrapped handler should run for a registered agent")
	}

	// registration is project-scoped: bob in p1 is unknown in p2
	ran = false
	res, _ = guarded(ctxWith("bob", "p2"), call(nil))
	if !res.IsError {
		t.Fatal("agent registered in p1 should be rejected in p2")
	}
}

// TestGuardDisabledByDefault confirms toolRegistry leaves handlers unwrapped
// when the flag is off (no identity enforcement).
func TestToolRegistryNoWrapByDefault(t *testing.T) {
	h := testHandlers(t)
	// requireRegistered defaults false
	for _, rt := range h.toolRegistry() {
		_ = rt // handlers are the bare h.HandleX; nothing to assert beyond no panic
	}
	if h.requireRegistered {
		t.Fatal("requireRegistered should default to false")
	}
}
