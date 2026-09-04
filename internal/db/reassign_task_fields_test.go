package db

import (
	"testing"
)

// TestReassignTaskFields_ClaimedTransfersLease: on a held task, transferLease
// moves claimed_by/lease_holder to the new assignee, keeps status unchanged, and
// stamps + audits a lease_transfer{reason:"reassigned",by:caller}.
func TestReassignTaskFields_ClaimedTransfersLease(t *testing.T) {
	d := testDB(t)
	taskID := dispatchClaimed(t, d, "p1", "dev-a")
	regAgent(t, d, "p1", "dev-b")

	newAssignee := "dev-b"
	task, err := d.ReassignTaskFields(taskID, "p1", "dispatcher", &newAssignee, nil, true)
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if task.ClaimedBy == nil || *task.ClaimedBy != "dev-b" {
		t.Errorf("claimed_by = %v, want dev-b", task.ClaimedBy)
	}
	if task.LeaseHolder == nil || *task.LeaseHolder != "dev-b" {
		t.Errorf("lease_holder = %v, want dev-b", task.LeaseHolder)
	}
	if task.Status != "accepted" {
		t.Errorf("status = %q, want accepted (unchanged)", task.Status)
	}
	if task.LeaseTransfer == nil {
		t.Fatal("expected a lease_transfer marker")
	}
	lt := task.LeaseTransfer
	if lt.From != "dev-a" || lt.To != "dev-b" || lt.Reason != "reassigned" || lt.By != "dispatcher" {
		t.Errorf("lease_transfer = %+v, want {dev-a,dev-b,reassigned,dispatcher}", lt)
	}

	// The transfer is persisted; the marker is not (transient) — re-read confirms.
	fresh, err := d.GetTask(taskID, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.LeaseTransfer != nil {
		t.Errorf("lease_transfer must not persist, got %+v", fresh.LeaseTransfer)
	}
	if fresh.ClaimedBy == nil || *fresh.ClaimedBy != "dev-b" {
		t.Errorf("persisted claimed_by = %v, want dev-b", fresh.ClaimedBy)
	}

	entries, err := d.ListAudit("p1", taskID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "lease_transferred" && e.Actor == "dispatcher" && e.Reason == "reassigned" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a lease_transferred audit entry (actor dispatcher, reason reassigned), got %+v", entries)
	}
}

// TestReassignTaskFields_PendingMintsNoLease: on a pending task the fields update
// but no lease is minted, so the task stays claimable.
func TestReassignTaskFields_PendingMintsNoLease(t *testing.T) {
	d := testDB(t)
	regAgent(t, d, "p1", "dev-b")
	task, err := d.DispatchTask("p1", "", "dispatcher", "pending one", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	newAssignee := "dev-b"
	newProfile := "custom-prof"
	got, err := d.ReassignTaskFields(task.ID, "p1", "dispatcher", &newAssignee, &newProfile, false)
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if got.AssignedTo == nil || *got.AssignedTo != "dev-b" {
		t.Errorf("assigned_to = %v, want dev-b", got.AssignedTo)
	}
	if got.ProfileSlug != "custom-prof" {
		t.Errorf("profile_slug = %q, want custom-prof (explicit wins)", got.ProfileSlug)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.LeaseHolder != nil || got.ClaimedBy != nil {
		t.Errorf("pending reassign must not mint a lease: holder=%v claimed_by=%v", got.LeaseHolder, got.ClaimedBy)
	}
	if got.LeaseTransfer != nil {
		t.Errorf("pending reassign must not stamp a lease_transfer, got %+v", got.LeaseTransfer)
	}
}

// TestReassignTaskFields_LinearReadOnly: a Linear-mirrored task is read-only here.
func TestReassignTaskFields_LinearReadOnly(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p1", "", "dispatcher", "mirror", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := d.conn.Exec("UPDATE tasks SET source = 'linear' WHERE id = ?", task.ID); err != nil {
		t.Fatalf("set linear: %v", err)
	}
	newAssignee := "dev-b"
	if _, err := d.ReassignTaskFields(task.ID, "p1", "dispatcher", &newAssignee, nil, false); err == nil {
		t.Fatal("expected a read-only refusal for a Linear-mirrored task")
	}
}
