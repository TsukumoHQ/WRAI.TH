package db

import (
	"agent-relay/internal/models"
	"strings"
	"testing"
)

func auditEntry(project, actor, action, resource, summary string) models.AuditEntry {
	return models.AuditEntry{
		Project: project, Actor: actor, Action: action,
		ResourceType: "task", ResourceID: resource, Summary: summary,
	}
}

// Dependencies gate: an agent cannot claim/start a task whose dependency is
// still open, but the orchestrator ("user") may force it through.
func TestDependencyGate_BlocksAgentAllowsUser(t *testing.T) {
	d := testDB(t)

	dep, err := d.DispatchTask("p1", "dev", "cto", "build api", "", "P2", nil, nil)
	if err != nil {
		t.Fatalf("dispatch dep: %v", err)
	}
	task, err := d.DispatchTask("p1", "dev", "cto", "wire frontend", "", "P2", nil, nil)
	if err != nil {
		t.Fatalf("dispatch task: %v", err)
	}
	if _, err = d.SetTaskDependencies(task.ID, "p1", []string{dep.ID}); err != nil {
		t.Fatalf("set deps: %v", err)
	}

	// Agent claim must be blocked while the dependency is open.
	if _, err = d.ClaimTask(task.ID, "bot-a", "p1"); err == nil {
		t.Fatal("expected claim to be blocked by open dependency")
	} else if !strings.Contains(err.Error(), "unfinished dependenc") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Orchestrator override (user) bypasses the gate.
	if _, err = d.ClaimTask(task.ID, "user", "p1"); err != nil {
		t.Fatalf("user override should pass: %v", err)
	}

	// Once the dependency is done, the agent can claim.
	other, _ := d.DispatchTask("p1", "dev", "cto", "another", "", "P2", nil, nil)
	if _, err = d.SetTaskDependencies(other.ID, "p1", []string{dep.ID}); err != nil {
		t.Fatalf("set deps 2: %v", err)
	}
	if _, err = d.CompleteTask(dep.ID, "bot-b", "p1", nil); err != nil {
		t.Fatalf("complete dep: %v", err)
	}
	if _, err = d.ClaimTask(other.ID, "bot-a", "p1"); err != nil {
		t.Fatalf("claim after dep done should pass: %v", err)
	}
}

// Cycle rejection: a dependency edge that would close a loop is refused.
func TestSetTaskDependencies_RejectsCycle(t *testing.T) {
	d := testDB(t)

	a, _ := d.DispatchTask("p1", "dev", "cto", "A", "", "P2", nil, nil)
	b, _ := d.DispatchTask("p1", "dev", "cto", "B", "", "P2", nil, nil)

	if _, err := d.SetTaskDependencies(b.ID, "p1", []string{a.ID}); err != nil {
		t.Fatalf("b depends on a: %v", err)
	}
	// a depends on b would form a → b → a cycle.
	if _, err := d.SetTaskDependencies(a.ID, "p1", []string{b.ID}); err == nil {
		t.Fatal("expected cycle to be rejected")
	}
	// self-dependency is also rejected.
	if _, err := d.SetTaskDependencies(a.ID, "p1", []string{a.ID}); err == nil {
		t.Fatal("expected self-dependency to be rejected")
	}
}

// Reassign hands the task to a new agent without changing status.
func TestReassignTask(t *testing.T) {
	d := testDB(t)

	task, _ := d.DispatchTask("p1", "dev", "cto", "task", "", "P2", nil, nil)
	if _, err := d.ClaimTask(task.ID, "bot-a", "p1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err := d.ReassignTask(task.ID, "p1", "bot-b")
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if got.AssignedTo == nil || *got.AssignedTo != "bot-b" {
		t.Fatalf("expected assigned_to=bot-b, got %v", got.AssignedTo)
	}
	if got.Status != "accepted" {
		t.Fatalf("status should be unchanged (accepted), got %s", got.Status)
	}
}

// Linear-mirrored tasks reject orchestrator mutations — Linear is the SSOT.
func TestCommandLayer_RejectsLinearMirroredTasks(t *testing.T) {
	d := testDB(t)

	native, _ := d.DispatchTask("p1", "dev", "cto", "native", "", "P2", nil, nil)
	if err := d.UpsertLinearTask(LinearTaskSeed{
		ID: "lin-1", Project: "p1", Title: "from linear", Status: "in-progress",
		Priority: "P2", DispatchedAt: "2026-01-01T00:00:00.000000Z", Labels: "[]",
	}); err != nil {
		t.Fatalf("seed linear task: %v", err)
	}

	if _, err := d.SetTaskDependencies("lin-1", "p1", []string{native.ID}); err == nil {
		t.Fatal("expected set-dependencies on a Linear task to be rejected")
	}
	if _, err := d.ReassignTask("lin-1", "p1", "bot-a"); err == nil {
		t.Fatal("expected reassign on a Linear task to be rejected")
	}
	// A native task depending ON a Linear task is fine (Linear stays read-only).
	if _, err := d.SetTaskDependencies(native.ID, "p1", []string{"lin-1"}); err != nil {
		t.Fatalf("native task may depend on a linear task: %v", err)
	}
}

// Audit round-trips entries newest-first, scoped to a resource.
func TestAuditRoundTrip(t *testing.T) {
	d := testDB(t)

	if err := d.RecordAudit(auditEntry("p1", "user", "transition", "task-1", "pending → done")); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := d.RecordAudit(auditEntry("p1", "user", "reassign", "task-1", "a → b")); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	if err := d.RecordAudit(auditEntry("p1", "user", "transition", "task-2", "x → y")); err != nil {
		t.Fatalf("record 3: %v", err)
	}

	scoped, err := d.ListAudit("p1", "task-1", 50)
	if err != nil {
		t.Fatalf("list scoped: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("expected 2 entries for task-1, got %d", len(scoped))
	}

	all, err := d.ListAudit("p1", "", 50)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 entries for project, got %d", len(all))
	}
}
