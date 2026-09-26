package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agent-relay/internal/models"

	"github.com/google/uuid"
)

// Escalation ladder, slice T2 (design 1111292b §4-§6, ruling af6e3a5f). An
// open systemic exception walks a ladder of rungs frozen at start. Each rung
// IS an obligation (norm exc.<rung>, subject 'exception', escalation_depth =
// rung index), chained by parent_obligation_id; the cursor (exceptions.rung)
// moves only by CAS in the same tx as the obligation transition. A rung
// requests work (a task or a message); it never does the work, and no rung
// re-runs a gate round. The human is the last rung and nothing else.

// Rung names (design §4.1).
const (
	RungMatchPrecedent    = "match_precedent"
	RungConsultKnowledge  = "consult_knowledge"
	RungAskSource         = "ask_source"
	RungRouteSpecialist   = "route_specialist"
	RungAdversarialReview = "adversarial_review"
	RungReversibleAction  = "reversible_action"
	RungWaitIfVolume      = "wait_if_volume"
	RungSupervisorAgent   = "supervisor_agent"
	RungHuman             = "human"
)

// ErrLadderInvalid is returned when a class's ladder fails validation; the
// class_budgets row is disabled and the systemic never starts a ladder.
var ErrLadderInvalid = errors.New("invalid escalation ladder")

// defaultLadder is every budgeted kind's ladder (ruling: adversarial_review,
// reversible_action and wait_if_volume are opt-in per code row).
var defaultLadder = []string{RungMatchPrecedent, RungConsultKnowledge, RungAskSource, RungRouteSpecialist, RungSupervisorAgent, RungHuman}

// deniedActionTools can never be named by reversible_action: retrying a gate
// round is Niwa's job (design §5).
var deniedActionTools = map[string]bool{
	"qa-submit": true, "qa_submit": true, "qa": true, "merge": true, "gate": true, "resubmit": true, "review_task": true,
}

// LadderRung is one frozen rung of a snapshot.
type LadderRung struct {
	Rung        string            `json:"rung"`
	NormID      string            `json:"norm_id"`
	NormVersion int               `json:"norm_version"`
	Params      map[string]string `json:"params,omitempty"`
	DeadlineS   int               `json:"deadline_s"`
}

// LadderSnapshot is exceptions.ladder_snapshot_json.
type LadderSnapshot struct {
	Rungs              []LadderRung `json:"rungs"`
	Source             string       `json:"source"` // default | kind | code
	Trims              []string     `json:"trims,omitempty"`
	UpstreamAttempts   int          `json:"upstream_attempts"`
	ReviewerProfiles   []string     `json:"reviewer_profiles,omitempty"`
	TotalAttemptBudget int          `json:"total_attempt_budget"`
	Board              string       `json:"board,omitempty"`
}

// Index returns the position of rung name in the snapshot (-1 if absent).
func (s LadderSnapshot) Index(name string) int {
	for i, r := range s.Rungs {
		if r.Rung == name {
			return i
		}
	}
	return -1
}

type ladderSpecRung struct {
	Rung   string            `json:"rung"`
	Params map[string]string `json:"params,omitempty"`
}

// ValidateLadder enforces the design §4.2 rules: known rungs, human last and
// exactly once, adversarial_review right after route_specialist,
// reversible_action only on a reversible class and never naming a gate tool.
func ValidateLadder(rungs []LadderRung, reversibility string) error {
	if len(rungs) == 0 {
		return fmt.Errorf("%w: empty", ErrLadderInvalid)
	}
	known := map[string]bool{}
	for _, r := range excLadderRungs {
		known[r.rung] = true
	}
	humans := 0
	for i, r := range rungs {
		if !known[r.Rung] {
			return fmt.Errorf("%w: unknown rung %q", ErrLadderInvalid, r.Rung)
		}
		switch r.Rung {
		case RungHuman:
			humans++
			if i != len(rungs)-1 {
				return fmt.Errorf("%w: human must be the last rung", ErrLadderInvalid)
			}
		case RungAdversarialReview:
			if i == 0 || rungs[i-1].Rung != RungRouteSpecialist {
				return fmt.Errorf("%w: adversarial_review must follow route_specialist", ErrLadderInvalid)
			}
		case RungReversibleAction:
			if reversibility != "reversible" && reversibility != "compensable" {
				return fmt.Errorf("%w: reversible_action on a class with reversibility %q", ErrLadderInvalid, reversibility)
			}
			tool := strings.ToLower(strings.TrimSpace(r.Params["tool"]))
			if tool == "" || r.Params["compensate"] == "" {
				return fmt.Errorf("%w: reversible_action needs tool and compensate", ErrLadderInvalid)
			}
			if deniedActionTools[tool] {
				return fmt.Errorf("%w: reversible_action may not run gate tool %q", ErrLadderInvalid, tool)
			}
		}
	}
	if humans != 1 {
		return fmt.Errorf("%w: exactly one human rung required", ErrLadderInvalid)
	}
	return nil
}

// SystemicRef is an open systemic exception.
type SystemicRef struct {
	ID, Project, Kind, ReasonCode, Owner, EvidenceJSON, OpenedAt string
}

// PendingSystemics lists open systemic exceptions whose ladder has not started.
func (d *DB) PendingSystemics() ([]SystemicRef, error) {
	rows, err := d.ro().Query(`SELECT e.id, e.project, COALESCE(c.kind, ''), e.reason_code, COALESCE(e.owner, ''),
			COALESCE(e.evidence_json, '{}'), e.opened_at
		FROM exceptions e LEFT JOIN exception_classes c ON c.id = e.class_id
		WHERE e.source_kind = ? AND e.status = 'open' AND e.rung IS NULL ORDER BY e.opened_at`, excSourceClassBudget)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SystemicRef
	for rows.Next() {
		var s SystemicRef
		if err := rows.Scan(&s.ID, &s.Project, &s.Kind, &s.ReasonCode, &s.Owner, &s.EvidenceJSON, &s.OpenedAt); err != nil {
			return nil, err
		}
		// The systemic class row carries kind 'systemic'; the budgeted kind is
		// the instances' kind.
		_ = d.ro().QueryRow(`SELECT kind FROM exceptions WHERE cause_id = ? LIMIT 1`, s.ID).Scan(&s.Kind)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ladderSpec resolves the class's ladder: the (kind, code) row's ladder_json,
// else the (kind, '*') row's, else the default. Returns which row it used.
func (d *DB) ladderSpec(kind, code string) (rungs []ladderSpecRung, rowCode, reversibility string, budget int, source string, err error) {
	type row struct {
		ladder        sql.NullString
		reversibility string
		budget        int
	}
	read := func(c string) (*row, error) {
		var r row
		err := d.ro().QueryRow(`SELECT ladder_json, reversibility, total_attempt_budget FROM class_budgets WHERE kind = ? AND reason_code = ?`,
			kind, c).Scan(&r.ladder, &r.reversibility, &r.budget)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &r, err
	}
	codeRow, err := read(code)
	if err != nil {
		return nil, "", "", 0, "", err
	}
	kindRow, err := read("*")
	if err != nil {
		return nil, "", "", 0, "", err
	}
	base := kindRow
	rowCode = "*"
	if codeRow != nil {
		base, rowCode = codeRow, code
	}
	reversibility, budget = "none", 6
	if base != nil {
		reversibility, budget = base.reversibility, base.budget
	}
	switch {
	case codeRow != nil && codeRow.ladder.Valid && codeRow.ladder.String != "":
		source = "code"
		err = json.Unmarshal([]byte(codeRow.ladder.String), &rungs)
		rowCode = code
	case kindRow != nil && kindRow.ladder.Valid && kindRow.ladder.String != "":
		source = "kind"
		err = json.Unmarshal([]byte(kindRow.ladder.String), &rungs)
		rowCode = "*"
	default:
		source = "default"
		for _, r := range defaultLadder {
			rungs = append(rungs, ladderSpecRung{Rung: r})
		}
	}
	if err != nil {
		err = fmt.Errorf("%w: ladder_json: %v", ErrLadderInvalid, err)
	}
	return rungs, rowCode, reversibility, budget, source, err
}

// upstreamRE parses the fixed Niwa gate-exit formats (design §5).
var (
	upstreamRoundsRE = regexp.MustCompile(`(?i)rejected (\d+) rounds`)
	upstreamHealthRE = regexp.MustCompile(`(?i)health check (\d+) time`)
	upstreamRoundRE  = regexp.MustCompile(`(?i)after round-(\d+)`)
)

// upstreamOf aggregates what Niwa already tried on the systemic's instances:
// structured evidence_json.upstream when a producer declared it, else the
// fixed gate-exit text of the instance's task. Also returns the most common
// board of those tasks (where rung tasks go).
func (d *DB) upstreamOf(systemicID string) (attempts int, reviewers []string, board string, err error) {
	rows, err := d.ro().Query(`SELECT COALESCE(e.evidence_json, ''), COALESCE(t.blocked_reason, ''), COALESCE(t.board_id, '')
		FROM exceptions e LEFT JOIN tasks t ON t.id = e.task_id WHERE e.cause_id = ?`, systemicID)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	boards := map[string]int{}
	for rows.Next() {
		var ev, reason, b string
		if err := rows.Scan(&ev, &reason, &b); err != nil {
			return 0, nil, "", err
		}
		if b != "" {
			boards[b]++
		}
		var parsed struct {
			Upstream struct {
				Rounds           int      `json:"rounds"`
				HealthChecks     int      `json:"health_checks"`
				ReviewerProfiles []string `json:"reviewer_profiles"`
			} `json:"upstream"`
		}
		if ev != "" && json.Unmarshal([]byte(ev), &parsed) == nil && (parsed.Upstream.Rounds > 0 || parsed.Upstream.HealthChecks > 0 || len(parsed.Upstream.ReviewerProfiles) > 0) {
			attempts += parsed.Upstream.Rounds + parsed.Upstream.HealthChecks
			for _, p := range parsed.Upstream.ReviewerProfiles {
				if p != "" && !seen[p] {
					seen[p] = true
					reviewers = append(reviewers, p)
				}
			}
			continue
		}
		for _, re := range []*regexp.Regexp{upstreamRoundsRE, upstreamHealthRE, upstreamRoundRE} {
			if m := re.FindStringSubmatch(reason); m != nil {
				n, _ := strconv.Atoi(m[1])
				attempts += n
				break
			}
		}
	}
	best := 0
	for b, n := range boards {
		if n > best || (n == best && b < board) {
			board, best = b, n
		}
	}
	sort.Strings(reviewers)
	return attempts, reviewers, board, rows.Err()
}

// normVersion reads a seeded exc.* norm's version.
func (d *DB) normVersion(normID string) int {
	v := 1
	_ = d.ro().QueryRow(`SELECT version FROM norms WHERE id = ?`, normID).Scan(&v)
	return v
}

// StartLadder freezes the systemic's ladder and opens rung 0, in one tx (CAS
// on rung IS NULL). An invalid ladder disables its class_budgets row, writes
// an audit event and returns ErrLadderInvalid; the systemic stays unladdered.
func (d *DB) StartLadder(s SystemicRef, now time.Time) (bool, error) {
	spec, rowCode, reversibility, budget, source, err := d.ladderSpec(s.Kind, s.ReasonCode)
	rungs := make([]LadderRung, 0, len(spec))
	deadlines := map[string]int{}
	for _, r := range excLadderRungs {
		deadlines[r.rung] = r.deadlineS
	}
	for _, r := range spec {
		rungs = append(rungs, LadderRung{Rung: r.Rung, NormID: "exc." + r.Rung, NormVersion: d.normVersion("exc." + r.Rung),
			Params: r.Params, DeadlineS: deadlines[r.Rung]})
	}
	if err == nil {
		err = ValidateLadder(rungs, reversibility)
	}
	if err != nil {
		_, _ = d.writerExec(`UPDATE class_budgets SET enabled = 0 WHERE kind = ? AND reason_code = ?`, s.Kind, rowCode)
		_ = d.RecordAudit(models.AuditEntry{Project: s.Project, Actor: excSweeper, Action: "ladder_invalid",
			ResourceType: "class_budget", ResourceID: s.Kind + "|" + rowCode,
			Summary: fmt.Sprintf("escalation ladder for %s/%s disabled: %v", s.Kind, rowCode, err)})
		return false, err
	}
	attempts, reviewers, board, err := d.upstreamOf(s.ID)
	if err != nil {
		return false, fmt.Errorf("upstream: %w", err)
	}
	snap := LadderSnapshot{Source: source, UpstreamAttempts: attempts, ReviewerProfiles: reviewers,
		TotalAttemptBudget: budget, Board: board}
	for _, r := range rungs {
		// Budget product (design §5.3): once Niwa spent the class budget, the
		// source already reported and a second reviewer opinion is not asked.
		if attempts >= budget && (r.Rung == RungAskSource || r.Rung == RungAdversarialReview) {
			snap.Trims = append(snap.Trims, r.Rung+":upstream_budget_spent")
			continue
		}
		snap.Rungs = append(snap.Rungs, r)
	}
	raw, _ := json.Marshal(snap)
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, fmt.Errorf("ladder begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE exceptions SET ladder_snapshot_json = ?, rung = 0 WHERE id = ? AND status = 'open' AND rung IS NULL`,
		string(raw), s.ID)
	if err != nil {
		return false, fmt.Errorf("ladder snapshot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := d.openRungTx(tx, s.Project, s.ID, snap, 0, "", now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("ladder commit: %w", err)
	}
	return true, nil
}

// openRungTx inserts the obligation of rung depth (INSERT OR IGNORE on the
// bindings hash: a double advance opens nothing twice).
func (d *DB) openRungTx(tx *writerTx, project, excID string, snap LadderSnapshot, depth int, parent string, now time.Time) error {
	r := snap.Rungs[depth]
	deadline := now.Add(time.Duration(r.DeadlineS) * time.Second)
	if r.Rung == RungHuman {
		deadline = now.Add(d.SettingDuration("exc_human_ttl", 72*time.Hour, time.Hour, 30*24*time.Hour))
	}
	var par any
	if parent != "" {
		par = parent
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO obligations
		(id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state,
		 created_at, deadline_at, escalation_depth, parent_obligation_id)
		VALUES (?, ?, ?, ?, ?, 'exception', ?, ?, '', 'active', ?, ?, ?, ?)`,
		uuid.New().String(), project, r.NormID, r.NormVersion, bindingsHash(r.NormID, "exception", excID, depth),
		excID, rungBearerKind(r.Rung), now.UTC().Format(memoryTimeFmt), deadline.UTC().Format(memoryTimeFmt), depth, par)
	if err != nil {
		return fmt.Errorf("open rung %s: %w", r.Rung, err)
	}
	return nil
}

func rungBearerKind(rung string) string {
	for _, r := range excLadderRungs {
		if r.rung == rung {
			return r.bearer
		}
	}
	return "relay"
}

// ActiveRung is an open systemic's current rung and its obligation.
type ActiveRung struct {
	ExceptionID, Project, ReasonCode, Owner, OpenedAt string
	Rung                                              int
	Snapshot                                          LadderSnapshot
	ObligationID, Bearer, CreatedAt, DeadlineAt       string
	Evidence                                          map[string]any
	SystemicEvidence                                  string
}

// Name is the current rung's name.
func (a ActiveRung) Name() string { return a.Snapshot.Rungs[a.Rung].Rung }

// Params is the current rung's params.
func (a ActiveRung) Params() map[string]string { return a.Snapshot.Rungs[a.Rung].Params }

// ActionRef is the task or message the rung requested ("" before it fired).
func (a ActiveRung) ActionRef() string {
	s, _ := a.Evidence["action_ref"].(string)
	return s
}

// ActiveRungs lists every open systemic with a started ladder and its active
// rung obligation.
func (d *DB) ActiveRungs() ([]ActiveRung, error) {
	rows, err := d.ro().Query(`SELECT e.id, e.project, e.reason_code, COALESCE(e.owner, ''), e.opened_at, e.rung,
			e.ladder_snapshot_json, COALESCE(e.evidence_json, '{}'),
			o.id, o.bearer, o.created_at, COALESCE(o.deadline_at, ''), COALESCE(o.discharge_evidence, '{}')
		FROM exceptions e JOIN obligations o
		  ON o.subject_kind = 'exception' AND o.subject_id = e.id AND o.state = 'active' AND o.escalation_depth = e.rung
		WHERE e.source_kind = ? AND e.status = 'open' AND e.rung IS NOT NULL
		ORDER BY e.opened_at`, excSourceClassBudget)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ActiveRung
	for rows.Next() {
		var a ActiveRung
		var snap, ev string
		if err := rows.Scan(&a.ExceptionID, &a.Project, &a.ReasonCode, &a.Owner, &a.OpenedAt, &a.Rung, &snap,
			&a.SystemicEvidence, &a.ObligationID, &a.Bearer, &a.CreatedAt, &a.DeadlineAt, &ev); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(snap), &a.Snapshot) != nil || a.Rung < 0 || a.Rung >= len(a.Snapshot.Rungs) {
			continue
		}
		a.Evidence = map[string]any{}
		_ = json.Unmarshal([]byte(ev), &a.Evidence)
		out = append(out, a)
	}
	return out, rows.Err()
}

func mergeEvidence(base, add map[string]any) string {
	m := map[string]any{}
	for k, v := range base {
		m[k] = v
	}
	for k, v := range add {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

// SetRungAction records the rung's bearer and requested action (task or
// message id) on its still-active obligation. Idempotent.
func (d *DB) SetRungAction(a ActiveRung, bearer string, add map[string]any) error {
	_, err := d.writerExec(`UPDATE obligations SET bearer = ?, discharge_evidence = ? WHERE id = ? AND state = 'active'`,
		strings.ToLower(bearer), mergeEvidence(a.Evidence, add), a.ObligationID)
	return err
}

// AdvanceRung closes the current rung obligation with closeState
// (unfulfilled | inactive | fulfilled) and opens rung `to`, CAS on the cursor.
// ok is false when another writer already moved it.
func (d *DB) AdvanceRung(a ActiveRung, closeState string, add map[string]any, to int, now time.Time) (bool, error) {
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, fmt.Errorf("advance begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := d.advanceRungTx(tx, a, closeState, add, to, "hard_stop", now)
	if !ok || err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("advance commit: %w", err)
	}
	return true, nil
}

// advanceRungTx is AdvanceRung on the caller's writer tx; rungs jumped over
// are recorded inactive with {"skipped": skipped}.
func (d *DB) advanceRungTx(tx *writerTx, a ActiveRung, closeState string, add map[string]any, to int, skipped string, now time.Time) (bool, error) {
	if to <= a.Rung || to >= len(a.Snapshot.Rungs) {
		return false, fmt.Errorf("advance %s: rung %d -> %d out of ladder", a.ExceptionID, a.Rung, to)
	}
	ts := now.UTC().Format(memoryTimeFmt)
	skipEv, _ := json.Marshal(map[string]string{"skipped": skipped})
	res, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, discharge_evidence = ? WHERE id = ? AND state = 'active'`,
		closeState, ts, mergeEvidence(a.Evidence, add), a.ObligationID)
	if err != nil {
		return false, fmt.Errorf("advance close: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	// Rungs jumped over (hard stop, guard route) are recorded as skipped, so
	// the attempt view shows every rung of the ladder.
	for skip := a.Rung + 1; skip < to; skip++ {
		r := a.Snapshot.Rungs[skip]
		if _, err := tx.Exec(`INSERT OR IGNORE INTO obligations
			(id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state,
			 created_at, closed_at, escalation_depth, parent_obligation_id, discharge_evidence)
			VALUES (?, ?, ?, ?, ?, 'exception', ?, ?, '', 'inactive', ?, ?, ?, ?, ?)`,
			uuid.New().String(), a.Project, r.NormID, r.NormVersion, bindingsHash(r.NormID, "exception", a.ExceptionID, skip),
			a.ExceptionID, rungBearerKind(r.Rung), ts, ts, skip, a.ObligationID, string(skipEv)); err != nil {
			return false, fmt.Errorf("advance skip: %w", err)
		}
	}
	res, err = tx.Exec(`UPDATE exceptions SET rung = ? WHERE id = ? AND rung = ? AND status = 'open'`, to, a.ExceptionID, a.Rung)
	if err != nil {
		return false, fmt.Errorf("advance cursor: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := d.openRungTx(tx, a.Project, a.ExceptionID, a.Snapshot, to, a.ObligationID, now); err != nil {
		return false, err
	}
	return true, nil
}

// ResolveAtRung fulfils the current rung and resolves the systemic, CAS on the
// cursor.
func (d *DB) ResolveAtRung(a ActiveRung, resolvedBy, reason string, add map[string]any, now time.Time) (bool, error) {
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, fmt.Errorf("resolve begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := resolveAtRungTx(tx, a, resolvedBy, reason, add, now)
	if !ok || err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("resolve commit: %w", err)
	}
	return true, nil
}

// resolveAtRungTx is ResolveAtRung on the caller's writer tx.
func resolveAtRungTx(tx *writerTx, a ActiveRung, resolvedBy, reason string, add map[string]any, now time.Time) (bool, error) {
	ts := now.UTC().Format(memoryTimeFmt)
	state := ObligationFulfilled
	if resolvedBy == "expired" {
		state = ObligationUnfulfilled
	}
	res, err := tx.Exec(`UPDATE obligations SET state = ?, closed_at = ?, done_at = ?, discharge_evidence = ? WHERE id = ? AND state = 'active'`,
		state, ts, ts, mergeEvidence(a.Evidence, add), a.ObligationID)
	if err != nil {
		return false, fmt.Errorf("resolve rung: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	res, err = tx.Exec(`UPDATE exceptions SET status = 'resolved', resolved_by = ?, resolution_reason = ?, resolved_at = ?
		WHERE id = ? AND rung = ? AND status = 'open'`, resolvedBy, reason, ts, a.ExceptionID, a.Rung)
	if err != nil {
		return false, fmt.Errorf("resolve systemic: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	return true, nil
}

// LadderTag is the idempotency tag carried by every task/message a rung
// requests: "[exc <id8> r<n>]".
func LadderTag(excID string, rung int) string {
	id := excID
	if len(id) > 8 {
		id = id[:8]
	}
	return fmt.Sprintf("[exc %s r%d]", id, rung)
}

// FindLadderTask returns the task a rung already dispatched (by tag), "" if none.
func (d *DB) FindLadderTask(project, tag string) string {
	var id string
	_ = d.ro().QueryRow(`SELECT id FROM tasks WHERE project = ? AND substr(title, 1, ?) = ? ORDER BY dispatched_at LIMIT 1`,
		project, len(tag), tag).Scan(&id)
	return id
}

// FindLadderMessage returns the newest message a rung already sent (by tag).
func (d *DB) FindLadderMessage(project, tag string) string {
	var id string
	_ = d.ro().QueryRow(`SELECT id FROM messages WHERE project = ? AND from_agent = 'relay' AND substr(subject, 1, ?) = ?
		ORDER BY created_at DESC LIMIT 1`, project, len(tag), tag).Scan(&id)
	return id
}

// LadderReply is a reply to a rung's message.
type LadderReply struct {
	ID, From, Metadata, Content, CreatedAt string
}

// RepliesTo lists replies to messageID, oldest first.
func (d *DB) RepliesTo(messageID string) ([]LadderReply, error) {
	rows, err := d.ro().Query(`SELECT id, from_agent, COALESCE(metadata, ''), COALESCE(content, ''), created_at
		FROM messages WHERE reply_to = ? ORDER BY created_at`, messageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LadderReply
	for rows.Next() {
		var r LadderReply
		if err := rows.Scan(&r.ID, &r.From, &r.Metadata, &r.Content, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TopRaiser is the live agent that raised most of the systemic's instances
// ("" when none is live): the ask_source target.
func (d *DB) TopRaiser(project, systemicID string) string {
	rows, err := d.ro().Query(`SELECT COALESCE(raised_by, ''), COUNT(*) n FROM exceptions WHERE cause_id = ?
		GROUP BY 1 ORDER BY n DESC, 1`, systemicID)
	if err != nil {
		return ""
	}
	var cands []string
	for rows.Next() {
		var who string
		var n int
		if rows.Scan(&who, &n) == nil {
			cands = append(cands, who)
		}
	}
	_ = rows.Close()
	for _, who := range cands {
		if who == "" || nonAgentDispatchers[strings.ToLower(who)] {
			continue
		}
		if a, err := d.GetAgent(project, who); err == nil && a != nil && a.Status == "active" {
			return a.Name
		}
	}
	return ""
}

// LiveSupervisor returns an active agent to escalate to: the owner's
// reports_to, else the project's first active executive, else the most
// recently seen active executive of any project. Never the founder and never
// an inactive agent; "" only when the fleet has no live executive at all.
func (d *DB) LiveSupervisor(project, owner string) string {
	if owner != "" {
		if a, err := d.GetAgent(project, owner); err == nil && a != nil && a.ReportsTo != nil && *a.ReportsTo != "" {
			if m, err := d.GetAgent(project, *a.ReportsTo); err == nil && m != nil && m.Status == "active" &&
				!nonAgentDispatchers[strings.ToLower(m.Name)] {
				return m.Name
			}
		}
	}
	if agents, err := d.ListAgents(project); err == nil {
		for _, a := range agents {
			if a.IsExecutive && a.Status == "active" && a.Name != owner && !nonAgentDispatchers[strings.ToLower(a.Name)] {
				return a.Name
			}
		}
	}
	var name string
	_ = d.ro().QueryRow(`SELECT name FROM agents WHERE is_executive = 1 AND status = 'active' AND lower(name) NOT IN
		('user', 'human', 'founder', 'relay', 'relay-sweeper') ORDER BY last_seen DESC LIMIT 1`).Scan(&name)
	return name
}

// IsLiveAgent reports whether name is an active agent of project.
func (d *DB) IsLiveAgent(project, name string) bool {
	if name == "" || nonAgentDispatchers[strings.ToLower(name)] {
		return false
	}
	a, err := d.GetAgent(project, name)
	return err == nil && a != nil && a.Status == "active"
}

// PrecedentFor returns the last resolved systemic of the same reason_code in
// any project (classes are global), for match_precedent's evidence.
func (d *DB) PrecedentFor(code, excludeID string) map[string]any {
	var id, project, by, reason, at string
	err := d.ro().QueryRow(`SELECT id, project, COALESCE(resolved_by, ''), COALESCE(resolution_reason, ''), COALESCE(resolved_at, '')
		FROM exceptions WHERE source_kind = ? AND reason_code = ? AND status = 'resolved' AND id <> ?
		ORDER BY resolved_at DESC LIMIT 1`, excSourceClassBudget, code, excludeID).Scan(&id, &project, &by, &reason, &at)
	if err != nil {
		return nil
	}
	return map[string]any{"id": id, "project": project, "resolved_by": by, "resolution_reason": reason, "resolved_at": at}
}

// LinkedVolume counts instances linked to the systemic since `since`.
func (d *DB) LinkedVolume(systemicID, since string) int {
	var n int
	_ = d.ro().QueryRow(`SELECT COUNT(*) FROM exceptions WHERE cause_id = ? AND opened_at >= ?`, systemicID, since).Scan(&n)
	return n
}

// SuppressClass sets suppressed_until on the (kind, code) budget row, creating
// the code row from the kind default when absent (human accept_known).
func (d *DB) SuppressClass(kind, code, until string) error {
	tx, err := d.beginWriterTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO class_budgets (kind, reason_code, intensity, period_s, total_attempt_budget, reversibility, ladder_json, enabled)
		SELECT kind, ?, intensity, period_s, total_attempt_budget, reversibility, ladder_json, enabled FROM class_budgets
		WHERE kind = ? AND reason_code = '*'`, code, kind); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE class_budgets SET suppressed_until = ? WHERE kind = ? AND reason_code = ?`, until, kind, code); err != nil {
		return err
	}
	return tx.Commit()
}

// SystemicKind returns the budgeted kind of a systemic (its instances' kind).
func (d *DB) SystemicKind(systemicID string) string {
	var kind string
	_ = d.ro().QueryRow(`SELECT kind FROM exceptions WHERE cause_id = ? LIMIT 1`, systemicID).Scan(&kind)
	return kind
}
