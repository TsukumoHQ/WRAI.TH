package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Knowledge coherence protocol, slice T1 (design e731f3c9, ruling 0019d6f5).
// A breaking, narrowing or retraction knowledge_log row that replaced a
// version opens a rollout, and each ACTIVE consumer of the old version gets
// one reassess obligation. Advisory only: nothing is refused. Obligations die
// by their maintenance condition (subject active AND new version current),
// a re-boot onto the new state or a lease expiry counts as an ack, and an
// unfulfilled reassess escalates to a supervisor agent and stops there —
// never the human.
//
// T1 consumers (ruling: A1 claim-basis consumers land with T2):
//   A2 leased task citing the key → its assignee
//   A3 pending task citing the key → its dispatcher (confirm / rewrite / cancel)
//   A4 live agent whose context head (or a recall on it) holds the old version

const (
	NormCoherenceReassess     = "coherence.reassess"
	NormCoherenceReassessRole = "coherence.reassess_role"

	SubjectKnowledgeTask    = "knowledge_task"
	SubjectKnowledgeSession = "knowledge_session"

	// SettingCoherenceMode is off | advisory (default) | enforce (cto only;
	// enforce is read by T2's claim fence).
	SettingCoherenceMode  = "coherence_mode"
	CoherenceModeOff      = "off"
	CoherenceModeAdvisory = "advisory"

	settingCoherenceCursor = "coherence_cursor_rev"
	coherenceBatch         = 50
	coherenceFanoutCap     = 200
	coherenceMinCiteKeyLen = 6 // shorter keys would match task prose by accident

	RolloutPreparing           = "preparing"
	RolloutCommitted           = "committed"
	RolloutCommittedWithOrphan = "committed_with_orphans"
	RolloutSuperseded          = "superseded"
)

func migrateCoherence(conn *sql.DB) {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS knowledge_rollouts (
		id              TEXT PRIMARY KEY,
		project         TEXT NOT NULL,
		rev             INTEGER NOT NULL UNIQUE,
		scope           TEXT NOT NULL,
		key             TEXT NOT NULL,
		layer           TEXT NOT NULL,
		change_class    TEXT NOT NULL,
		old_memory_id   TEXT NOT NULL,
		new_memory_id   TEXT,
		gate            TEXT NOT NULL,
		state           TEXT NOT NULL,
		affected        INTEGER NOT NULL,
		acked_by_expiry INTEGER NOT NULL DEFAULT 0,
		fanout_capped   INTEGER NOT NULL DEFAULT 0,
		created_at      TEXT NOT NULL,
		closed_at       TEXT
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_rollouts_open ON knowledge_rollouts(state, project) WHERE state IN ('preparing', 'contested')`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_rollouts_key ON knowledge_rollouts(project, scope, key, rev)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_context_heads_set ON context_heads(set_hash)`)
	_, _ = conn.Exec(`INSERT OR IGNORE INTO norms (id, subject_kind, trigger, what, while_pred, deadline_kind, deadline_anchor,
		deadline_setting, deadline_default_s, deadline_min_s, deadline_max_s, bearer_kind, sanction, sanction_template,
		on_unfulfilled, max_depth, eval_order)
		VALUES
		('coherence.reassess', 'knowledge', 'knowledge_change', 'knowledge_reassessed', 'consumer_active_and_key_current',
		 'time', 'created_at', 'coherence_reassess_age', 86400, 900, 259200, 'consumer', 'open_child',
		 'Knowledge %s changed (%s, rev %d): reassess your work against it, then obligation_discharge %s.',
		 'coherence.reassess_role', 1, 0),
		('coherence.reassess_role', 'knowledge', 'knowledge_change', 'knowledge_reassessed', 'consumer_active_and_key_current',
		 'time', 'created_at', 'coherence_role_age', 86400, 900, 259200, 'supervisor', 'message',
		 '%s did not reassess against the change of %s (rev %d) in time; you are its supervisor: obligation %s.',
		 NULL, 1, 1)`)
	// Start from the current head: history written before this slice is not
	// replayed into obligations (the log started empty at S1 anyway).
	_, _ = conn.Exec(`INSERT OR IGNORE INTO settings (key, value)
		SELECT ?, CAST(COALESCE(MAX(rev), 0) AS TEXT) FROM knowledge_log`, settingCoherenceCursor)
}

// knowledgeChange is one knowledge_log row the sweeper reads.
type knowledgeChange struct {
	Rev                                                      int64
	Project, Scope, Key, Author, MemoryID, PrevID, Op, Layer string
	Class, Agent                                             string
}

// opensRollout: only a breaking / narrowing / retraction change that replaced
// a version has consumers to reassess.
func (c knowledgeChange) opensRollout() bool {
	switch c.Class {
	case ChangeBreaking, ChangeNarrowing, ChangeRetraction:
		return c.PrevID != ""
	}
	return false
}

// CoherenceNotice is one message the relay layer sends after a tick.
type CoherenceNotice struct {
	Project, To, Subject, Body, ObligationID string
}

// CoherenceReport is what one tick did.
type CoherenceReport struct {
	Rollouts  int
	Opened    int
	Closed    int
	Committed int
	Notices   []CoherenceNotice
}

// EvaluateCoherence is one sweeper tick: open rollouts for new log rows, close
// moot / acked / expired obligations, escalate breached ones one rung, commit
// finished rollouts.
func (d *DB) EvaluateCoherence(now time.Time) (CoherenceReport, error) {
	var rep CoherenceReport
	if err := d.openRollouts(now, &rep); err != nil {
		return rep, err
	}
	if err := d.closeCoherence(now, &rep); err != nil {
		return rep, err
	}
	n, err := d.commitRollouts(now)
	rep.Committed = n
	return rep, err
}

func (d *DB) coherenceCursor() string {
	c := d.GetSetting(settingCoherenceCursor)
	if c == "" {
		return "0"
	}
	return c
}

func (d *DB) openRollouts(now time.Time, rep *CoherenceReport) error {
	cursor := d.coherenceCursor()
	from, _ := strconv.ParseInt(cursor, 10, 64)
	rows, err := d.ro().Query(`SELECT rev, project, scope, key, author, COALESCE(memory_id, ''), COALESCE(prev_memory_id, ''),
			op, layer, change_class, agent
		FROM knowledge_log WHERE rev > ? ORDER BY rev LIMIT ?`, from, coherenceBatch)
	if err != nil {
		return fmt.Errorf("coherence scan: %w", err)
	}
	var changes []knowledgeChange
	for rows.Next() {
		var c knowledgeChange
		if err := rows.Scan(&c.Rev, &c.Project, &c.Scope, &c.Key, &c.Author, &c.MemoryID, &c.PrevID, &c.Op, &c.Layer, &c.Class, &c.Agent); err != nil {
			_ = rows.Close()
			return err
		}
		changes = append(changes, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, c := range changes {
		if !c.opensRollout() {
			// Batch the cursor over irrelevant rows: one write at the end of the
			// run, or right before the next rollout.
			if i == len(changes)-1 {
				if ok, err := d.advanceCursor(cursor, c.Rev); err != nil || !ok {
					return err
				}
			}
			continue
		}
		opened, ok, err := d.openRollout(c, cursor, now, rep)
		if err != nil {
			return err
		}
		if !ok {
			return nil // another tick owns this range
		}
		rep.Rollouts++
		rep.Opened += opened
		cursor = strconv.FormatInt(c.Rev, 10)
	}
	return nil
}

func (d *DB) advanceCursor(prev string, to int64) (bool, error) {
	res, err := d.writerExec(`UPDATE settings SET value = ? WHERE key = ? AND value = ?`,
		strconv.FormatInt(to, 10), settingCoherenceCursor, prev)
	if err != nil {
		return false, fmt.Errorf("coherence cursor: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// consumer is one bearer to oblige.
type consumer struct {
	Project, SubjectKind, SubjectID, BearerKind, Bearer, Via, TaskPriority string
}

// affectedSet computes A2/A3/A4 for a change (design §3), read-only. capped
// reports whether pending tasks and sessions were dropped by the fan-out cap.
func (d *DB) affectedSet(c knowledgeChange) (out []consumer, capped bool, err error) {
	projectFilter := ` AND t.project = ?`
	args := []any{}
	if c.Project != "*" {
		args = append(args, c.Project)
	} else {
		projectFilter = ``
	}
	var leased, pending []consumer
	taskAgents := map[string]bool{}
	if len(c.Key) >= coherenceMinCiteKeyLen {
		q := `SELECT t.id, t.project, t.status, COALESCE(t.assigned_to, ''), t.dispatched_by, t.priority FROM tasks t
			WHERE t.archived_at IS NULL AND t.status IN ('pending', 'accepted', 'in-progress', 'in-review', 'blocked')` + projectFilter + `
			  AND instr(t.title || ' ' || COALESCE(t.description, '') || ' ' || COALESCE(t.goal, '') || ' ' ||
			            COALESCE(t.acceptance_criteria, ''), ?) > 0`
		rows, err := d.ro().Query(q, append(args, c.Key)...)
		if err != nil {
			return nil, false, fmt.Errorf("cited tasks: %w", err)
		}
		for rows.Next() {
			var id, project, status, assignee, dispatcher, prio string
			if err := rows.Scan(&id, &project, &status, &assignee, &dispatcher, &prio); err != nil {
				_ = rows.Close()
				return nil, false, err
			}
			if status == "pending" {
				if d.IsLiveAgent(project, dispatcher) && !strings.EqualFold(dispatcher, c.Agent) {
					pending = append(pending, consumer{project, SubjectKnowledgeTask, id, "dispatcher", dispatcher, "cited_pending", prio})
				}
				continue
			}
			if assignee != "" && !strings.EqualFold(assignee, c.Agent) {
				leased = append(leased, consumer{project, SubjectKnowledgeTask, id, "consumer", assignee, "cited_leased", prio})
				taskAgents[project+"/"+strings.ToLower(assignee)] = true
			}
		}
		_ = rows.Close()
	}
	holders, err := d.holdersOf(c.PrevID)
	if err != nil {
		return nil, false, err
	}
	var sessions []consumer
	for _, h := range holders {
		if c.Project != "*" && h.Project != c.Project {
			continue
		}
		key := h.Project + "/" + strings.ToLower(h.Agent)
		if taskAgents[key] || strings.EqualFold(h.Agent, c.Agent) || !h.Active {
			continue
		}
		sessions = append(sessions, consumer{h.Project, SubjectKnowledgeSession, key, "consumer", h.Agent, "context_" + h.Via, ""})
	}
	out = append(out, leased...)
	if len(leased)+len(pending)+len(sessions) > coherenceFanoutCap {
		return out, true, nil
	}
	out = append(out, pending...)
	out = append(out, sessions...)
	return out, false, nil
}

// holdersOf lists agents whose head (or a recall extending it, or a head-less
// recall) holds memoryID. One row per agent.
func (d *DB) holdersOf(memoryID string) ([]Consumer, error) {
	rows, err := d.ro().Query(`SELECT DISTINCT s.project, s.agent_name, s.kind, COALESCE(a.status, '')
		FROM consumption_edges e
		JOIN context_snapshots s ON s.set_hash = e.set_hash
		LEFT JOIN context_heads h ON h.project = s.project AND h.agent_name = s.agent_name
		LEFT JOIN agents a ON a.project = s.project AND a.name = s.agent_name
		WHERE e.memory_id = ?
		  AND (s.id = h.snapshot_id
		       OR (s.kind = 'recall' AND ((h.snapshot_id IS NOT NULL AND s.parent_id = h.snapshot_id)
		                                  OR (h.snapshot_id IS NULL AND s.parent_id IS NULL))))`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("holders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	var out []Consumer
	for rows.Next() {
		var c Consumer
		var kind, status string
		if err := rows.Scan(&c.Project, &c.Agent, &kind, &status); err != nil {
			return nil, err
		}
		k := c.Project + "/" + c.Agent
		if seen[k] {
			continue
		}
		seen[k] = true
		c.Active = status == "active"
		c.Via = SnapshotBoot
		if kind == SnapshotRecall {
			c.Via = SnapshotRecall
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) reassessAge(class string) time.Duration {
	if class == ChangeBreaking {
		return d.SettingDuration("coherence_reassess_breaking_age", 4*time.Hour, 15*time.Minute, 72*time.Hour)
	}
	return d.SettingDuration("coherence_reassess_age", 24*time.Hour, 15*time.Minute, 72*time.Hour)
}

// openRollout writes one rollout, its obligations, the supersession of older
// rollouts of the key and the cursor CAS, in one writer tx. ok=false: the
// cursor moved (another tick won) and nothing was written.
func (d *DB) openRollout(c knowledgeChange, prevCursor string, now time.Time, rep *CoherenceReport) (int, bool, error) {
	affected, capped, err := d.affectedSet(c)
	if err != nil {
		return 0, false, err
	}
	ts := now.UTC().Format(memoryTimeFmt)
	deadline := now.Add(d.reassessAge(c.Class)).UTC().Format(memoryTimeFmt)
	id := uuid.New().String()
	var version int
	var template string
	if err := d.ro().QueryRow(`SELECT version, sanction_template FROM norms WHERE id = ?`, NormCoherenceReassess).Scan(&version, &template); err != nil {
		return 0, false, fmt.Errorf("reassess norm: %w", err)
	}
	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, false, fmt.Errorf("rollout begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE settings SET value = ? WHERE key = ? AND value = ?`, strconv.FormatInt(c.Rev, 10), settingCoherenceCursor, prevCursor)
	if err != nil {
		return 0, false, fmt.Errorf("rollout cursor: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	// Older open rollouts of the key are superseded: consumers reassess only
	// against the newest version.
	if _, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?,
			discharge_evidence = json_set(COALESCE(discharge_evidence, '{}'), '$.moot', 'superseded')
		WHERE state = ? AND norm_id LIKE 'coherence.%' AND json_extract(discharge_evidence, '$.rollout_id') IN (
			SELECT id FROM knowledge_rollouts WHERE project = ? AND scope = ? AND key = ? AND state IN ('preparing', 'contested'))`,
		ObligationInactive, ts, ObligationActive, c.Project, c.Scope, c.Key); err != nil {
		return 0, false, fmt.Errorf("supersede obligations: %w", err)
	}
	if _, err := tx.Exec(`UPDATE knowledge_rollouts SET state = ?, closed_at = ? WHERE project = ? AND scope = ? AND key = ?
		AND state IN ('preparing', 'contested')`, RolloutSuperseded, ts, c.Project, c.Scope, c.Key); err != nil {
		return 0, false, fmt.Errorf("supersede rollouts: %w", err)
	}
	var newID any
	if c.MemoryID != "" {
		newID = c.MemoryID
	}
	gate := "advisory"
	if c.Class == ChangeBreaking && c.Layer == "constraints" {
		gate = "block_claims" // honoured only by T2's fence under coherence_mode=enforce
	}
	cappedFlag := 0
	if capped {
		cappedFlag = 1
	}
	if _, err := tx.Exec(`INSERT INTO knowledge_rollouts (id, project, rev, scope, key, layer, change_class, old_memory_id,
			new_memory_id, gate, state, affected, fanout_capped, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, c.Project, c.Rev, c.Scope, c.Key, c.Layer, c.Class, c.PrevID, newID, gate, RolloutPreparing, len(affected), cappedFlag, ts); err != nil {
		return 0, false, fmt.Errorf("rollout insert: %w", err)
	}
	opened := 0
	for _, a := range affected {
		oid := uuid.New().String()
		ev, _ := json.Marshal(map[string]any{"rollout_id": id, "key": c.Key, "rev": c.Rev, "via": a.Via, "priority": a.TaskPriority})
		res, err := tx.Exec(`INSERT OR IGNORE INTO obligations
			(id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state,
			 created_at, deadline_at, escalation_depth, discharge_evidence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			oid, a.Project, NormCoherenceReassess, version, bindingsHash(NormCoherenceReassess, a.SubjectKind, id+"|"+a.SubjectID, 0),
			a.SubjectKind, a.SubjectID, a.BearerKind, strings.ToLower(a.Bearer), ObligationActive, ts, deadline, string(ev))
		if err != nil {
			return 0, false, fmt.Errorf("reassess obligation: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			opened++
			rep.Notices = append(rep.Notices, CoherenceNotice{Project: a.Project, To: a.Bearer, ObligationID: oid,
				Subject: fmt.Sprintf("reassess: %s changed (%s)", c.Key, c.Class),
				Body:    fmt.Sprintf(template, c.Key, c.Class, c.Rev, oid)})
		}
	}
	if capped {
		if _, err := openExceptionTx(tx, exceptionOpen{Project: c.Project, SourceKind: "coherence_fanout", SourceRef: id,
			RaisedBy: excSweeper, Text: "coherence fan-out over cap for " + c.Key,
			Code: "coherence_fanout", Kind: "coherence_fanout", Retry: "non_retryable", At: ts}); err != nil {
			return 0, false, fmt.Errorf("fanout exception: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("rollout commit: %w", err)
	}
	return opened, true, nil
}

// activeCoherence is one active coherence obligation with its rollout.
type activeCoherence struct {
	ID, Project, Norm, SubjectKind, SubjectID, Bearer, Deadline, Evidence string
	Depth                                                                 int
	RolloutID, RolloutState, Key, NewID, OldID, Class                     string
	Rev                                                                   int64
}

func (d *DB) activeCoherence() ([]activeCoherence, error) {
	rows, err := d.ro().Query(`SELECT o.id, o.project, o.norm_id, o.subject_kind, o.subject_id, o.bearer,
			COALESCE(o.deadline_at, ''), COALESCE(o.discharge_evidence, '{}'), o.escalation_depth,
			r.id, r.state, r.key, COALESCE(r.new_memory_id, ''), r.old_memory_id, r.change_class, r.rev
		FROM obligations o JOIN knowledge_rollouts r ON r.id = json_extract(o.discharge_evidence, '$.rollout_id')
		WHERE o.state = ? AND o.norm_id IN (?, ?)`, ObligationActive, NormCoherenceReassess, NormCoherenceReassessRole)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []activeCoherence
	for rows.Next() {
		var a activeCoherence
		if err := rows.Scan(&a.ID, &a.Project, &a.Norm, &a.SubjectKind, &a.SubjectID, &a.Bearer, &a.Deadline, &a.Evidence,
			&a.Depth, &a.RolloutID, &a.RolloutState, &a.Key, &a.NewID, &a.OldID, &a.Class, &a.Rev); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// closeCoherenceTx closes one active obligation with state + evidence (CAS).
func (d *DB) closeCoherenceObligation(a activeCoherence, state string, add map[string]any, now time.Time) (bool, error) {
	ev := map[string]any{}
	_ = json.Unmarshal([]byte(a.Evidence), &ev)
	for k, v := range add {
		ev[k] = v
	}
	raw, _ := json.Marshal(ev)
	ts := now.UTC().Format(memoryTimeFmt)
	res, err := d.writerExec(`UPDATE obligations SET state = ?, closed_at = ?, done_at = CASE WHEN ? = 'fulfilled' THEN ? ELSE done_at END,
		discharge_evidence = ? WHERE id = ? AND state = ?`, state, ts, state, ts, string(raw), a.ID, ObligationActive)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// closeCoherence applies the maintenance condition, the acks and the deadline
// sanction to every active coherence obligation.
func (d *DB) closeCoherence(now time.Time, rep *CoherenceReport) error {
	obs, err := d.activeCoherence()
	if err != nil {
		return fmt.Errorf("coherence obligations: %w", err)
	}
	ts := now.UTC().Format(memoryTimeFmt)
	for _, a := range obs {
		state, add := d.coherenceMoot(a)
		if state == "" && a.Deadline != "" && a.Deadline < ts {
			if err := d.breachCoherence(a, now, rep); err != nil {
				return err
			}
			rep.Closed++
			continue
		}
		if state == "" {
			continue
		}
		ok, err := d.closeCoherenceObligation(a, state, add, now)
		if err != nil {
			return fmt.Errorf("close coherence %s: %w", a.ID, err)
		}
		if ok {
			rep.Closed++
			if add["acked_by"] == "lease_expiry" {
				_, _ = d.writerExec(`UPDATE knowledge_rollouts SET acked_by_expiry = acked_by_expiry + 1 WHERE id = ?`, a.RolloutID)
			}
		}
	}
	return nil
}

// coherenceMoot evaluates the maintenance condition and the acks. "" means
// the obligation stays active.
func (d *DB) coherenceMoot(a activeCoherence) (string, map[string]any) {
	if a.RolloutState == RolloutSuperseded {
		return ObligationInactive, map[string]any{"moot": "superseded"}
	}
	if a.NewID != "" {
		var archived sql.NullString
		if err := d.ro().QueryRow(`SELECT archived_at FROM memories WHERE id = ?`, a.NewID).Scan(&archived); err == nil && archived.Valid {
			return ObligationInactive, map[string]any{"moot": "superseded"}
		}
	}
	switch a.SubjectKind {
	case SubjectKnowledgeTask:
		var status string
		var archived sql.NullString
		err := d.ro().QueryRow(`SELECT status, archived_at FROM tasks WHERE id = ?`, a.SubjectID).Scan(&status, &archived)
		if errors.Is(err, sql.ErrNoRows) || archived.Valid || status == "done" || status == "cancelled" {
			return ObligationInactive, map[string]any{"moot": "task_done"}
		}
	case SubjectKnowledgeSession:
		project, agent, _ := strings.Cut(a.SubjectID, "/")
		if !d.IsLiveAgent(project, agent) {
			return ObligationInactive, map[string]any{"acked_by": "lease_expiry"}
		}
		if a.Depth == 0 && !d.holds(project, agent, a.OldID) {
			return ObligationFulfilled, map[string]any{"acked_by": "reboot"}
		}
	}
	return "", nil
}

// holds reports whether agent's current head (or a recall on it) still holds id.
func (d *DB) holds(project, agent, memoryID string) bool {
	hs, err := d.holdersOf(memoryID)
	if err != nil {
		return true // unknown: keep the obligation
	}
	for _, h := range hs {
		if h.Project == project && strings.EqualFold(h.Agent, agent) {
			return true
		}
	}
	return false
}

// breachCoherence closes a past-deadline obligation unfulfilled and, at depth
// 0, opens the role rung on a live supervisor. The chain ends there: a role
// breach is an orphan (committed_with_orphans), never the human. A P0/P1
// task orphan also writes an exception (escalation only via class budgets).
func (d *DB) breachCoherence(a activeCoherence, now time.Time, rep *CoherenceReport) error {
	ts := now.UTC().Format(memoryTimeFmt)
	var supervisor string
	if a.Depth == 0 {
		supervisor = d.LiveSupervisor(a.Project, a.Bearer)
		if strings.EqualFold(supervisor, a.Bearer) {
			supervisor = ""
		}
	}
	var prio string
	if a.SubjectKind == SubjectKnowledgeTask {
		_ = d.ro().QueryRow(`SELECT priority FROM tasks WHERE id = ?`, a.SubjectID).Scan(&prio)
	}
	var version int
	var template string
	_ = d.ro().QueryRow(`SELECT version, sanction_template FROM norms WHERE id = ?`, NormCoherenceReassessRole).Scan(&version, &template)
	tx, err := d.beginWriterTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	ev := map[string]any{}
	_ = json.Unmarshal([]byte(a.Evidence), &ev)
	ev["outcome"] = "deadline"
	raw, _ := json.Marshal(ev)
	res, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = ? WHERE id = ? AND state = ?`,
		ObligationUnfulfilled, ts, string(raw), a.ID, ObligationActive)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if supervisor != "" {
		oid := uuid.New().String()
		cev, _ := json.Marshal(map[string]any{"rollout_id": a.RolloutID, "key": a.Key, "rev": a.Rev, "consumer": a.Bearer})
		res, err := tx.Exec(`INSERT OR IGNORE INTO obligations
			(id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state,
			 created_at, deadline_at, escalation_depth, parent_obligation_id, discharge_evidence)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'supervisor', ?, ?, ?, ?, 1, ?, ?)`,
			oid, a.Project, NormCoherenceReassessRole, version,
			bindingsHash(NormCoherenceReassessRole, a.SubjectKind, a.RolloutID+"|"+a.SubjectID, 1),
			a.SubjectKind, a.SubjectID, strings.ToLower(supervisor), ObligationActive, ts,
			now.Add(d.SettingDuration("coherence_role_age", 24*time.Hour, 15*time.Minute, 72*time.Hour)).UTC().Format(memoryTimeFmt),
			a.ID, string(cev))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 && template != "" {
			rep.Notices = append(rep.Notices, CoherenceNotice{Project: a.Project, To: supervisor, ObligationID: oid,
				Subject: fmt.Sprintf("reassess overdue: %s (%s)", a.Key, a.Bearer),
				Body:    fmt.Sprintf(template, a.Bearer, a.Key, a.Rev, oid)})
		}
	} else if a.SubjectKind == SubjectKnowledgeTask && (prio == "P0" || prio == "P1") {
		if _, err := openExceptionTx(tx, exceptionOpen{Project: a.Project, SourceKind: "coherence_orphan", SourceRef: a.ID,
			RaisedBy: excSweeper, TaskID: a.SubjectID, Text: "reassess orphaned for " + a.Key,
			Code: "coherence_orphan", Kind: "coherence", Retry: "non_retryable", At: ts}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// commitRollouts closes preparing rollouts with no active obligation left.
func (d *DB) commitRollouts(now time.Time) (int, error) {
	rows, err := d.ro().Query(`SELECT r.id,
			EXISTS (SELECT 1 FROM obligations o WHERE json_extract(o.discharge_evidence, '$.rollout_id') = r.id
			        AND o.norm_id LIKE 'coherence.%' AND o.state = 'unfulfilled'
			        AND NOT EXISTS (SELECT 1 FROM obligations c WHERE c.parent_obligation_id = o.id))
		FROM knowledge_rollouts r
		WHERE r.state = ? AND NOT EXISTS (SELECT 1 FROM obligations o WHERE json_extract(o.discharge_evidence, '$.rollout_id') = r.id
		        AND o.norm_id LIKE 'coherence.%' AND o.state = 'active')`, RolloutPreparing)
	if err != nil {
		return 0, err
	}
	type done struct {
		id      string
		orphans bool
	}
	var todo []done
	for rows.Next() {
		var x done
		if err := rows.Scan(&x.id, &x.orphans); err != nil {
			_ = rows.Close()
			return 0, err
		}
		todo = append(todo, x)
	}
	_ = rows.Close()
	ts := now.UTC().Format(memoryTimeFmt)
	n := 0
	for _, x := range todo {
		state := RolloutCommitted
		if x.orphans {
			state = RolloutCommittedWithOrphan
		}
		res, err := d.writerExec(`UPDATE knowledge_rollouts SET state = ?, closed_at = ? WHERE id = ? AND state = ?`, state, ts, x.id, RolloutPreparing)
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c == 1 {
			n++
		}
	}
	return n, nil
}

// DischargeReassess fulfils a coherence obligation as prepared. At depth 0 the
// relay re-checks that the bearer was served the new version after the
// rollout opened (a boot head or a recall); a retraction needs no read.
// reason "unknown obligation" when id is not a coherence obligation.
func (d *DB) DischargeReassess(project, id, by, evidence string, now time.Time) (bool, string, error) {
	var bearer, state, norm, rolloutID, rolloutAt, newID, key string
	var depth int
	err := d.ro().QueryRow(`SELECT o.bearer, o.state, o.norm_id, o.escalation_depth, r.id, r.created_at,
			COALESCE(r.new_memory_id, ''), r.key
		FROM obligations o JOIN knowledge_rollouts r ON r.id = json_extract(o.discharge_evidence, '$.rollout_id')
		WHERE o.id = ? AND o.project = ? AND o.norm_id IN (?, ?)`, id, project, NormCoherenceReassess, NormCoherenceReassessRole).
		Scan(&bearer, &state, &norm, &depth, &rolloutID, &rolloutAt, &newID, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "unknown obligation", nil
	}
	if err != nil {
		return false, "", err
	}
	if state != ObligationActive {
		return false, "obligation is " + state + ", not active", nil
	}
	if !strings.EqualFold(bearer, by) {
		return false, "only the obligation's bearer can discharge it", nil
	}
	if depth == 0 && newID != "" {
		var served int
		_ = d.ro().QueryRow(`SELECT COUNT(*) FROM context_snapshots s JOIN consumption_edges e ON e.set_hash = s.set_hash
			WHERE s.project = ? AND lower(s.agent_name) = lower(?) AND e.memory_id = ? AND s.created_at >= ?`,
			project, by, newID, rolloutAt).Scan(&served)
		if served == 0 {
			return false, fmt.Sprintf("read the new version first: get_memory(%q), then discharge again", key), nil
		}
	}
	a := activeCoherence{ID: id, Evidence: "{}"}
	_ = d.ro().QueryRow(`SELECT COALESCE(discharge_evidence, '{}') FROM obligations WHERE id = ?`, id).Scan(&a.Evidence)
	ok, err := d.closeCoherenceObligation(a, ObligationFulfilled, map[string]any{"verdict": "prepared", "by": by, "evidence": evidence}, now)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "obligation moved concurrently", nil
	}
	return true, "", nil
}

// MyCoherenceObligations lists the caller's active coherence obligations.
func (d *DB) MyCoherenceObligations(project, agent string) ([]ObligationView, error) {
	rows, err := d.ro().Query(`SELECT o.id, o.norm_id, o.subject_kind, o.subject_id, o.bearer_kind, o.escalation_depth,
			COALESCE(o.deadline_at, ''), r.key, r.change_class, r.rev
		FROM obligations o JOIN knowledge_rollouts r ON r.id = json_extract(o.discharge_evidence, '$.rollout_id')
		WHERE o.project = ? AND o.state = ? AND o.bearer = ? AND o.norm_id IN (?, ?)
		ORDER BY o.created_at`, project, ObligationActive, strings.ToLower(agent), NormCoherenceReassess, NormCoherenceReassessRole)
	if err != nil {
		return nil, fmt.Errorf("my coherence obligations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ObligationView
	for rows.Next() {
		var v ObligationView
		var key, class string
		var rev int64
		if err := rows.Scan(&v.ID, &v.Norm, &v.SubjectKind, &v.SubjectID, &v.BearerKind, &v.Depth, &v.Deadline, &key, &class, &rev); err != nil {
			return nil, err
		}
		v.What = "knowledge_reassessed"
		v.Subject = fmt.Sprintf("%s changed (%s, rev %d)", key, class, rev)
		out = append(out, v)
	}
	return out, rows.Err()
}
