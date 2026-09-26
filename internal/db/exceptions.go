package db

import (
	"agent-relay/internal/models"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Exceptions and exception classes (design 220f4f3d, ruling cto-tsukumo
// 10b62e06). Every task block, cancel-with-reason and limbo block writes ONE
// typed exception row, grouped into a deterministic class, in the SAME writer
// transaction as the task transition that caused it. Classification is local
// and O(tokens): L0 reason lexicon -> parameterize -> fingerprint -> exact
// lookup -> Drain-style attach within the (kind, reason_code) bucket -> new
// class. There is no model call on the write path, and nothing here reads an
// exception to change behaviour: this is the measured substrate that budgets,
// ladders and guards will read later. Side tables only: taskColumns/scanTask
// are untouched and the link to a task is one-way (exceptions.source_ref).

// exceptionGroupingVersion is part of every fingerprint. Ruling 10b62e06 keeps
// it at 1 with no secondary-grouping writer; a future lexicon or parameterizer
// change bumps it.
const exceptionGroupingVersion = 1

// Source kinds written by this slice.
const (
	excSourceTaskBlock  = "task_block"
	excSourceTaskCancel = "task_cancel"
	excSourceLimbo      = "limbo_sweep"
)

// excOpenBlockSources are the rows a task leaving 'blocked' resolves: an
// agent/operator block, or a limbo-sweep block.
var excOpenBlockSources = []string{excSourceTaskBlock, excSourceLimbo}

// excDrainDepth / excDrainSim mirror the dry-run that sized the classes
// (designs/220f4f3d-exceptions-dryrun.py): a new fingerprint attaches to an
// existing class of the same bucket when the first excDrainDepth tokens agree
// and at least excDrainSim of the positions are identical.
const (
	excDrainDepth  = 3
	excDrainSim    = 0.5
	excMaxTokens   = 24
	excEmptyMarker = "<empty>"
)

func migrateExceptions(conn *sql.DB) {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS exception_classes (
		id                    TEXT PRIMARY KEY,
		kind                  TEXT NOT NULL,
		reason_code           TEXT NOT NULL,
		fingerprint           TEXT NOT NULL,
		secondary_fingerprint TEXT,
		template              TEXT NOT NULL,
		grouping_version      INTEGER NOT NULL,
		matched_by            TEXT NOT NULL,
		match_distance        REAL,
		match_model           TEXT,
		merged_into           TEXT,
		occurrences           INTEGER NOT NULL DEFAULT 0,
		first_seen            TEXT NOT NULL,
		last_seen             TEXT NOT NULL,
		UNIQUE (fingerprint, grouping_version)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_exception_classes_bucket ON exception_classes(kind, reason_code, grouping_version)`)
	// fingerprint / matched_by / match_distance are per occurrence: an attached
	// variant keeps its own fingerprint so the next identical text resolves by
	// exact lookup and is never re-matched (the persisted-decision rule).
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS exceptions (
		id                TEXT PRIMARY KEY,
		project           TEXT NOT NULL,
		class_id          TEXT NOT NULL,
		source_kind       TEXT NOT NULL,
		source_ref        TEXT NOT NULL,
		kind              TEXT NOT NULL,
		reason_code       TEXT NOT NULL,
		retry_class       TEXT NOT NULL DEFAULT 'unknown',
		fingerprint       TEXT NOT NULL,
		matched_by        TEXT NOT NULL,
		match_distance    REAL,
		raised_by         TEXT,
		task_id           TEXT,
		trace_id          TEXT,
		evidence_json     TEXT,
		status            TEXT NOT NULL,
		resolved_by       TEXT,
		resolution_reason TEXT,
		opened_at         TEXT NOT NULL,
		resolved_at       TEXT,
		UNIQUE (source_kind, source_ref, opened_at)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_exceptions_class ON exceptions(class_id, opened_at)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_exceptions_source ON exceptions(source_kind, source_ref) WHERE status = 'open'`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_exceptions_open ON exceptions(project, status, opened_at)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_exceptions_fingerprint ON exceptions(fingerprint)`)
	// One read surface for metrics: integrity_quarantine already is a
	// class-keyed detected/resolved log, so it joins through a view instead of
	// being copied (design OQ3, 0 new writes).
	_, _ = conn.Exec(`CREATE VIEW IF NOT EXISTS exception_occurrences AS
		SELECT id, project, class_id, source_kind, source_ref, kind, reason_code, status, opened_at, resolved_at FROM exceptions
		UNION ALL
		SELECT 'q:' || id, project, 'integrity:' || class, 'quarantine', table_name || ':' || row_id, 'integrity', class,
		       CASE WHEN resolved_at IS NULL THEN 'open' ELSE 'resolved' END, detected_at, resolved_at
		FROM integrity_quarantine`)
	// Class budgets + the inert escalation-ladder schema (design 1111292b T1).
	migrateClassBudgets(conn)
}

// excQ is the statement surface classification and the exception writes run
// on: the producer's own writer transaction (*writerTx satisfies it).
type excQ interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// reasonRule is one entry of the L0 reason lexicon: first match wins.
type reasonRule struct {
	code, kind, retry string
	re                *regexp.Regexp
}

// reasonLexiconV1 is the ordered L0 lexicon measured on the 385 prod
// blocked_reason rows (design §3.2: 17 codes, 79.5% coverage). Precision on
// free prose is imperfect, so a code here is a trend signal, never an
// automation trigger. Changing it means bumping exceptionGroupingVersion.
var reasonLexiconV1 = []reasonRule{
	{"niwa_gate_rounds_exhausted", "gate_exhausted", "non_retryable", regexp.MustCompile(`(?i)rejected \d+ rounds.*human needed`)},
	{"niwa_gate_postmerge_failed", "gate_exhausted", "non_retryable", regexp.MustCompile(`(?i)(merge result failed|post-merge check|github checks failed).*human needed|human needed`)},
	{"niwa_merge_blocked", "gate_exhausted", "retryable", regexp.MustCompile(`(?i)merge blocked|checkout .* is dirty|merge failed|conflict`)},
	{"limbo_sweep", "limbo", "retryable", regexp.MustCompile(`(?i)^limbo-sweep`)},
	{"dead_lane", "routing", "non_retryable", regexp.MustCompile(`(?i)dead-lane|profile .* removed|team restructure|orphan[- ]profile|lane (is )?dead`)},
	{"superseded", "plan_change", "benign", regexp.MustCompile(`(?i)supersed|redundant|replaced by|re-?dispatch(ed)? as|re-filed|refiled|folded into`)},
	{"duplicate", "plan_change", "benign", regexp.MustCompile(`(?i)duplicat|dup of|same as `)},
	{"misrouted", "routing", "non_retryable", regexp.MustCompile(`(?i)misrout|wrong (project|lane|board|profile)|belongs to`)},
	{"postponed", "plan_change", "benign", regexp.MustCompile(`(?i)postpon|deferred|later wave|parked|backlog`)},
	{"obsolete", "plan_change", "benign", regexp.MustCompile(`(?i)obsol[eè]t|no longer (needed|relevant)|stale|vague avril|not needed|moot|pertinent`)},
	{"stale_no_output", "stall", "retryable", regexp.MustCompile(`(?i)ttl'?d without output|no resubmit|no output|timed? ?out|went silent`)},
	{"gate_pending", "blocker", "retryable", regexp.MustCompile(`(?i)(approval|review|plan|spec).*pending|awaiting|waiting (on|for)|blocked (on|by)|depends on|until`)},
	{"deprioritized", "plan_change", "benign", regexp.MustCompile(`(?i)token economy|focus on|budget|deprioriti|not now|pause`)},
	{"operator_override", "plan_change", "benign", regexp.MustCompile(`(?i)user override|founder|loic|operator|order:|ordre`)},
	{"done_elsewhere", "plan_change", "benign", regexp.MustCompile(`(?i)already (done|merged|shipped|fixed)|done (by|in)|merged (in|as)|shipped`)},
	{"cancelled_by_request", "plan_change", "benign", regexp.MustCompile(`(?i)cancel`)},
}

const excUnclassified = "unclassified"

// classifyReason returns the lexicon code, kind and retry class for text.
// Unclassified text keeps retry_class=unknown and the caller's default kind:
// prose the lexicon cannot read is never laundered as benign (ruling 10b62e06).
func classifyReason(text, defaultKind string) (code, kind, retry string) {
	for _, r := range reasonLexiconV1 {
		if r.re.MatchString(text) {
			return r.code, r.kind, r.retry
		}
	}
	return excUnclassified, defaultKind, "unknown"
}

// excParamRE is the ordered parameterization list (Sentry's list plus WRAI.TH
// tokens); at each position the first alternative that matches wins.
var excParamRE = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`(?P<url>https?://\S+)`,
	`(?P<email>[\w.+-]+@[\w-]+\.[\w.]+)`,
	`(?P<uuid>\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b)`,
	`(?P<ts>\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?Z?\b)`,
	`(?P<date>\b\d{4}-\d{2}-\d{2}\b)`,
	`(?P<time>\b\d{1,2}:\d{2}(?::\d{2})?Z?\b)`,
	`(?P<sha>\b[0-9a-f]{40}\b)`,
	`(?P<hexid>\b[0-9a-f]{7,12}\b)`,
	`(?P<pr>#\d+\b)`,
	`(?P<path>(?:[\w.-]+/)+[\w.-]+)`,
	`(?P<dur>\b\d+(?:\.\d+)?\s?(?:ms|s|m|min|h|d|x)\b)`,
	`(?P<ver>\bv\d+\.\d+(?:\.\d+)?\b)`,
	`(?P<num>\b\d+(?:\.\d+)?\b)`,
	"(?P<quoted>\"[^\"]{1,80}\"|'[^']{1,80}'|`[^`]{1,80}`)",
}, "|"))

var excParamNames = excParamRE.SubexpNames()

// parameterize replaces variable spans with typed placeholders, then known
// agent names with <agent>. A hexid must mix digits and letters (a task id or
// a sha); an all-digit run is a number and an all-letter word stays text.
func parameterize(text string, agents []string) string {
	text = strings.TrimSpace(text)
	var b strings.Builder
	last := 0
	for _, m := range excParamRE.FindAllStringSubmatchIndex(text, -1) {
		name := ""
		for g := 1; g < len(excParamNames); g++ {
			if m[2*g] >= 0 {
				name = excParamNames[g]
				break
			}
		}
		span := text[m[0]:m[1]]
		repl := "<" + name + ">"
		if name == "hexid" {
			hasDigit := strings.IndexFunc(span, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0
			hasAlpha := strings.IndexFunc(span, func(r rune) bool { return (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') }) >= 0
			switch {
			case hasDigit && hasAlpha:
			case hasDigit:
				repl = "<num>"
			default:
				repl = span
			}
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(repl)
		last = m[1]
	}
	b.WriteString(text[last:])
	out := b.String()
	if re := agentNameRE(agents); re != nil {
		out = re.ReplaceAllString(out, "${1}<agent>${3}")
	}
	return out
}

// agentNameRE matches any known agent name (length >= 3, longest first) as a
// whole token: not glued to a word character or '-'. RE2 has no lookaround,
// so the boundaries are captured and put back.
func agentNameRE(agents []string) *regexp.Regexp {
	var names []string
	for _, a := range agents {
		if len(a) >= 3 {
			names = append(names, regexp.QuoteMeta(a))
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.SliceStable(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	return regexp.MustCompile(`(?i)(^|[^\w-])(` + strings.Join(names, "|") + `)($|[^\w-])`)
}

// excTokens keeps the first line and at most excMaxTokens whitespace tokens:
// the class lives in the head of a reason, the tail is free text.
func excTokens(parameterized string) []string {
	line, _, _ := strings.Cut(parameterized, "\n")
	toks := strings.Fields(line)
	if len(toks) > excMaxTokens {
		toks = toks[:excMaxTokens]
	}
	if len(toks) == 0 {
		return []string{excEmptyMarker}
	}
	return toks
}

func excFingerprint(kind, code string, toks []string) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%d|%s", kind, code, exceptionGroupingVersion, strings.Join(toks, " "))))
	return hex.EncodeToString(h[:])[:16]
}

// excAgentNames lists the known agent names on the caller's transaction, so the
// parameterizer sees the same roster the write commits against.
func excAgentNames(q excQ) ([]string, error) {
	rows, err := q.Query(`SELECT DISTINCT LOWER(name) FROM agents WHERE LENGTH(name) >= 3 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// excClassification is where one occurrence landed.
type excClassification struct {
	ClassID       string
	Fingerprint   string
	Template      string
	MatchedBy     string // new | exact | drain
	MatchDistance *float64
}

// classifyTx resolves (kind, code, text) to a class on q, creating or updating
// the class row. Order: exact class fingerprint, exact variant fingerprint seen
// on an earlier occurrence, Drain attach inside the (kind, code) bucket, new
// class. Deterministic for a given DB state; an attach is persisted through
// the occurrence's own fingerprint, so the same text never re-matches.
func classifyTx(q excQ, kind, code, text, now string) (excClassification, error) {
	agents, err := excAgentNames(q)
	if err != nil {
		return excClassification{}, err
	}
	toks := excTokens(parameterize(text, agents))
	fp := excFingerprint(kind, code, toks)
	c := excClassification{Fingerprint: fp, Template: strings.Join(toks, " ")}

	var classID string
	err = q.QueryRow(`SELECT id FROM exception_classes WHERE fingerprint = ? AND grouping_version = ?`,
		fp, exceptionGroupingVersion).Scan(&classID)
	if err == nil {
		c.ClassID, c.MatchedBy = classID, "exact"
		return c, bumpClassTx(q, classID, "", now)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	err = q.QueryRow(`SELECT class_id FROM exceptions WHERE fingerprint = ? ORDER BY opened_at LIMIT 1`, fp).Scan(&classID)
	if err == nil {
		c.ClassID, c.MatchedBy = classID, "exact"
		return c, bumpClassTx(q, classID, "", now)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}

	// Drain attach: same token count, same prefix, best similarity >= excDrainSim.
	rows, err := q.Query(`SELECT id, template FROM exception_classes
		WHERE kind = ? AND reason_code = ? AND grouping_version = ? AND merged_into IS NULL
		ORDER BY first_seen, id`, kind, code, exceptionGroupingVersion)
	if err != nil {
		return c, err
	}
	bestID, bestTpl, bestSim := "", []string(nil), -1.0
	for rows.Next() {
		var id, tpl string
		if err := rows.Scan(&id, &tpl); err != nil {
			rows.Close()
			return c, err
		}
		cand := strings.Fields(tpl)
		if sim, ok := drainSimilarity(cand, toks); ok && sim > bestSim {
			bestID, bestTpl, bestSim = id, cand, sim
		}
	}
	if err := rows.Close(); err != nil {
		return c, err
	}
	if bestID != "" && bestSim >= excDrainSim {
		merged := make([]string, len(bestTpl))
		for i := range bestTpl {
			if bestTpl[i] == toks[i] {
				merged[i] = bestTpl[i]
			} else {
				merged[i] = "<*>"
			}
		}
		dist := 1 - bestSim
		c.ClassID, c.MatchedBy, c.MatchDistance = bestID, "drain", &dist
		c.Template = strings.Join(merged, " ")
		return c, bumpClassTx(q, bestID, c.Template, now)
	}

	c.ClassID, c.MatchedBy = uuid.New().String(), "new"
	_, err = q.Exec(`INSERT INTO exception_classes
		(id, kind, reason_code, fingerprint, template, grouping_version, matched_by, occurrences, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, 'new', 1, ?, ?)`,
		c.ClassID, kind, code, fp, c.Template, exceptionGroupingVersion, now, now)
	return c, err
}

// drainSimilarity compares a class template to incoming tokens the Drain way:
// only same-length sequences whose first excDrainDepth tokens agree (a
// placeholder or <*> agrees with any placeholder) are comparable; the score is
// the share of identical positions.
func drainSimilarity(tpl, toks []string) (float64, bool) {
	if len(tpl) != len(toks) || len(toks) == 0 {
		return 0, false
	}
	isVar := func(s string) bool { return strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") }
	for i := 0; i < excDrainDepth && i < len(toks); i++ {
		if tpl[i] != toks[i] && !(isVar(tpl[i]) && isVar(toks[i])) {
			return 0, false
		}
	}
	same := 0
	for i := range toks {
		if tpl[i] == toks[i] {
			same++
		}
	}
	return float64(same) / float64(len(toks)), true
}

func bumpClassTx(q excQ, classID, template, now string) error {
	if template != "" {
		_, err := q.Exec(`UPDATE exception_classes SET occurrences = occurrences + 1, last_seen = ?, template = ? WHERE id = ?`,
			now, template, classID)
		return err
	}
	_, err := q.Exec(`UPDATE exception_classes SET occurrences = occurrences + 1, last_seen = ? WHERE id = ?`, now, classID)
	return err
}

// exceptionOpen is one exception to write. Code/Kind/Retry are the
// source-declared L0 class; when Code is empty the lexicon classifies Text and
// DefaultKind is used for unclassified text. A non-nil Resolved inserts the row
// already resolved (an event that opens and closes at once, e.g. a cancel).
type exceptionOpen struct {
	Project, SourceKind, SourceRef, RaisedBy, TaskID string
	TraceID                                          *string
	Text                                             string
	Code, Kind, Retry, DefaultKind                   string
	At                                               string
	Resolved                                         *exceptionResolution
}

type exceptionResolution struct {
	By, Reason, At string
}

// openExceptionTx classifies and inserts one exception on q (the producer's tx).
func openExceptionTx(q excQ, e exceptionOpen) (string, error) {
	code, kind, retry := e.Code, e.Kind, e.Retry
	if code == "" {
		code, kind, retry = classifyReason(e.Text, e.DefaultKind)
	}
	cls, err := classifyTx(q, kind, code, e.Text, e.At)
	if err != nil {
		return "", fmt.Errorf("classify exception: %w", err)
	}
	status := "open"
	var resolvedBy, resolutionReason, resolvedAt any
	if e.Resolved != nil {
		status = "resolved"
		resolvedBy, resolutionReason, resolvedAt = e.Resolved.By, e.Resolved.Reason, e.Resolved.At
	}
	var taskID any
	if e.TaskID != "" {
		taskID = e.TaskID
	}
	id := uuid.New().String()
	_, err = q.Exec(`INSERT INTO exceptions
		(id, project, class_id, source_kind, source_ref, kind, reason_code, retry_class, fingerprint, matched_by, match_distance,
		 raised_by, task_id, trace_id, status, resolved_by, resolution_reason, opened_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, e.Project, cls.ClassID, e.SourceKind, e.SourceRef, kind, code, retry, cls.Fingerprint, cls.MatchedBy, cls.MatchDistance,
		e.RaisedBy, taskID, e.TraceID, status, resolvedBy, resolutionReason, e.At, resolvedAt)
	if err != nil {
		return "", fmt.Errorf("insert exception: %w", err)
	}
	return id, nil
}

// openExceptionRaiserTx returns who raised the newest open exception of the
// given sources for ref ("" when none is open).
func openExceptionRaiserTx(q excQ, ref string, sources []string) (string, error) {
	ph, args := inPlaceholders(sources)
	args = append([]interface{}{ref}, args...)
	var raiser sql.NullString
	err := q.QueryRow(`SELECT raised_by FROM exceptions WHERE source_ref = ? AND status = 'open' AND source_kind IN (`+ph+`)
		ORDER BY opened_at DESC LIMIT 1`, args...).Scan(&raiser)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raiser.String, err
}

// resolveOpenTx closes every open exception of the given sources for ref.
func resolveOpenTx(q excQ, ref string, sources []string, r exceptionResolution) (int64, error) {
	ph, args := inPlaceholders(sources)
	args = append([]interface{}{r.By, r.Reason, r.At, ref}, args...)
	res, err := q.Exec(`UPDATE exceptions SET status = 'resolved', resolved_by = ?, resolution_reason = ?, resolved_at = ?
		WHERE source_ref = ? AND status = 'open' AND source_kind IN (`+ph+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("resolve exception: %w", err)
	}
	return res.RowsAffected()
}

// excResolver derives resolved_by from who moved the task, never from a claim:
// the operator is human, the raiser is self, the dispatcher is supervisor, any
// other agent is peer.
func excResolver(actor, raisedBy, dispatcher string) string {
	switch {
	case actor == "human" || actor == "user":
		return "human"
	case raisedBy != "" && strings.EqualFold(actor, raisedBy):
		return "self"
	case dispatcher != "" && strings.EqualFold(actor, dispatcher):
		return "supervisor"
	default:
		return "peer"
	}
}

// excResolutionReason maps the status a blocked task moves to (and, for a
// cancel, the lexicon code of its reason) to resolution_reason.
func excResolutionReason(newStatus, cancelCode string) string {
	switch newStatus {
	case "pending":
		return "requeued"
	case "cancelled":
		switch cancelCode {
		case "superseded", "duplicate", "obsolete", "misrouted":
			return cancelCode
		}
		return "cancelled"
	default:
		return "fixed"
	}
}

// writeTransitionExceptions records the exception side of a task transition on
// the transition's own writer tx (called after the status CAS landed):
//   - leaving 'blocked' (or an operator re-block) resolves the open block row,
//     with resolved_by derived from who moved the task;
//   - entering 'blocked' opens a task_block row;
//   - cancelling a task that was NOT blocked, with a reason, writes one
//     task_cancel row opened and resolved at once.
func (d *DB) writeTransitionExceptions(tx *writerTx, task *models.Task, actor, oldStatus, newStatus string, reason *string, now string) error {
	text := ""
	if reason != nil {
		text = strings.TrimSpace(*reason)
	}
	if oldStatus == "blocked" {
		raiser, err := openExceptionRaiserTx(tx, task.ID, excOpenBlockSources)
		if err != nil {
			return fmt.Errorf("read open exception: %w", err)
		}
		cancelCode := ""
		if newStatus == "cancelled" && text != "" {
			cancelCode, _, _ = classifyReason(text, "")
		}
		r := exceptionResolution{
			By:     excResolver(actor, raiser, task.DispatchedBy),
			Reason: excResolutionReason(newStatus, cancelCode),
			At:     now,
		}
		if newStatus == "blocked" {
			r.Reason = "superseded" // operator force re-block: the new block replaces the old one
		}
		if _, err := resolveOpenTx(tx, task.ID, excOpenBlockSources, r); err != nil {
			return err
		}
	}
	switch {
	case newStatus == "blocked":
		_, err := openExceptionTx(tx, exceptionOpen{
			Project: task.Project, SourceKind: excSourceTaskBlock, SourceRef: task.ID,
			RaisedBy: actor, TaskID: task.ID, TraceID: task.TraceID,
			Text: text, DefaultKind: "blocker", At: now,
		})
		return err
	case newStatus == "cancelled" && oldStatus != "blocked" && text != "":
		by := "self"
		if actor == "human" || actor == "user" {
			by = "human"
		}
		code, _, _ := classifyReason(text, "")
		_, err := openExceptionTx(tx, exceptionOpen{
			Project: task.Project, SourceKind: excSourceTaskCancel, SourceRef: task.ID,
			RaisedBy: actor, TaskID: task.ID, TraceID: task.TraceID,
			Text: text, DefaultKind: "plan_change", At: now,
			Resolved: &exceptionResolution{By: by, Reason: excResolutionReason("cancelled", code), At: now},
		})
		return err
	}
	return nil
}
