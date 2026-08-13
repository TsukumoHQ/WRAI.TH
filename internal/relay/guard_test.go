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

// TestGuardIdentity verifies the always-on identity gate: a call with no
// resolvable project or agent, or an agent not registered in the project, is
// rejected before the wrapped handler runs; a registered agent passes through.
func TestGuardIdentity(t *testing.T) {
	h := testHandlers(t)

	ran := false
	next := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ran = true
		return mcp.NewToolResultText("ok"), nil
	}
	guarded := h.guardIdentity(next)

	// no project resolvable → rejected, next never runs
	res, _ := guarded(ctxWith("", ""), call(nil))
	if !res.IsError {
		t.Fatal("call with no project should be rejected")
	}
	if ran {
		t.Fatal("wrapped handler must not run without a project")
	}

	// project present but no agent identity → rejected
	ran = false
	res, _ = guarded(ctxWith("", "p1"), call(nil))
	if !res.IsError {
		t.Fatal("call with no agent identity should be rejected")
	}
	if ran {
		t.Fatal("wrapped handler must not run without an identity")
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

type fakeSession string

func (f fakeSession) SessionID() string { return string(f) }

func ctxWithSession(agent, project, sessionID string) context.Context {
	return context.WithValue(ctxWith(agent, project), sessionKey, fakeSession(sessionID))
}

// TestGuardIdentityBinding verifies the Option-A binding: a connection whose
// session is provably bound to one agent cannot act as another; an unknown
// session fails open (the write proceeds) so legitimate traffic is never dropped.
func TestGuardIdentityBinding(t *testing.T) {
	h := testHandlers(t)
	// Register alice on session sA and bob on session sB, both in p1. Registering
	// with a session in context populates the reverse session→agent binding.
	if r, _ := h.HandleRegisterAgent(ctxWithSession("", "p1", "sA"), call(map[string]any{"name": "alice", "role": "dev"})); r.IsError {
		t.Fatalf("register alice: %s", expectError(t, r))
	}
	if r, _ := h.HandleRegisterAgent(ctxWithSession("", "p1", "sB"), call(map[string]any{"name": "bob", "role": "dev"})); r.IsError {
		t.Fatalf("register bob: %s", expectError(t, r))
	}

	ran := false
	next := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ran = true
		return mcp.NewToolResultText("ok"), nil
	}
	guarded := h.guardIdentity(next)

	// alice acting as herself from her own session → passes
	ran = false
	if res, _ := guarded(ctxWithSession("", "p1", "sA"), call(map[string]any{"as": "alice"})); res != nil && res.IsError {
		t.Fatalf("alice on her own session should pass: %s", res.Content[0].(mcp.TextContent).Text)
	}
	if !ran {
		t.Fatal("legit self-identified write should run")
	}

	// alice's session claiming as:bob → impersonation, rejected
	ran = false
	res, _ := guarded(ctxWithSession("", "p1", "sA"), call(map[string]any{"as": "bob"}))
	if !res.IsError {
		t.Fatal("session bound to alice must not act as bob")
	}
	if ran {
		t.Fatal("impersonating write must not run")
	}

	// unknown session claiming as:alice → fail open, passes (alice is registered)
	ran = false
	if res, _ := guarded(ctxWithSession("", "p1", "sX"), call(map[string]any{"as": "alice"})); res != nil && res.IsError {
		t.Fatalf("unknown session should fail open: %s", res.Content[0].(mcp.TextContent).Text)
	}
	if !ran {
		t.Fatal("write from an unbound session should proceed (fail-open)")
	}
}

// TestGuardInstalledOnMutatingTools confirms toolRegistry now wraps every
// mutating tool with the identity gate (no opt-in flag): an unidentified
// send_message is rejected, while a bootstrap tool like create_project is not
// wrapped and stays reachable.
func TestGuardInstalledOnMutatingTools(t *testing.T) {
	h := testHandlers(t)
	var sendGuarded, createProjectWrapped bool
	for _, rt := range h.toolRegistry() {
		switch rt.Tool.Name {
		case "send_message":
			// An unidentified call must be rejected by the wrapper.
			res, _ := rt.Handler(ctxWith("", ""), call(map[string]any{"to": "x", "content": "y"}))
			sendGuarded = res != nil && res.IsError
		case "create_project":
			createProjectWrapped = true // presence in mutatingTools would wrap it; asserted below
		}
	}
	if !sendGuarded {
		t.Fatal("send_message should be identity-guarded in the registry")
	}
	if mutatingTools["create_project"] {
		t.Fatal("create_project must stay a bootstrap tool (unguarded)")
	}
	_ = createProjectWrapped
}
