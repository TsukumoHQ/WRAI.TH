package relay

import (
	"context"
	"fmt"
	"testing"
	"time"

	"agent-relay/internal/db"
	"agent-relay/internal/models"

	"github.com/mark3labs/mcp-go/mcp"
)

func identityTestSetup(t *testing.T) (*Handlers, func(tool string) (*mcp.CallToolResult, bool)) {
	t.Helper()
	h := testHandlers(t)
	if r, _ := h.HandleRegisterAgent(ctxWith("", "p1"), call(map[string]any{"name": "bob", "role": "dev"})); r.IsError {
		t.Fatalf("register bob: %s", expectError(t, r))
	}
	run := func(tool string) (*mcp.CallToolResult, bool) {
		ran := false
		next := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ran = true
			return mcp.NewToolResultText("ok"), nil
		}
		res, _ := h.guardIdentity(tool, next)(ctxWith("bob", "p1"), call(nil))
		return res, ran
	}
	return h, run
}

// B1 (33dab33d): one deadline on the identity read — a host stall — no longer
// fails the authed call; the gate retries the lookup once.
func TestGuardIdentity_RetriesOnceOnDeadline(t *testing.T) {
	_, run := identityTestSetup(t)
	orig := identityLookup
	defer func() { identityLookup = orig }()
	calls := 0
	identityLookup = func(d *db.DB, project, name string) (*models.Agent, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("get agent: %w", context.DeadlineExceeded)
		}
		return d.GetAgent(project, name)
	}
	res, ran := run("update_task")
	if res.IsError || !ran {
		t.Fatalf("one stalled identity read must not fail the call (calls=%d): %v", calls, res.Content)
	}
	if calls != 2 {
		t.Fatalf("want exactly one retry (2 lookups), got %d", calls)
	}

	// Two deadlines in a row still fail — the retry is once, not a loop.
	h2, run2 := identityTestSetup(t)
	_ = h2
	calls = 0
	identityLookup = func(d *db.DB, project, name string) (*models.Agent, error) {
		calls++
		return nil, fmt.Errorf("get agent: %w", context.DeadlineExceeded)
	}
	if res, ran := run2("update_task"); !res.IsError || ran || calls != 2 {
		t.Fatalf("persistent deadline: want refusal after 2 lookups, got isError=%v ran=%v calls=%d", res.IsError, ran, calls)
	}
}

// B2 (33dab33d): a repeat check of a live agent is served from the cache
// without a DB read, and the cache never grants past identityCacheTTL — a
// deactivated agent is refused by the liveness gate within the TTL bound even
// when the revocation bypasses the handlers' invalidation.
func TestGuardIdentity_CacheTTLBoundsRevocation(t *testing.T) {
	h, run := identityTestSetup(t)
	origL, origTTL := identityLookup, identityCacheTTL
	defer func() { identityLookup, identityCacheTTL = origL, origTTL }()
	identityCacheTTL = 150 * time.Millisecond
	reads := 0
	identityLookup = func(d *db.DB, project, name string) (*models.Agent, error) {
		reads++
		return d.GetAgent(project, name)
	}

	if res, ran := run("send_message"); res.IsError || !ran {
		t.Fatalf("live agent refused: %v", res.Content)
	}
	if res, ran := run("send_message"); res.IsError || !ran {
		t.Fatalf("live agent refused on repeat: %v", res.Content)
	}
	if reads != 1 {
		t.Fatalf("repeat check within TTL must not read the DB: %d reads", reads)
	}

	// Revoke at the DB layer only (no handler, no invalidation): worst case.
	revokedAt := time.Now()
	if err := h.db.DeactivateAgent("p1", "bob"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	deadline := revokedAt.Add(identityCacheTTL + 50*time.Millisecond)
	refused := false
	for time.Now().Before(deadline) {
		if res, ran := run("send_message"); res.IsError && !ran {
			refused = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !refused {
		t.Fatalf("deactivated agent still granted %s after revocation (TTL %s)", time.Since(revokedAt), identityCacheTTL)
	}
	// Refusals are never cached: it stays refused, every check reads the DB.
	before := reads
	run("send_message")
	run("send_message")
	if reads-before != 2 {
		t.Fatalf("refused identity must be re-read each time, got %d reads", reads-before)
	}
}

// B2: the deactivate handler drops the cached grant at once — no TTL wait.
func TestGuardIdentity_DeactivateHandlerInvalidates(t *testing.T) {
	h, run := identityTestSetup(t)
	origTTL := identityCacheTTL
	defer func() { identityCacheTTL = origTTL }()
	identityCacheTTL = time.Hour // only invalidation can explain a refusal

	if res, _ := run("send_message"); res.IsError {
		t.Fatalf("live agent refused: %v", res.Content)
	}
	if r, _ := h.HandleDeactivateAgent(ctxWith("bob", "p1"), call(map[string]any{"name": "bob"})); r.IsError {
		t.Fatalf("deactivate: %s", expectError(t, r))
	}
	if res, ran := run("send_message"); !res.IsError || ran {
		t.Fatal("deactivated agent served from a stale cached grant")
	}
}
