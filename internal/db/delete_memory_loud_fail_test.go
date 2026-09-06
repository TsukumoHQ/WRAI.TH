package db

import (
	"strings"
	"testing"
)

// AC1: a targeted cross-author delete (explicit `agent` target distinct from the
// caller, agent scope) that matches no row fails LOUDLY, naming both the key and
// the target author — the founder failure-is-loud fix for the live incident where
// an assumed author differed and a silent no-op left the purge believed-done.
func TestDeleteMemoryAs_TargetedNoRowNamesKeyAndAuthor(t *testing.T) {
	d := testDB(t)

	// A real agent-scope memory exists, but authored by "alice". Targeting a
	// DIFFERENT author ("ghost") for the same key matches no row.
	if _, err := d.SetMemory("p1", "alice", "secret", "v", "[]", "agent", "stated", "behavior"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := d.DeleteMemoryAs("p1", "admin", "ghost", "secret", "agent", "purge")
	if err == nil {
		t.Fatal("expected a loud error on a targeted delete that matched no row, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "secret") {
		t.Errorf("error must name the key: %q", msg)
	}
	if !strings.Contains(msg, "ghost") {
		t.Errorf("error must name the target author: %q", msg)
	}

	// The seeded row (alice's) is untouched — the wrong-author delete archived
	// nothing.
	live, _ := d.GetMemory("p1", "alice", "secret", "agent")
	if len(live) != 1 {
		t.Errorf("alice's memory should be untouched by the wrong-author delete, got %d live rows", len(live))
	}
}

// AC2: a self-delete (no `agent` target — actingAgent == targetAuthor) of a
// missing key keeps its current lenient behavior UNCHANGED: the plain
// "memory not found: <key> (scope=<scope>)" message, with no target-author phrasing.
func TestDeleteMemory_SelfMissingKeyLenientUnchanged(t *testing.T) {
	d := testDB(t)

	err := d.DeleteMemory("p1", "bob", "missing", "project", "cleanup")
	if err == nil {
		t.Fatal("expected the existing not-found error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "memory not found: missing (scope=project)") {
		t.Errorf("self-delete message changed; want the plain not-found form, got %q", msg)
	}
	if strings.Contains(msg, "authored by") {
		t.Errorf("self-delete must NOT use the targeted author-naming message: %q", msg)
	}

	// Same for an agent-scope self-delete (targetAuthor resolves to the caller):
	// still the plain message, no author naming.
	errAgent := d.DeleteMemoryAs("p1", "bob", "bob", "missing", "agent", "cleanup")
	if errAgent == nil {
		t.Fatal("expected the existing not-found error for agent self-delete, got nil")
	}
	if strings.Contains(errAgent.Error(), "authored by") {
		t.Errorf("agent-scope self-delete must NOT name a target author: %q", errAgent.Error())
	}
}
