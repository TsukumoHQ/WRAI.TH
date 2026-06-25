package ingest

import (
	"testing"
	"time"
)

// TestDetector_TickEmitsNonBlocking guards two properties of tick's emit:
//  1. it never blocks on d.out — nothing consumes that channel in production, so
//     a blocking send would wedge tick and freeze idle/exit detection (the prior
//     deadlock). The send is non-blocking (drops when unread).
//  2. it does not hold d.mu across the emit, so RecordEvent/GetSessions stay free.
func TestDetector_TickEmitsNonBlocking(t *testing.T) {
	// Unbuffered + no reader: a blocking send here would hang tick forever.
	out := make(chan AgentEvent)
	d := newDetector(out)

	// Session old enough to trigger an idle event on the next tick.
	d.RecordEvent(AgentEvent{
		SessionID: "s1",
		Type:      EventToolEnd,
		Timestamp: time.Now().Add(-idleThreshold - time.Second),
	})

	done := make(chan struct{})
	go func() {
		d.tick(time.Now()) // must return despite no reader on out
		d.RecordEvent(AgentEvent{SessionID: "s2", Type: EventToolEnd, Timestamp: time.Now()})
		_ = d.GetSessions()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick blocked on an unread channel (deadlock regression)")
	}
}

// TestDetector_TickDeliversWhenDrained confirms the idle event still reaches a
// reader when one is present (buffered sink).
func TestDetector_TickDeliversWhenDrained(t *testing.T) {
	out := make(chan AgentEvent, 4)
	d := newDetector(out)
	d.RecordEvent(AgentEvent{
		SessionID: "s1",
		Type:      EventToolEnd,
		Timestamp: time.Now().Add(-idleThreshold - time.Second),
	})
	d.tick(time.Now())
	select {
	case ev := <-out:
		if ev.Type != EventIdle {
			t.Fatalf("expected idle event, got %s", ev.Type)
		}
	default:
		t.Fatal("expected an idle event in the buffered sink")
	}
}
