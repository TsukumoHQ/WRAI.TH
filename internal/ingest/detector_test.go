package ingest

import (
	"testing"
	"time"
)

// TestDetector_TickEmitsWithoutHoldingLock is a regression test for C3: tick
// must not hold d.mu while sending on d.out. If it does, a stalled consumer
// (full/unbuffered channel) blocks tick with the lock held, head-of-line
// blocking every RecordEvent/GetSessions. We make d.out unbuffered so tick's
// emit blocks, then assert RecordEvent + GetSessions still proceed.
func TestDetector_TickEmitsWithoutHoldingLock(t *testing.T) {
	out := make(chan AgentEvent) // unbuffered: tick's send blocks until drained
	d := newDetector(out)

	// Session old enough to trigger an idle event on the next tick.
	d.RecordEvent(AgentEvent{
		SessionID: "s1",
		Type:      EventToolEnd,
		Timestamp: time.Now().Add(-idleThreshold - time.Second),
	})

	tickDone := make(chan struct{})
	go func() {
		d.tick(time.Now())
		close(tickDone)
	}()

	// While tick is blocked emitting on the unbuffered channel, d.mu must be
	// free — these must not hang.
	lockFree := make(chan struct{})
	go func() {
		d.RecordEvent(AgentEvent{SessionID: "s2", Type: EventToolEnd, Timestamp: time.Now()})
		_ = d.GetSessions()
		close(lockFree)
	}()

	select {
	case <-lockFree:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordEvent/GetSessions blocked while tick emitted — lock held during channel send (C3 regression)")
	}

	// Drain the event tick is emitting so it can finish.
	select {
	case ev := <-out:
		if ev.Type != EventIdle {
			t.Fatalf("expected idle event, got %s", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("expected an idle event from tick")
	}
	<-tickDone
}
