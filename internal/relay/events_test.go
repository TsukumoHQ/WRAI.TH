package relay

import (
	"sync"
	"testing"
)

// TestEventBus_ConcurrentEmitSubscribe stresses the bus the way the SSE
// handlers do: goroutines Emit while subscribers connect and disconnect.
// Before the fix, Emit iterated b.subs after releasing the lock, which the race
// detector flags as "concurrent map iteration and map write" and which can
// panic with "send on closed channel" when Unsubscribe closes mid-fan-out.
// Run with -race to be meaningful.
func TestEventBus_ConcurrentEmitSubscribe(t *testing.T) {
	bus := NewEventBus()
	stop := make(chan struct{})

	// Emitters run until stopped.
	var emitters sync.WaitGroup
	for i := 0; i < 4; i++ {
		emitters.Add(1)
		go func() {
			defer emitters.Done()
			for {
				select {
				case <-stop:
					return
				default:
					bus.Emit(MCPEvent{Type: "task.done", Project: "p1"})
				}
			}
		}()
	}

	// Subscribers churn: connect, maybe drain one event, disconnect.
	var subs sync.WaitGroup
	for i := 0; i < 8; i++ {
		subs.Add(1)
		go func() {
			defer subs.Done()
			for j := 0; j < 300; j++ {
				ch := bus.Subscribe()
				select {
				case <-ch:
				default:
				}
				bus.Unsubscribe(ch)
			}
		}()
	}

	subs.Wait() // let all the connect/disconnect churn race against Emit
	close(stop) // then stop the emitters
	emitters.Wait()
}
