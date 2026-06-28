package db

import (
	"fmt"
	"testing"
)

func TestMetricsSnapshot(t *testing.T) {
	d := testDB(t)

	for i := 0; i < 3; i++ {
		if _, _, err := d.RegisterAgent("default", fmt.Sprintf("a%d", i), "m", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register a%d: %v", i, err)
		}
	}
	// One agent goes inactive — active count must exclude it.
	if _, err := d.conn.Exec(`UPDATE agents SET status='inactive' WHERE name='a2'`); err != nil {
		t.Fatalf("mark inactive: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := d.InsertMessageWithDeliveries("default", "a0", "a1", "notification", "s", "m", "{}", "P2", 3600, nil, nil, []string{"a1"}); err != nil {
			t.Fatalf("insert msg %d: %v", i, err)
		}
	}
	if err := d.RecordAudit(auditEntry("default", "a0", "transition", "t1", "s")); err != nil {
		t.Fatalf("audit: %v", err)
	}

	m := d.MetricsSnapshot()
	if m.AgentsTotal != 3 {
		t.Errorf("agents_total = %d, want 3", m.AgentsTotal)
	}
	if m.AgentsActive != 2 {
		t.Errorf("agents_active = %d, want 2", m.AgentsActive)
	}
	if m.MessagesTotal != 4 {
		t.Errorf("messages_total = %d, want 4", m.MessagesTotal)
	}
	if m.MessagesLastMinute != 4 {
		t.Errorf("messages_last_minute = %d, want 4 (all just inserted)", m.MessagesLastMinute)
	}
	if m.DeliveriesUnacked != 4 {
		t.Errorf("deliveries_unacked = %d, want 4", m.DeliveriesUnacked)
	}
	if m.AuditLogRows != 1 {
		t.Errorf("audit_log_rows = %d, want 1", m.AuditLogRows)
	}
}
