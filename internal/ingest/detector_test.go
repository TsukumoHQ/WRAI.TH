package ingest

import (
	"fmt"
	"sync"
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
	d := newDetector(out, nil, nil)

	// Session old enough to trigger an idle event on the next tick.
	d.RecordEvent(AgentEvent{
		SessionID: "s1",
		Type:      EventToolEnd,
		Timestamp: time.Now().Add(-DefaultThresholds.Idle - time.Second),
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

// TestDetector_ResolvesAgent confirms RecordEvent tags a session with its owning
// agent via the resolver, that the resolver is consulted only until it resolves
// (cached thereafter), and that retries happen while a session stays unresolved.
func TestDetector_ResolvesAgent(t *testing.T) {
	out := make(chan AgentEvent, 4)
	calls := 0
	resolved := false
	d := newDetector(out, func(sid string) (string, string, bool) {
		calls++
		if !resolved {
			return "", "", false // unbound on first event
		}
		return "proj", "wraith-dev", true
	}, nil)

	// First event: resolver returns not-found → session stays untagged.
	d.RecordEvent(AgentEvent{SessionID: "s1", Type: EventToolStart, Tool: "Edit", Timestamp: time.Now()})
	if got := d.GetSessions(); len(got) != 1 || got[0].Agent != "" {
		t.Fatalf("expected untagged session, got %+v", got)
	}

	// Now the rebind lands; the next event resolves and caches the agent.
	resolved = true
	d.RecordEvent(AgentEvent{SessionID: "s1", Type: EventToolStart, Tool: "Bash", Timestamp: time.Now()})
	got := d.GetSessions()
	if len(got) != 1 || got[0].Agent != "wraith-dev" || got[0].Project != "proj" {
		t.Fatalf("expected session tagged wraith-dev/proj, got %+v", got)
	}

	// A third event must NOT call the resolver again (already cached).
	before := calls
	d.RecordEvent(AgentEvent{SessionID: "s1", Type: EventToolEnd, Timestamp: time.Now()})
	if calls != before {
		t.Fatalf("resolver called again after resolution (%d → %d); should be cached", before, calls)
	}
}

// TestDetector_TickDeliversWhenDrained confirms the idle event still reaches a
// reader when one is present (buffered sink).
func TestDetector_TickDeliversWhenDrained(t *testing.T) {
	out := make(chan AgentEvent, 4)
	d := newDetector(out, nil, nil)
	d.RecordEvent(AgentEvent{
		SessionID: "s1",
		Type:      EventToolEnd,
		Timestamp: time.Now().Add(-DefaultThresholds.Idle - time.Second),
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

// TestDetector_SlowResolveDoesNotHoldLock is the regression test for the
// 2026-10-02 wedge (task 55323073): RecordEvent called the resolver (a DB read)
// with d.mu held, so one resolve parked on a starved reader pool froze every
// other hook (284 goroutines at detector.go:155) and every GetSessions. A
// resolve that never returns must now block only its own event.
func TestDetector_SlowResolveDoesNotHoldLock(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	resolve := func(sid string) (string, string, bool) {
		if sid == "stuck" {
			<-gate // a DB read waiting on a pool connection that never frees
		}
		return "p", "agent-" + sid, true
	}
	d := newDetector(make(chan AgentEvent, 16), resolve, nil)

	go d.RecordEvent(AgentEvent{SessionID: "stuck", Type: EventToolStart, Timestamp: time.Now()})
	time.Sleep(20 * time.Millisecond) // let it reach the resolver

	done := make(chan struct{})
	go func() {
		d.RecordEvent(AgentEvent{SessionID: "other", Type: EventToolStart, Timestamp: time.Now()})
		_ = d.GetSessions()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a stuck resolve blocked an unrelated RecordEvent/GetSessions — d.mu is held across the DB read")
	}
}

// TestDetector_BurstWithSlowResolver: 200 concurrent first events, each
// resolve taking 50ms (a loaded DB). Holding d.mu across resolve serialized
// them (~10s); with the resolve outside the lock the burst finishes in about
// one resolve's time. Every session still ends up bound to its agent.
func TestDetector_BurstWithSlowResolver(t *testing.T) {
	resolve := func(sid string) (string, string, bool) {
		time.Sleep(50 * time.Millisecond)
		return "p", "agent-" + sid, true
	}
	d := newDetector(make(chan AgentEvent, 16), resolve, nil)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.RecordEvent(AgentEvent{SessionID: fmt.Sprintf("s%d", i), Type: EventToolStart, Timestamp: time.Now()})
			_ = d.GetSessions()
		}(i)
	}
	wg.Wait()
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("200-event burst took %s — resolves are serialized under d.mu", el)
	}
	sessions := d.GetSessions()
	if len(sessions) != 200 {
		t.Fatalf("got %d sessions, want 200", len(sessions))
	}
	for _, s := range sessions {
		if s.Agent != "agent-"+s.SessionID {
			t.Errorf("session %s bound to %q", s.SessionID, s.Agent)
		}
	}
}
