package db

import (
	"testing"
	"time"
)

// Bug 6509668c hardening: the stale-agent sweep must NOT inactivate an agent that
// holds an UNEXPIRED task lease (a heads-down worker makes no relay calls mid-task
// so its last_seen goes stale), but MUST inactivate one whose lease already lapsed
// (a truly-hung holder) and a plain idle agent with no lease.
func TestMarkStaleAgentsInactive_LeaseHolderGuard(t *testing.T) {
	d := testDB(t)

	mk := func(name string) {
		if _, _, err := d.RegisterAgent("p1", name, "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		// Force last_seen stale so the sweep is eligible to flip it.
		if _, err := d.conn.Exec(
			"UPDATE agents SET last_seen = '2020-01-01T00:00:00Z' WHERE name = ? AND project = 'p1'", name,
		); err != nil {
			t.Fatalf("age %s: %v", name, err)
		}
	}
	mk("leased-live")
	mk("leased-expired")
	mk("idle")

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	t1, err := d.DispatchTask("p1", "dev", "cto", "live-lease", "", "P2", nil, nil, TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch t1: %v", err)
	}
	if _, err := d.conn.Exec("UPDATE tasks SET lease_holder = ?, lease_expires_at = ? WHERE id = ?", "leased-live", future, t1.ID); err != nil {
		t.Fatalf("set live lease: %v", err)
	}
	t2, err := d.DispatchTask("p1", "dev", "cto", "expired-lease", "", "P2", nil, nil, TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch t2: %v", err)
	}
	if _, err := d.conn.Exec("UPDATE tasks SET lease_holder = ?, lease_expires_at = ? WHERE id = ?", "leased-expired", past, t2.ID); err != nil {
		t.Fatalf("set expired lease: %v", err)
	}

	n, err := d.MarkStaleAgentsInactive(30 * time.Minute)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 swept (expired-lease + idle), got %d", n)
	}

	status := func(name string) string {
		a, err := d.GetAgent("p1", name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return a.Status
	}
	if s := status("leased-live"); s != "active" {
		t.Errorf("agent holding an unexpired lease must stay active, got %q", s)
	}
	if s := status("leased-expired"); s != "inactive" {
		t.Errorf("agent whose lease lapsed must be swept inactive, got %q", s)
	}
	if s := status("idle"); s != "inactive" {
		t.Errorf("idle no-lease agent must be swept inactive, got %q", s)
	}
}
