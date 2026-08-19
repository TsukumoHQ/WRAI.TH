package db

import "testing"

// TestInsertEvent_DedupAndReplay covers the outbox foundation (TSU-52 slice-A):
// dedup on delivery_id (INSERT OR IGNORE), the replay log, and the
// undelivered→delivered sweeper queue.
func TestInsertEvent_DedupAndReplay(t *testing.T) {
	d := testDB(t)
	const project = "p1"

	// A stable delivery_id (e.g. a webhook GUID) inserts once, then dedupes.
	id1, ins1, err := d.InsertEvent("deliv-1", project, "event:task-stale", "wraith-dev", `{"task_id":"t1"}`)
	if err != nil || !ins1 {
		t.Fatalf("first insert: ins=%v err=%v", ins1, err)
	}
	id2, ins2, err := d.InsertEvent("deliv-1", project, "event:task-stale", "wraith-dev", `{"task_id":"t1"}`)
	if err != nil {
		t.Fatalf("second insert err: %v", err)
	}
	if ins2 {
		t.Fatalf("duplicate delivery_id must be ignored (inserted=false)")
	}
	if id1 == "" || id2 == "" {
		t.Fatalf("expected ids returned")
	}

	// Empty delivery_id → always inserts (fresh UUID).
	if _, ins, err := d.InsertEvent("", project, "task.done", "bob", `{}`); err != nil || !ins {
		t.Fatalf("empty delivery_id should insert: ins=%v err=%v", ins, err)
	}

	// Replay log: 2 distinct events (the dup didn't add a row).
	recent, err := d.RecentEvents(project, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("want 2 events in replay log (dup ignored), got %d", len(recent))
	}

	// Sweeper queue: both undelivered until marked.
	undel, err := d.UndeliveredEvents(10)
	if err != nil {
		t.Fatalf("undelivered: %v", err)
	}
	if len(undel) != 2 {
		t.Fatalf("want 2 undelivered, got %d", len(undel))
	}
	if err := d.MarkEventDelivered(id1); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	undel, _ = d.UndeliveredEvents(10)
	if len(undel) != 1 {
		t.Fatalf("want 1 undelivered after marking one, got %d", len(undel))
	}
}

// TestDeleteEventByDelivery: the compensating action releases a reservation so a
// redelivery can retry (insert again) instead of deduping to a no-op.
func TestDeleteEventByDelivery(t *testing.T) {
	d := testDB(t)
	const project = "p1"

	if _, ins, _ := d.InsertEvent("sig-1", project, "signal:ci", "signal:ci", `{}`); !ins {
		t.Fatal("first insert should succeed")
	}
	// Same delivery dedupes while the row exists.
	if _, ins, _ := d.InsertEvent("sig-1", project, "signal:ci", "signal:ci", `{}`); ins {
		t.Fatal("redelivery should dedupe before compensate-delete")
	}
	// Compensate-delete, then the same delivery inserts again (retryable).
	d.DeleteEventByDelivery(project, "sig-1")
	if _, ins, _ := d.InsertEvent("sig-1", project, "signal:ci", "signal:ci", `{}`); !ins {
		t.Fatal("after compensate-delete a redelivery must insert again")
	}
	// Wrong project is a no-op (scoped delete).
	d.DeleteEventByDelivery("other", "sig-1")
	if _, ins, _ := d.InsertEvent("sig-1", project, "signal:ci", "signal:ci", `{}`); ins {
		t.Fatal("cross-project delete must not release this project's reservation")
	}
}
