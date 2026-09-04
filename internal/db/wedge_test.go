package db

import (
	"testing"
	"time"
)

// TestBackup_DoesNotBlockWriter is the regression test for the serve wedge
// (task 34037526): Backup()'s VACUUM INTO used to run on d.conn, the single
// connection the writer pool serializes every app write behind
// (SetMaxOpenConns(1)). Here the writer pool's sole connection is held open
// via an uncommitted Begin() — exactly what a normal in-flight write looks
// like from Go's database/sql pool. Backup() must still complete: it now
// runs VACUUM INTO on its own dedicated connection, never touching d.conn.
func TestBackup_DoesNotBlockWriter(t *testing.T) {
	d := soakDB(t)
	if _, _, err := d.RegisterAgent("default", "a1", "role", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := d.conn.Begin() // holds the writer pool's ONLY connection, uncommitted
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	done := make(chan error, 1)
	go func() {
		_, err := d.Backup(3)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Backup errored: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Backup blocked behind the writer connection — regression: it must run VACUUM INTO on its own connection, not d.conn")
	}
}

// TestWriterExec_TimesOutOnPoolExhaustion pins the AC "busy-handler spin
// bounded: hard timeout -> error response, never infinite spin behind a
// leaked lock". With the writer pool's sole connection held open by another
// caller, writerExec must return a bounded timeout error instead of hanging
// forever waiting to acquire a connection from the pool.
func TestWriterExec_TimesOutOnPoolExhaustion(t *testing.T) {
	d := soakDB(t)
	orig := writerTimeout
	writerTimeout = 100 * time.Millisecond
	defer func() { writerTimeout = orig }()

	tx, err := d.conn.Begin() // holds the writer pool's ONLY connection
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	done := make(chan error, 1)
	go func() {
		_, err := d.writerExec("UPDATE agents SET last_seen = ? WHERE name = ?", "x", "nobody")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writerExec succeeded despite the writer pool being fully exhausted — expected a bounded timeout error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writerExec hung well past writerTimeout — pool-wait is not bounded")
	}
}

// TestBeginWriterTx_TimesOutOnPoolExhaustion is the beginWriterTx sibling of
// TestWriterExec_TimesOutOnPoolExhaustion.
func TestBeginWriterTx_TimesOutOnPoolExhaustion(t *testing.T) {
	d := soakDB(t)
	orig := writerTimeout
	writerTimeout = 100 * time.Millisecond
	defer func() { writerTimeout = orig }()

	tx, err := d.conn.Begin() // holds the writer pool's ONLY connection
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	done := make(chan error, 1)
	go func() {
		_, err := d.beginWriterTx()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("beginWriterTx succeeded despite the writer pool being fully exhausted — expected a bounded timeout error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("beginWriterTx hung well past writerTimeout — pool-wait is not bounded")
	}
}

// NOTE: outbox-replay dedup (ticket part B) was deliberately NOT implemented
// as a content-based heuristic — a first attempt (dedup on byte-identical
// project/from/to/type/subject/content within a time window) broke 5
// existing tests (TestMetricsSnapshot, TestBackupRestoreDrill,
// TestMarkDeliveriesSurfaced_Batch, TestCrossProjectUnreadRollup,
// TestInboxTruncationP0AndFirstRead) that legitimately insert multiple
// byte-identical messages (heartbeats, repeated status, test fixtures) —
// proving identical-content-from-same-sender is normal traffic, not
// evidence of a retry. A correct fix needs an explicit idempotency key from
// the caller (an API change, out of scope here); with root cause A fixed
// (writer calls now bounded, no longer wedging), the client-timeout trigger
// that caused replays should itself become rare-to-none. Follow-up: add a
// caller-supplied idempotency key if replay is still observed after this fix
// ships.

// TestBeginWriterTx_FreshBodyBudgetAfterSlowAcquire is the regression test for
// the "sql: transaction has already been committed or rolled back" error
// observed in the expire-deliveries sweep during the 2026-09-02 writer-
// contention window. beginWriterTx once bounded the pool-acquire AND the
// transaction body with ONE shared deadline: a slow acquire (writer held by a
// concurrent op) left only a sliver of that deadline for the body, the deadline
// then fired mid-transaction, database/sql rolled the tx back on its own
// goroutine, and the caller's next Exec/Commit hit an already-finished tx.
//
// Here the sole writer connection is held for most of writerTimeout, so the
// acquire consumes nearly the whole (old) shared budget; a small body delay then
// pushes the Exec/Commit past where the old shared deadline would have fired.
// With the acquire/body split the body has its own fresh budget, so Exec and
// Commit succeed instead of returning ErrTxDone.
func TestBeginWriterTx_FreshBodyBudgetAfterSlowAcquire(t *testing.T) {
	d := soakDB(t)
	orig := writerTimeout
	writerTimeout = 300 * time.Millisecond
	defer func() { writerTimeout = orig }()

	// Hold the writer pool's ONLY connection for most of writerTimeout, then
	// release it so beginWriterTx can acquire with almost no shared budget left.
	hold, err := d.conn.Begin()
	if err != nil {
		t.Fatalf("hold begin: %v", err)
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		_ = hold.Rollback()
	}()

	tx, err := d.beginWriterTx() // blocks ~250ms acquiring the connection
	if err != nil {
		t.Fatalf("beginWriterTx after slow acquire: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A body slice that outlives the *remaining* sliver of the old shared
	// deadline but fits comfortably in a fresh per-body budget. Pre-fix, the
	// shared ctx expired here and the Exec below returned
	// "sql: transaction has already been committed or rolled back".
	time.Sleep(100 * time.Millisecond)
	if _, err := tx.Exec("UPDATE agents SET last_seen = ? WHERE name = ?", "x", "nobody"); err != nil {
		t.Fatalf("tx.Exec after slow acquire (tx guillotined mid-body?): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx.Commit after slow acquire (tx guillotined mid-body?): %v", err)
	}
}
