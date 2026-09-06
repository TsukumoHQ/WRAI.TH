package relay

import (
	"strings"
	"testing"

	"agent-relay/internal/db"
)

// seedAgentMemory writes an agent-scope memory authored by `author` straight
// through the store (bypassing the set_memory handler's tag/validity checks —
// this is a fixture, not the surface under test).
func seedAgentMemory(t *testing.T, h *Handlers, project, author, key string) {
	t.Helper()
	if _, err := h.db.SetMemory(project, author, key, "leftover value",
		db.TagsToJSON([]string{"cleanup"}), "agent", "observed", "context", true); err != nil {
		t.Fatalf("seed memory %s/%s: %v", author, key, err)
	}
}

// tombstone reads back the (possibly archived) agent-scope memory at key authored
// by `author`.
func tombstone(t *testing.T, h *Handlers, project, author, key string) (archivedBy, agentName, status string) {
	t.Helper()
	mems, err := h.db.GetMemoryIncludingArchived(project, author, key, "agent")
	if err != nil {
		t.Fatalf("GetMemoryIncludingArchived: %v", err)
	}
	if len(mems) == 0 {
		t.Fatalf("no memory row for %s/%s", author, key)
	}
	m := mems[0]
	by := ""
	if m.ArchivedBy != nil {
		by = *m.ArchivedBy
	}
	return by, m.AgentName, m.Status
}

// TestDeleteMemory_ExecutiveArchivesDeadAgentMemory is AC1: an executive, passing
// the `agent` target param, archives an agent-scope memory authored by an
// inactive agent, and the tombstone records BOTH the acting agent (archived_by)
// and the original author (agent_name).
func TestDeleteMemory_ExecutiveArchivesDeadAgentMemory(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "chief", "role": "exec", "is_executive": true}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "ghost", "role": "dev"}))
	seedAgentMemory(t, h, "p1", "ghost", "ghost-resume")
	if err := h.db.DeactivateAgent("p1", "ghost"); err != nil {
		t.Fatalf("deactivate ghost: %v", err)
	}

	res, _ := h.HandleDeleteMemory(ctx, call(map[string]any{
		"project": "p1", "as": "chief", "key": "ghost-resume", "scope": "agent", "agent": "ghost",
		"reason": "memory-hygiene sweep",
	}))
	if res.IsError {
		t.Fatalf("executive archiving a dead agent's memory should succeed: %s", expectError(t, res))
	}

	by, author, status := tombstone(t, h, "p1", "ghost", "ghost-resume")
	if status != "archived" {
		t.Errorf("status = %q, want archived", status)
	}
	if by != "chief" {
		t.Errorf("archived_by = %q, want chief (the acting agent)", by)
	}
	if author != "ghost" {
		t.Errorf("agent_name = %q, want ghost (the target author preserved)", author)
	}
}

// TestDeleteMemory_NonExecCannotTargetLiveAgent is AC2: a non-executive caller
// aiming the `agent` param at another LIVE agent's memory is refused with a
// message naming both the caller and the target, and the memory is untouched.
func TestDeleteMemory_NonExecCannotTargetLiveAgent(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "peer", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "victim", "role": "dev"}))
	seedAgentMemory(t, h, "p1", "victim", "victim-note")

	res, _ := h.HandleDeleteMemory(ctx, call(map[string]any{
		"project": "p1", "as": "peer", "key": "victim-note", "scope": "agent", "agent": "victim",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "peer") || !strings.Contains(msg, "victim") {
		t.Errorf("refusal must name caller (peer) and target (victim), got: %s", msg)
	}

	_, _, status := tombstone(t, h, "p1", "victim", "victim-note")
	if status == "archived" {
		t.Fatal("victim's live memory must be UNCHANGED after a refused cross-agent delete")
	}
}

// TestDeleteMemory_JanitorArchivesDeadAndUnregisteredAuthor covers the AC3 purge
// mechanism: a NON-executive caller may archive an agent-scope memory whose author
// is dead — whether an inactive registered agent, or one never registered at all
// (e.g. an "anonymous" checkpoint author). This is what lets the fleet janitor
// clear the orphan niwa checkpoints without executive rights.
func TestDeleteMemory_JanitorArchivesDeadAndUnregisteredAuthor(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "janitor", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "departed", "role": "dev"}))
	seedAgentMemory(t, h, "p1", "departed", "dep-ckpt")
	if err := h.db.DeactivateAgent("p1", "departed"); err != nil {
		t.Fatalf("deactivate departed: %v", err)
	}
	// Never-registered author (nil agent row) — treated as dead.
	seedAgentMemory(t, h, "p1", "anonymous", "anon-ckpt")

	for _, tc := range []struct{ author, key string }{
		{"departed", "dep-ckpt"},
		{"anonymous", "anon-ckpt"},
	} {
		res, _ := h.HandleDeleteMemory(ctx, call(map[string]any{
			"project": "p1", "as": "janitor", "key": tc.key, "scope": "agent", "agent": tc.author,
		}))
		if res.IsError {
			t.Fatalf("janitor archiving dead author %s should succeed: %s", tc.author, expectError(t, res))
		}
		if by, author, status := tombstone(t, h, "p1", tc.author, tc.key); status != "archived" || by != "janitor" || author != tc.author {
			t.Errorf("%s: tombstone = by %q author %q status %q; want by janitor author %s archived", tc.author, by, author, status, tc.author)
		}
	}
}
