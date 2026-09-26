package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-relay/internal/models"
	"agent-relay/internal/normalize"

	"github.com/google/uuid"
)

const memoryTimeFmt = "2006-01-02T15:04:05.000000Z"

// SetMemoryOpts carries the optional causal context of a memory write
// (DEC-wraith-memory-causal-1).
type SetMemoryOpts struct {
	// BasedOn is the memory id the writer read before writing ("" = no causal
	// context, legacy last-writer-wins; BasedOnNew = "I expect no live value").
	// When it names anything other than the live row, a differing value is
	// written as a live SIBLING (conflict_with = live row) instead of archiving
	// the live row — a writer that read an older version never silently
	// replaces a newer one.
	BasedOn string
}

// BasedOnNew is the SetMemoryOpts.BasedOn value for a create-only write.
const BasedOnNew = "new"

// ErrBasedOnMismatch is returned (nothing written) when based_on names a row
// that is not a version of the same project/scope/key.
var ErrBasedOnMismatch = errors.New("based_on does not match key/scope")

// SetMemory creates or versions a memory. If upsert is true, overwrites
// existing values silently (archives old version). If false, flags a conflict.
// It is SetMemoryWith without causal context.
func (d *DB) SetMemory(project, agentName, key, value, tagsJSON, scope, confidence, layer string, upsert ...bool) (*models.Memory, error) {
	doUpsert := true
	if len(upsert) > 0 {
		doUpsert = upsert[0]
	}
	return d.SetMemoryWith(project, agentName, key, value, tagsJSON, scope, confidence, layer, doUpsert, SetMemoryOpts{})
}

// SetMemoryWith is SetMemory with causal context. Decision table, evaluated in
// one writer tx against the live row (current):
//   - no live row: fresh version 1.
//   - same value, same tags/confidence/layer: touch updated_at only.
//   - same value, different tags/confidence/layer: supersede (new version with
//     the new metadata; the old metadata stays in history).
//   - different value, upsert=false: conflict mode (unchanged).
//   - different value, based_on "" or == current.id: supersede (as before).
//   - different value, based_on names another version or BasedOnNew: sibling —
//     current stays live, the new row has conflict_with=current and
//     supersedes=based_on (nil for BasedOnNew).
//
// Every supersede closes the predecessor's validity window where the
// successor's opens (valid_until = now unless an earlier one is set).
func (d *DB) SetMemoryWith(project, agentName, key, value, tagsJSON, scope, confidence, layer string, upsert bool, opts SetMemoryOpts) (*models.Memory, error) {
	value = normalize.JSONKeys(value)
	now := time.Now().UTC().Format(memoryTimeFmt)
	if confidence == "" {
		confidence = "stated"
	}
	if tagsJSON == "" {
		tagsJSON = "[]"
	}
	if layer == "" {
		layer = "behavior"
	}

	// Wrap the read-modify-write in a BEGIN IMMEDIATE transaction so SQLite
	// acquires the write lock before the SELECT. Without this, concurrent
	// writers on the same key can both read the same max version and both
	// insert as version+1, breaking the supersedes chain. It also makes the
	// based_on compare-and-write atomic.
	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if opts.BasedOn != "" && opts.BasedOn != BasedOnNew {
		if err := checkBasedOnTx(tx.Tx, opts.BasedOn, project, scope, agentName, key); err != nil {
			return nil, err
		}
	}

	existing, err := d.findActiveMemoryTx(tx.Tx, project, scope, agentName, key)
	if err != nil {
		return nil, err
	}

	mem := &models.Memory{
		ID:         uuid.New().String(),
		Key:        key,
		Value:      value,
		Tags:       tagsJSON,
		Scope:      scope,
		Project:    project,
		AgentName:  agentName,
		Confidence: confidence,
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
		Layer:      layer,
		ValidFrom:  &now,
		Status:     "live",
	}

	switch {
	case existing == nil:
		// No live memory — create fresh.

	case existing.Value == value && existing.Tags == tagsJSON && existing.Confidence == confidence && existing.Layer == layer:
		// Convergent write — just update the timestamp.
		if _, err := tx.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, now, existing.ID); err != nil {
			return nil, fmt.Errorf("update memory: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit memory noop: %w", err)
		}
		existing.UpdatedAt = now
		return existing, nil

	case existing.Value != value && !upsert:
		// Conflict mode — create new version, flag conflict.
		mem.Version = existing.Version + 1
		mem.Supersedes = &existing.ID
		mem.ConflictWith = &existing.ID

	case existing.Value != value && opts.BasedOn != "" && opts.BasedOn != existing.ID:
		// The writer read something other than the live row: keep both live.
		mem.Version = existing.Version + 1
		mem.ConflictWith = &existing.ID
		if opts.BasedOn != BasedOnNew {
			basedOn := opts.BasedOn
			mem.Supersedes = &basedOn
		}

	default:
		// Supersede: a different value (no causal context, or based on the
		// live row), or the same value with new tags/confidence/layer.
		// Tombstone: archived_by="upsert" (why), status=archived, reason set.
		if _, err := tx.Exec(
			`UPDATE memories SET archived_at = ?, archived_by = ?, status = 'archived', archived_reason = ?, `+closeValidityClause+` WHERE id = ?`,
			now, "upsert", "superseded", now, now, existing.ID,
		); err != nil {
			return nil, fmt.Errorf("archive old memory: %w", err)
		}
		mem.Version = existing.Version + 1
		mem.Supersedes = &existing.ID
	}

	if _, err := tx.Exec(
		`INSERT INTO memories (id, key, value, tags, scope, project, agent_name, confidence, version, supersedes, conflict_with, created_at, updated_at, layer, valid_from, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'live')`,
		mem.ID, mem.Key, mem.Value, mem.Tags, mem.Scope, mem.Project,
		mem.AgentName, mem.Confidence, mem.Version, mem.Supersedes, mem.ConflictWith,
		mem.CreatedAt, mem.UpdatedAt, mem.Layer, now,
	); err != nil {
		return nil, fmt.Errorf("insert memory: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit memory: %w", err)
	}
	return mem, nil
}

// closeValidityClause closes an archived row's validity window at the archive
// instant (two args: now, now) unless the row already expires earlier. Applied
// going forward only; historical rows are never rewritten.
const closeValidityClause = `valid_until = CASE WHEN valid_until IS NULL OR valid_until > ? THEN ? ELSE valid_until END`

// checkBasedOnTx verifies that basedOn names a version (live or archived) of
// the same project/scope/key — and, for agent scope, the same agent.
func checkBasedOnTx(tx *sql.Tx, basedOn, project, scope, agentName, key string) error {
	var gotProject, gotScope, gotAgent, gotKey string
	err := tx.QueryRow(`SELECT project, scope, agent_name, key FROM memories WHERE id = ?`, basedOn).
		Scan(&gotProject, &gotScope, &gotAgent, &gotKey)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBasedOnMismatch
	}
	if err != nil {
		return fmt.Errorf("read based_on: %w", err)
	}
	if gotKey != key || gotScope != scope {
		return ErrBasedOnMismatch
	}
	switch scope {
	case "agent":
		if gotProject != project || gotAgent != agentName {
			return ErrBasedOnMismatch
		}
	case "project":
		if gotProject != project {
			return ErrBasedOnMismatch
		}
	}
	return nil
}

// findActiveMemoryTx finds the newest active memory version through an open
// write transaction. Required so SetMemory holds the lock from read to
// write and prevents the version race observed under concurrent writers
// on the same key.
func (d *DB) findActiveMemoryTx(tx *sql.Tx, project, scope, agentName, key string) (*models.Memory, error) {
	var query string
	var args []any
	switch scope {
	case "agent":
		query = fmt.Sprintf(`SELECT %s
			 FROM memories WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL
			 ORDER BY version DESC LIMIT 1`, memorySelectCols)
		args = []any{key, project, agentName}
	case "project":
		query = fmt.Sprintf(`SELECT %s
			 FROM memories WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL
			 ORDER BY version DESC LIMIT 1`, memorySelectCols)
		args = []any{key, project}
	case "global":
		query = fmt.Sprintf(`SELECT %s
			 FROM memories WHERE key = ? AND scope = 'global' AND archived_at IS NULL
			 ORDER BY version DESC LIMIT 1`, memorySelectCols)
		args = []any{key}
	default:
		return nil, fmt.Errorf("invalid scope: %s", scope)
	}
	now := time.Now().UTC().Format(memoryTimeFmt)
	m, err := scanMemory(tx.QueryRow(query, args...), now)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

// GetMemory retrieves a memory by key with scope cascade: agent → project → global.
func (d *DB) GetMemory(project, agentName, key, scope string) ([]models.Memory, error) {
	// If a specific scope is requested, search that scope only (+ check for conflicts)
	if scope != "" {
		return d.getMemoryAtScope(project, agentName, key, scope)
	}

	// Cascade: agent → project → global
	for _, s := range []string{"agent", "project", "global"} {
		results, err := d.getMemoryAtScope(project, agentName, key, s)
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			return results, nil
		}
	}
	return []models.Memory{}, nil
}

func (d *DB) getMemoryAtScope(project, agentName, key, scope string) ([]models.Memory, error) {
	var query string
	var args []any

	switch scope {
	case "agent":
		query = fmt.Sprintf(`SELECT %s
				 FROM memories WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL
				 ORDER BY version DESC`, memorySelectCols)
		args = []any{key, project, agentName}
	case "project":
		query = fmt.Sprintf(`SELECT %s
				 FROM memories WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL
				 ORDER BY version DESC`, memorySelectCols)
		args = []any{key, project}
	case "global":
		query = fmt.Sprintf(`SELECT %s
				 FROM memories WHERE key = ? AND scope = 'global' AND archived_at IS NULL
				 ORDER BY version DESC`, memorySelectCols)
		args = []any{key}
	default:
		return nil, fmt.Errorf("invalid scope: %s", scope)
	}

	return d.queryMemories(query, args...)
}

// validityClause returns the SQL fragment (prefixed with " AND ") that keeps a
// read to live-only rows, plus the args it consumes. When includeStale is true
// it returns an empty clause so callers see live + stale (archived is filtered
// separately via archived_at IS NULL). alias is "" for unaliased tables or e.g.
// "m." for the FTS join. now is the comparison timestamp.
func validityClause(alias, now string, includeStale bool) (string, []any) {
	if includeStale {
		return "", nil
	}
	// Live only: not explicitly stale, and valid_until not yet passed.
	return fmt.Sprintf(" AND %sstatus != 'stale' AND (%svalid_until IS NULL OR %svalid_until > ?)", alias, alias, alias), []any{now}
}

// SearchMemory performs full-text search across memories. By default it returns
// live memories only; pass includeStale=true to also return time-expired (stale)
// memories. Archived (tombstoned) memories are never returned here.
func (d *DB) SearchMemory(project, agentName, query string, tags []string, scope string, limit int, includeStale ...bool) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	stale := len(includeStale) > 0 && includeStale[0]
	sql, args := d.buildMemorySearchQuery(project, agentName, query, tags, scope, limit, stale)
	return d.queryMemories(sql, args...)
}

// buildMemorySearchQuery assembles the scope/tag/validity-filtered search SQL and
// its args. Extracted so the default recall (SearchMemory) and ranked recall
// (SearchMemoryRanked) share ONE filter definition — a scope or validity change
// can never drift between the two paths. FTS order (bm25 `rank`) is preserved;
// the ranked path re-orders the returned candidate set in Go.
func (d *DB) buildMemorySearchQuery(project, agentName, query string, tags []string, scope string, limit int, stale bool) (string, []any) {
	now := time.Now().UTC().Format(memoryTimeFmt)

	// Build WHERE clauses for the main table filter
	var conditions []string
	var args []any

	conditions = append(conditions, "m.archived_at IS NULL")

	if scope != "" {
		switch scope {
		case "agent":
			conditions = append(conditions, "m.scope = 'agent'", "m.project = ?", "m.agent_name = ?")
			args = append(args, project, agentName)
		case "project":
			conditions = append(conditions, "m.scope IN ('project', 'agent')", "m.project = ?")
			args = append(args, project)
		case "global":
			conditions = append(conditions, "m.scope = 'global'")
		}
	} else {
		// Cross-scope search: show project + global + own agent memories
		conditions = append(conditions, "(m.scope = 'global' OR (m.project = ? AND (m.scope = 'project' OR (m.scope = 'agent' AND m.agent_name = ?))))")
		args = append(args, project, agentName)
	}

	if len(tags) > 0 {
		for _, tag := range tags {
			conditions = append(conditions, "m.tags LIKE ?")
			args = append(args, "%\""+tag+"\"%")
		}
	}

	where := strings.Join(conditions, " AND ")
	validity, vArgs := validityClause("m.", now, stale)

	var sql string
	if query != "" {
		sql = fmt.Sprintf(
			`SELECT %s
			 FROM memories m
			 JOIN memories_fts f ON m.rowid = f.rowid
			 WHERE %s%s AND memories_fts MATCH ?
			 ORDER BY rank
			 LIMIT ?`, memorySelectColsAliased("m"), where, validity,
		)
		args = append(args, vArgs...)
		args = append(args, escapeFTSQuery(query), limit)
	} else {
		sql = fmt.Sprintf(
			`SELECT %s
			 FROM memories m
			 WHERE %s%s
			 ORDER BY m.updated_at DESC
			 LIMIT ?`, memorySelectColsAliased("m"), where, validity,
		)
		args = append(args, vArgs...)
		args = append(args, limit)
	}

	return sql, args
}

// ListMemories returns memories matching the given filters. By default it
// returns live memories only; pass includeStale=true to also return stale
// (time-expired) memories. Archived memories are never returned here.
func (d *DB) ListMemories(project, scope, agentName string, tags []string, limit int, includeStale ...bool) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 50
	}
	stale := len(includeStale) > 0 && includeStale[0]
	now := time.Now().UTC().Format(memoryTimeFmt)

	var conditions []string
	var args []any

	conditions = append(conditions, "archived_at IS NULL")

	if project != "" {
		conditions = append(conditions, "project = ?")
		args = append(args, project)
	}
	if scope != "" {
		conditions = append(conditions, "scope = ?")
		args = append(args, scope)
	}
	if agentName != "" {
		conditions = append(conditions, "agent_name = ?")
		args = append(args, agentName)
	}
	if len(tags) > 0 {
		for _, tag := range tags {
			conditions = append(conditions, "tags LIKE ?")
			args = append(args, "%\""+tag+"\"%")
		}
	}

	where := strings.Join(conditions, " AND ")
	validity, vArgs := validityClause("", now, stale)
	args = append(args, vArgs...)
	args = append(args, limit)

	q := fmt.Sprintf(
		`SELECT %s
		 FROM memories WHERE %s%s ORDER BY updated_at DESC LIMIT ?`, memorySelectCols, where, validity,
	)

	return d.queryMemories(q, args...)
}

// Boot slots reserved per layer (task f4efab3a). Constraints used to sort first
// under one LIMIT, so a project with >= limit live constraints (tsukumo, niwa,
// trovex-growth: 50/50) never let a behavior or context memory into boot.
// Each layer now gets its reserved slots first (newest first inside the layer);
// slots a layer cannot fill are refilled from the remaining rows in the legacy
// order, so a constraints-only project still fills the whole limit. The
// projection floors that keep the newest of each layer are next to
// sessionConstraintFloor in internal/relay/project.go.
const (
	BootQuotaConstraints = 25
	BootQuotaBehavior    = 10
	BootQuotaContext     = 10
)

// bootLayerGroup maps a layer to its quota group; every other layer (decision,
// facts, state, ...) has no reserved slot and competes in the refill.
func bootLayerGroup(layer string) string {
	switch layer {
	case "constraints", "behavior", "context":
		return layer
	}
	return "other"
}

var bootQuota = map[string]int{
	"constraints": BootQuotaConstraints,
	"behavior":    BootQuotaBehavior,
	"context":     BootQuotaContext,
}

// ListBootMemories returns the memories an agent should see at boot:
// global + project-scope + its own agent-scope memories, mirroring
// SearchMemory's cross-scope visibility clause. ListMemories with agentName
// set filters agent_name on ALL scopes, which hides project/global memories
// written by other agents — wrong for session_context (Def. 7 boot view).
// Selection reserves BootQuota* slots per layer, then refills; the result is
// ordered constraints first, then updated_at DESC, so budget projection keeps
// its constraint floor.
func (d *DB) ListBootMemories(project, agentName string, limit int) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 50
	}
	// Boot view is live-only: a stale (time-expired) memory must not be
	// re-injected at session start as if it were current canon. It stays stored
	// and searchable via search_memory(include_stale=true), just not surfaced here.
	// The window function bounds the read to `limit` rows per layer group, so a
	// layer can fill the whole limit on refill but no layer is read unbounded.
	now := time.Now().UTC().Format(memoryTimeFmt)
	q := fmt.Sprintf(`SELECT %s FROM (
	   SELECT *, ROW_NUMBER() OVER (
	     PARTITION BY CASE WHEN layer IN ('constraints', 'behavior', 'context') THEN layer ELSE 'other' END
	     ORDER BY updated_at DESC) AS layer_rank
	   FROM memories
	   WHERE archived_at IS NULL
	     AND status != 'stale' AND (valid_until IS NULL OR valid_until > ?)
	     AND (scope = 'global' OR (project = ? AND (scope = 'project' OR (scope = 'agent' AND agent_name = ?))))
	 )
	 WHERE layer_rank <= ?
	 ORDER BY CASE WHEN layer = 'constraints' THEN 0 ELSE 1 END, updated_at DESC`, memorySelectCols)
	candidates, err := d.queryMemories(q, now, project, agentName, limit)
	if err != nil {
		return nil, err
	}
	return selectBootMemories(candidates, limit), nil
}

// selectBootMemories picks at most limit rows from candidates (already in boot
// order): first each layer's reserved quota, then the remaining rows in order
// until the limit. The output keeps the candidates' order.
func selectBootMemories(candidates []models.Memory, limit int) []models.Memory {
	if len(candidates) <= limit {
		return candidates
	}
	picked := make([]bool, len(candidates))
	taken := map[string]int{}
	n := 0
	for i, m := range candidates {
		if n == limit {
			break
		}
		g := bootLayerGroup(m.Layer)
		if taken[g] < bootQuota[g] {
			taken[g]++
			picked[i] = true
			n++
		}
	}
	for i := range candidates {
		if n == limit {
			break
		}
		if !picked[i] {
			picked[i] = true
			n++
		}
	}
	out := make([]models.Memory, 0, n)
	for i, m := range candidates {
		if picked[i] {
			out = append(out, m)
		}
	}
	return out
}

// DeleteMemory soft-deletes a memory (archives it, never a hard DELETE). This
// writes a full tombstone: archived_at (when), archived_by (who = agentName),
// archived_reason (why), and status='archived'. A recall of the key can still
// surface the tombstone rather than an unexplained empty result. An optional
// reason overrides the default "deleted".
func (d *DB) DeleteMemory(project, agentName, key, scope string, reason ...string) error {
	return d.DeleteMemoryAs(project, agentName, agentName, key, scope, reason...)
}

// DeleteMemoryAs archives a memory while recording BOTH sides of the tombstone:
// actingAgent becomes archived_by (WHO ran the delete), and for agent scope
// targetAuthor selects WHOSE memory is archived (the agent_name dimension). When
// targetAuthor == actingAgent this is the ordinary self-delete (the DeleteMemory
// shim). A distinct targetAuthor is the admin arm — an executive, or the janitor
// clearing a dead agent's leftovers, archiving another author's agent-scope
// memory — so the row's agent_name (the original author) stays intact while
// archived_by names who reaped it. Authorization for a cross-author target is the
// caller's (handler's) responsibility; this store call only wires the provenance.
// For project/global scope there is no agent_name dimension, so targetAuthor is
// irrelevant and only archived_by (actingAgent) is recorded.
func (d *DB) DeleteMemoryAs(project, actingAgent, targetAuthor, key, scope string, reason ...string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	why := "deleted"
	if len(reason) > 0 && reason[0] != "" {
		why = reason[0]
	}

	var query string
	var args []any

	switch scope {
	case "agent":
		query = `UPDATE memories SET archived_at = ?, archived_by = ?, archived_reason = ?, status = 'archived' WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL`
		args = []any{now, actingAgent, why, key, project, targetAuthor}
	case "project":
		query = `UPDATE memories SET archived_at = ?, archived_by = ?, archived_reason = ?, status = 'archived' WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL`
		args = []any{now, actingAgent, why, key, project}
	case "global":
		query = `UPDATE memories SET archived_at = ?, archived_by = ?, archived_reason = ?, status = 'archived' WHERE key = ? AND scope = 'global' AND archived_at IS NULL`
		args = []any{now, actingAgent, why, key}
	default:
		return fmt.Errorf("invalid scope: %s", scope)
	}

	res, err := d.writerExec(query, args...)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// A targeted cross-author delete (the admin/janitor arm: an explicit
		// `agent` target distinct from the caller, agent scope) that matches no
		// row means the operator aimed at the WRONG author — the live incident
		// where assumed authors differed (frontend-lead-resume was anonymous,
		// DEV-checkpoint was dev) and a no-op would leave the operator believing
		// the purge done. Fail loudly naming BOTH the key AND the target author
		// (founder failure-is-loud). The self-delete path (targetAuthor ==
		// actingAgent, incl. no `agent` param) keeps the plain message unchanged.
		if scope == "agent" && !strings.EqualFold(targetAuthor, actingAgent) {
			return fmt.Errorf("memory not found: no agent-scope memory %q authored by %q (scope=agent)", key, targetAuthor)
		}
		return fmt.Errorf("memory not found: %s (scope=%s)", key, scope)
	}
	return nil
}

// SetMemoryValidity stamps a temporal window on the active (non-archived) memory
// at key+scope. A validUntil in the past immediately makes the memory read as
// stale (still stored + searchable, hidden from default reads). validFrom is
// optional. Returns an error if no active memory matches. This is a guarded
// UPDATE (RowsAffected) on the single-writer conn.
func (d *DB) SetMemoryValidity(project, agentName, key, scope, validFrom, validUntil string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	// Normalize caller-supplied timestamps to the canonical fixed-width UTC form
	// so lexical comparisons (SQL `valid_until > ?` and effectiveStatus) are
	// reliable regardless of input precision/zone — a bare 'Z' form sorts wrong
	// against the microsecond form otherwise. Reject unparseable input loudly.
	if validFrom != "" {
		nf, ok := parseMemoryTime(validFrom)
		if !ok {
			return fmt.Errorf("invalid valid_from timestamp: %q", validFrom)
		}
		validFrom = nf
	}
	if validUntil != "" {
		nu, ok := parseMemoryTime(validUntil)
		if !ok {
			return fmt.Errorf("invalid valid_until timestamp: %q", validUntil)
		}
		validUntil = nu
	}
	set := []string{"updated_at = ?"}
	args := []any{now}
	if validFrom != "" {
		set = append(set, "valid_from = ?")
		args = append(args, validFrom)
	}
	// validUntil is always applied (empty clears any prior expiry -> NULL).
	set = append(set, "valid_until = ?")
	if validUntil == "" {
		args = append(args, nil)
	} else {
		args = append(args, validUntil)
	}

	var where string
	switch scope {
	case "agent":
		where = "key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL"
		args = append(args, key, project, agentName)
	case "project":
		where = "key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL"
		args = append(args, key, project)
	case "global":
		where = "key = ? AND scope = 'global' AND archived_at IS NULL"
		args = append(args, key)
	default:
		return fmt.Errorf("invalid scope: %s", scope)
	}

	q := fmt.Sprintf("UPDATE memories SET %s WHERE %s", strings.Join(set, ", "), where)
	res, err := d.writerExec(q, args...)
	if err != nil {
		return fmt.Errorf("set memory validity: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory not found: %s (scope=%s)", key, scope)
	}
	return nil
}

// GetMemoryIncludingArchived returns the newest memory at key — INCLUDING an
// archived (tombstoned) one — so a recall of a deleted/stale key surfaces the
// tombstone (who/when/why + status) rather than an unexplained empty result.
// Uses the same agent -> project -> global cascade as GetMemory when scope is "".
func (d *DB) GetMemoryIncludingArchived(project, agentName, key, scope string) ([]models.Memory, error) {
	scopes := []string{scope}
	if scope == "" {
		scopes = []string{"agent", "project", "global"}
	}
	for _, s := range scopes {
		var where string
		var args []any
		switch s {
		case "agent":
			where = "key = ? AND scope = 'agent' AND project = ? AND agent_name = ?"
			args = []any{key, project, agentName}
		case "project":
			where = "key = ? AND scope = 'project' AND project = ?"
			args = []any{key, project}
		case "global":
			where = "key = ? AND scope = 'global'"
			args = []any{key}
		default:
			return nil, fmt.Errorf("invalid scope: %s", s)
		}
		q := fmt.Sprintf(`SELECT %s FROM memories WHERE %s ORDER BY version DESC LIMIT 1`, memorySelectCols, where)
		mems, err := d.queryMemories(q, args...)
		if err != nil {
			return nil, err
		}
		if len(mems) > 0 {
			return mems, nil
		}
	}
	return []models.Memory{}, nil
}

// ResolveConflict resolves a conflict by setting the chosen value and archiving alternatives.
func (d *DB) ResolveConflict(project, agentName, key, chosenValue, scope string) (*models.Memory, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)

	// Find all active memories at this key+scope
	var memories []models.Memory
	var err error

	switch scope {
	case "agent":
		memories, err = d.queryMemories(
			fmt.Sprintf(`SELECT %s
			 FROM memories WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL
			 ORDER BY version DESC`, memorySelectCols),
			key, project, agentName,
		)
	case "project":
		memories, err = d.queryMemories(
			fmt.Sprintf(`SELECT %s
			 FROM memories WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL
			 ORDER BY version DESC`, memorySelectCols),
			key, project,
		)
	case "global":
		memories, err = d.queryMemories(
			fmt.Sprintf(`SELECT %s
			 FROM memories WHERE key = ? AND scope = 'global' AND archived_at IS NULL
			 ORDER BY version DESC`, memorySelectCols),
			key,
		)
	default:
		return nil, fmt.Errorf("invalid scope: %s", scope)
	}
	if err != nil {
		return nil, err
	}

	if len(memories) == 0 {
		return nil, fmt.Errorf("no active memories found for key=%s scope=%s", key, scope)
	}

	// Find the winner (exact match on value) or create a new resolution
	var winner *models.Memory
	var losers []models.Memory

	for i := range memories {
		if memories[i].Value == chosenValue {
			winner = &memories[i]
		} else {
			losers = append(losers, memories[i])
		}
	}

	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Archive all losers
	for _, l := range losers {
		_, err := tx.Exec(
			`UPDATE memories SET archived_at = ?, archived_by = ?, archived_reason = 'conflict_resolution', status = 'archived', `+closeValidityClause+` WHERE id = ?`,
			now, "conflict_resolution", now, now, l.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("archive loser: %w", err)
		}
	}

	if winner != nil {
		// Clear conflict flag on winner
		_, err := tx.Exec(
			`UPDATE memories SET conflict_with = NULL, updated_at = ? WHERE id = ?`,
			now, winner.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("clear conflict: %w", err)
		}
		winner.ConflictWith = nil
		winner.UpdatedAt = now
	} else {
		// Neither matched — create a new resolution memory, archive all
		for _, m := range memories {
			if m.ArchivedAt == nil { // not already archived above
				_, err := tx.Exec(
					`UPDATE memories SET archived_at = ?, archived_by = ?, archived_reason = 'conflict_resolution', status = 'archived', `+closeValidityClause+` WHERE id = ?`,
					now, "conflict_resolution", now, now, m.ID,
				)
				if err != nil {
					return nil, fmt.Errorf("archive for resolution: %w", err)
				}
			}
		}

		// Collect tags from the highest-version memory
		highestTags := memories[0].Tags
		highestVersion := memories[0].Version

		id := uuid.New().String()
		highestLayer := memories[0].Layer
		if highestLayer == "" {
			highestLayer = "behavior"
		}
		winner = &models.Memory{
			ID:         id,
			Key:        key,
			Value:      chosenValue,
			Tags:       highestTags,
			Scope:      scope,
			Project:    project,
			AgentName:  agentName,
			Confidence: "stated",
			Version:    highestVersion + 1,
			Supersedes: &memories[0].ID,
			CreatedAt:  now,
			UpdatedAt:  now,
			Layer:      highestLayer,
			ValidFrom:  &now,
			Status:     "live",
		}

		_, err := tx.Exec(
			`INSERT INTO memories (id, key, value, tags, scope, project, agent_name, confidence, version, supersedes, created_at, updated_at, layer, valid_from, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'live')`,
			winner.ID, winner.Key, winner.Value, winner.Tags, winner.Scope, winner.Project,
			winner.AgentName, winner.Confidence, winner.Version, winner.Supersedes,
			winner.CreatedAt, winner.UpdatedAt, winner.Layer, now,
		)
		if err != nil {
			return nil, fmt.Errorf("insert resolution: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return winner, nil
}

// GetMemoriesByLayer returns all active memories for an agent filtered by layer.
// Cross-scope: returns agent + project + global memories (same cascade as search).
func (d *DB) GetMemoriesByLayer(project, agentName, layer string) ([]models.Memory, error) {
	return d.queryMemories(
		fmt.Sprintf(`SELECT %s
		 FROM memories
		 WHERE archived_at IS NULL AND layer = ?
		   AND (scope = 'global' OR (project = ? AND (scope = 'project' OR (scope = 'agent' AND agent_name = ?))))
		 ORDER BY updated_at DESC
		 LIMIT 500`, memorySelectCols),
		layer, project, agentName,
	)
}

// ListAllMemories returns all active memories across projects (for web UI).
func (d *DB) ListAllMemories(limit int) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 200
	}
	return d.queryMemories(
		fmt.Sprintf(`SELECT %s
		 FROM memories WHERE archived_at IS NULL ORDER BY updated_at DESC LIMIT ?`, memorySelectCols),
		limit,
	)
}

// SearchAllMemories searches across all projects (for web UI).
func (d *DB) SearchAllMemories(query string, limit int) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 50
	}
	if query == "" {
		return d.ListAllMemories(limit)
	}
	return d.queryMemories(
		fmt.Sprintf(`SELECT %s
		 FROM memories m
		 JOIN memories_fts f ON m.rowid = f.rowid
		 WHERE m.archived_at IS NULL AND memories_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`, memorySelectColsAliased("m")),
		escapeFTSQuery(query), limit,
	)
}

// escapeFTSQuery wraps each token in double quotes so FTS5 does not interpret
// punctuation (especially hyphens) as column filters or operators. Empty tokens
// are skipped. A bare token stays a bare token, but `state-machine` becomes
// `"state-machine"` and is treated as a literal string to search.
func escapeFTSQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	// Fields() splits on any whitespace
	tokens := strings.Fields(q)
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		// Escape embedded double-quotes by doubling them (FTS5 convention)
		t = strings.ReplaceAll(t, `"`, `""`)
		out = append(out, `"`+t+`"`)
	}
	return strings.Join(out, " ")
}

// DeleteMemoryByID soft-deletes a specific memory by ID (for web UI). Writes a
// tombstone (archived_at/by/reason + status='archived'); optional reason
// overrides the default "deleted".
func (d *DB) DeleteMemoryByID(id, archivedBy string, reason ...string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	why := "deleted"
	if len(reason) > 0 && reason[0] != "" {
		why = reason[0]
	}
	res, err := d.writerExec(
		`UPDATE memories SET archived_at = ?, archived_by = ?, archived_reason = ?, status = 'archived' WHERE id = ? AND archived_at IS NULL`,
		now, archivedBy, why, id,
	)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory not found: %s", id)
	}
	return nil
}

// GetMemoryByID returns a single memory by ID (for web UI).
func (d *DB) GetMemoryByID(id string) (*models.Memory, error) {
	mems, err := d.queryMemories(
		fmt.Sprintf(`SELECT %s
		 FROM memories WHERE id = ?`, memorySelectCols), id,
	)
	if err != nil {
		return nil, err
	}
	if len(mems) == 0 {
		return nil, nil
	}
	return &mems[0], nil
}

// MemoryStats returns summary stats for the CLI/API.
func (d *DB) MemoryStats(project string) (total int, conflicts int, err error) {
	q := `SELECT COUNT(*), COALESCE(SUM(CASE WHEN conflict_with IS NOT NULL THEN 1 ELSE 0 END), 0) FROM memories WHERE archived_at IS NULL`
	var args []any
	if project != "" {
		q += " AND project = ?"
		args = append(args, project)
	}
	err = d.ro().QueryRow(q, args...).Scan(&total, &conflicts)
	return
}

// --- internal helpers ---

// memorySelectCols is the canonical column list every SELECT that feeds
// queryMemories MUST use, in this exact order. scanMemory reads them back in
// lockstep — a mismatch between this list and the Scan below silently drops
// every row, so the two are edited together, always. Callers that alias the
// table (e.g. "m") prepend the alias via memorySelectColsAliased.
const memorySelectCols = `id, key, value, tags, scope, project, agent_name, confidence, version,
	supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer,
	valid_from, valid_until, status, archived_reason`

// memorySelectColsAliased returns memorySelectCols with each column prefixed by
// the given table alias (used by the FTS join query which aliases memories as m).
func memorySelectColsAliased(alias string) string {
	cols := strings.Split(memorySelectCols, ",")
	for i, c := range cols {
		cols[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

// parseMemoryTime normalizes a caller-supplied timestamp to memoryTimeFmt so
// lexical comparisons against `now` stay correct regardless of the input's
// precision or timezone. Accepts the canonical form plus common ISO-8601
// layouts; returns ok=false if none parse.
func parseMemoryTime(s string) (string, bool) {
	for _, layout := range []string{
		memoryTimeFmt,
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(memoryTimeFmt), true
		}
	}
	return "", false
}

// effectiveStatus derives the status a read should report. Archival (a tombstone)
// always wins; otherwise a memory whose valid_until has passed is "stale" (still
// stored, still searchable), else it reports its stored status (normally "live").
func effectiveStatus(m *models.Memory, now string) string {
	if m.ArchivedAt != nil || m.Status == "archived" {
		return "archived"
	}
	if m.Status == "stale" {
		return "stale"
	}
	if m.ValidUntil != nil && *m.ValidUntil != "" && *m.ValidUntil <= now {
		return "stale"
	}
	return "live"
}

func (d *DB) queryMemories(query string, args ...any) ([]models.Memory, error) {
	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	now := time.Now().UTC().Format(memoryTimeFmt)
	var result []models.Memory
	for rows.Next() {
		m, err := scanMemory(rows, now)
		if err != nil {
			return nil, err
		}
		result = append(result, *m)
	}
	return result, rows.Err()
}

// scanMemory reads one memories row in lockstep with memorySelectCols and stamps
// the derived effective status. rowScanner is satisfied by both *sql.Rows and
// *sql.Row so the version-lookup path can share it.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanMemory(row rowScanner, now string) (*models.Memory, error) {
	var m models.Memory
	err := row.Scan(
		&m.ID, &m.Key, &m.Value, &m.Tags, &m.Scope, &m.Project,
		&m.AgentName, &m.Confidence, &m.Version,
		&m.Supersedes, &m.ConflictWith,
		&m.CreatedAt, &m.UpdatedAt, &m.ArchivedAt, &m.ArchivedBy, &m.Layer,
		&m.ValidFrom, &m.ValidUntil, &m.Status, &m.ArchivedReason,
	)
	if err != nil {
		return nil, fmt.Errorf("scan memory: %w", err)
	}
	m.Status = effectiveStatus(&m, now)
	m.Importance = scoreImportance(&m, now, activeImportanceWeights())
	return &m, nil
}

// ParseTags parses a JSON array of strings, returning the tags slice.
func ParseTags(tagsJSON string) []string {
	if tagsJSON == "" || tagsJSON == "[]" {
		return nil
	}
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	return tags
}

// TagsToJSON converts a string slice to a JSON array string.
func TagsToJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(tags)
	return string(b)
}
