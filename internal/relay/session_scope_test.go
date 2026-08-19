package relay

import "testing"

// R5: session resolution is project-scoped. A session bound to an agent in one
// project must NOT resolve in another, resolves to the right agent within its
// project, is ambiguous (fail-open) when bound to two agents in the same project,
// and clears on Unregister.
func TestAgentForSession_ProjectScoped(t *testing.T) {
	r := NewSessionRegistry(nil)
	r.Register("p1", "alice", "sess1")
	r.Register("p2", "bob", "sess2")

	if name, ok := r.AgentForSession("p1", "sess1"); !ok || name != "alice" {
		t.Fatalf("p1/sess1 = %q,%v; want alice,true", name, ok)
	}
	// Same session id is unknown in a different project — no cross-project bleed.
	if _, ok := r.AgentForSession("p2", "sess1"); ok {
		t.Fatalf("sess1 must not resolve in p2")
	}
	if name, ok := r.AgentForSession("p2", "sess2"); !ok || name != "bob" {
		t.Fatalf("p2/sess2 = %q,%v; want bob,true", name, ok)
	}
	if _, ok := r.AgentForSession("p1", ""); ok {
		t.Fatalf("empty session must not resolve")
	}

	// Ambiguous: the same session bound to two agents in the SAME project fails
	// open (returns false) so a guarded write is never dropped on ambiguity.
	r.Register("p1", "alice2", "sess1")
	if _, ok := r.AgentForSession("p1", "sess1"); ok {
		t.Fatalf("ambiguous session must fail open (false)")
	}

	// Unregister removes the binding.
	r.Unregister("bob", "sess2")
	if _, ok := r.AgentForSession("p2", "sess2"); ok {
		t.Fatalf("sess2 must be gone after Unregister")
	}
}
