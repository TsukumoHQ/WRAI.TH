package db

import (
	"agent-relay/internal/models"
	"testing"
)

func auditEntry(project, actor, action, resource, summary string) models.AuditEntry {
	return models.AuditEntry{
		Project: project, Actor: actor, Action: action,
		ResourceType: "task", ResourceID: resource, Summary: summary,
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

// Linear-mirrored tasks reject native orchestrator mutations — Linear is the SSOT.
func TestReassign_RejectsLinearMirroredTasks(t *testing.T) {
	d := testDB(t)

	if err := d.UpsertLinearTask(LinearTaskSeed{
		ID: "lin-1", Project: "p1", Title: "from linear", Status: "in-progress",
		Priority: "P2", DispatchedAt: "2026-01-01T00:00:00.000000Z", Labels: "[]",
	}); err != nil {
		t.Fatalf("seed linear task: %v", err)
	}
	if _, err := d.ReassignTask("lin-1", "p1", "bot-a"); err == nil {
		t.Fatal("expected reassign on a Linear task to be rejected")
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
