package db

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"agent-relay/internal/models"
)

// Typed edges with consequences (design d523e74e, ruling cto-tsukumo
// b3a6ab43). One polymorphic edge table; every edge type is a row of the
// semantics registry whose consequence code enforces, and an unregistered type
// is rejected. Readiness is DERIVED by one predicate (readyPredicate) shared by
// the ready listing, ClaimNextTask and the hold release; nothing is persisted
// on tasks. The one thing settled at write time is the hold: a task dispatched
// blocked_by an unfinished prerequisite gets no claim signal until its hold is
// released, exactly once, in the writer tx of the change that made it ready
// (bounded to the direct dependents) or by the ReleaseReadyHolds sweep.

// Registered edge types (v1).
const (
	EdgeBlockedBy      = "blocked_by"
	EdgeDiscoveredFrom = "discovered_from"
)

// Edge error codes (TaskError.Code).
const (
	CodeEdgeInvalidArgument = "INVALID_ARGUMENT"
	CodeEdgeCycle           = "EDGE_CYCLE"
	CodeEdgeCheckLimit      = "EDGE_CHECK_LIMIT"
	CodeLinearReadOnly      = "LINEAR_READ_ONLY"
)

// Hold flags (task_holds.flagged).
const (
	HoldFlagPrerequisiteCancelled = "prerequisite_cancelled"
	HoldFlagEdgeExpired           = "edge_expired"
	HoldFlagLeftPending           = "left_pending"
)

const (
	// settleLimit bounds the dependents one transition settles in its tx; the
	// rest are released by the ReleaseReadyHolds sweep.
	settleLimit = 64
	// cycleCheckLimit bounds the nodes a cycle check visits; past it the edge
	// is refused (EDGE_CHECK_LIMIT) rather than guessed.
	cycleCheckLimit = 256
	// unblockImpactDepth bounds the transitive-dependents walk of unblock_impact.
	unblockImpactDepth = 8
	// untilInReview is the blocked_by metadata value that is satisfied once the
	// prerequisite is in review ("START after X is submitted").
	untilInReview = "in-review"
)

func migrateOrgEdges(conn *sql.DB) {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS org_edges (
		id          TEXT PRIMARY KEY,
		project     TEXT NOT NULL,
		src_kind    TEXT NOT NULL,
		src_id      TEXT NOT NULL,
		type        TEXT NOT NULL,
		dst_kind    TEXT NOT NULL,
		dst_id      TEXT NOT NULL,
		metadata    TEXT NOT NULL DEFAULT '{}',
		created_by  TEXT NOT NULL,
		created_at  TEXT NOT NULL,
		removed_at  TEXT,
		removed_by  TEXT,
		valid_until TEXT
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_org_edges_src ON org_edges(src_kind, src_id, type) WHERE removed_at IS NULL`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_org_edges_dst ON org_edges(dst_kind, dst_id, type) WHERE removed_at IS NULL`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS edge_semantics (
		type                    TEXT PRIMARY KEY,
		src_kinds               TEXT NOT NULL,
		dst_kinds               TEXT NOT NULL,
		blocks_readiness        INTEGER NOT NULL,
		propagates_invalidation INTEGER NOT NULL,
		display_only            INTEGER NOT NULL,
		acyclic                 INTEGER NOT NULL,
		since_version           TEXT NOT NULL
	)`)
	// Code owns the registry: a type is added only with the code enforcing it.
	_, _ = conn.Exec(`INSERT OR IGNORE INTO edge_semantics
		(type, src_kinds, dst_kinds, blocks_readiness, propagates_invalidation, display_only, acyclic, since_version)
		VALUES ('blocked_by', '["task"]', '["task"]', 1, 1, 0, 1, 'graph-s1'),
		       ('discovered_from', '["task"]', '["task"]', 0, 0, 1, 0, 'graph-s1')`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS task_holds (
		task_id     TEXT PRIMARY KEY,
		project     TEXT NOT NULL,
		reason      TEXT NOT NULL,
		held_at     TEXT NOT NULL,
		released_at TEXT,
		flagged     TEXT
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_task_holds_open ON task_holds(project) WHERE released_at IS NULL`)
}

type edgeSemantics struct {
	srcKinds, dstKinds                         []string
	blocks, invalidation, displayOnly, acyclic bool
}

func loadSemantics(q excQ, typ string) (*edgeSemantics, error) {
	var src, dst string
	var s edgeSemantics
	err := q.QueryRow(`SELECT src_kinds, dst_kinds, blocks_readiness, propagates_invalidation, display_only, acyclic
		FROM edge_semantics WHERE type = ?`, typ).Scan(&src, &dst, &s.blocks, &s.invalidation, &s.displayOnly, &s.acyclic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(src), &s.srcKinds)
	_ = json.Unmarshal([]byte(dst), &s.dstKinds)
	return &s, nil
}

func edgeKindAllowed(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// EdgeInput is one edge to write.
type EdgeInput struct {
	SrcKind, SrcID, Type, DstKind, DstID string
	Metadata                             map[string]any
	CreatedBy                            string
	ValidUntil                           string
}

// edgeID is deterministic from the natural key, so a re-add is idempotent.
func edgeID(srcKind, srcID, typ, dstKind, dstID string) string {
	h := sha1.Sum([]byte(srcKind + "|" + srcID + "|" + typ + "|" + dstKind + "|" + dstID))
	return hex.EncodeToString(h[:])[:16]
}

// addEdgeTx validates and writes one edge on the caller's writer tx (rules of
// design §2.3, in order): registered type and kind pair; both endpoints in the
// project; no blocking edge out of a Linear-mirrored task; the bounded cycle
// check for acyclic types. A blocking edge on a pending task that is not ready
// opens (or re-opens) its hold.
func (d *DB) addEdgeTx(tx *writerTx, project string, e EdgeInput, now string) error {
	sem, err := loadSemantics(tx, e.Type)
	if err != nil {
		return fmt.Errorf("edge semantics: %w", err)
	}
	if sem == nil {
		return newTaskError(CodeEdgeInvalidArgument, "edge type %q is not registered", e.Type)
	}
	if !edgeKindAllowed(sem.srcKinds, e.SrcKind) || !edgeKindAllowed(sem.dstKinds, e.DstKind) {
		return newTaskError(CodeEdgeInvalidArgument, "edge type %q does not allow %s -> %s", e.Type, e.SrcKind, e.DstKind)
	}
	until, _ := e.Metadata["until"].(string)
	if e.Type == EdgeBlockedBy && until != "" && until != "done" && until != untilInReview {
		return newTaskError(CodeEdgeInvalidArgument, "blocked_by until must be done or in-review, got %q", until)
	}
	var srcSource string
	var srcSecondary bool
	if err := tx.QueryRow(`SELECT source, COALESCE(linear_secondary, 0) FROM tasks WHERE id = ? AND project = ?`, e.SrcID, project).
		Scan(&srcSource, &srcSecondary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return newTaskError(CodeTaskNotFound, "task %s not found in project %s", e.SrcID, project)
		}
		return err
	}
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM tasks WHERE id = ? AND project = ?`, e.DstID, project).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return newTaskError(CodeTaskNotFound, "task %s not found in project %s", e.DstID, project)
		}
		return err
	}
	// Linear is SSOT for a mirrored task's ordering (the round-trip that
	// removed depends_on in ade0c39): no blocking edge out of it.
	if sem.blocks && srcSource == "linear" && !srcSecondary {
		return newTaskError(CodeLinearReadOnly, "task %s is mirrored from Linear; its blocking edges live in Linear", e.SrcID)
	}
	if sem.acyclic {
		if err := checkNoCycleTx(tx, e.Type, e.SrcID, e.DstID); err != nil {
			return err
		}
	}
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	mj, _ := json.Marshal(meta)
	var validUntil any
	if e.ValidUntil != "" {
		validUntil = e.ValidUntil
	}
	if _, err := tx.Exec(`INSERT INTO org_edges (id, project, src_kind, src_id, type, dst_kind, dst_id, metadata, created_by, created_at, valid_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET removed_at = NULL, removed_by = NULL, metadata = excluded.metadata, valid_until = excluded.valid_until`,
		edgeID(e.SrcKind, e.SrcID, e.Type, e.DstKind, e.DstID), project, e.SrcKind, e.SrcID, e.Type, e.DstKind, e.DstID,
		string(mj), e.CreatedBy, now, validUntil); err != nil {
		return fmt.Errorf("insert edge: %w", err)
	}
	if sem.blocks {
		return holdIfNotReadyTx(tx, project, e.SrcID, now)
	}
	return nil
}

// checkNoCycleTx refuses a blocking edge src -> dst when src is already
// reachable from dst over live edges of the same type, visiting at most
// cycleCheckLimit nodes.
func checkNoCycleTx(tx *writerTx, typ, src, dst string) error {
	if src == dst {
		return newTaskError(CodeEdgeCycle, "edge %s -> %s is a self-cycle", src, dst)
	}
	// src -> dst closes a cycle only if src is reachable from dst, which needs
	// someone to depend on src. A task nobody depends on (every fresh dispatch)
	// cannot close one: skip the walk, so long chains stay extendable.
	var dependent int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM org_edges WHERE dst_kind = 'task' AND dst_id = ? AND type = ? AND removed_at IS NULL`,
		src, typ).Scan(&dependent); err != nil {
		return err
	}
	if dependent == 0 {
		return nil
	}
	type node struct {
		id   string
		path []string
	}
	queue := []node{{dst, []string{src, dst}}}
	seen := map[string]bool{dst: true}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		rows, err := tx.Query(`SELECT dst_id FROM org_edges WHERE src_kind = 'task' AND src_id = ? AND type = ? AND removed_at IS NULL`, n.id, typ)
		if err != nil {
			return err
		}
		var next []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			next = append(next, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range next {
			if id == src {
				return newTaskError(CodeEdgeCycle, "edge would close a cycle: %s", strings.Join(append(n.path, src), " -> "))
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			if len(seen) > cycleCheckLimit {
				return newTaskError(CodeEdgeCheckLimit, "cycle check visited more than %d tasks; edge refused", cycleCheckLimit)
			}
			queue = append(queue, node{id, append(append([]string(nil), n.path...), id)})
		}
	}
	return nil
}

// AddEdge writes one edge in its own writer tx.
func (d *DB) AddEdge(project string, e EdgeInput) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.addEdgeTx(tx, project, e, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveEdge soft-deletes one edge and settles its src in the same tx.
// Returns the task ids whose hold it released (for the announce).
func (d *DB) RemoveEdge(project, srcID, typ, dstID, removedBy string) ([]string, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE org_edges SET removed_at = ?, removed_by = ? WHERE id = ? AND project = ? AND removed_at IS NULL`,
		now, removedBy, edgeID("task", srcID, typ, "task", dstID), project)
	if err != nil {
		return nil, fmt.Errorf("remove edge: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, newTaskError(CodeTaskNotFound, "no live %s edge %s -> %s", typ, srcID, dstID)
	}
	released, err := settleTaskTx(tx, project, srcID, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if released {
		return []string{srcID}, nil
	}
	return nil, nil
}

// readyPredicate is THE readiness predicate, used verbatim by the ready
// listing, ClaimNextTask and the hold settle: the task is pending, not
// archived, and no live, unexpired blocking edge out of it is unsatisfied. A
// blocked_by edge is satisfied when its prerequisite is done, or in review for
// until=in-review. A cancelled prerequisite never satisfies (ruling OQ2).
// alias names the tasks row being tested.
func readyPredicate(alias, now string) (string, []any) {
	return fmt.Sprintf(`%[1]s.status = 'pending' AND %[1]s.archived_at IS NULL AND NOT EXISTS (
		SELECT 1 FROM org_edges re JOIN tasks rp ON rp.id = re.dst_id
		WHERE re.src_kind = 'task' AND re.src_id = %[1]s.id AND re.dst_kind = 'task' AND re.removed_at IS NULL
		  AND re.type IN (SELECT type FROM edge_semantics WHERE blocks_readiness = 1)
		  AND (re.valid_until IS NULL OR re.valid_until > ?)
		  AND NOT (rp.status = 'done'
		           OR (rp.status = 'in-review' AND COALESCE(json_extract(re.metadata, '$.until'), 'done') = 'in-review')))`, alias), []any{now}
}

func isReadyTx(q excQ, project, taskID, now string) (bool, error) {
	pred, args := readyPredicate("t", now)
	var one int
	err := q.QueryRow(`SELECT 1 FROM tasks t WHERE t.id = ? AND t.project = ? AND `+pred,
		append([]any{taskID, project}, args...)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// holdIfNotReadyTx opens (or re-opens) the hold of a pending task that is not
// ready. A task already held keeps its row; a ready or non-pending task gets
// no hold.
func holdIfNotReadyTx(tx *writerTx, project, taskID, now string) error {
	var status string
	if err := tx.QueryRow(`SELECT status FROM tasks WHERE id = ? AND project = ?`, taskID, project).Scan(&status); err != nil {
		return err
	}
	if status != "pending" {
		return nil
	}
	ready, err := isReadyTx(tx, project, taskID, now)
	if err != nil || ready {
		return err
	}
	_, err = tx.Exec(`INSERT INTO task_holds (task_id, project, reason, held_at) VALUES (?, ?, 'blocked_by', ?)
		ON CONFLICT(task_id) DO UPDATE SET held_at = excluded.held_at, released_at = NULL, flagged = NULL
		WHERE task_holds.released_at IS NOT NULL`, taskID, project, now)
	return err
}

// settleTaskTx settles one held task: released (CAS, exactly once) when it is
// ready; flagged prerequisite_cancelled when a prerequisite was cancelled
// (never released, never auto-cancelled); closed as left_pending when it is no
// longer pending (claimed directly: nothing to announce). True = released now.
func settleTaskTx(q excQ, project, taskID, now string) (bool, error) {
	var status string
	err := q.QueryRow(`SELECT t.status FROM task_holds h JOIN tasks t ON t.id = h.task_id
		WHERE h.task_id = ? AND h.project = ? AND h.released_at IS NULL`, taskID, project).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != "pending" {
		_, err := q.Exec(`UPDATE task_holds SET released_at = ?, flagged = ? WHERE task_id = ? AND released_at IS NULL`,
			now, HoldFlagLeftPending, taskID)
		return false, err
	}
	ready, err := isReadyTx(q, project, taskID, now)
	if err != nil {
		return false, err
	}
	if ready {
		flag := any(nil)
		var expired int
		_ = q.QueryRow(`SELECT COUNT(*) FROM org_edges WHERE src_kind = 'task' AND src_id = ? AND removed_at IS NULL
			AND valid_until IS NOT NULL AND valid_until <= ?`, taskID, now).Scan(&expired)
		if expired > 0 {
			flag = HoldFlagEdgeExpired
		}
		res, err := q.Exec(`UPDATE task_holds SET released_at = ?, flagged = COALESCE(?, flagged) WHERE task_id = ? AND released_at IS NULL`,
			now, flag, taskID)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n == 1, nil
	}
	var cancelled int
	if err := q.QueryRow(`SELECT COUNT(*) FROM org_edges e JOIN tasks p ON p.id = e.dst_id
		WHERE e.src_kind = 'task' AND e.src_id = ? AND e.removed_at IS NULL
		  AND e.type IN (SELECT type FROM edge_semantics WHERE propagates_invalidation = 1)
		  AND (p.status = 'cancelled' OR (p.archived_at IS NOT NULL AND p.status <> 'done'))`, taskID).Scan(&cancelled); err != nil {
		return false, err
	}
	if cancelled > 0 {
		_, err := q.Exec(`UPDATE task_holds SET flagged = ? WHERE task_id = ? AND released_at IS NULL AND COALESCE(flagged, '') <> ?`,
			HoldFlagPrerequisiteCancelled, taskID, HoldFlagPrerequisiteCancelled)
		return false, err
	}
	return false, nil
}

// settleHoldsTx settles the held direct dependents of a task whose status just
// changed, at most settleLimit of them (blocking is one hop, so the direct
// dependents are the whole affected set; the limit bounds the tx and the
// sweep finishes the rest). Returns the ids released now.
func settleHoldsTx(q excQ, project, changedTaskID, now string) ([]string, error) {
	rows, err := q.Query(`SELECT DISTINCT e.src_id FROM org_edges e
		JOIN task_holds h ON h.task_id = e.src_id AND h.released_at IS NULL
		WHERE e.dst_kind = 'task' AND e.dst_id = ? AND e.src_kind = 'task' AND e.removed_at IS NULL
		  AND e.type IN (SELECT type FROM edge_semantics WHERE blocks_readiness = 1)
		ORDER BY e.src_id LIMIT ?`, changedTaskID, settleLimit)
	if err != nil {
		return nil, err
	}
	var deps []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		deps = append(deps, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var released []string
	for _, id := range deps {
		ok, err := settleTaskTx(q, project, id, now)
		if err != nil {
			return nil, err
		}
		if ok {
			released = append(released, id)
		}
	}
	return released, nil
}

// hasBlockingDependentsRO reports whether any live blocking edge points at the
// task (the RO pre-check that decides whether a transition needs a settle).
func (d *DB) hasBlockingDependentsRO(taskID string) bool {
	var one int
	err := d.ro().QueryRow(`SELECT 1 FROM org_edges e JOIN task_holds h ON h.task_id = e.src_id AND h.released_at IS NULL
		WHERE e.dst_kind = 'task' AND e.dst_id = ? AND e.removed_at IS NULL LIMIT 1`, taskID).Scan(&one)
	return err == nil
}

// ReleasedHold is one hold released by the sweep, for the announce.
type ReleasedHold struct{ TaskID, Project string }

// ReleaseReadyHolds is the repair path (cleanup tick): it settles every open
// hold, releasing the ready ones (beyond a transition's settleLimit, expired
// edges, status writes that bypassed the settle) and closing the ones that
// left pending. One writer tx per chunk of 200. Returns what it released.
func (d *DB) ReleaseReadyHolds(now time.Time) ([]ReleasedHold, error) {
	ts := now.UTC().Format(memoryTimeFmt)
	rows, err := d.ro().Query(`SELECT task_id, project FROM task_holds WHERE released_at IS NULL ORDER BY held_at, task_id`)
	if err != nil {
		return nil, err
	}
	var open []ReleasedHold
	for rows.Next() {
		var h ReleasedHold
		if err := rows.Scan(&h.TaskID, &h.Project); err != nil {
			_ = rows.Close()
			return nil, err
		}
		open = append(open, h)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []ReleasedHold
	for start := 0; start < len(open); start += 200 {
		end := min(start+200, len(open))
		tx, err := d.beginWriterTx()
		if err != nil {
			return out, err
		}
		var chunk []ReleasedHold
		for _, h := range open[start:end] {
			ok, err := settleTaskTx(tx, h.Project, h.TaskID, ts)
			if err != nil {
				_ = tx.Rollback()
				return out, err
			}
			if ok {
				chunk = append(chunk, h)
			}
		}
		if err := tx.Commit(); err != nil {
			return out, err
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// ListReadyTasks lists the ready tasks of a project through readyPredicate,
// optionally for one profile and/or board, by priority then pending age.
func (d *DB) ListReadyTasks(project, profileSlug, boardID string, limit int) ([]models.Task, error) {
	return d.readyTasks(project, profileSlug, boardID, "priority, COALESCE(pending_since, dispatched_at), id", limit)
}

func (d *DB) readyTasks(project, profileSlug, boardID, orderBy string, limit int) ([]models.Task, error) {
	pred, pargs := readyPredicate("tasks", time.Now().UTC().Format(memoryTimeFmt))
	q := "SELECT " + taskColumns + " FROM tasks WHERE project = ? AND " + pred
	args := append([]any{project}, pargs...)
	if profileSlug != "" {
		q += " AND profile_slug = ?"
		args = append(args, profileSlug)
	}
	if boardID != "" {
		q += " AND board_id = ?"
		args = append(args, boardID)
	}
	q += " ORDER BY " + orderBy
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.ro().Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list ready tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []models.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Claim-next sort policies.
const (
	SortPriority      = "priority"
	SortOldest        = "oldest"
	SortUnblockImpact = "unblock_impact"
)

// ClaimNextTask claims the first ready task of the profile (and board) for
// agent, walking past candidates a racing claimer took (TASK_STATE_CONFLICT).
// The candidate list is the SAME predicate as ListReadyTasks and is not capped,
// so a race on the top row never yields a false "nothing to claim". Each claim
// is the existing ClaimTask CAS. Returns (nil, nil) when nothing is ready.
func (d *DB) ClaimNextTask(project, agent, profileSlug, boardID, sortBy string) (*models.Task, error) {
	order := "priority, COALESCE(pending_since, dispatched_at), id"
	if sortBy == SortOldest {
		order = "COALESCE(pending_since, dispatched_at), id"
	}
	cands, err := d.readyTasks(project, profileSlug, boardID, order, 0)
	if err != nil {
		return nil, err
	}
	if sortBy == SortUnblockImpact {
		impact := make(map[string]int, len(cands))
		for _, c := range cands {
			impact[c.ID] = d.UnblockImpact(project, c.ID)
		}
		sort.SliceStable(cands, func(i, j int) bool { return impact[cands[i].ID] > impact[cands[j].ID] })
	}
	for _, c := range cands {
		t, err := d.ClaimTask(c.ID, agent, project)
		if err == nil {
			return t, nil
		}
		var te *TaskError
		if errors.As(err, &te) && te.Code == CodeTaskStateConflict {
			continue // a racing claimer took it: next candidate
		}
		return nil, err
	}
	return nil, nil
}

// UnblockImpact counts the open tasks that transitively depend on taskID over
// live blocking edges, up to unblockImpactDepth hops (design §4.1 B11).
func (d *DB) UnblockImpact(project, taskID string) int {
	seen := map[string]bool{taskID: true}
	frontier := []string{taskID}
	count := 0
	for depth := 0; depth < unblockImpactDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, id := range frontier {
			rows, err := d.ro().Query(`SELECT e.src_id FROM org_edges e JOIN tasks t ON t.id = e.src_id
				WHERE e.dst_kind = 'task' AND e.dst_id = ? AND e.src_kind = 'task' AND e.removed_at IS NULL AND t.project = ?
				  AND e.type IN (SELECT type FROM edge_semantics WHERE blocks_readiness = 1)
				  AND t.status NOT IN ('done', 'cancelled')`, id, project)
			if err != nil {
				return count
			}
			for rows.Next() {
				var s string
				if rows.Scan(&s) == nil && !seen[s] {
					seen[s] = true
					count++
					next = append(next, s)
				}
			}
			_ = rows.Close()
		}
		frontier = next
	}
	return count
}

// TaskReadiness returns whether a task is ready and its unsatisfied blockers
// (the claim warning names them, ruling b3a6ab43). Read-only.
func (d *DB) TaskReadiness(project, taskID string) (bool, []models.EdgeRef, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	ready, err := isReadyTx(d.ro(), project, taskID, now)
	if err != nil {
		return false, nil, err
	}
	rows, err := d.ro().Query(`SELECT e.dst_id, p.status, COALESCE(json_extract(e.metadata, '$.until'), 'done')
		FROM org_edges e JOIN tasks p ON p.id = e.dst_id
		WHERE e.src_kind = 'task' AND e.src_id = ? AND e.dst_kind = 'task' AND e.removed_at IS NULL
		  AND e.type IN (SELECT type FROM edge_semantics WHERE blocks_readiness = 1)
		  AND (e.valid_until IS NULL OR e.valid_until > ?)
		  AND NOT (p.status = 'done' OR (p.status = 'in-review' AND COALESCE(json_extract(e.metadata, '$.until'), 'done') = 'in-review'))
		ORDER BY e.dst_id`, taskID, now)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []models.EdgeRef
	for rows.Next() {
		var r models.EdgeRef
		if err := rows.Scan(&r.ID, &r.Status, &r.Until); err != nil {
			return false, nil, err
		}
		out = append(out, r)
	}
	return ready, out, rows.Err()
}

// parseBlockedBy splits "<task_id>" / "<task_id>@in-review" entries.
func parseBlockedBy(entries []string) ([][2]string, error) {
	var out [][2]string
	for _, raw := range entries {
		id, until, _ := strings.Cut(strings.TrimSpace(raw), "@")
		if id == "" {
			continue
		}
		if until != "" && until != untilInReview && until != "done" {
			return nil, newTaskError(CodeEdgeInvalidArgument, "blocked_by %q: suffix must be @in-review", raw)
		}
		out = append(out, [2]string{id, until})
	}
	return out, nil
}
