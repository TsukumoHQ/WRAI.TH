package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Regression tests for the 2026-10-02 relay wedge (task 55323073): every pooled
// DB connection pinned, handlers parked forever in database/sql's conn wait, and
// GET / kept answering 200 so nothing noticed. A starved DB must now surface as
// a prompt error response and a 503 on /healthz.

func getHealthz(r *Relay) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.serveHealthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return w
}

func TestHealthz_OKWhenDBResponsive(t *testing.T) {
	r := testRelay(t)
	w := getHealthz(r)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz on a healthy DB: %d %s", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["status"]; got != "ok" {
		t.Fatalf("status = %v, want ok", got)
	}
}

func TestHealthz_503WhenDBStarved(t *testing.T) {
	orig := healthzTimeout
	healthzTimeout = 100 * time.Millisecond
	defer func() { healthzTimeout = orig }()

	for _, tc := range []struct {
		name    string
		readers int
		writer  bool
	}{
		{"reader pool saturated", 10, false},
		{"writer held", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testRelay(t)
			release, err := r.DB.HoldPoolsForTest(tc.readers, tc.writer)
			if err != nil {
				t.Fatalf("hold: %v", err)
			}
			defer release()

			start := time.Now()
			w := getHealthz(r)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("healthz with %s: %d %s, want 503", tc.name, w.Code, w.Body.String())
			}
			if el := time.Since(start); el > time.Second {
				t.Fatalf("healthz took %s, want ~healthzTimeout", el)
			}
		})
	}
}

// TestHandler_StarvedDBReturnsErrorWithinDeadline: with the reader pool pinned
// (the field shape) and, separately, the writer held (a slow writer), the
// handlers seen hanging in the dumps must answer with an error inside the DB
// deadline instead of hanging.
func TestHandler_StarvedDBReturnsErrorWithinDeadline(t *testing.T) {
	r := testRelay(t)
	defer dbSetTimeouts(100 * time.Millisecond)()

	t.Run("reader pool saturated", func(t *testing.T) {
		release, err := r.DB.HoldPoolsForTest(10, false)
		if err != nil {
			t.Fatalf("hold: %v", err)
		}
		defer release()
		for _, path := range []string{"/projects", "/tasks?project=p1"} {
			w := within(t, 2*time.Second, "GET "+path, func() *httptest.ResponseRecorder { return doAPI(r, "GET", path, "") })
			if w.Code < 500 {
				t.Errorf("GET %s with a starved reader pool: %d, want a 5xx error", path, w.Code)
			}
		}
	})

	t.Run("writer held", func(t *testing.T) {
		release, err := r.DB.HoldPoolsForTest(0, true)
		if err != nil {
			t.Fatalf("hold: %v", err)
		}
		defer release()
		w := within(t, 2*time.Second, "POST /tasks", func() *httptest.ResponseRecorder {
			return doAPI(r, "POST", "/tasks", `{"project":"p1","dispatched_by":"bot-a","profile":"dev","title":"Fix bug","description":"fix it"}`)
		})
		if w.Code < 400 {
			t.Errorf("POST /tasks with the writer held: %d, want an error", w.Code)
		}
	})
}

// TestBurst_IngestAndTaskReads: 200 concurrent hook events (fresh sessions, so
// every one runs the DB-backed resolver) interleaved with 200 task reads. All
// complete, all succeed, none exceeds the deadline. A second round with the
// reader pool pinned must still finish every request inside the deadline: hooks
// stay 204 (resolve fails fast, outside Detector.mu) and reads error out.
func TestBurst_IngestAndTaskReads(t *testing.T) {
	r := testRelay(t)
	withIngester(t, r)
	defer dbSetTimeouts(200 * time.Millisecond)()
	const deadline = 5 * time.Second

	burst := func(t *testing.T, wantReadOK bool) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var slowest time.Duration
		fail := make(chan string, 400)
		for i := 0; i < 200; i++ {
			wg.Add(2)
			go func(i int) {
				defer wg.Done()
				start := time.Now()
				w := doAPI(r, "POST", "/ingest/activity", fmt.Sprintf(`{"session_id":"burst-%d","type":"tool_start","tool":"Edit"}`, i))
				el := time.Since(start)
				mu.Lock()
				slowest = max(slowest, el)
				mu.Unlock()
				if w.Code != http.StatusNoContent {
					fail <- fmt.Sprintf("ingest %d: %d", i, w.Code)
				}
			}(i)
			go func(i int) {
				defer wg.Done()
				start := time.Now()
				w := doAPI(r, "GET", "/tasks?project=p1", "")
				el := time.Since(start)
				mu.Lock()
				slowest = max(slowest, el)
				mu.Unlock()
				if wantReadOK && w.Code != http.StatusOK {
					fail <- fmt.Sprintf("task read %d: %d", i, w.Code)
				}
			}(i)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * deadline):
			t.Fatal("burst did not complete — handlers are hanging")
		}
		close(fail)
		for f := range fail {
			t.Error(f)
		}
		if slowest > deadline {
			t.Errorf("slowest request took %s, want <= %s", slowest, deadline)
		}
	}

	t.Run("healthy", func(t *testing.T) { burst(t, true) })
	t.Run("reader pool saturated", func(t *testing.T) {
		release, err := r.DB.HoldPoolsForTest(10, false)
		if err != nil {
			t.Fatalf("hold: %v", err)
		}
		defer release()
		burst(t, false)
	})
}

// dbSetTimeouts shrinks both DB pool deadlines for a test; returns the restore.
func dbSetTimeouts(d time.Duration) func() {
	rr := db.SetReaderTimeoutForTest(d)
	rw := db.SetWriterTimeoutForTest(d)
	return func() { rr(); rw() }
}

// within runs fn and fails the test if it does not return inside limit.
func within(t *testing.T, limit time.Duration, what string, fn func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() { ch <- fn() }()
	select {
	case w := <-ch:
		return w
	case <-time.After(limit):
		t.Fatalf("%s hung past %s", what, limit)
		return nil
	}
}
