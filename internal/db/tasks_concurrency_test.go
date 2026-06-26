package db

import (
	"sync"
	"testing"
)

// TestClaimTask_NoDoubleClaim is the TOCTOU regression guard. transitionTask
// used to read the task, validate in Go, then UPDATE with only id+project in the
// WHERE — so N concurrent claims all read "pending", all validated, and all
// overwrote each other (double-claim). The fix adds a compare-and-swap guard
// (AND status = oldStatus) + a RowsAffected check. This test fires many claims
// at one pending task and asserts EXACTLY ONE wins.
func TestClaimTask_NoDoubleClaim(t *testing.T) {
	d := testDB(t)
	const project = "p1"

	task, err := d.DispatchTask(project, "", "dispatcher", "race me", "", "P1", nil, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	const racers = 12
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		winners  []string
		failures int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		agent := "agent-" + string(rune('a'+i))
		go func() {
			defer wg.Done()
			<-start // line everyone up so the claims truly overlap
			got, err := d.ClaimTask(task.ID, agent, project)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && got != nil {
				winners = append(winners, agent)
			} else {
				failures++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("want exactly 1 winner, got %d (%v) — double-claim regression", len(winners), winners)
	}
	if failures != racers-1 {
		t.Fatalf("want %d losers to get a conflict error, got %d", racers-1, failures)
	}

	// The persisted row must reflect the single winner.
	final, err := d.GetTask(task.ID, project)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if final.Status != "accepted" {
		t.Fatalf("want status accepted, got %q", final.Status)
	}
	if final.ClaimedBy == nil || *final.ClaimedBy != winners[0] {
		t.Fatalf("claimed_by must be the winner %q, got %v", winners[0], final.ClaimedBy)
	}
}
