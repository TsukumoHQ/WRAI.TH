package db

import "testing"

// T4: delivery_status makes the ack state auditable — surfaced/acknowledged
// timestamps are queryable by message_id and by recipient agent.
func TestDeliveryStatusQueryable(t *testing.T) {
	d := testDB(t)
	m, _, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "s", "c", "{}", "P1", 0, nil, nil, []string{"bot-a", "bot-b"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := d.DeliveryStatus("p1", m.ID, "")
	if err != nil {
		t.Fatalf("delivery_status by message: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 deliveries, got %d", len(rows))
	}
	var aID string
	for _, r := range rows {
		if r.State != "queued" {
			t.Errorf("fresh delivery state: want queued, got %q", r.State)
		}
		if r.ToAgent == "bot-a" {
			aID = r.DeliveryID
		}
	}
	if aID == "" {
		t.Fatal("no delivery row for bot-a")
	}

	// Surface then acknowledge bot-a's delivery; timestamps must become queryable.
	d.MarkDeliveriesSurfaced([]string{aID})
	if err := d.AcknowledgeDelivery(aID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	arows, err := d.DeliveryStatus("p1", "", "bot-a")
	if err != nil {
		t.Fatalf("delivery_status by agent: %v", err)
	}
	if len(arows) != 1 {
		t.Fatalf("want 1 delivery for bot-a, got %d", len(arows))
	}
	r := arows[0]
	if r.State != "acknowledged" {
		t.Errorf("state: want acknowledged, got %q", r.State)
	}
	if r.SurfacedAt == nil || r.AcknowledgedAt == nil {
		t.Errorf("surfaced_at/acknowledged_at must be set once seen+acked, got %v/%v", r.SurfacedAt, r.AcknowledgedAt)
	}

	// Neither filter -> error, not a full-table dump.
	if _, err := d.DeliveryStatus("p1", "", ""); err == nil {
		t.Error("delivery_status with no filter must error")
	}
}

// T4: a multi-project agent's cross-project unread rollup counts unread + P0 per
// OTHER project (current project excluded), bodies never included.
func TestCrossProjectUnreadRollup(t *testing.T) {
	d := testDB(t)
	if _, _, err := d.InsertMessageWithDeliveries("p1", "s", "bot-a", "notification", "s", "c", "{}", "P2", 0, nil, nil, []string{"bot-a"}, ""); err != nil {
		t.Fatalf("p1 msg: %v", err)
	}
	if _, _, err := d.InsertMessageWithDeliveries("p2", "s", "bot-a", "notification", "s", "c", "{}", "P0", 0, nil, nil, []string{"bot-a"}, ""); err != nil {
		t.Fatalf("p2 p0: %v", err)
	}
	if _, _, err := d.InsertMessageWithDeliveries("p2", "s", "bot-a", "notification", "s", "c", "{}", "P2", 0, nil, nil, []string{"bot-a"}, ""); err != nil {
		t.Fatalf("p2 p2: %v", err)
	}

	roll, err := d.CrossProjectUnread("bot-a", "p1")
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if _, ok := roll["p1"]; ok {
		t.Error("current project p1 must be excluded from the rollup")
	}
	p2 := roll["p2"]
	if p2 == nil {
		t.Fatal("p2 missing from rollup")
	}
	if p2["unread"] != 2 || p2["p0"] != 1 {
		t.Errorf("p2 rollup: want {unread:2,p0:1}, got %v", p2)
	}
}

// T4: DeliveryIDsForAgent maps message_id -> the caller's own delivery_id (and
// omits messages with no delivery to that agent) — the glue that lets get_thread
// / get_message / get_team_inbox attach an ack-able delivery_id.
func TestDeliveryIDsForAgent(t *testing.T) {
	d := testDB(t)
	m, _, err := d.InsertMessageWithDeliveries("p1", "s", "bot-a", "notification", "s", "c", "{}", "P2", 0, nil, nil, []string{"bot-a", "bot-b"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	mp, err := d.DeliveryIDsForAgent("p1", "bot-a", []string{m.ID})
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if mp[m.ID] == "" {
		t.Error("bot-a should have a delivery_id for the message")
	}
	// An agent with no delivery to the message gets nothing (not an error).
	none, err := d.DeliveryIDsForAgent("p1", "bot-c", []string{m.ID})
	if err != nil {
		t.Fatalf("ids none: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("bot-c has no delivery; want empty map, got %v", none)
	}
}
