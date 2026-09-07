package db

import (
	"strings"
	"testing"
	"time"
)

// Writer-wait diagnostics (task 2d5fb3c2). Writer starvation was invisible: a
// scan/flush holding the sole writer connection (SetMaxOpenConns(1)) made every
// other write silently wait out writerTimeout with no attributable cause in the
// log. These pin the `writer wait:` / `integrity scan: took` observability lines.
//
// Contention is created the same way as wedge_test.go: d.conn.Begin() checks out
// the writer pool's ONLY connection and holds it uncommitted, so any other writer
// op blocks on pool-acquire until it is released. All timing vars are shrunk (the
// wedge_test trick) so the tests run in tens of ms, never a real 2s wait.

// acquireWriterTxForTest is a NAMED wrapper so beginWriterTx's runtime.Caller(1)
// resolves to a stable, assertable caller name in the begin-tx wait line.
func acquireWriterTxForTest(d *DB) (*writerTx, error) {
	return d.beginWriterTx()
}

// TestWriterExecLogsSlowWait (AC1): a writerExec that waits past writerSlowWait
// to acquire the sole writer connection logs exactly one `writer wait:` line
// carrying the query prefix.
func TestWriterExecLogsSlowWait(t *testing.T) {
	d := testDB(t)
	orig := writerSlowWait
	writerSlowWait = 20 * time.Millisecond
	defer func() { writerSlowWait = orig }()

	tx, err := d.conn.Begin() // holds the writer pool's ONLY connection
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = tx.Rollback() // frees the writer so writerExec can finally acquire it
	}()

	var res string
	out := captureLog(t, func() {
		_, _ = d.writerExec("UPDATE agents SET last_seen = ? WHERE name = ?", "x", "nobody")
		res = "done"
	})
	_ = res

	if n := strings.Count(out, "writer wait:"); n != 1 {
		t.Fatalf("want exactly 1 `writer wait:` line, got %d\nlog:\n%s", n, out)
	}
	if !strings.Contains(out, "UPDATE agents") {
		t.Fatalf("writer-wait line missing query prefix\nlog:\n%s", out)
	}
}

// TestWriterExecHighThresholdNoLog (AC1 revert-check): the SAME ~60ms contention
// with writerSlowWait raised above the wait logs nothing — the threshold, not the
// wait itself, decides.
func TestWriterExecHighThresholdNoLog(t *testing.T) {
	d := testDB(t)
	orig := writerSlowWait
	writerSlowWait = 500 * time.Millisecond
	defer func() { writerSlowWait = orig }()

	tx, err := d.conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = tx.Rollback()
	}()

	out := captureLog(t, func() {
		_, _ = d.writerExec("UPDATE agents SET last_seen = ? WHERE name = ?", "x", "nobody")
	})

	if strings.Contains(out, "writer wait:") {
		t.Fatalf("want no `writer wait:` line below threshold, got:\n%s", out)
	}
}

// TestWriterExecBelowThresholdLogsNothing (AC2): an uncontended, fast writerExec
// is silent — the diagnostic never fires on the common healthy path.
func TestWriterExecBelowThresholdLogsNothing(t *testing.T) {
	d := testDB(t)
	orig := writerSlowWait
	writerSlowWait = 500 * time.Millisecond
	defer func() { writerSlowWait = orig }()

	out := captureLog(t, func() {
		if _, err := d.writerExec("UPDATE agents SET last_seen = ? WHERE name = ?", "x", "nobody"); err != nil {
			t.Fatalf("writerExec: %v", err)
		}
	})

	if strings.Contains(out, "writer wait:") {
		t.Fatalf("uncontended fast writerExec must be silent, got:\n%s", out)
	}
}

// TestBeginWriterTxLogsSlowAcquire (AC3): a slow beginWriterTx acquire logs the
// begin-tx wait line with the caller resolved via runtime.Caller(1).
func TestBeginWriterTxLogsSlowAcquire(t *testing.T) {
	d := testDB(t)
	orig := writerSlowWait
	writerSlowWait = 20 * time.Millisecond
	defer func() { writerSlowWait = orig }()

	tx, err := d.conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = tx.Rollback()
	}()

	var wtx *writerTx
	out := captureLog(t, func() {
		var e error
		wtx, e = acquireWriterTxForTest(d)
		if e != nil {
			t.Errorf("acquireWriterTxForTest: %v", e)
		}
	})
	if wtx != nil {
		_ = wtx.Rollback()
	}

	if !strings.Contains(out, "writer wait: begin-tx caller=acquireWriterTxForTest") {
		t.Fatalf("want begin-tx wait line with caller=acquireWriterTxForTest, got:\n%s", out)
	}
}

// TestWriterExecErrorPathLogsAndReturnsError (AC4): when the wait exhausts
// writerTimeout, writerExec both logs the wait AND returns the error — the
// diagnostic covers the deadline path, not only the success path.
func TestWriterExecErrorPathLogsAndReturnsError(t *testing.T) {
	d := testDB(t)
	origT := writerTimeout
	origW := writerSlowWait
	writerTimeout = 40 * time.Millisecond
	writerSlowWait = 10 * time.Millisecond
	defer func() { writerTimeout = origT; writerSlowWait = origW }()

	tx, err := d.conn.Begin() // held for the whole test — never released in window
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var execErr error
	out := captureLog(t, func() {
		_, execErr = d.writerExec("UPDATE agents SET last_seen = ? WHERE name = ?", "x", "nobody")
	})

	if execErr == nil {
		t.Fatal("want writerExec to return the deadline error, got nil")
	}
	if !strings.Contains(out, "writer wait:") {
		t.Fatalf("error path must still log the wait, got:\n%s", out)
	}
}

// TestReferentialScanLogsSlowDuration: a scan whose total duration crosses
// writerSlowWait logs `integrity scan: took` (silent on the fast clean-DB path).
// The read-phase seam refScanReadHook is used to make the scan deterministically
// slow without real contention.
func TestReferentialScanLogsSlowDuration(t *testing.T) {
	d := testDB(t)
	orig := writerSlowWait
	writerSlowWait = 5 * time.Millisecond
	defer func() { writerSlowWait = orig }()

	refScanReadHook = func() { time.Sleep(15 * time.Millisecond) }
	defer func() { refScanReadHook = nil }()

	out := captureLog(t, func() {
		if _, err := d.RunReferentialScan(); err != nil {
			t.Fatalf("RunReferentialScan: %v", err)
		}
	})

	if !strings.Contains(out, "integrity scan: took") {
		t.Fatalf("want `integrity scan: took` on a slow scan, got:\n%s", out)
	}
}
