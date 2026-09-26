package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Consumption snapshots, slice T1 (design 54e529d8, ruling f97023b7). Records
// which memory row ids (= versions) each agent was SERVED, so the relay can
// answer "who holds version N of key K" without any write on a read path:
//   - boot writes at most one tx, and none when the served set is unchanged;
//   - sets are content-addressed and shared across agents; edges (version →
//     set) are written once per distinct set, never per boot;
//   - recalls are buffered in memory and flushed in one tx per tick.
// Liveness comes from agents.last_seen, so an unchanged head is never refreshed.

const (
	SnapshotBoot        = "boot"
	SnapshotBootMinimal = "boot_minimal"
	SnapshotRecall      = "recall"

	pruneChunk = 500
)

func migrateConsumption(conn *sql.DB) {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS context_sets (
		set_hash   TEXT PRIMARY KEY,
		n          INTEGER NOT NULL,
		created_at TEXT NOT NULL
	) WITHOUT ROWID`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS consumption_edges (
		memory_id TEXT NOT NULL,
		set_hash  TEXT NOT NULL,
		PRIMARY KEY (memory_id, set_hash)
	) WITHOUT ROWID`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_consumption_edges_set ON consumption_edges(set_hash)`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS context_snapshots (
		id            TEXT PRIMARY KEY,
		project       TEXT NOT NULL,
		agent_name    TEXT NOT NULL,
		session_id    TEXT,
		kind          TEXT NOT NULL,
		set_hash      TEXT NOT NULL,
		parent_id     TEXT,
		knowledge_rev INTEGER,
		created_at    TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_context_snapshots_agent ON context_snapshots(project, agent_name, created_at)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_context_snapshots_set ON context_snapshots(set_hash)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_context_snapshots_parent ON context_snapshots(parent_id) WHERE parent_id IS NOT NULL`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS context_heads (
		project     TEXT NOT NULL,
		agent_name  TEXT NOT NULL,
		snapshot_id TEXT NOT NULL,
		set_hash    TEXT NOT NULL,
		PRIMARY KEY (project, agent_name)
	) WITHOUT ROWID`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_context_heads_set ON context_heads(set_hash)`)
	// task_basis is written by T2 (claim/complete stamps); created here so the
	// prune below can already honour it.
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS task_basis (
		task_id        TEXT NOT NULL,
		event          TEXT NOT NULL,
		project        TEXT NOT NULL,
		agent_name     TEXT NOT NULL,
		snapshot_id    TEXT,
		recall_through TEXT,
		knowledge_rev  INTEGER,
		stamped_at     TEXT NOT NULL,
		PRIMARY KEY (task_id, event, stamped_at)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_task_basis_snapshot ON task_basis(snapshot_id)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_task_basis_recall ON task_basis(recall_through) WHERE recall_through IS NOT NULL`)
}

// ContextSetHash is the content address of a set of memory row ids:
// hex(sha256(sorted ids joined by ","))[:32]. The empty set has a hash too.
func ContextSetHash(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, ",")))
	return hex.EncodeToString(sum[:])[:32]
}

// dedupIDs drops empty and repeated ids, keeping the first occurrence.
func dedupIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Basis formats a basis token "<knowledge_rev>:<snapshot id8>" ("-" when the
// knowledge log does not exist yet or is empty).
func Basis(rev sql.NullInt64, snapshotID string) string {
	id := snapshotID
	if len(id) > 8 {
		id = id[:8]
	}
	r := "-"
	if rev.Valid {
		r = fmt.Sprint(rev.Int64)
	}
	return r + ":" + id
}

// knowledgeRevQ reads max(knowledge_log.rev) on q. NULL when the log is empty
// or does not exist (knowledge S1 not migrated yet): never an error.
func knowledgeRevQ(q excQ) sql.NullInt64 {
	var rev sql.NullInt64
	if err := q.QueryRow(`SELECT MAX(rev) FROM knowledge_log`).Scan(&rev); err != nil {
		return sql.NullInt64{}
	}
	return rev
}

// ContextHead is an agent's current boot basis.
type ContextHead struct {
	SnapshotID, SetHash, Kind, CreatedAt string
	KnowledgeRev                         sql.NullInt64
}

// Basis returns the head's basis token.
func (h ContextHead) Basis() string { return Basis(h.KnowledgeRev, h.SnapshotID) }

// GetContextHead returns the agent's head, or nil when it never booted with
// capture on.
func (d *DB) GetContextHead(project, agent string) (*ContextHead, error) {
	var h ContextHead
	err := d.ro().QueryRow(`SELECT h.snapshot_id, h.set_hash, s.kind, s.created_at, s.knowledge_rev
		FROM context_heads h JOIN context_snapshots s ON s.id = h.snapshot_id
		WHERE h.project = ? AND h.agent_name = ?`, project, agent).
		Scan(&h.SnapshotID, &h.SetHash, &h.Kind, &h.CreatedAt, &h.KnowledgeRev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// insertSetTx stores a set and its edges if the set is new.
func insertSetTx(tx *writerTx, setHash string, ids []string, now string) error {
	res, err := tx.Exec(`INSERT OR IGNORE INTO context_sets (set_hash, n, created_at) VALUES (?, ?, ?)`, setHash, len(ids), now)
	if err != nil {
		return fmt.Errorf("context set: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // already stored with its edges
	}
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO consumption_edges (memory_id, set_hash) VALUES (?, ?)`, id, setHash); err != nil {
			return fmt.Errorf("consumption edge: %w", err)
		}
	}
	return nil
}

// RecordSnapshot records the set an agent was served at boot. It writes
// nothing when the set equals the agent's head, or when a minimal boot finds
// no head (nothing held, nothing to record). Otherwise one writer tx stores
// the set (and its edges, the first time it is seen), the snapshot and the
// head. A racing identical boot leaves exactly one head and one snapshot.
// Returns the head after the call (nil when there is none) and whether it wrote.
func (d *DB) RecordSnapshot(project, agent, sessionID, kind string, ids []string) (*ContextHead, bool, error) {
	ids = dedupIDs(ids)
	setHash := ContextSetHash(ids)
	head, err := d.GetContextHead(project, agent)
	if err != nil {
		return nil, false, fmt.Errorf("context head: %w", err)
	}
	if head != nil && head.SetHash == setHash {
		return head, false, nil
	}
	if head == nil && len(ids) == 0 {
		return nil, false, nil
	}
	now := time.Now().UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, false, fmt.Errorf("snapshot begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertSetTx(tx, setHash, ids, now); err != nil {
		return nil, false, err
	}
	rev := knowledgeRevQ(tx)
	id := uuid.New().String()
	var session any
	if sessionID != "" {
		session = sessionID
	}
	if _, err := tx.Exec(`INSERT INTO context_snapshots (id, project, agent_name, session_id, kind, set_hash, knowledge_rev, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, project, agent, session, kind, setHash, rev, now); err != nil {
		return nil, false, fmt.Errorf("snapshot insert: %w", err)
	}
	res, err := tx.Exec(`INSERT INTO context_heads (project, agent_name, snapshot_id, set_hash) VALUES (?, ?, ?, ?)
		ON CONFLICT(project, agent_name) DO UPDATE SET snapshot_id = excluded.snapshot_id, set_hash = excluded.set_hash
		WHERE context_heads.set_hash <> excluded.set_hash`, project, agent, id, setHash)
	if err != nil {
		return nil, false, fmt.Errorf("head upsert: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// A concurrent boot already moved the head to this set: keep its
		// snapshot, write nothing.
		_ = tx.Rollback()
		h, err := d.GetContextHead(project, agent)
		return h, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("snapshot commit: %w", err)
	}
	return &ContextHead{SnapshotID: id, SetHash: setHash, Kind: kind, CreatedAt: now, KnowledgeRev: rev}, true, nil
}

// RecallKey identifies one agent's recall buffer.
type RecallKey struct{ Project, Agent string }

// RecordRecallBatch writes every agent's buffered recalls in ONE writer tx: per
// agent, the ids not already in its head set become one recall snapshot whose
// parent is the head. Writes nothing when no agent has a new id.
func (d *DB) RecordRecallBatch(batch map[RecallKey][]string) (int, error) {
	type pending struct {
		key    RecallKey
		ids    []string
		parent string
	}
	var todo []pending
	keys := make([]RecallKey, 0, len(batch))
	for k := range batch {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Project != keys[j].Project {
			return keys[i].Project < keys[j].Project
		}
		return keys[i].Agent < keys[j].Agent
	})
	for _, k := range keys {
		ids := dedupIDs(batch[k])
		head, err := d.GetContextHead(k.Project, k.Agent)
		if err != nil {
			return 0, fmt.Errorf("recall head: %w", err)
		}
		parent := ""
		if head != nil {
			parent = head.SnapshotID
			held, err := d.setMembers(head.SetHash)
			if err != nil {
				return 0, err
			}
			kept := ids[:0]
			for _, id := range ids {
				if !held[id] {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		if len(ids) > 0 {
			todo = append(todo, pending{k, ids, parent})
		}
	}
	if len(todo) == 0 {
		return 0, nil
	}
	now := time.Now().UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, fmt.Errorf("recall begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rev := knowledgeRevQ(tx)
	for _, p := range todo {
		setHash := ContextSetHash(p.ids)
		if err := insertSetTx(tx, setHash, p.ids, now); err != nil {
			return 0, err
		}
		var parent any
		if p.parent != "" {
			parent = p.parent
		}
		if _, err := tx.Exec(`INSERT INTO context_snapshots (id, project, agent_name, kind, set_hash, parent_id, knowledge_rev, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, uuid.New().String(), p.key.Project, p.key.Agent, SnapshotRecall, setHash, parent, rev, now); err != nil {
			return 0, fmt.Errorf("recall snapshot: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("recall commit: %w", err)
	}
	return len(todo), nil
}

func (d *DB) setMembers(setHash string) (map[string]bool, error) {
	rows, err := d.ro().Query(`SELECT memory_id FROM consumption_edges WHERE set_hash = ?`, setHash)
	if err != nil {
		return nil, fmt.Errorf("set members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// PruneConsumption deletes snapshots older than retention unless they are a
// current head (or the head a recall extends) or a task basis references them,
// then the sets (and edges) no snapshot references any more. One writer tx,
// at most pruneChunk snapshots per call; writes nothing when nothing is due.
func (d *DB) PruneConsumption(retention time.Duration, now time.Time) (int, error) {
	cutoff := now.Add(-retention).UTC().Format(memoryTimeFmt)
	const due = `FROM context_snapshots s
		WHERE s.created_at < ?
		  AND NOT EXISTS (SELECT 1 FROM context_heads h WHERE h.snapshot_id = s.id)
		  AND NOT EXISTS (SELECT 1 FROM context_heads h WHERE h.snapshot_id = s.parent_id AND s.kind = 'recall')
		  AND NOT EXISTS (SELECT 1 FROM task_basis b WHERE b.snapshot_id = s.id OR b.recall_through = s.id)`
	var n int
	if err := d.ro().QueryRow(`SELECT COUNT(*) `+due, cutoff).Scan(&n); err != nil {
		return 0, fmt.Errorf("prune check: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, fmt.Errorf("prune begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM context_snapshots WHERE id IN (SELECT s.id `+due+` LIMIT ?)`, cutoff, pruneChunk)
	if err != nil {
		return 0, fmt.Errorf("prune snapshots: %w", err)
	}
	deleted, _ := res.RowsAffected()
	const orphanSets = `SELECT c.set_hash FROM context_sets c
		WHERE NOT EXISTS (SELECT 1 FROM context_snapshots s WHERE s.set_hash = c.set_hash)
		  AND NOT EXISTS (SELECT 1 FROM context_heads h WHERE h.set_hash = c.set_hash)`
	if _, err := tx.Exec(`DELETE FROM consumption_edges WHERE set_hash IN (` + orphanSets + `)`); err != nil {
		return 0, fmt.Errorf("prune edges: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM context_sets WHERE set_hash IN (` + orphanSets + `)`); err != nil {
		return 0, fmt.Errorf("prune sets: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("prune commit: %w", err)
	}
	return int(deleted), nil
}

// Consumer is one agent holding a version of a key.
type Consumer struct {
	Project  string `json:"project"`
	Agent    string `json:"agent"`
	MemoryID string `json:"memory_id"`
	Version  int    `json:"version"`
	Current  bool   `json:"current"`
	Via      string `json:"via"` // boot | recall
	Since    string `json:"since"`
	Basis    string `json:"basis"`
	Active   bool   `json:"active"`
	// ActiveTasks are the agent's non-terminal tasks whose claim stamp's
	// basis (head or recall) holds a version of the key.
	ActiveTasks []string `json:"active_tasks"`
}

// WhoConsumed lists the agents whose current head (or a recall snapshot that
// extends it) holds a version of key visible from project. activeOnly keeps
// agents with status active. Read-only.
func (d *DB) WhoConsumed(project, key, scope string, activeOnly bool) ([]Consumer, error) {
	q := `SELECT id, version, archived_at IS NULL FROM memories WHERE key = ? AND (project = ? OR scope = 'global')`
	args := []any{key, project}
	if scope != "" {
		q += ` AND scope = ?`
		args = append(args, scope)
	}
	rows, err := d.ro().Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("who_consumed versions: %w", err)
	}
	type ver struct {
		version int
		current bool
	}
	versions := map[string]ver{}
	var ids []any
	for rows.Next() {
		var id string
		var v ver
		if err := rows.Scan(&id, &v.version, &v.current); err != nil {
			_ = rows.Close()
			return nil, err
		}
		versions[id] = v
		ids = append(ids, id)
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return []Consumer{}, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	// A holder is the snapshot that IS the agent's head, or a recall snapshot
	// extending it; an agent that never booted with capture holds what its
	// head-less recalls served.
	crow, err := d.ro().Query(`SELECT s.project, s.agent_name, e.memory_id, s.kind, s.created_at, s.knowledge_rev, s.id,
			COALESCE(a.status, '')
		FROM consumption_edges e
		JOIN context_snapshots s ON s.set_hash = e.set_hash
		LEFT JOIN context_heads h ON h.project = s.project AND h.agent_name = s.agent_name
		LEFT JOIN agents a ON a.project = s.project AND a.name = s.agent_name
		WHERE e.memory_id IN (`+ph+`)
		  AND (s.id = h.snapshot_id
		       OR (s.kind = 'recall' AND ((h.snapshot_id IS NOT NULL AND s.parent_id = h.snapshot_id)
		                                  OR (h.snapshot_id IS NULL AND s.parent_id IS NULL))))
		ORDER BY s.project, s.agent_name, s.created_at`, ids...)
	if err != nil {
		return nil, fmt.Errorf("who_consumed: %w", err)
	}
	defer func() { _ = crow.Close() }()
	out := []Consumer{}
	seen := map[string]int{}
	for crow.Next() {
		var c Consumer
		var kind, status, snapshotID string
		var rev sql.NullInt64
		if err := crow.Scan(&c.Project, &c.Agent, &c.MemoryID, &kind, &c.Since, &rev, &snapshotID, &status); err != nil {
			return nil, err
		}
		c.Active = status == "active"
		if activeOnly && !c.Active {
			continue
		}
		c.Via = SnapshotBoot
		if kind == SnapshotRecall {
			c.Via = SnapshotRecall
		}
		v := versions[c.MemoryID]
		c.Version, c.Current, c.Basis = v.version, v.current, Basis(rev, snapshotID)
		// One row per agent: the newest version it holds wins.
		k := c.Project + "/" + c.Agent
		if i, ok := seen[k]; ok {
			if c.Version > out[i].Version {
				out[i] = c
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, c)
	}
	if err := crow.Err(); err != nil {
		return nil, err
	}
	_ = crow.Close()
	for i := range out {
		tasks, err := d.activeTasksHolding(out[i].Project, out[i].Agent, ph, ids)
		if err != nil {
			return nil, err
		}
		out[i].ActiveTasks = tasks
	}
	return out, nil
}

// activeTasksHolding lists the agent's non-terminal tasks whose claim stamp
// (head snapshot or recall_through) holds one of the given memory ids.
func (d *DB) activeTasksHolding(project, agent, ph string, ids []any) ([]string, error) {
	args := append([]any{project, agent}, ids...)
	rows, err := d.ro().Query(`SELECT DISTINCT tb.task_id FROM task_basis tb JOIN tasks t ON t.id = tb.task_id
		WHERE tb.project = ? AND tb.agent_name = ? AND tb.event = 'claim'
		  AND t.status NOT IN ('done', 'cancelled') AND t.archived_at IS NULL
		  AND EXISTS (SELECT 1 FROM context_snapshots s JOIN consumption_edges e ON e.set_hash = s.set_hash
		              WHERE s.id IN (tb.snapshot_id, tb.recall_through) AND e.memory_id IN (`+ph+`))
		ORDER BY tb.task_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("who_consumed tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Task basis events (task_basis.event).
const (
	BasisClaim    = "claim"
	BasisComplete = "complete"
)

// StampTaskBasis records the knowledge basis an agent ran taskIDs on, for
// event (claim | complete), in ONE writer tx (design 54e529d8 §4.3, ruling
// f97023b7 OQ3: its own tx, after the transition). recallIDs are the agent's
// drained buffered recalls: those not already in its head become one recall
// snapshot first, so recall_through is exact at stamp time. Returns the basis
// token "<knowledge_rev>:<head id8>" ("none" as the id when the agent never
// booted with capture on).
func (d *DB) StampTaskBasis(project, agent, event string, taskIDs, recallIDs []string) (string, error) {
	if len(taskIDs) == 0 {
		return "", nil
	}
	head, err := d.GetContextHead(project, agent)
	if err != nil {
		return "", fmt.Errorf("stamp head: %w", err)
	}
	ids := dedupIDs(recallIDs)
	headID := ""
	if head != nil {
		headID = head.SnapshotID
		held, err := d.setMembers(head.SetHash)
		if err != nil {
			return "", err
		}
		kept := ids[:0]
		for _, id := range ids {
			if !held[id] {
				kept = append(kept, id)
			}
		}
		ids = kept
	}
	now := time.Now().UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return "", fmt.Errorf("stamp begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rev := knowledgeRevQ(tx)
	var parent any
	if headID != "" {
		parent = headID
	}
	recallThrough := ""
	if len(ids) > 0 {
		setHash := ContextSetHash(ids)
		if err := insertSetTx(tx, setHash, ids, now); err != nil {
			return "", err
		}
		recallThrough = uuid.New().String()
		if _, err := tx.Exec(`INSERT INTO context_snapshots (id, project, agent_name, kind, set_hash, parent_id, knowledge_rev, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, recallThrough, project, agent, SnapshotRecall, setHash, parent, rev, now); err != nil {
			return "", fmt.Errorf("stamp recall snapshot: %w", err)
		}
	} else {
		// The latest recall extending the current head (or head-less recalls).
		q := `SELECT id FROM context_snapshots WHERE project = ? AND agent_name = ? AND kind = 'recall' AND parent_id IS NULL
			ORDER BY created_at DESC, id DESC LIMIT 1`
		args := []any{project, agent}
		if headID != "" {
			q = `SELECT id FROM context_snapshots WHERE project = ? AND agent_name = ? AND kind = 'recall' AND parent_id = ?
				ORDER BY created_at DESC, id DESC LIMIT 1`
			args = append(args, headID)
		}
		if err := tx.QueryRow(q, args...).Scan(&recallThrough); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("stamp recall_through: %w", err)
		}
	}
	var snap, through any
	if headID != "" {
		snap = headID
	}
	if recallThrough != "" {
		through = recallThrough
	}
	for _, taskID := range taskIDs {
		if _, err := tx.Exec(`INSERT INTO task_basis (task_id, event, project, agent_name, snapshot_id, recall_through, knowledge_rev, stamped_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, taskID, event, project, agent, snap, through, rev, now); err != nil {
			return "", fmt.Errorf("stamp task_basis: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("stamp commit: %w", err)
	}
	if headID == "" {
		headID = "none"
	}
	return Basis(rev, headID), nil
}

// BasisMemory is one memory in an agent's context basis.
type BasisMemory struct {
	Key                string `json:"key"`
	Scope              string `json:"scope"`
	MemoryID           string `json:"memory_id"`
	Version            int    `json:"version"`
	NewerVersionExists bool   `json:"newer_version_exists"`
}

// ContextBasis returns the agent's head and the memories it holds (head set
// plus the recall snapshots extending it), each flagged when a newer live
// version of its key exists. nil head when the agent has none. Read-only.
func (d *DB) ContextBasis(project, agent string) (*ContextHead, []BasisMemory, error) {
	head, err := d.GetContextHead(project, agent)
	if err != nil || head == nil {
		return head, nil, err
	}
	rows, err := d.ro().Query(`SELECT DISTINCT m.key, m.scope, m.id, m.version,
			EXISTS (SELECT 1 FROM memories n WHERE n.key = m.key AND n.scope = m.scope AND n.project = m.project
			        AND n.archived_at IS NULL AND n.id <> m.id AND n.version > m.version)
		FROM context_snapshots s
		JOIN consumption_edges e ON e.set_hash = s.set_hash
		JOIN memories m ON m.id = e.memory_id
		WHERE s.id = ? OR (s.kind = 'recall' AND s.parent_id = ?)
		ORDER BY m.key`, head.SnapshotID, head.SnapshotID)
	if err != nil {
		return nil, nil, fmt.Errorf("context basis: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BasisMemory
	for rows.Next() {
		var b BasisMemory
		if err := rows.Scan(&b.Key, &b.Scope, &b.MemoryID, &b.Version, &b.NewerVersionExists); err != nil {
			return nil, nil, err
		}
		out = append(out, b)
	}
	return head, out, rows.Err()
}
