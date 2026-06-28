package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	conn   *sql.DB // writer: single connection, serializes all writes
	reader *sql.DB // reader: multiple connections for concurrent reads
	path   string
}

// resolveDBPath returns the database file path. RELAY_DB overrides the default
// (~/.agent-relay/relay.db) — set it in dev / CI / tests so a local `agent-relay`
// run never opens (and migrates) the production database. This is the guard
// against the footgun where a dev `agent-relay send` mutates prod.
func resolveDBPath() (string, error) {
	if p := os.Getenv("RELAY_DB"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".agent-relay", "relay.db"), nil
}

func New() (*DB, error) {
	dbPath, err := resolveDBPath()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	// Writer pool: single connection serializes writes at Go level (SQLite only allows 1 writer).
	writer, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open writer db: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	_, _ = writer.Exec("PRAGMA temp_store = MEMORY")

	// Reader pool: multiple connections for concurrent reads via WAL.
	reader, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=10000&_foreign_keys=ON")
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open reader db: %w", err)
	}
	reader.SetMaxOpenConns(10)
	reader.SetMaxIdleConns(5)
	_, _ = reader.Exec("PRAGMA mmap_size = 268435456") // 256MB
	_, _ = reader.Exec("PRAGMA temp_store = MEMORY")
	_, _ = reader.Exec("PRAGMA cache_size = -20000") // 20MB

	if err := migrate(writer); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &DB{conn: writer, reader: reader, path: dbPath}, nil
}

func (d *DB) Close() error {
	_, _ = d.conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	if d.reader != nil && d.reader != d.conn {
		_ = d.reader.Close()
	}
	return d.conn.Close()
}

// ro returns the read-only connection pool.
func (d *DB) ro() *sql.DB {
	return d.reader
}

// Path returns the database file path.
func (d *DB) Path() string {
	return d.path
}

// Optimize runs PRAGMA optimize and a passive WAL checkpoint on the writer.
// Safe to call periodically (e.g. every 5 minutes).
func (d *DB) Optimize() {
	_, _ = d.conn.Exec("PRAGMA optimize")
	_, _ = d.conn.Exec("PRAGMA wal_checkpoint(PASSIVE)")
}

// Backup writes a consistent snapshot of the database next to it via SQLite's
// VACUUM INTO (safe on a live WAL database), rotating through `keep` numbered
// files: relay.db.bak.0 (newest) .. relay.db.bak.(keep-1). Returns the path of
// the new snapshot. Gives a recovery point against corruption or an accidental
// destructive operation — the single DB file was previously the only copy.
func (d *DB) Backup(keep int) (string, error) {
	if keep < 1 {
		keep = 1
	}
	// Rotate older snapshots up by one slot; the oldest drops off.
	for i := keep - 1; i > 0; i-- {
		older := fmt.Sprintf("%s.bak.%d", d.path, i)
		newer := fmt.Sprintf("%s.bak.%d", d.path, i-1)
		_ = os.Remove(older)
		if _, err := os.Stat(newer); err == nil {
			_ = os.Rename(newer, older)
		}
	}
	dst := fmt.Sprintf("%s.bak.0", d.path)
	_ = os.Remove(dst) // VACUUM INTO fails if the target already exists
	if _, err := d.conn.Exec("VACUUM INTO ?", dst); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", dst, err)
	}
	return dst, nil
}

// GetHealthStats returns aggregate database statistics for the /health endpoint.
func (d *DB) GetHealthStats() map[string]int64 {
	stats := map[string]int64{}
	tables := []string{"agents", "messages", "tasks", "projects", "memories", "boards", "conversations"}
	for _, t := range tables {
		var c int64
		_ = d.ro().QueryRow("SELECT COUNT(*) FROM " + t).Scan(&c)
		stats[t] = c
	}
	return stats
}

// DBPath returns the database path (RELAY_DB override or default) without
// opening it.
func DBPath() (string, error) {
	return resolveDBPath()
}

// NewReadOnly opens the database in read-only mode for CLI queries.
// Does not run migrations or create the directory.
func NewReadOnly() (*DB, error) {
	dbPath, err := DBPath()
	if err != nil {
		return nil, fmt.Errorf("get db path: %w", err)
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("database not found at %s (relay never started?)", dbPath)
	}

	conn, err := sql.Open("sqlite3", dbPath+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db readonly: %w", err)
	}

	conn.SetMaxOpenConns(1)

	return &DB{conn: conn, reader: conn, path: dbPath}, nil
}

// ensureColumns checks a table for missing columns and adds them via ALTER TABLE.
func ensureColumns(conn *sql.DB, table string, columns map[string]string) {
	rows, err := conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	existing := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		_ = rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
		existing[name] = true
	}

	for col, def := range columns {
		if !existing[col] {
			_, _ = conn.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, def))
		}
	}
}

func migrate(conn *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS agents (
		id              TEXT PRIMARY KEY,
		name            TEXT NOT NULL,
		role            TEXT NOT NULL DEFAULT '',
		description     TEXT NOT NULL DEFAULT '',
		registered_at   TEXT NOT NULL,
		last_seen       TEXT NOT NULL,
		project         TEXT NOT NULL DEFAULT 'default',
		reports_to      TEXT,
		profile_slug    TEXT,
		status          TEXT NOT NULL DEFAULT 'active',
		deactivated_at  TEXT,
		is_executive    INTEGER NOT NULL DEFAULT 0,
		session_id      TEXT
	);

	CREATE TABLE IF NOT EXISTS messages (
		id         TEXT PRIMARY KEY,
		from_agent TEXT NOT NULL,
		to_agent   TEXT NOT NULL,
		reply_to   TEXT,
		type       TEXT NOT NULL DEFAULT 'notification',
		subject    TEXT NOT NULL DEFAULT '',
		content    TEXT NOT NULL,
		metadata   TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		read_at    TEXT,
		project    TEXT NOT NULL DEFAULT 'default'
	);

	CREATE INDEX IF NOT EXISTS idx_messages_to ON messages(to_agent);
	CREATE INDEX IF NOT EXISTS idx_messages_from ON messages(from_agent);
	CREATE INDEX IF NOT EXISTS idx_messages_unread ON messages(to_agent, read_at) WHERE read_at IS NULL;
	CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(reply_to);

	-- Conversations
	CREATE TABLE IF NOT EXISTS conversations (
		id          TEXT PRIMARY KEY,
		title       TEXT NOT NULL,
		created_by  TEXT NOT NULL,
		created_at  TEXT NOT NULL,
		archived_at TEXT,
		project     TEXT NOT NULL DEFAULT 'default'
	);

	CREATE TABLE IF NOT EXISTS conversation_members (
		conversation_id TEXT NOT NULL,
		agent_name      TEXT NOT NULL,
		joined_at       TEXT NOT NULL,
		left_at         TEXT,
		PRIMARY KEY (conversation_id, agent_name)
	);
	CREATE INDEX IF NOT EXISTS idx_conv_members_agent ON conversation_members(agent_name);

	CREATE TABLE IF NOT EXISTS conversation_reads (
		conversation_id TEXT NOT NULL,
		agent_name      TEXT NOT NULL,
		last_read_at    TEXT NOT NULL,
		PRIMARY KEY (conversation_id, agent_name)
	);
	`

	if _, err := conn.Exec(schema); err != nil {
		return err
	}

	// --- Ensure all columns exist on every table (safe for old and new DBs) ---

	ensureColumns(conn, "agents", map[string]string{
		"project":           "TEXT NOT NULL DEFAULT 'default'",
		"reports_to":        "TEXT",
		"profile_slug":      "TEXT",
		"status":            "TEXT NOT NULL DEFAULT 'active'",
		"deactivated_at":    "TEXT",
		"is_executive":      "INTEGER NOT NULL DEFAULT 0",
		"session_id":        "TEXT",
		"org_id":            "TEXT",
		"interest_tags":     "TEXT NOT NULL DEFAULT '[]'",
		"max_context_bytes": "INTEGER NOT NULL DEFAULT 16384",
		"avatar_url":        "TEXT",
		// cwd is the stable identity key for hook-based session binding: a Claude
		// Code session_id rotates on /clear, but the worktree cwd does not. Kept
		// out of agentColumns/scanAgent on purpose (dedicated queries only).
		"cwd": "TEXT NOT NULL DEFAULT ''",
	})

	// Projects table (planet_type assigned per project)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS projects (
		name        TEXT PRIMARY KEY,
		planet_type TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL DEFAULT ''
	)`)

	// Settings table (key-value, e.g. sun_type)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`)

	// Backfill projects from existing agents
	backfillProjects(conn)

	// Ensure the 'default' project row always exists — CLI and MCP tools accept
	// project="default" implicitly but never created the row, so the UI hid
	// activity that happened in the implicit default scope.
	nowStr := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, _ = conn.Exec("INSERT OR IGNORE INTO projects (name, planet_type, created_at) VALUES ('default', 'forest/1', ?)", nowStr)

	ensureColumns(conn, "messages", map[string]string{
		"conversation_id": "TEXT",
		"project":         "TEXT NOT NULL DEFAULT 'default'",
		"task_id":         "TEXT",
		"priority":        "TEXT NOT NULL DEFAULT 'P2'",
		"ttl_seconds":     "INTEGER NOT NULL DEFAULT 14400",
		"expired_at":      "TEXT",
	})

	ensureColumns(conn, "conversations", map[string]string{
		"project": "TEXT NOT NULL DEFAULT 'default'",
	})

	// Indexes (all idempotent)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_agents_project ON agents(project)`)
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_project_name ON agents(project, name)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_agents_cwd ON agents(cwd)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_task ON messages(task_id)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_priority ON messages(priority)`)
	// Drives the retention GC purge of soft-expired messages (PurgeExpiredMessages).
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_expired ON messages(expired_at) WHERE expired_at IS NOT NULL`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_conversations_project ON conversations(project)`)

	// Remove old global UNIQUE constraint on agents.name (existing DBs only).
	migrateDropGlobalUnique(conn)

	// Memory system
	if err := migrateMemories(conn); err != nil {
		return fmt.Errorf("migrate memories: %w", err)
	}

	// Per-agent read receipts
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS message_reads (
		message_id TEXT NOT NULL,
		agent_name TEXT NOT NULL,
		project    TEXT NOT NULL DEFAULT 'default',
		read_at    TEXT NOT NULL,
		UNIQUE(message_id, agent_name)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_message_reads_agent ON message_reads(agent_name, project)`)

	// Deliveries (per-recipient message tracking)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS deliveries (
		id              TEXT PRIMARY KEY,
		message_id      TEXT NOT NULL,
		to_agent        TEXT NOT NULL,
		state           TEXT NOT NULL DEFAULT 'queued',
		sequence_number INTEGER NOT NULL DEFAULT 0,
		created_at      TEXT NOT NULL,
		surfaced_at     TEXT,
		acknowledged_at TEXT,
		expired_at      TEXT,
		project         TEXT NOT NULL DEFAULT 'default',
		FOREIGN KEY (message_id) REFERENCES messages(id)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_deliveries_message ON deliveries(message_id)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_deliveries_agent_state ON deliveries(to_agent, project, state)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_deliveries_state ON deliveries(state)`)

	// Backfill deliveries for existing messages
	migrateDeliveries(conn)

	// File locks
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS file_locks (
		id          TEXT PRIMARY KEY,
		agent_name  TEXT NOT NULL,
		project     TEXT NOT NULL,
		file_paths  TEXT NOT NULL,
		claimed_at  TEXT NOT NULL,
		released_at TEXT,
		ttl_seconds INTEGER NOT NULL DEFAULT 1800
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_file_locks_project ON file_locks(project)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_file_locks_agent ON file_locks(agent_name, project)`)

	// Profiles
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS profiles (
		id           TEXT PRIMARY KEY,
		slug         TEXT NOT NULL,
		name         TEXT NOT NULL,
		role         TEXT NOT NULL DEFAULT '',
		context_pack TEXT NOT NULL DEFAULT '',
		soul_keys    TEXT NOT NULL DEFAULT '[]',
		project      TEXT NOT NULL DEFAULT 'default',
		org_id       TEXT,
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_profiles_project_slug ON profiles(project, slug)`)
	ensureColumns(conn, "profiles", map[string]string{
		"skills":        "TEXT NOT NULL DEFAULT '[]'",
		"vault_paths":   "TEXT NOT NULL DEFAULT '[]'",
		"allowed_tools": "TEXT NOT NULL DEFAULT '[]'",
		"pool_size":     "INTEGER NOT NULL DEFAULT 3",
		"exit_prompt":   "TEXT NOT NULL DEFAULT ''",
	})

	// Tasks
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id              TEXT PRIMARY KEY,
		profile_slug    TEXT NOT NULL,
		assigned_to     TEXT,
		dispatched_by   TEXT NOT NULL,
		title           TEXT NOT NULL,
		description     TEXT NOT NULL DEFAULT '',
		priority        TEXT NOT NULL DEFAULT 'P2',
		status          TEXT NOT NULL DEFAULT 'pending',
		result          TEXT,
		blocked_reason  TEXT,
		project         TEXT NOT NULL DEFAULT 'default',
		dispatched_at   TEXT NOT NULL,
		accepted_at     TEXT,
		started_at      TEXT,
		completed_at    TEXT,
		reply_to_task   TEXT
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_project_status ON tasks(project, status)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_profile ON tasks(project, profile_slug)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_priority ON tasks(project, priority, status)`)
	ensureColumns(conn, "tasks", map[string]string{
		"ack_notified_at":  "TEXT",
		"ack_escalated_at": "TEXT",
		"parent_task_id":   "TEXT",
		"board_id":         "TEXT",
		"archived_at":      "TEXT",

		// --- Linear zone (read-only, replicated from Linear SSOT) ---
		"source":            "TEXT NOT NULL DEFAULT 'native'", // 'native' | 'linear'
		"linear_issue_id":   "TEXT",
		"linear_key":        "TEXT", // e.g. SYN-123
		"external_url":      "TEXT",
		"points":            "INTEGER",
		"labels":            "TEXT NOT NULL DEFAULT '[]'", // json array
		"linear_state":      "TEXT",
		"assignee":          "TEXT",
		"cycle_id":          "TEXT",
		"cycle_name":        "TEXT",
		"cycle_start":       "TEXT",
		"cycle_end":         "TEXT",
		"linear_project_id": "TEXT", // Linear project UUID — drives project→agent routing
		// priority already exists on the base table.

		// --- Execution overlay (relay-owned, auto-stamped temporal trail) ---
		// dispatched_at / started_at / completed_at already exist — reused, not duplicated.
		// done_at is an explicit alias of completed_at (stamped together on → done).
		"claimed_by":      "TEXT",
		"claimed_at":      "TEXT",
		"blocked_periods": "TEXT NOT NULL DEFAULT '[]'", // json array of {start,end} (blocked_at[])
		"in_review_at":    "TEXT",
		"done_at":         "TEXT",
		// last_activity_at bumps on any agent activity (transition / comment /
		// progress note) so the stale-scanner measures idle-since-activity, not
		// idle-since-dispatch — a heads-down lead on a multi-hour build isn't stale.
		"last_activity_at": "TEXT",
	})
	// Migrate legacy reply_to_task -> parent_task_id
	_, _ = conn.Exec(`UPDATE tasks SET parent_task_id = reply_to_task WHERE reply_to_task IS NOT NULL AND parent_task_id IS NULL`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_board ON tasks(board_id)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_linear_issue ON tasks(linear_issue_id)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_cycle ON tasks(cycle_id)`)
	// Goals subsystem removed — drop the table and its stale index for existing DBs.
	_, _ = conn.Exec(`DROP INDEX IF EXISTS idx_tasks_goal`)
	_, _ = conn.Exec(`DROP TABLE IF EXISTS goals`)

	// Task progress notes (surfaced in web UI between claim and complete)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS task_progress_notes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id    TEXT NOT NULL,
		project    TEXT NOT NULL DEFAULT 'default',
		agent      TEXT NOT NULL,
		note       TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_task_progress_notes_task ON task_progress_notes(task_id, created_at)`)

	// Audit log — the "why" trail behind orchestrator/agent actions on the board.
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS audit_log (
		id            TEXT PRIMARY KEY,
		project       TEXT NOT NULL DEFAULT 'default',
		actor         TEXT NOT NULL,
		action        TEXT NOT NULL,
		resource_type TEXT NOT NULL DEFAULT 'task',
		resource_id   TEXT NOT NULL,
		summary       TEXT NOT NULL DEFAULT '',
		details       TEXT NOT NULL DEFAULT '',
		reason        TEXT NOT NULL DEFAULT '',
		created_at    TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_resource ON audit_log(project, resource_id, created_at)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_project ON audit_log(project, created_at)`)

	// Linear connector sync log (capped audit trail of write-back outcomes).
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS linear_sync_log (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		ts        TEXT NOT NULL,
		issue_id  TEXT NOT NULL DEFAULT '',
		action    TEXT NOT NULL DEFAULT '',
		outcome   TEXT NOT NULL DEFAULT '',
		detail    TEXT NOT NULL DEFAULT ''
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_linear_sync_log_ts ON linear_sync_log(id DESC)`)

	// Teams + Orgs
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS orgs (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL,
		slug        TEXT UNIQUE NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL
	)`)

	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS teams (
		id             TEXT PRIMARY KEY,
		name           TEXT NOT NULL,
		slug           TEXT NOT NULL,
		org_id         TEXT,
		project        TEXT NOT NULL DEFAULT 'default',
		description    TEXT NOT NULL DEFAULT '',
		type           TEXT NOT NULL DEFAULT 'regular',
		parent_team_id TEXT,
		created_at     TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_teams_project_slug ON teams(project, slug)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_teams_org ON teams(org_id)`)

	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS team_members (
		team_id    TEXT NOT NULL,
		agent_name TEXT NOT NULL,
		project    TEXT NOT NULL DEFAULT 'default',
		role       TEXT NOT NULL DEFAULT 'member',
		joined_at  TEXT NOT NULL,
		left_at    TEXT,
		PRIMARY KEY (team_id, agent_name)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_team_members_agent ON team_members(agent_name, project)`)

	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS team_inbox (
		team_id    TEXT NOT NULL,
		message_id TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (team_id, message_id)
	)`)

	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS agent_notify_channels (
		agent_name TEXT NOT NULL,
		project    TEXT NOT NULL DEFAULT 'default',
		target     TEXT NOT NULL,
		PRIMARY KEY (agent_name, project, target)
	)`)

	// Boards
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS boards (
		id          TEXT PRIMARY KEY,
		project     TEXT NOT NULL DEFAULT 'default',
		name        TEXT NOT NULL,
		slug        TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		created_by  TEXT NOT NULL DEFAULT 'user',
		created_at  TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_boards_project_slug ON boards(project, slug)`)
	ensureColumns(conn, "boards", map[string]string{
		"archived_at": "TEXT",
	})

	// Vaults (per-project config)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS vaults (
		project     TEXT PRIMARY KEY,
		path        TEXT NOT NULL,
		created_at  TEXT NOT NULL
	)`)

	// Vault docs
	if err := migrateVault(conn); err != nil {
		return fmt.Errorf("migrate vault: %w", err)
	}

	// Token usage tracking
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS token_usage (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		project    TEXT NOT NULL DEFAULT 'default',
		agent      TEXT NOT NULL DEFAULT '',
		tool       TEXT NOT NULL DEFAULT '',
		bytes      INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	)`)
	// Real token counts from the Claude Code transcript (hook-POSTed), vs the
	// legacy bytes/4 estimate. Rows from the old relay-payload path leave these 0;
	// reporting prefers real counts per-row and falls back to bytes/4.
	ensureColumns(conn, "token_usage", map[string]string{
		"input_tokens":          "INTEGER NOT NULL DEFAULT 0",
		"output_tokens":         "INTEGER NOT NULL DEFAULT 0",
		"cache_read_tokens":     "INTEGER NOT NULL DEFAULT 0",
		"cache_creation_tokens": "INTEGER NOT NULL DEFAULT 0",
		// model tier the turn ran on (e.g. "claude-opus-4-8") — drives per-tier $
		// cost. Empty when the hook didn't report it → costed at the default tier.
		"model": "TEXT NOT NULL DEFAULT ''",
	})
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_token_usage_project_time ON token_usage(project, created_at)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_token_usage_agent_time ON token_usage(project, agent, created_at)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_token_usage_created ON token_usage(created_at)`)

	// Spawn children tracking
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS spawn_children (
		id          TEXT PRIMARY KEY,
		parent_agent TEXT NOT NULL,
		project     TEXT NOT NULL,
		profile     TEXT NOT NULL,
		pid         INTEGER,
		status      TEXT NOT NULL DEFAULT 'running',
		prompt      TEXT NOT NULL DEFAULT '',
		started_at  TEXT NOT NULL,
		finished_at TEXT,
		exit_code   INTEGER,
		error       TEXT DEFAULT ''
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_spawn_children_parent ON spawn_children(parent_agent, project)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_spawn_children_status ON spawn_children(status)`)
	ensureColumns(conn, "spawn_children", map[string]string{
		"stdout_tail": "TEXT NOT NULL DEFAULT ''",
		"stderr_tail": "TEXT NOT NULL DEFAULT ''",
	})

	// Schedules (cron jobs stored in DB)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS schedules (
		id          TEXT PRIMARY KEY,
		agent_name  TEXT NOT NULL,
		project     TEXT NOT NULL,
		name        TEXT NOT NULL,
		cron_expr   TEXT NOT NULL,
		prompt      TEXT NOT NULL DEFAULT '',
		ttl         TEXT NOT NULL DEFAULT '10m',
		enabled     INTEGER NOT NULL DEFAULT 1,
		created_at  TEXT NOT NULL,
		updated_at  TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_agent_name ON schedules(project, agent_name, name)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_schedules_project ON schedules(project)`)
	ensureColumns(conn, "schedules", map[string]string{
		"allowed_tools": "TEXT NOT NULL DEFAULT ''",
		"cycle":         "TEXT NOT NULL DEFAULT ''",
	})

	// Cycle history (execution metrics)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS cycle_history (
		id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		agent_name            TEXT NOT NULL,
		project               TEXT NOT NULL,
		cycle_name            TEXT NOT NULL,
		duration_ms           INTEGER NOT NULL,
		success               INTEGER NOT NULL,
		exit_code             INTEGER NOT NULL DEFAULT 0,
		error                 TEXT DEFAULT '',
		input_tokens          INTEGER NOT NULL DEFAULT 0,
		output_tokens         INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
		cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
		created_at            TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_cycle_history_agent ON cycle_history(agent_name, project)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_cycle_history_time ON cycle_history(created_at)`)

	// Triggers (event-driven spawn rules)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS triggers (
		id           TEXT PRIMARY KEY,
		project      TEXT NOT NULL,
		event        TEXT NOT NULL,
		match_rules  TEXT NOT NULL DEFAULT '{}',
		profile_slug TEXT NOT NULL,
		cycle        TEXT NOT NULL,
		max_duration TEXT NOT NULL DEFAULT '10m',
		enabled      INTEGER NOT NULL DEFAULT 1,
		created_at   TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_triggers_project_event ON triggers(project, event)`)
	ensureColumns(conn, "triggers", map[string]string{
		"cooldown_seconds": "INTEGER DEFAULT 60",
		"last_fired_at":    "TEXT",
	})

	// Trigger history (event-driven spawn audit log)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS trigger_history (
		id         TEXT PRIMARY KEY,
		trigger_id TEXT NOT NULL,
		project    TEXT NOT NULL,
		event      TEXT NOT NULL,
		child_id   TEXT,
		error      TEXT,
		fired_at   TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_trigger_history_project ON trigger_history(project, fired_at)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_trigger_history_trigger ON trigger_history(trigger_id)`)

	// Poll triggers (external URL monitoring)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS poll_triggers (
		id               TEXT PRIMARY KEY,
		project          TEXT NOT NULL,
		name             TEXT NOT NULL,
		url              TEXT NOT NULL,
		headers          TEXT DEFAULT '{}',
		condition_path   TEXT NOT NULL,
		condition_op     TEXT NOT NULL,
		condition_value  TEXT NOT NULL,
		poll_interval    TEXT NOT NULL,
		fire_event       TEXT NOT NULL,
		fire_meta        TEXT DEFAULT '{}',
		enabled          INTEGER DEFAULT 1,
		last_polled_at   TEXT,
		last_result      TEXT,
		last_matched     INTEGER DEFAULT 0,
		cooldown_seconds INTEGER DEFAULT 300,
		created_at       TEXT NOT NULL,
		UNIQUE(project, name)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_poll_triggers_project ON poll_triggers(project)`)

	// Skill registry
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS skills (
		id          TEXT PRIMARY KEY,
		project     TEXT NOT NULL,
		name        TEXT NOT NULL,
		description TEXT DEFAULT '',
		tags        TEXT DEFAULT '[]',
		created_at  TEXT NOT NULL,
		UNIQUE(project, name)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_skills_project ON skills(project)`)

	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS profile_skills (
		profile_id  TEXT NOT NULL,
		skill_id    TEXT NOT NULL,
		proficiency TEXT DEFAULT 'capable',
		PRIMARY KEY (profile_id, skill_id),
		FOREIGN KEY (profile_id) REFERENCES profiles(id) ON DELETE CASCADE,
		FOREIGN KEY (skill_id) REFERENCES skills(id) ON DELETE CASCADE
	)`)

	// Elevated privileges (temporary privilege escalation)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS elevated_privileges (
		id            TEXT PRIMARY KEY,
		project       TEXT NOT NULL,
		agent_name    TEXT NOT NULL,
		elevated_role TEXT NOT NULL,
		granted_by    TEXT NOT NULL,
		reason        TEXT DEFAULT '',
		expires_at    TEXT NOT NULL,
		revoked_at    TEXT,
		created_at    TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_elevated_project_agent ON elevated_privileges(project, agent_name)`)

	// Agent quotas (per-agent rate limits)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS agent_quotas (
		project              TEXT NOT NULL,
		agent_name           TEXT NOT NULL,
		max_tokens_per_day   INTEGER DEFAULT 0,
		max_messages_per_hour INTEGER DEFAULT 0,
		max_tasks_per_hour   INTEGER DEFAULT 0,
		max_spawns_per_hour  INTEGER DEFAULT 0,
		created_at           TEXT NOT NULL,
		updated_at           TEXT NOT NULL,
		PRIMARY KEY (project, agent_name)
	)`)

	// Workflows (visual DAG definitions)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS workflows (
		id          TEXT PRIMARY KEY,
		project     TEXT NOT NULL,
		name        TEXT NOT NULL,
		description TEXT DEFAULT '',
		nodes       TEXT NOT NULL DEFAULT '[]',
		edges       TEXT NOT NULL DEFAULT '[]',
		enabled     INTEGER DEFAULT 1,
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_workflows_project ON workflows(project)`)

	// Workflow runs (execution log)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS workflow_runs (
		id            TEXT PRIMARY KEY,
		workflow_id   TEXT NOT NULL,
		project       TEXT NOT NULL,
		trigger_event TEXT,
		trigger_meta  TEXT DEFAULT '{}',
		status        TEXT DEFAULT 'running',
		started_at    TEXT NOT NULL DEFAULT (datetime('now')),
		finished_at   TEXT,
		error         TEXT
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_workflow_runs_workflow ON workflow_runs(workflow_id)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_workflow_runs_project ON workflow_runs(project, started_at)`)

	// Workflow node runs (per-node execution within a run)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS workflow_node_runs (
		id          TEXT PRIMARY KEY,
		run_id      TEXT NOT NULL,
		node_id     TEXT NOT NULL,
		node_type   TEXT NOT NULL,
		status      TEXT DEFAULT 'pending',
		input       TEXT DEFAULT '{}',
		output      TEXT DEFAULT '{}',
		started_at  TEXT,
		finished_at TEXT,
		error       TEXT
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_workflow_node_runs_run ON workflow_node_runs(run_id)`)

	// Custom events (user-defined event types with meta field schemas)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS custom_events (
		id          TEXT PRIMARY KEY,
		project     TEXT NOT NULL,
		name        TEXT NOT NULL,
		description TEXT DEFAULT '',
		meta_fields TEXT DEFAULT '[]',
		icon        TEXT DEFAULT '',
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(project, name)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_custom_events_project ON custom_events(project)`)

	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS cycles (
		id            TEXT PRIMARY KEY,
		project       TEXT NOT NULL,
		name          TEXT NOT NULL,
		prompt        TEXT NOT NULL DEFAULT '',
		ttl           INTEGER NOT NULL DEFAULT 10,
		created_at    TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(project, name)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_cycles_project ON cycles(project)`)

	// Notification rules: configurable event→action→target rules engine.
	// match/opts are JSON blobs. Human-authored via the web UI (no MCP tools).
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS notification_rules (
		id          TEXT PRIMARY KEY,
		project     TEXT NOT NULL DEFAULT 'default',
		name        TEXT NOT NULL,
		enabled     INTEGER NOT NULL DEFAULT 1,
		event       TEXT NOT NULL,
		match_json  TEXT NOT NULL DEFAULT '{}',
		action      TEXT NOT NULL,
		target      TEXT NOT NULL DEFAULT '',
		opts_json   TEXT NOT NULL DEFAULT '{}',
		created_at  TEXT NOT NULL,
		updated_at  TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_notification_rules_event ON notification_rules(project, event, enabled)`)

	// Notification deliveries: capped delivery log for debugging launcher wiring.
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS notification_deliveries (
		id          TEXT PRIMARY KEY,
		project     TEXT NOT NULL DEFAULT 'default',
		rule_id     TEXT NOT NULL,
		rule_name   TEXT NOT NULL DEFAULT '',
		event       TEXT NOT NULL,
		action      TEXT NOT NULL DEFAULT '',
		target      TEXT NOT NULL DEFAULT '',
		outcome     TEXT NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		error       TEXT NOT NULL DEFAULT '',
		payload     TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_notification_deliveries_created ON notification_deliveries(created_at)`)

	// events — the durable outbox + replay log for the event bus (TSU-52). Every
	// semantic event is persisted here; delivery_id is the idempotency key
	// (UNIQUE) so an at-least-once source — e.g. a retried GitHub webhook — is
	// deduped via INSERT OR IGNORE. delivered_at/attempts/last_error are owned by
	// the sweeper (slice-B): NULL delivered_at = not yet processed.
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS events (
		id           TEXT PRIMARY KEY,
		delivery_id  TEXT NOT NULL UNIQUE,
		project      TEXT NOT NULL DEFAULT 'default',
		event_type   TEXT NOT NULL,
		agent        TEXT NOT NULL DEFAULT '',
		payload      TEXT NOT NULL DEFAULT '{}',
		created_at   TEXT NOT NULL,
		delivered_at TEXT,
		attempts     INTEGER NOT NULL DEFAULT 0,
		last_error   TEXT NOT NULL DEFAULT ''
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_events_undelivered ON events(delivered_at) WHERE delivered_at IS NULL`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_events_created ON events(id DESC)`)

	// Backfill last_activity_at for rows that predate the column: seed it from the
	// best existing timestamp so the stale-scanner has a baseline (idempotent —
	// only fills NULLs).
	_, _ = conn.Exec(`UPDATE tasks SET last_activity_at = COALESCE(started_at, accepted_at, dispatched_at)
	                  WHERE last_activity_at IS NULL`)

	// Lowercase all agent names for case-insensitive matching
	migrateLowercaseAgentNames(conn)

	return nil
}

// backfillProjects creates project entries for any existing agents that don't have a project row yet.
func backfillProjects(conn *sql.DB) {
	planetPool := []string{
		"barren/1", "barren/2", "barren/3", "barren/4",
		"desert/1", "desert/2",
		"forest/1", "forest/2",
		"gas_giant/1", "gas_giant/2", "gas_giant/3", "gas_giant/4",
		"ice/1",
		"lava/1", "lava/2", "lava/3",
		"ocean/1",
		"terran/1", "terran/2",
		"tundra/1", "tundra/2",
	}

	rows, err := conn.Query("SELECT DISTINCT project FROM agents WHERE project NOT IN (SELECT name FROM projects)")
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	var projects []string
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		projects = append(projects, p)
	}

	for _, p := range projects {
		h := 0
		for _, c := range p {
			h = ((h << 5) - h + int(c))
		}
		if h < 0 {
			h = -h
		}
		planet := planetPool[h%len(planetPool)]
		_, _ = conn.Exec("INSERT OR IGNORE INTO projects (name, planet_type, created_at) VALUES (?, ?, datetime('now'))", p, planet)
	}
}

// migrateVault creates the vault_docs table, FTS5 virtual table, and sync triggers.
func migrateVault(conn *sql.DB) error {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS vault_docs (
		path       TEXT NOT NULL,
		project    TEXT NOT NULL,
		title      TEXT NOT NULL DEFAULT '',
		owner      TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL DEFAULT '',
		tags       TEXT NOT NULL DEFAULT '[]',
		content    TEXT NOT NULL DEFAULT '',
		size_bytes INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		indexed_at TEXT NOT NULL,
		PRIMARY KEY (path, project)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_vault_docs_project ON vault_docs(project)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_vault_docs_tags ON vault_docs(project, tags)`)

	if _, err := conn.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS vault_docs_fts USING fts5(
		path, title, tags, content,
		content=vault_docs,
		content_rowid=rowid
	)`); err != nil {
		fmt.Fprintf(os.Stderr, "warning: vault FTS5 not available: %v\n", err)
		return nil
	}

	_, _ = conn.Exec(`CREATE TRIGGER IF NOT EXISTS vault_docs_ai AFTER INSERT ON vault_docs BEGIN
		INSERT INTO vault_docs_fts(rowid, path, title, tags, content)
		VALUES (new.rowid, new.path, new.title, new.tags, new.content);
	END`)

	_, _ = conn.Exec(`CREATE TRIGGER IF NOT EXISTS vault_docs_ad AFTER DELETE ON vault_docs BEGIN
		INSERT INTO vault_docs_fts(vault_docs_fts, rowid, path, title, tags, content)
		VALUES ('delete', old.rowid, old.path, old.title, old.tags, old.content);
	END`)

	_, _ = conn.Exec(`CREATE TRIGGER IF NOT EXISTS vault_docs_au AFTER UPDATE ON vault_docs BEGIN
		INSERT INTO vault_docs_fts(vault_docs_fts, rowid, path, title, tags, content)
		VALUES ('delete', old.rowid, old.path, old.title, old.tags, old.content);
		INSERT INTO vault_docs_fts(rowid, path, title, tags, content)
		VALUES (new.rowid, new.path, new.title, new.tags, new.content);
	END`)

	return nil
}

// migrateDropGlobalUnique removes the old UNIQUE constraint on agents.name
// that was created in early versions. Only runs if the constraint still exists.
func migrateDropGlobalUnique(conn *sql.DB) {
	// Check if the old UNIQUE(name) autoindex exists (sqlite_autoindex_agents_2).
	// Note: sqlite_autoindex_agents_1 is the PRIMARY KEY, not the UNIQUE(name).
	var count int
	err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='sqlite_autoindex_agents_2'`).Scan(&count)
	if err != nil || count == 0 {
		return // no old constraint, nothing to do
	}

	// Rebuild the table without the inline UNIQUE on name.
	tx, err := conn.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()

	stmts := []string{
		`CREATE TABLE agents_new (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			role            TEXT NOT NULL DEFAULT '',
			description     TEXT NOT NULL DEFAULT '',
			registered_at   TEXT NOT NULL,
			last_seen       TEXT NOT NULL,
			project         TEXT NOT NULL DEFAULT 'default',
			reports_to      TEXT,
			profile_slug    TEXT,
			status          TEXT NOT NULL DEFAULT 'active',
			deactivated_at  TEXT,
			is_executive    INTEGER NOT NULL DEFAULT 0,
			session_id      TEXT
		)`,
		`INSERT INTO agents_new SELECT id, name, role, description, registered_at, last_seen, project, reports_to, NULL, 'active', NULL, 0, NULL FROM agents`,
		`DROP TABLE agents`,
		`ALTER TABLE agents_new RENAME TO agents`,
		`CREATE INDEX IF NOT EXISTS idx_agents_project ON agents(project)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_project_name ON agents(project, name)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return
		}
	}

	_ = tx.Commit()
}

// migrateMemories creates the memories table, FTS5 virtual table, and sync triggers.
func migrateMemories(conn *sql.DB) error {
	// Main table
	if _, err := conn.Exec(`
	CREATE TABLE IF NOT EXISTS memories (
		id            TEXT PRIMARY KEY,
		key           TEXT NOT NULL,
		value         TEXT NOT NULL,
		tags          TEXT NOT NULL DEFAULT '[]',
		scope         TEXT NOT NULL,
		project       TEXT NOT NULL DEFAULT 'default',
		agent_name    TEXT NOT NULL,
		confidence    TEXT NOT NULL DEFAULT 'stated',
		version       INTEGER NOT NULL DEFAULT 1,
		supersedes    TEXT,
		conflict_with TEXT,
		created_at    TEXT NOT NULL,
		updated_at    TEXT NOT NULL,
		archived_at   TEXT,
		archived_by   TEXT,
		layer         TEXT NOT NULL DEFAULT 'behavior'
	)`); err != nil {
		return fmt.Errorf("create memories table: %w", err)
	}

	// Layer column migration for existing DBs (idempotent).
	_, _ = conn.Exec(`ALTER TABLE memories ADD COLUMN layer TEXT NOT NULL DEFAULT 'behavior'`)

	// Indexes (all idempotent)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_key_scope ON memories(project, scope, key) WHERE archived_at IS NULL`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_agent ON memories(agent_name)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_tags ON memories(project, scope)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_updated ON memories(updated_at DESC)`)

	// FTS5 virtual table for full-text search.
	// Requires building with -tags "fts5" for github.com/mattn/go-sqlite3.
	if _, err := conn.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
		key, value, tags,
		content=memories,
		content_rowid=rowid
	)`); err != nil {
		fmt.Fprintf(os.Stderr, "warning: FTS5 not available (build with -tags \"fts5\"): %v\n", err)
		return nil // non-fatal: memory CRUD works, search degrades
	}

	// Triggers to keep FTS in sync
	_, _ = conn.Exec(`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
		INSERT INTO memories_fts(rowid, key, value, tags)
		VALUES (new.rowid, new.key, new.value, new.tags);
	END`)

	_, _ = conn.Exec(`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
		INSERT INTO memories_fts(memories_fts, rowid, key, value, tags)
		VALUES ('delete', old.rowid, old.key, old.value, old.tags);
	END`)

	_, _ = conn.Exec(`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
		INSERT INTO memories_fts(memories_fts, rowid, key, value, tags)
		VALUES ('delete', old.rowid, old.key, old.value, old.tags);
		INSERT INTO memories_fts(rowid, key, value, tags)
		VALUES (new.rowid, new.key, new.value, new.tags);
	END`)

	return nil
}

// migrateLowercaseAgentNames normalizes all agent name fields to lowercase
// for case-insensitive matching. Idempotent — skips rows already lowercase.
// If duplicate agent names would result (e.g. "Bot-A" and "bot-a" in same project),
// the migration is skipped entirely and a warning is logged.
func migrateLowercaseAgentNames(conn *sql.DB) {
	// Check for duplicates that would violate UNIQUE(project, name)
	var dupes int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM (
		SELECT project, LOWER(name) FROM agents GROUP BY project, LOWER(name) HAVING COUNT(*) > 1
	)`).Scan(&dupes)
	if dupes > 0 {
		log.Printf("warning: skipping agent name lowercase migration — %d duplicate name(s) found (differing only in case). Resolve manually.", dupes)
		return
	}

	tx, err := conn.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()

	stmts := []string{
		"UPDATE agents SET name = LOWER(name) WHERE name != LOWER(name)",
		"UPDATE agents SET reports_to = LOWER(reports_to) WHERE reports_to IS NOT NULL AND reports_to != LOWER(reports_to)",
		"UPDATE messages SET from_agent = LOWER(from_agent) WHERE from_agent != LOWER(from_agent)",
		"UPDATE messages SET to_agent = LOWER(to_agent) WHERE to_agent != LOWER(to_agent)",
		"UPDATE tasks SET assigned_to = LOWER(assigned_to) WHERE assigned_to IS NOT NULL AND assigned_to != LOWER(assigned_to)",
		"UPDATE tasks SET dispatched_by = LOWER(dispatched_by) WHERE dispatched_by != LOWER(dispatched_by)",
		"UPDATE conversations SET created_by = LOWER(created_by) WHERE created_by != LOWER(created_by)",
		"UPDATE conversation_members SET agent_name = LOWER(agent_name) WHERE agent_name != LOWER(agent_name)",
		"UPDATE memories SET agent_name = LOWER(agent_name) WHERE agent_name != LOWER(agent_name)",
		"UPDATE team_members SET agent_name = LOWER(agent_name) WHERE agent_name != LOWER(agent_name)",
		"UPDATE agent_notify_channels SET agent_name = LOWER(agent_name) WHERE agent_name != LOWER(agent_name)",
		"UPDATE message_reads SET agent_name = LOWER(agent_name) WHERE agent_name != LOWER(agent_name)",
		"UPDATE boards SET created_by = LOWER(created_by) WHERE created_by != LOWER(created_by)",
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return
		}
	}

	_ = tx.Commit()
}
