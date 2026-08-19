package db

import "testing"

// R3: MarkDeliveriesSurfaced flips every queued delivery in one batched UPDATE,
// leaves already-surfaced / unknown ids untouched, and is a no-op on empty input.
func TestMarkDeliveriesSurfaced_Batch(t *testing.T) {
	d := testDB(t)
	var ids []string
	for i := 0; i < 5; i++ {
		if _, err := d.InsertMessageWithDeliveries("p", "cto", "b", "notification", "s", "c", "{}", "P2", 3600, nil, nil, []string{"b"}, ""); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	rows, err := d.ro().Query("SELECT id FROM deliveries WHERE project='p' AND to_agent='b' AND state='queued'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 5 {
		t.Fatalf("expected 5 queued deliveries, got %d", len(ids))
	}

	// Include an unknown id — it must be a harmless no-op.
	d.MarkDeliveriesSurfaced(append(ids, "does-not-exist"))

	var queued, surfaced int
	_ = d.ro().QueryRow("SELECT COUNT(*) FROM deliveries WHERE project='p' AND to_agent='b' AND state='queued'").Scan(&queued)
	_ = d.ro().QueryRow("SELECT COUNT(*) FROM deliveries WHERE project='p' AND to_agent='b' AND state='surfaced'").Scan(&surfaced)
	if queued != 0 || surfaced != 5 {
		t.Fatalf("expected 0 queued / 5 surfaced, got %d / %d", queued, surfaced)
	}

	// Empty input is a no-op (no panic).
	d.MarkDeliveriesSurfaced(nil)
}
