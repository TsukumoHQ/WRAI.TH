package db

import (
	"agent-relay/internal/models"
	"testing"
)

func TestNotificationRuleCRUD(t *testing.T) {
	d := testDB(t)

	// Create
	r, err := d.CreateNotificationRule(&models.NotificationRule{
		Project: "default",
		Name:    "Blocked → manager",
		Enabled: true,
		Event:   "task.blocked",
		Action:  "message",
		Target:  "manager",
		Opts:    `{"priority":"P1"}`,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.ID == "" {
		t.Fatal("expected generated ID")
	}
	if r.Match != "{}" {
		t.Fatalf("expected default match {}, got %q", r.Match)
	}

	// List
	rules, err := d.ListNotificationRules("default")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if !rules[0].Enabled {
		t.Fatal("expected enabled")
	}

	// Get
	got, err := d.GetNotificationRule(r.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Blocked → manager" {
		t.Fatalf("unexpected name %q", got.Name)
	}

	// Patch: disable + change target
	dis := false
	newTarget := "cto"
	patched, err := d.PatchNotificationRule(r.ID, nil, nil, nil, nil, &newTarget, nil, &dis)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.Enabled {
		t.Fatal("expected disabled after patch")
	}
	if patched.Target != "cto" {
		t.Fatalf("expected target cto, got %q", patched.Target)
	}

	// Enabled-for-event should now skip the disabled rule
	enabled, err := d.ListEnabledNotificationRulesForEvent("task.blocked")
	if err != nil {
		t.Fatalf("enabled list: %v", err)
	}
	if len(enabled) != 0 {
		t.Fatalf("expected 0 enabled rules, got %d", len(enabled))
	}

	// Re-enable and verify it appears
	en := true
	if _, err := d.PatchNotificationRule(r.ID, nil, nil, nil, nil, nil, nil, &en); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	enabled, _ = d.ListEnabledNotificationRulesForEvent("task.blocked")
	if len(enabled) != 1 {
		t.Fatalf("expected 1 enabled rule, got %d", len(enabled))
	}

	// Count
	n, _ := d.CountNotificationRules()
	if n != 1 {
		t.Fatalf("expected count 1, got %d", n)
	}

	// Delete
	if err := d.DeleteNotificationRule(r.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rules, _ = d.ListNotificationRules("default")
	if len(rules) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(rules))
	}
}

func TestNotificationDeliveryLogAndPrune(t *testing.T) {
	d := testDB(t)

	rec := &models.NotificationDelivery{
		Project: "default",
		RuleID:  "rule-1",
		Event:   "task.blocked",
		Action:  "message",
		Target:  "manager",
		Outcome: "ok",
		Payload: `{"line":"x"}`,
	}
	if err := d.LogNotificationDelivery(rec); err != nil {
		t.Fatalf("log: %v", err)
	}
	list, err := d.ListNotificationDeliveries(10)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(list))
	}
	if list[0].Outcome != "ok" {
		t.Fatalf("unexpected outcome %q", list[0].Outcome)
	}
}

func TestComputeDigestStats(t *testing.T) {
	d := testDB(t)

	// Register a profile-less set of tasks across statuses.
	mk := func(status string) {
		task, err := d.DispatchTask("default", "p1", "cto", "task "+status, "", "P2", nil, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		switch status {
		case "in-progress":
			if _, err := d.StartTask(task.ID, "agent-a", "default"); err != nil {
				t.Fatalf("start: %v", err)
			}
		case "done":
			if _, err := d.CompleteTask(task.ID, "agent-a", "default", nil); err != nil {
				t.Fatalf("complete: %v", err)
			}
		case "blocked":
			if _, err := d.StartTask(task.ID, "agent-a", "default"); err != nil {
				t.Fatalf("start before block: %v", err)
			}
			if _, err := d.BlockTask(task.ID, "agent-a", "default", nil); err != nil {
				t.Fatalf("block: %v", err)
			}
		}
	}
	mk("pending")
	mk("in-progress")
	mk("in-progress")
	mk("done")
	mk("blocked")

	stats, err := d.ComputeDigestStats("default")
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if stats.Total != 5 {
		t.Fatalf("expected total 5, got %d", stats.Total)
	}
	if stats.Done != 1 {
		t.Fatalf("expected done 1, got %d", stats.Done)
	}
	if stats.Blocked != 1 {
		t.Fatalf("expected blocked 1, got %d", stats.Blocked)
	}
	if stats.InProgress != 2 {
		t.Fatalf("expected in_progress 2, got %d", stats.InProgress)
	}
	if stats.InReview != 2 {
		t.Fatalf("expected in_review (derived) 2, got %d", stats.InReview)
	}
}
