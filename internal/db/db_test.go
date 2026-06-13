package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Writer pool (matches production config)
	writer, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON&_txlock=immediate")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)

	// Reader pool (matches production config)
	reader, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=10000&_foreign_keys=ON")
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	reader.SetMaxOpenConns(10)
	reader.SetMaxIdleConns(5)

	if err := migrate(writer); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	return &DB{conn: writer, reader: reader, path: dbPath}
}

func TestConcurrentReadsAndWrite(t *testing.T) {
	d := testDB(t)

	// Seed an agent
	_, _, _ = d.RegisterAgent("default", "bot-a", "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})

	var wg sync.WaitGroup
	var errors atomic.Int32

	// 10 concurrent reads
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.ListAgents("default")
			if err != nil {
				errors.Add(1)
			}
		}()
	}

	// 5 concurrent writes
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := d.InsertMessage("default", "bot-a", "bot-b", "notification", "test", fmt.Sprintf("msg-%d", n), "{}", "P2", 3600, nil, nil)
			if err != nil {
				errors.Add(1)
			}
		}(i)
	}

	wg.Wait()
	if errors.Load() > 0 {
		t.Errorf("got %d errors during concurrent operations", errors.Load())
	}

	// Verify all writes landed
	msgs, err := d.GetAllRecentMessages("default", 100)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 5 {
		t.Errorf("expected 5 messages, got %d", len(msgs))
	}
}

func TestConcurrentWriters(t *testing.T) {
	d := testDB(t)

	_, _, _ = d.RegisterAgent("default", "bot-a", "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})

	var wg sync.WaitGroup
	var errors atomic.Int32
	n := 20

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := d.InsertMessage("default", "bot-a", "bot-b", "notification", "test", fmt.Sprintf("write-%d", idx), "{}", "P2", 3600, nil, nil)
			if err != nil {
				errors.Add(1)
				t.Logf("write error: %v", err)
			}
		}(i)
	}

	wg.Wait()
	if errors.Load() > 0 {
		t.Errorf("got %d errors during %d concurrent writes", errors.Load(), n)
	}

	msgs, _ := d.GetAllRecentMessages("default", 100)
	if len(msgs) != n {
		t.Errorf("expected %d messages, got %d", n, len(msgs))
	}
}

func TestOptimizeNoCorruption(t *testing.T) {
	d := testDB(t)

	// Insert some data
	_, _, _ = d.RegisterAgent("default", "bot-a", "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	for i := 0; i < 10; i++ {
		_, _ = d.InsertMessage("default", "bot-a", "bot-b", "notification", "test", fmt.Sprintf("msg-%d", i), "{}", "P2", 3600, nil, nil)
	}

	// Run optimize
	d.Optimize()

	// Verify integrity
	var result string
	err := d.conn.QueryRow("PRAGMA integrity_check").Scan(&result)
	if err != nil {
		t.Fatalf("integrity_check failed: %v", err)
	}
	if result != "ok" {
		t.Errorf("integrity_check returned: %s", result)
	}

	// Verify data still accessible
	msgs, err := d.GetAllRecentMessages("default", 100)
	if err != nil {
		t.Fatalf("get messages after optimize: %v", err)
	}
	if len(msgs) != 10 {
		t.Errorf("expected 10 messages after optimize, got %d", len(msgs))
	}
}

func TestReaderPoolIsSeparate(t *testing.T) {
	d := testDB(t)

	// Reader and writer should be different pool instances
	if d.conn == d.reader {
		t.Fatal("reader and writer pools should be separate *sql.DB instances")
	}

	// Reader pool should allow more concurrent connections
	writerMax := d.conn.Stats().MaxOpenConnections
	readerMax := d.reader.Stats().MaxOpenConnections
	if writerMax != 1 {
		t.Errorf("writer MaxOpenConns should be 1, got %d", writerMax)
	}
	if readerMax < 5 {
		t.Errorf("reader MaxOpenConns should be >= 5, got %d", readerMax)
	}
}

func TestReadAfterWrite(t *testing.T) {
	d := testDB(t)

	// Write via writer
	_, _, _ = d.RegisterAgent("default", "bot-a", "tester", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})

	// Read via reader should see the write (WAL visibility)
	agents, err := d.ListAgents("default")
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Name != "bot-a" {
		t.Errorf("expected bot-a, got %s", agents[0].Name)
	}
}

func TestReadsNeverBlockedByWrite(t *testing.T) {
	d := testDB(t)
	_, _, _ = d.RegisterAgent("default", "bot-a", "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})

	// Start a long write transaction on the writer
	tx, err := d.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		_, _ = tx.Exec("INSERT INTO messages (id, from_agent, to_agent, type, content, created_at, project) VALUES (?, 'bot-a', 'bot-b', 'notification', 'test', datetime('now'), 'default')", fmt.Sprintf("tx-msg-%d", i))
	}

	// While the write tx is open, reads via reader pool should succeed immediately
	var wg sync.WaitGroup
	var readErrors atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.ListAgents("default")
			if err != nil {
				readErrors.Add(1)
				t.Logf("read error during open tx: %v", err)
			}
		}()
	}
	wg.Wait()

	if readErrors.Load() > 0 {
		t.Errorf("got %d read errors while write tx was open", readErrors.Load())
	}

	_ = tx.Commit()
}

func TestWritesDontUseManyConns(t *testing.T) {
	d := testDB(t)
	_, _, _ = d.RegisterAgent("default", "bot-a", "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})

	// Writer pool has MaxOpenConns=1. Concurrent writes should serialize, not error.
	var wg sync.WaitGroup
	var errors atomic.Int32
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := d.InsertMessage("default", "bot-a", "bot-b", "notification", "test", fmt.Sprintf("serial-%d", idx), "{}", "P2", 3600, nil, nil)
			if err != nil {
				errors.Add(1)
				t.Logf("write error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if errors.Load() > 0 {
		t.Errorf("got %d write errors with single-conn writer pool", errors.Load())
	}

	msgs, _ := d.GetAllRecentMessages("default", 100)
	if len(msgs) != 30 {
		t.Errorf("expected 30 messages, got %d", len(msgs))
	}
}

func TestMixedReadWriteFunction(t *testing.T) {
	d := testDB(t)

	// RegisterAgent reads (check existing) then writes (insert) — tests mixed path
	agent1, isRespawn1, err := d.RegisterAgent("default", "bot-a", "tester", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	if isRespawn1 {
		t.Error("expected new agent, got respawn")
	}
	if agent1.Name != "bot-a" {
		t.Errorf("expected bot-a, got %s", agent1.Name)
	}

	// Re-register same agent — should read existing via writer, then update
	agent2, isRespawn2, err := d.RegisterAgent("default", "bot-a", "updated", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("re-register agent: %v", err)
	}
	if !isRespawn2 {
		t.Error("expected respawn, got new")
	}
	if agent2.Role != "updated" {
		t.Errorf("expected role 'updated', got %s", agent2.Role)
	}

	// Dispatch + complete task — tests transitionTask mixed read/write
	task, err := d.DispatchTask("default", "dev", "bot-a", "test task", "desc", "P2", nil, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	_, err = d.ClaimTask(task.ID, "bot-a", "default")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, err = d.StartTask(task.ID, "bot-a", "default")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	result := "done!"
	completed, err := d.CompleteTask(task.ID, "bot-a", "default", &result)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.Status != "done" {
		t.Errorf("expected done, got %s", completed.Status)
	}
}

func TestCloseCheckpoint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	writer, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_foreign_keys=ON&_txlock=immediate")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	writer.SetMaxOpenConns(1)
	reader, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=10000&_foreign_keys=ON")
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	reader.SetMaxOpenConns(10)
	if err := migrate(writer); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	d := &DB{conn: writer, reader: reader, path: dbPath}

	// Insert data to create WAL entries
	_, _, _ = d.RegisterAgent("default", "bot-a", "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	for i := 0; i < 50; i++ {
		_, _ = d.InsertMessage("default", "bot-a", "bot-b", "notification", "test", fmt.Sprintf("msg-%d", i), "{}", "P2", 3600, nil, nil)
	}

	// Close should TRUNCATE checkpoint
	_ = d.Close()

	// Reopen and verify data intact
	writer2, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=10000&_foreign_keys=ON")
	if err != nil {
		t.Fatalf("reopen writer: %v", err)
	}
	defer func() { _ = writer2.Close() }()
	var count int
	_ = writer2.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if count != 50 {
		t.Errorf("expected 50 messages after close+reopen, got %d", count)
	}
}

func strptr(s string) *string { return &s }

// TestRegisterAgentPreservesProfileSlugOnOmit reproduces the production bug:
// orchestrator registers an agent WITH profile_slug, then the agent's own
// in-pane /relay register re-registers WITHOUT profile_slug (per skill/relay.md).
// The slug must survive — NULLing it hides the agent's dispatched/pending tasks.
func TestRegisterAgentPreservesProfileSlugOnOmit(t *testing.T) {
	d := testDB(t)

	// Orchestrator registers with a profile slug.
	_, _, err := d.RegisterAgent("default", "bot-a", "dev", "", nil, strptr("backend"), false, nil, "[]", 0, RegisterOptions{
		ProfileSlugSet: true,
	})
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	// Agent self-registers WITHOUT profile_slug (omitted → nil, not "set").
	agent, isRespawn, err := d.RegisterAgent("default", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if !isRespawn {
		t.Fatal("expected respawn on re-register")
	}
	if agent.ProfileSlug == nil || *agent.ProfileSlug != "backend" {
		t.Fatalf("profile_slug must survive omitted re-register, got %v", agent.ProfileSlug)
	}

	// Verify persisted (not just the returned struct).
	got, _ := d.GetAgent("default", "bot-a")
	if got.ProfileSlug == nil || *got.ProfileSlug != "backend" {
		t.Fatalf("persisted profile_slug must survive, got %v", got.ProfileSlug)
	}
}

// TestRegisterAgentPreservesReportsToOnOmit ensures org hierarchy survives respawn.
func TestRegisterAgentPreservesReportsToOnOmit(t *testing.T) {
	d := testDB(t)

	_, _, _ = d.RegisterAgent("default", "lead", "lead", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	_, _, err := d.RegisterAgent("default", "bot-a", "dev", "", strptr("lead"), nil, false, nil, "[]", 0, RegisterOptions{
		ReportsToSet: true,
	})
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	agent, _, err := d.RegisterAgent("default", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if agent.ReportsTo == nil || *agent.ReportsTo != "lead" {
		t.Fatalf("reports_to must survive omitted re-register, got %v", agent.ReportsTo)
	}
}

// TestRegisterAgentPreservesIsExecutiveOnOmit ensures executive flag survives respawn.
func TestRegisterAgentPreservesIsExecutiveOnOmit(t *testing.T) {
	d := testDB(t)

	_, _, err := d.RegisterAgent("default", "cto", "cto", "", nil, nil, true, nil, "[]", 0, RegisterOptions{
		IsExecutiveSet: true,
	})
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	agent, _, err := d.RegisterAgent("default", "cto", "cto", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if !agent.IsExecutive {
		t.Fatal("is_executive must survive omitted re-register")
	}
}

// TestRegisterAgentPreservesSessionIDOnOmit ensures activity-tracking session survives respawn.
func TestRegisterAgentPreservesSessionIDOnOmit(t *testing.T) {
	d := testDB(t)

	_, _, _ = d.RegisterAgent("default", "bot-a", "dev", "", nil, nil, false, strptr("sess-123"), "[]", 0, RegisterOptions{
		SessionIDSet: true,
	})
	agent, _, err := d.RegisterAgent("default", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if agent.SessionID == nil || *agent.SessionID != "sess-123" {
		t.Fatalf("session_id must survive omitted re-register, got %v", agent.SessionID)
	}
}

// TestRegisterAgentExplicitFieldsStillUpdate ensures preserve-on-omit does NOT
// block legitimate updates: when a field IS provided, it must overwrite.
func TestRegisterAgentExplicitFieldsStillUpdate(t *testing.T) {
	d := testDB(t)

	_, _, _ = d.RegisterAgent("default", "bot-a", "dev", "old", nil, strptr("backend"), false, nil, "[]", 0, RegisterOptions{
		ProfileSlugSet: true,
	})

	// Re-register with a DIFFERENT profile slug (explicitly set) — must update.
	agent, _, err := d.RegisterAgent("default", "bot-a", "dev2", "new", nil, strptr("frontend"), false, nil, "[]", 0, RegisterOptions{
		ProfileSlugSet: true,
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if agent.ProfileSlug == nil || *agent.ProfileSlug != "frontend" {
		t.Fatalf("explicit profile_slug must update, got %v", agent.ProfileSlug)
	}
	if agent.Role != "dev2" {
		t.Fatalf("role must update, got %s", agent.Role)
	}
	if agent.Description != "new" {
		t.Fatalf("description must update, got %s", agent.Description)
	}
}

func TestHeavyLoad(t *testing.T) {
	d := testDB(t)

	_, _, _ = d.RegisterAgent("default", "bot-a", "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})

	var wg sync.WaitGroup
	var writeErrors, readErrors atomic.Int32
	writes := 50
	reads := 50

	// Concurrent writes
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := d.InsertMessage("default", fmt.Sprintf("agent-%d", idx), "target", "notification", "test", "heavy load", "{}", "P2", 3600, nil, nil)
			if err != nil {
				writeErrors.Add(1)
			}
		}(i)
	}

	// Concurrent reads
	for i := 0; i < reads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.ListAgents("default")
			if err != nil {
				readErrors.Add(1)
			}
		}()
	}

	wg.Wait()

	if writeErrors.Load() > 0 {
		t.Errorf("got %d write errors during heavy load", writeErrors.Load())
	}
	if readErrors.Load() > 0 {
		t.Errorf("got %d read errors during heavy load", readErrors.Load())
	}

	msgs, _ := d.GetAllRecentMessages("default", 200)
	if len(msgs) != writes {
		t.Errorf("expected %d messages, got %d", writes, len(msgs))
	}
}
