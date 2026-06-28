package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// soakDB opens a DB with the production pool config (single writer + RO reader
// pool), usable from both tests and benchmarks (testing.TB).
func soakDB(tb testing.TB) *DB {
	tb.Helper()
	dbPath := filepath.Join(tb.TempDir(), "soak.db")
	writer, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON&_txlock=immediate")
	if err != nil {
		tb.Fatalf("open writer: %v", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	reader, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=10000&_foreign_keys=ON")
	if err != nil {
		tb.Fatalf("open reader: %v", err)
	}
	reader.SetMaxOpenConns(10)
	reader.SetMaxIdleConns(5)
	if err := migrate(writer); err != nil {
		tb.Fatalf("migrate: %v", err)
	}
	tb.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	return &DB{conn: writer, reader: reader, path: dbPath}
}

// TestSoakConcurrentAgents drives many concurrent agents through a sustained
// register → send → poll → mark-read loop (the production message hot path), then
// proves the GC reclaims the soft-expired rows. It asserts: no errors under
// contention, no deadlock (completes within a watchdog window), and bounded table
// growth (PurgeExpiredMessages drains the table). It also logs a perf baseline —
// message throughput — for TSU-134. Run with -v to see the numbers.
func TestSoakConcurrentAgents(t *testing.T) {
	d := soakDB(t)

	agents, iters := 50, 40
	if testing.Short() {
		agents, iters = 10, 10
	}

	names := make([]string, agents)
	for i := range names {
		names[i] = fmt.Sprintf("agent-%02d", i)
		if _, _, err := d.RegisterAgent("default", names[i], "soak", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", names[i], err)
		}
	}

	var sent, polled, marked, errs atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := range names {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			me := names[idx]
			for j := 0; j < iters; j++ {
				to := names[(idx+1+j)%agents] // deterministic rotating recipient
				if _, err := d.InsertMessageWithDeliveries("default", me, to, "notification", "soak", "m", "{}", "P2", 3600, nil, nil, []string{to}); err != nil {
					errs.Add(1)
					continue
				}
				sent.Add(1)
				msgs, err := d.GetInbox("default", me, true, 50)
				if err != nil {
					errs.Add(1)
					continue
				}
				polled.Add(1)
				if len(msgs) > 0 {
					ids := make([]string, len(msgs))
					for k, m := range msgs {
						ids[k] = m.ID
					}
					if _, err := d.MarkRead(ids, me, "default"); err != nil {
						errs.Add(1)
						continue
					}
					marked.Add(1)
				}
			}
		}(i)
	}

	// Watchdog: a deadlock (e.g. writer-pool starvation) must fail loudly, not
	// hang until the whole-suite timeout.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("soak did not complete within 60s — possible deadlock / writer starvation")
	}
	dur := time.Since(start)

	if errs.Load() > 0 {
		t.Fatalf("%d errors under concurrent load", errs.Load())
	}
	wantSent := int64(agents * iters)
	if sent.Load() != wantSent {
		t.Fatalf("sent = %d, want %d", sent.Load(), wantSent)
	}
	thru := float64(sent.Load()) / dur.Seconds()
	t.Logf("SOAK baseline: %d agents × %d iters → %d msgs sent, %d polls, %d mark-reads in %s → %.0f msg/s",
		agents, iters, sent.Load(), polled.Load(), marked.Load(), dur.Round(time.Millisecond), thru)

	// Bounded growth: soft-expire everything, then the GC must reclaim it all.
	before := countRows(t, d, `SELECT COUNT(*) FROM messages`)
	if _, err := d.conn.Exec(`UPDATE messages SET expired_at = ?`,
		time.Now().UTC().Add(-time.Hour).Format(memoryTimeFmt)); err != nil {
		t.Fatalf("force-expire: %v", err)
	}
	purged, err := d.PurgeExpiredMessages(0)
	if err != nil {
		t.Fatalf("PurgeExpiredMessages: %v", err)
	}
	afterMsgs := countRows(t, d, `SELECT COUNT(*) FROM messages`)
	afterDeliv := countRows(t, d, `SELECT COUNT(*) FROM deliveries`)
	t.Logf("GC baseline: %d msg rows → purged %d → %d msgs / %d deliveries remain", before, purged, afterMsgs, afterDeliv)
	if afterMsgs != 0 || afterDeliv != 0 {
		t.Errorf("GC left rows behind: %d messages, %d deliveries (want 0/0)", afterMsgs, afterDeliv)
	}
}

// BenchmarkInsertMessageWithDeliveries baselines the message send hot path.
func BenchmarkInsertMessageWithDeliveries(b *testing.B) {
	d := soakDB(b)
	_, _, _ = d.RegisterAgent("default", "from", "b", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	_, _, _ = d.RegisterAgent("default", "to", "b", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.InsertMessageWithDeliveries("default", "from", "to", "notification", "s", "m", "{}", "P2", 3600, nil, nil, []string{"to"}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetInbox baselines the poll (inbox read) path against a primed inbox.
func BenchmarkGetInbox(b *testing.B) {
	d := soakDB(b)
	_, _, _ = d.RegisterAgent("default", "from", "b", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	_, _, _ = d.RegisterAgent("default", "to", "b", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	for i := 0; i < 100; i++ {
		_, _ = d.InsertMessageWithDeliveries("default", "from", "to", "notification", "s", "m", "{}", "P2", 3600, nil, nil, []string{"to"})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.GetInbox("default", "to", true, 50); err != nil {
			b.Fatal(err)
		}
	}
}
