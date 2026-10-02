package db

import (
	"sync"
	"testing"
	"time"
)

// TestReaderQuery_TimesOutOnPoolExhaustion is the regression test for the
// 2026-10-02 wedge (task 55323073): with all 10 reader connections pinned (the
// field dump had them in sqlite3_step on ListProjectsWithInfoFiltered), every
// d.ro() read used to park forever in database/sql's conn wait. Both shapes —
// Query (ListProjectsWithInfo, ListTasks) and QueryRow (GetAgentBySessionID,
// the Detector resolver) — must now return an error within readerTimeout.
func TestReaderQuery_TimesOutOnPoolExhaustion(t *testing.T) {
	d := soakDB(t)
	defer SetReaderTimeoutForTest(100 * time.Millisecond)()

	release, err := d.HoldPoolsForTest(10, false)
	if err != nil {
		t.Fatalf("hold reader pool: %v", err)
	}
	defer release()

	type result struct {
		name string
		err  error
	}
	done := make(chan result, 2)
	go func() {
		_, err := d.ListProjectsWithInfo()
		done <- result{"ListProjectsWithInfo (Query)", err}
	}()
	go func() {
		_, _, _, err := d.GetAgentBySessionID("sess-x")
		done <- result{"GetAgentBySessionID (QueryRow)", err}
	}()

	deadline := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			if r.err == nil {
				t.Errorf("%s succeeded with the reader pool fully held — expected a bounded timeout error", r.name)
			}
		case <-deadline:
			t.Fatal("reader call hung well past readerTimeout — pool-wait is not bounded")
		}
	}
}

// TestReaderQuery_RecoversAfterRelease: the deadline must not poison the pool —
// once the pinned connections come back, reads succeed again.
func TestReaderQuery_RecoversAfterRelease(t *testing.T) {
	d := soakDB(t)
	defer SetReaderTimeoutForTest(100 * time.Millisecond)()

	release, err := d.HoldPoolsForTest(10, false)
	if err != nil {
		t.Fatalf("hold reader pool: %v", err)
	}
	if _, err := d.ListProjectsWithInfo(); err == nil {
		t.Fatal("expected a timeout while the pool is held")
	}
	release()
	if _, err := d.ListProjectsWithInfo(); err != nil {
		t.Fatalf("read after release: %v", err)
	}
}

// TestProbe covers the /healthz DB probe: ok on a healthy DB, an error naming
// the pool when the reader pool is saturated or the writer is held.
func TestProbe(t *testing.T) {
	d := soakDB(t)

	if checks, err := d.Probe(time.Second); err != nil || checks["reader"] != "ok" || checks["writer"] != "ok" {
		t.Fatalf("healthy probe: checks=%v err=%v", checks, err)
	}

	for _, tc := range []struct {
		name    string
		readers int
		writer  bool
		failed  string
	}{
		{"reader pool saturated", 10, false, "reader"},
		{"writer held", 0, true, "writer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, err := d.HoldPoolsForTest(tc.readers, tc.writer)
			if err != nil {
				t.Fatalf("hold: %v", err)
			}
			defer release()
			start := time.Now()
			checks, err := d.Probe(100 * time.Millisecond)
			if err == nil {
				t.Fatalf("probe passed with %s: %v", tc.failed, checks)
			}
			if checks[tc.failed] == "ok" || checks[tc.failed] == "" {
				t.Errorf("checks[%q] = %q, want the error", tc.failed, checks[tc.failed])
			}
			if el := time.Since(start); el > time.Second {
				t.Errorf("probe took %s, want ~2x100ms", el)
			}
		})
	}
}

// TestListProjectsWithInfo_ConcurrentCallersShareResult: the singleflight
// path hands every concurrent caller the same rows, each in its own slice.
func TestListProjectsWithInfo_ConcurrentCallersShareResult(t *testing.T) {
	d := soakDB(t)
	d.EnsureProject("alpha")
	want, err := d.ListProjectsWithInfo()
	if err != nil || len(want) == 0 {
		t.Fatalf("baseline: %v %v", want, err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := d.ListProjectsWithInfo()
			if err != nil {
				errs <- err
				return
			}
			if len(got) != len(want) {
				t.Errorf("got %d projects, want %d", len(got), len(want))
				return
			}
			got[0].Name = "mutated" // must not leak into another caller's slice
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent ListProjectsWithInfo: %v", err)
	}
}
