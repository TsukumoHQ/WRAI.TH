package db

import (
	"testing"
	"time"
)

// TestCommsMetricsSnapshot exercises the comms S4 view end to end: wake-rate
// over deliveries, per-sender action_required distribution, read-latency
// bucketing, and coalescing detection.
func TestCommsMetricsSnapshot(t *testing.T) {
	d := testDB(t)

	// cto: a wake (task/do), a no-wake (notification/none), a wake (question/ask).
	task, _ := d.InsertMessageWithDeliveries("p", "cto", "b", "task", "s", "x", "{}", "P2", 3600, nil, nil, []string{"b"}, "")
	if _, err := d.InsertMessageWithDeliveries("p", "cto", "b", "notification", "s", "x", "{}", "P2", 3600, nil, nil, []string{"b"}, ""); err != nil {
		t.Fatalf("notif: %v", err)
	}
	q, _ := d.InsertMessageWithDeliveries("p", "cto", "b", "question", "s", "x", "{}", "P2", 3600, nil, nil, []string{"b"}, "")
	// worker: two no-wakes.
	d.InsertMessageWithDeliveries("p", "worker", "b", "fyi", "s", "x", "{}", "P2", 3600, nil, nil, []string{"b"}, "")
	d.InsertMessageWithDeliveries("p", "worker", "b", "status", "s", "x", "{}", "P2", 3600, nil, nil, []string{"b"}, "")

	// Deterministic read receipt on the task: read 30s after send → under-1m bucket.
	created, _ := time.Parse(memoryTimeFmt, task.CreatedAt)
	readAt := created.Add(30 * time.Second).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(
		"INSERT INTO message_reads (message_id, agent_name, project, read_at) VALUES (?,?,?,?)",
		task.ID, "b", "p", readAt,
	); err != nil {
		t.Fatalf("read receipt: %v", err)
	}

	m := d.CommsMetricsSnapshot("p", time.Hour)

	// Wake-rate: 5 deliveries, 2 wake (task+question), 3 no-wake → 0.6 share.
	if m.Deliveries.Total != 5 {
		t.Errorf("total = %d, want 5", m.Deliveries.Total)
	}
	if m.Deliveries.WouldWake != 2 {
		t.Errorf("would_wake = %d, want 2", m.Deliveries.WouldWake)
	}
	if m.Deliveries.NoWake != 3 {
		t.Errorf("no_wake = %d, want 3", m.Deliveries.NoWake)
	}
	if m.Deliveries.NoWakeShare < 0.59 || m.Deliveries.NoWakeShare > 0.61 {
		t.Errorf("no_wake_share = %v, want ~0.60", m.Deliveries.NoWakeShare)
	}

	// Per-sender distribution.
	bySender := map[string]SenderActionDist{}
	for _, s := range m.ActionRequiredBySender {
		bySender[s.From] = s
	}
	if c := bySender["cto"]; c.Do != 1 || c.Ask != 1 || c.None != 1 || c.Total != 3 {
		t.Errorf("cto dist = %+v, want do1/ask1/none1/total3", c)
	}
	if wkr := bySender["worker"]; wkr.None != 2 || wkr.Total != 2 {
		t.Errorf("worker dist = %+v, want none2/total2", wkr)
	}

	// Read latency: one receipt, 30s → under-1m.
	if m.ReadLatency.Sampled != 1 || m.ReadLatency.Under1m != 1 {
		t.Errorf("read_latency = %+v, want sampled1/under1m1", m.ReadLatency)
	}

	// Coalescing: the question is a wake-eligible delivery landing after a prior
	// wake-eligible delivery (the task) to the same recipient within 5min → 1.
	if m.CoalescingCandidates != 1 {
		t.Errorf("coalescing = %d, want 1", m.CoalescingCandidates)
	}
	_ = q
}

// TestCommsMetricsEmpty: a project with no traffic returns a clean zero snapshot,
// never a divide-by-zero or nil-slice panic.
func TestCommsMetricsEmpty(t *testing.T) {
	d := testDB(t)
	m := d.CommsMetricsSnapshot("empty", time.Hour)
	if m.Deliveries.Total != 0 || m.Deliveries.NoWakeShare != 0 {
		t.Errorf("empty snapshot not zero: %+v", m.Deliveries)
	}
	if len(m.ActionRequiredBySender) != 0 {
		t.Errorf("empty senders = %v", m.ActionRequiredBySender)
	}
	if m.TargetNoWakeRate != 0.55 {
		t.Errorf("target = %v, want 0.55", m.TargetNoWakeRate)
	}
}
