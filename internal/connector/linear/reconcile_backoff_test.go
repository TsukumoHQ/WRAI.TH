package linear

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestReconcileBackoff_AuthLimitsCallsOverAnHour is the AC1 regression: a poll
// that keeps getting HTTP 401 (dead/revoked API key) must NOT retry every
// interval forever (the 21 803x-401 log flood). Driven with a simulated clock
// over a full hour at the 1-minute settings-mode cadence, the connector must hit
// Linear at most 3 times.
func TestReconcileBackoff_AuthLimitsCallsOverAnHour(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"authentication failed - not authenticated"}]}`))
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	// Silence the (expected) one-shot ERROR line so the suite output stays clean.
	log.SetOutput(&bytes.Buffer{})
	defer log.SetOutput(os.Stderr)

	// Simulated clock: 1-minute ticks for 60 minutes (settings-mode reconcile
	// interval is 1m). runReconcile is synchronous, so no locking is needed.
	fakeNow := time.Now()
	c.nowFn = func() time.Time { return fakeNow }
	for i := 0; i < 60; i++ {
		c.runReconcile()
		fakeNow = fakeNow.Add(time.Minute)
	}

	got := atomic.LoadInt32(&hits)
	if got == 0 {
		t.Fatal("reconcile never called Linear — test wiring broken")
	}
	if got > 3 {
		t.Fatalf("auth-failing reconcile hit Linear %d times over a simulated hour; want <= 3 (a dead key must pause the poll)", got)
	}
}

// TestReconcileBackoff_AuthSurfacesOnceAndNamesTheFix is the AC1/AC3 regression
// for the log flood: repeated auth failures must surface exactly ONE ERROR line
// (not one per interval), and that line must name the fix. Nothing about a
// reconcile failure may be logged at INFO (the old `[linear] reconcile error:`
// line was unlevelled = INFO).
func TestReconcileBackoff_AuthSurfacesOnceAndNamesTheFix(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"authentication required"}]}`))
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	fakeNow := time.Now()
	c.nowFn = func() time.Time { return fakeNow }
	// Force several *actual* polls by jumping the clock past each backoff window.
	for i := 0; i < 5; i++ {
		c.runReconcile()
		fakeNow = fakeNow.Add(2 * time.Hour) // always due again
	}

	out := buf.String()
	if n := strings.Count(out, "reconcile ERROR"); n != 1 {
		t.Fatalf("expected exactly ONE auth ERROR line across repeated failures, got %d\n%s", n, out)
	}
	if !strings.Contains(out, "LINEAR_API_KEY") || !strings.Contains(out, "linear_enabled=0") {
		t.Fatalf("auth ERROR line must name the fix (rotate LINEAR_API_KEY / disable connector); got:\n%s", out)
	}
	// AC3: reconcile failures are never logged at INFO — the old unlevelled flood
	// line must be gone.
	if strings.Contains(out, "reconcile error:") {
		t.Fatalf("found the old INFO-level `reconcile error:` flood line; failures must be ERROR/WARN:\n%s", out)
	}
}

// TestReconcileBackoff_TransientIsBoundedExponential is the AC2 regression:
// transient errors (5xx/network/timeout) must use a bounded exponential backoff
// (30s→10m), not a fixed 60s, and must log at WARN (never INFO).
func TestReconcileBackoff_TransientIsBoundedExponential(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	want := []time.Duration{
		30 * time.Second,
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		10 * time.Minute, // capped
		10 * time.Minute,
	}
	for i, w := range want {
		c.noteReconcileResult(&httpError{code: http.StatusBadGateway}) // 502 = transient
		if c.reconcileDelay != w {
			t.Fatalf("transient backoff step %d = %s, want %s", i, c.reconcileDelay, w)
		}
		if c.reconcileDelay == time.Minute && i > 1 {
			t.Fatalf("transient backoff stuck at the old fixed 60s at step %d", i)
		}
	}
	if !strings.Contains(buf.String(), "reconcile WARN") {
		t.Fatalf("transient errors must log at WARN; got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "reconcile error:") {
		t.Fatalf("found the old INFO-level flood line for a transient error:\n%s", buf.String())
	}
}

// TestReconcileBackoff_SuccessResets pins that a successful poll clears the
// backoff so a recovered connector resumes its normal cadence immediately.
func TestReconcileBackoff_SuccessResets(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	log.SetOutput(&bytes.Buffer{})
	defer log.SetOutput(os.Stderr)

	// Latch an auth backoff.
	c.noteReconcileResult(&httpError{code: http.StatusUnauthorized})
	if c.reconcileDelay == 0 || !c.authLatched {
		t.Fatal("auth failure should latch a backoff")
	}
	if c.reconcileDue() {
		t.Fatal("connector should be paused right after an auth failure")
	}

	// A success clears everything.
	c.noteReconcileResult(nil)
	if c.reconcileDelay != 0 || c.authLatched {
		t.Fatalf("success must clear backoff: delay=%s latched=%v", c.reconcileDelay, c.authLatched)
	}
	if !c.reconcileDue() {
		t.Fatal("connector should be due again after a successful poll")
	}
}

// TestIsAuthError classifies 401/403 as auth and everything else as transient.
func TestIsAuthError(t *testing.T) {
	if !isAuthError(&httpError{code: 401}) || !isAuthError(&httpError{code: 403}) {
		t.Fatal("401/403 must be classified as auth failures")
	}
	for _, code := range []int{400, 429, 500, 502, 503} {
		if isAuthError(&httpError{code: code}) {
			t.Fatalf("HTTP %d must NOT be classified as an auth failure", code)
		}
	}
	if isAuthError(nil) {
		t.Fatal("nil is not an auth error")
	}
}
