package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Compiled guards, slice S1 (design 4d2e57a3, ruling cto-tsukumo 3ece19c0). A
// resolved exception can be compiled into a scoped, expiring guard: a closed
// JSON matcher over typed exception fields, evaluated in Go with equality, set
// membership and one literal template prefix. There is no model, embedding or
// network at match time; this file imports the standard library only (pinned
// by TestGuards/NoModelAtMatchTime). Guards act on exception bookkeeping ONLY:
// an active open-point suppress guard excludes the instance from class-budget
// counting. They never change a task, a message, a delivery or a memory, never
// deny a transition, and never touch the hard-coded identity / self-grading
// guards (bf920f6c) or any authorization check. Every guard is born shadow,
// is promoted by a different agent on evidence, and demotes itself on
// regression with an audit event.

const (
	GuardPointOpen   = "open"
	GuardPointLadder = "ladder"

	GuardActionSuppress        = "suppress"
	GuardActionResolveSystemic = "resolve_systemic"
	GuardActionRoute           = "route"

	GuardModeShadow  = "shadow"
	GuardModeActive  = "active"
	GuardModeExpired = "expired"
	GuardModeRetired = "retired"

	GuardSourceHuman = "human_generalize"
	GuardSourceAgent = "agent_resolution"

	// Knobs (read via SettingInt / SettingDuration; ruling OQ4 defaults).
	settingGuardPromoteHits       = "guard_promote_hits"
	settingGuardDemoteRegressions = "guard_demote_regressions"
	settingGuardRegressionGrace   = "guard_regression_grace"
	settingGuardRetireAfter       = "guard_retire_after"

	defaultGuardPromoteHits       = 3
	defaultGuardDemoteRegressions = 2
	defaultGuardRegressionGrace   = 24 * time.Hour
	defaultGuardRetireAfter       = 30 * 24 * time.Hour

	guardMinExpiry    = 24 * time.Hour
	guardMaxExpiry    = 90 * 24 * time.Hour
	guardReplayWindow = 90 * 24 * time.Hour
	guardConflictIDs  = 20
)

// Guard refusal codes.
const (
	GuardErrForbidden        = "FORBIDDEN"
	GuardErrNotResolved      = "GUARD_ORIGIN_NOT_RESOLVED"
	GuardErrUnknownScopeKey  = "GUARD_UNKNOWN_SCOPE_KEY"
	GuardErrAnchorRequired   = "GUARD_ANCHOR_REQUIRED"
	GuardErrProjectRequired  = "GUARD_PROJECT_REQUIRED"
	GuardErrScopeExcludes    = "GUARD_SCOPE_EXCLUDES_ORIGIN"
	GuardErrExpiryRequired   = "GUARD_EXPIRY_REQUIRED"
	GuardErrExpiryTooLong    = "GUARD_EXPIRY_TOO_LONG"
	GuardErrBadAction        = "GUARD_BAD_ACTION"
	GuardErrProtectedClass   = "GUARD_PROTECTED_CLASS"
	GuardErrNotFound         = "GUARD_NOT_FOUND"
	GuardErrPromoteRefused   = "GUARD_PROMOTE_REFUSED"
	GuardErrRenewRefused     = "GUARD_RENEW_REFUSED"
	guardEndedDemoted        = "demoted(regressions)"
	guardEndedDemotedMass    = "demoted(suppress_mass)"
	guardEndedExpired        = "expired"
	guardEndedRetiredNoHits  = "retired_no_hits"
	guardEndedDemotedConflct = "demoted(shadow_conflict)"
)

// GuardError is a typed refusal: Code is one of the GuardErr* constants.
type GuardError struct {
	Code, Detail string
}

func (e *GuardError) Error() string { return e.Code + ": " + e.Detail }

func guardErr(code, format string, args ...any) error {
	return &GuardError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// guardProtectedKinds are exception kinds a compiled guard can never match:
// the identity gate, the self-grading guard (bf920f6c), authorization and
// integrity are safety invariants, not learned policy (design §0).
var guardProtectedKinds = map[string]bool{"identity": true, "auth": true, "self_grading": true, "integrity": true}

// guardScopeKeys is the closed matcher vocabulary (design §3). The bool is
// true for keys that accept a string list.
var guardScopeKeys = map[string]bool{
	"class_id": false, "kind": true, "reason_code": true, "retry_class": true, "project": false,
	"source_kind": true, "raised_by_profile": false, "template_prefix": false,
}

// guardActions is the closed action enum per point (design §4.1).
var guardActions = map[string]map[string]bool{
	GuardPointOpen:   {GuardActionSuppress: true},
	GuardPointLadder: {GuardActionResolveSystemic: true, GuardActionRoute: true},
}

func migrateGuards(conn *sql.DB) {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS compiled_guards (
		id                TEXT PRIMARY KEY,
		project           TEXT NOT NULL,
		class_id          TEXT,
		from_exception_id TEXT NOT NULL,
		point             TEXT NOT NULL,
		action            TEXT NOT NULL,
		action_params     TEXT NOT NULL DEFAULT '{}',
		scope_expr        TEXT NOT NULL,
		specificity       INTEGER NOT NULL,
		mode              TEXT NOT NULL,
		replay_json       TEXT NOT NULL,
		shadow_hits       INTEGER NOT NULL DEFAULT 0,
		shadow_conflicts  INTEGER NOT NULL DEFAULT 0,
		live_hits         INTEGER NOT NULL DEFAULT 0,
		regressions       INTEGER NOT NULL DEFAULT 0,
		anchor_at         TEXT,
		created_by        TEXT NOT NULL,
		challenged_by     TEXT,
		source            TEXT NOT NULL,
		created_at        TEXT NOT NULL,
		promoted_at       TEXT,
		last_hit_at       TEXT,
		expires_at        TEXT NOT NULL,
		ended_at          TEXT,
		ended_reason      TEXT
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_guards_eval ON compiled_guards(point, mode, class_id)`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS guard_hits (
		guard_id     TEXT NOT NULL,
		exception_id TEXT NOT NULL,
		mode         TEXT NOT NULL,
		acted        INTEGER NOT NULL,
		verdict      TEXT NOT NULL DEFAULT 'pending',
		at           TEXT NOT NULL,
		PRIMARY KEY (guard_id, exception_id)
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_guard_hits_exc ON guard_hits(exception_id)`)
}

// guardSuppressedSQL is true for an exception (aliased by %s) that an active
// suppress guard acted on: the class-budget scan never counts it.
const guardSuppressedSQL = `EXISTS (SELECT 1 FROM guard_hits h JOIN compiled_guards g ON g.id = h.guard_id
	WHERE h.exception_id = %s.id AND h.acted = 1 AND g.action = 'suppress')`

// ---------------------------------------------------------------------------
// Matcher (design §3)

// guardScope is a normalized matcher: key -> accepted values (AND of keys, OR
// within a key).
type guardScope map[string][]string

// parseGuardScope validates and normalizes a raw scope_expr.
func parseGuardScope(raw map[string]any) (guardScope, error) {
	s := guardScope{}
	for k, v := range raw {
		list, known := guardScopeKeys[k]
		if !known {
			return nil, guardErr(GuardErrUnknownScopeKey, "scope key %q is not in the closed set", k)
		}
		switch x := v.(type) {
		case string:
			if x == "" {
				return nil, guardErr(GuardErrUnknownScopeKey, "scope key %q is empty", k)
			}
			s[k] = []string{x}
		case []string:
			if !list || len(x) == 0 {
				return nil, guardErr(GuardErrUnknownScopeKey, "scope key %q does not take a list", k)
			}
			s[k] = append([]string(nil), x...)
		case []any:
			if !list || len(x) == 0 {
				return nil, guardErr(GuardErrUnknownScopeKey, "scope key %q does not take a list", k)
			}
			for _, it := range x {
				str, ok := it.(string)
				if !ok || str == "" {
					return nil, guardErr(GuardErrUnknownScopeKey, "scope key %q holds a non-string", k)
				}
				s[k] = append(s[k], str)
			}
		default:
			return nil, guardErr(GuardErrUnknownScopeKey, "scope key %q has type %T", k, v)
		}
		sort.Strings(s[k])
	}
	_, hasClass := s["class_id"]
	_, hasKind := s["kind"]
	_, hasCode := s["reason_code"]
	if !hasClass && !(hasKind && hasCode) {
		return nil, guardErr(GuardErrAnchorRequired, "scope needs class_id, or kind + reason_code")
	}
	return s, nil
}

func (s guardScope) json() string {
	out := map[string]any{}
	for k, v := range s {
		if len(v) == 1 {
			out[k] = v[0]
		} else {
			out[k] = v
		}
	}
	b, _ := json.Marshal(out) // map keys marshal sorted: canonical form
	return string(b)
}

// excFacts is what the matcher and the verdict rules read of one exception.
type excFacts struct {
	ID, Project, ClassID, RootClassID, SourceKind, Kind, ReasonCode, Retry string
	RaisedBy, RaisedByProfile, Template, OpenedAt, Status                  string
	ResolvedBy, ResolutionReason, CauseID, Owner, TaskID                   string
}

const excFactsSelect = `SELECT e.id, e.project, e.class_id, COALESCE(c.merged_into, e.class_id), e.source_kind, e.kind,
	e.reason_code, e.retry_class, COALESCE(e.raised_by, ''),
	COALESCE((SELECT a.profile_slug FROM agents a WHERE a.name = e.raised_by AND a.project = e.project LIMIT 1), ''),
	COALESCE(c.template, ''), e.opened_at, e.status, COALESCE(e.resolved_by, ''), COALESCE(e.resolution_reason, ''),
	COALESCE(e.cause_id, ''), COALESCE(e.owner, ''), COALESCE(e.task_id, '')
	FROM exceptions e LEFT JOIN exception_classes c ON c.id = e.class_id`

func scanExcFacts(sc interface{ Scan(...any) error }) (excFacts, error) {
	var f excFacts
	err := sc.Scan(&f.ID, &f.Project, &f.ClassID, &f.RootClassID, &f.SourceKind, &f.Kind, &f.ReasonCode, &f.Retry,
		&f.RaisedBy, &f.RaisedByProfile, &f.Template, &f.OpenedAt, &f.Status, &f.ResolvedBy, &f.ResolutionReason,
		&f.CauseID, &f.Owner, &f.TaskID)
	return f, err
}

func excFactsTx(q excQ, id string) (excFacts, error) {
	f, err := scanExcFacts(q.QueryRow(excFactsSelect+` WHERE e.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return f, guardErr(GuardErrNotFound, "exception %s not found", id)
	}
	return f, err
}

func excFactsListTx(q excQ, where string, args ...any) ([]excFacts, error) {
	rows, err := q.Query(excFactsSelect+` WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []excFacts
	for rows.Next() {
		f, err := scanExcFacts(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func anyOf(vals []string, v string) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}

// matches is the whole matcher: pure, O(clauses), no I/O.
func (s guardScope) matches(f excFacts) bool {
	for k, vals := range s {
		ok := false
		switch k {
		case "class_id":
			ok = anyOf(vals, f.ClassID) || anyOf(vals, f.RootClassID)
		case "kind":
			ok = anyOf(vals, f.Kind)
		case "reason_code":
			ok = anyOf(vals, f.ReasonCode)
		case "retry_class":
			ok = anyOf(vals, f.Retry)
		case "project":
			ok = anyOf(vals, f.Project)
		case "source_kind":
			ok = anyOf(vals, f.SourceKind)
		case "raised_by_profile":
			ok = f.RaisedByProfile != "" && anyOf(vals, f.RaisedByProfile)
		case "template_prefix":
			ok = strings.HasPrefix(f.Template, vals[0])
		}
		if !ok {
			return false
		}
	}
	return true
}

// protectedTx reports whether f belongs to a protected class, directly or, for
// a systemic, through the instances it was opened for.
func protectedTx(q excQ, f excFacts) (bool, error) {
	if guardProtectedKinds[f.Kind] {
		return true, nil
	}
	if f.SourceKind != excSourceClassBudget {
		return false, nil
	}
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM exceptions WHERE cause_id = ? AND kind IN ('identity', 'auth', 'self_grading', 'integrity')`,
		f.ID).Scan(&n)
	return n > 0, err
}

// ---------------------------------------------------------------------------
// Verdict rules (design §4.2 replay = §5.1 settling)

const (
	verdictAgree     = "agree"
	verdictConflict  = "conflict"
	verdictUndecided = "undecided"
)

// guardVerdictTx classifies what actually happened to f against a guard's
// action. final=true requires the outcome to be settled (live hits); replay
// accepts an unresolved, unlinked instance as agree (design §4.2).
func guardVerdictTx(q excQ, action string, params map[string]string, f excFacts, final bool) (string, error) {
	switch action {
	case GuardActionSuppress:
		if f.CauseID != "" {
			var status, reason string
			err := q.QueryRow(`SELECT status, COALESCE(resolution_reason, '') FROM exceptions WHERE id = ?`, f.CauseID).Scan(&status, &reason)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
			switch {
			case status == "resolved" && reason == "fixed":
				return verdictConflict, nil
			case status == "resolved":
				return verdictAgree, nil
			case status == "open":
				return verdictUndecided, nil
			}
		}
		if final && f.Status != "resolved" {
			return verdictUndecided, nil
		}
		return verdictAgree, nil
	case GuardActionResolveSystemic:
		if f.Status != "resolved" {
			return verdictUndecided, nil
		}
		if f.ResolutionReason == params["reason"] {
			return verdictAgree, nil
		}
		return verdictConflict, nil
	case GuardActionRoute:
		if f.Status != "resolved" || f.ResolutionReason != "fixed" {
			return verdictUndecided, nil
		}
		rows, err := q.Query(`SELECT t.profile_slug FROM obligations o JOIN tasks t ON t.id = json_extract(o.discharge_evidence, '$.action_ref')
			WHERE o.subject_kind = 'exception' AND o.subject_id = ? AND t.status = 'done'`, f.ID)
		if err != nil {
			return "", err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return "", err
			}
			if p == params["profile"] {
				return verdictAgree, nil
			}
		}
		return verdictConflict, rows.Err()
	}
	return "", guardErr(GuardErrBadAction, "unknown action %q", action)
}

// GuardReplay is the 90-day blast radius computed at compile (design §4.2).
type GuardReplay struct {
	WindowDays  int      `json:"window_days"`
	Matched     int      `json:"matched"`
	Agree       int      `json:"agree"`
	Conflicts   int      `json:"conflicts"`
	Undecided   int      `json:"undecided"`
	ConflictIDs []string `json:"conflict_ids"`
	ComputedAt  string   `json:"computed_at"`
}

// replayGuardTx applies the matcher to every exception of the origin's
// (kind, reason_code) bucket opened in the last 90 days (the origin excluded)
// and classifies each match against what actually happened.
func replayGuardTx(q excQ, origin excFacts, scope guardScope, action string, params map[string]string, now time.Time) (GuardReplay, error) {
	r := GuardReplay{WindowDays: int(guardReplayWindow / (24 * time.Hour)), ConflictIDs: []string{},
		ComputedAt: now.UTC().Format(memoryTimeFmt)}
	since := now.Add(-guardReplayWindow).UTC().Format(memoryTimeFmt)
	cands, err := excFactsListTx(q, `e.kind = ? AND e.reason_code = ? AND e.opened_at >= ? AND e.id <> ? ORDER BY e.opened_at, e.id`,
		origin.Kind, origin.ReasonCode, since, origin.ID)
	if err != nil {
		return r, fmt.Errorf("guard replay: %w", err)
	}
	for _, f := range cands {
		if !scope.matches(f) {
			continue
		}
		r.Matched++
		v, err := guardVerdictTx(q, action, params, f, false)
		if err != nil {
			return r, err
		}
		switch v {
		case verdictAgree:
			r.Agree++
		case verdictConflict:
			r.Conflicts++
			if len(r.ConflictIDs) < guardConflictIDs {
				r.ConflictIDs = append(r.ConflictIDs, f.ID)
			}
		default:
			r.Undecided++
		}
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Compile (design §4.2)

// GuardCompile is one compile_resolution request.
type GuardCompile struct {
	ExceptionID  string
	Caller       string // the compiling agent, or "human" for the relay on behalf of the human rung
	Point        string
	Action       string
	ActionParams map[string]string
	Scope        map[string]any // nil: {class_id, project} of the origin
	Global       bool           // human generalize scope=global only
	Lane         bool           // human generalize scope=lane: + the anchor's raised_by_profile
	ExpiresIn    time.Duration
	Source       string // GuardSourceAgent (default) | GuardSourceHuman
	Now          time.Time
}

// Guard is one compiled_guards row.
type Guard struct {
	ID, Project, ClassID, FromExceptionID, Point, Action string
	ActionParams                                         map[string]string
	Scope                                                guardScope
	Specificity                                          int
	Mode                                                 string
	Replay                                               GuardReplay
	ShadowHits, ShadowConflicts, LiveHits, Regressions   int
	AnchorAt, CreatedBy, ChallengedBy, Source            string
	CreatedAt, PromotedAt, LastHitAt, ExpiresAt          string
	EndedAt, EndedReason                                 string
}

// guardShort is the 8-char id prefix used in summaries (safe on short ids).
func guardShort(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func newGuardID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// originResolverTx names the agent whose transition resolved the origin, as
// far as the typed row allows: self is the raiser, supervisor the task's
// dispatcher, peer the task's assignee, human is "human". "" when unknown.
func originResolverTx(q excQ, f excFacts) string {
	switch f.ResolvedBy {
	case "self":
		return f.RaisedBy
	case "human":
		return "human"
	case "supervisor", "peer":
		if f.TaskID == "" {
			return ""
		}
		col := "dispatched_by"
		if f.ResolvedBy == "peer" {
			col = "assigned_to"
		}
		var who sql.NullString
		_ = q.QueryRow(`SELECT `+col+` FROM tasks WHERE id = ?`, f.TaskID).Scan(&who)
		return who.String
	}
	return ""
}

// guardAuditTx appends one audit_log entry on the caller's writer tx (the
// audit helper takes its own writer, which would self-deadlock here).
func guardAuditTx(q excQ, project, actor, action, guardID, summary string, details map[string]any, now string) error {
	det := ""
	if details != nil {
		b, _ := json.Marshal(details)
		det = string(b)
	}
	_, err := q.Exec(`INSERT INTO audit_log (id, project, actor, action, resource_type, resource_id, summary, details, reason, created_at, trace_id)
		VALUES (?, ?, ?, ?, 'guard', ?, ?, ?, '', ?, '')`, newGuardID(), project, actor, action, guardID, summary, det, now)
	if err != nil {
		return fmt.Errorf("guard audit: %w", err)
	}
	return nil
}

// CompileResolutionTx validates a compile request, replays the guard over 90
// days of history and inserts it in shadow, all on q (the writer tx that also
// resolves the origin when the human rung compiles). Every refusal leaves 0 rows.
func CompileResolutionTx(q excQ, in GuardCompile) (Guard, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	ts := now.UTC().Format(memoryTimeFmt)
	if in.Source == "" {
		in.Source = GuardSourceAgent
	}
	switch {
	case in.ExpiresIn <= 0:
		return Guard{}, guardErr(GuardErrExpiryRequired, "expires_in is required (1d..90d)")
	case in.ExpiresIn < guardMinExpiry:
		return Guard{}, guardErr(GuardErrExpiryRequired, "expires_in %s is below 1d", in.ExpiresIn)
	case in.ExpiresIn > guardMaxExpiry:
		return Guard{}, guardErr(GuardErrExpiryTooLong, "expires_in %s is above 90d", in.ExpiresIn)
	}
	if !guardActions[in.Point][in.Action] {
		return Guard{}, guardErr(GuardErrBadAction, "action %q is not valid at point %q", in.Action, in.Point)
	}
	origin, err := excFactsTx(q, in.ExceptionID)
	if err != nil {
		return Guard{}, err
	}
	if origin.Status != "resolved" {
		return Guard{}, guardErr(GuardErrNotResolved, "exception %s is %s", origin.ID, origin.Status)
	}
	if prot, err := protectedTx(q, origin); err != nil {
		return Guard{}, err
	} else if prot {
		return Guard{}, guardErr(GuardErrProtectedClass, "kind %q is a hard-coded safety guard, never compiled", origin.Kind)
	}
	human := in.Source == GuardSourceHuman && in.Caller == "human"
	systemic := origin.SourceKind == excSourceClassBudget
	// anchor is what the scope, class and replay are read from: the origin,
	// except when the human generalizes a systemic into an open-point guard
	// (§7 accept_known), which matches the systemic's instances, not itself.
	anchor := origin
	if human && systemic && in.Point == GuardPointOpen {
		if anchor, err = linkedAnchorTx(q, origin.ID); err != nil {
			return Guard{}, err
		}
		systemic = false
	}
	if (in.Point == GuardPointLadder) != systemic {
		return Guard{}, guardErr(GuardErrBadAction, "point %q does not apply to a %s exception", in.Point, origin.SourceKind)
	}
	if in.Source == GuardSourceHuman && !human {
		return Guard{}, guardErr(GuardErrForbidden, "human_generalize is compiled by the relay for the human only")
	}
	if !human {
		resolver := originResolverTx(q, origin)
		if in.Caller == "" || (!strings.EqualFold(in.Caller, resolver) && !strings.EqualFold(in.Caller, origin.Owner)) {
			return Guard{}, guardErr(GuardErrForbidden, "only the origin's resolver or the systemic owner may compile")
		}
	}
	params := map[string]string{}
	for k, v := range in.ActionParams {
		params[k] = v
	}
	switch in.Action {
	case GuardActionResolveSystemic:
		if params["reason"] == "" {
			return Guard{}, guardErr(GuardErrBadAction, "resolve_systemic needs {reason}")
		}
	case GuardActionRoute:
		var n int
		if err := q.QueryRow(`SELECT COUNT(*) FROM agents WHERE project = ? AND profile_slug = ?`, origin.Project, params["profile"]).Scan(&n); err != nil {
			return Guard{}, err
		}
		if params["profile"] == "" || n == 0 {
			return Guard{}, guardErr(GuardErrBadAction, "route profile %q has no agent in %s", params["profile"], origin.Project)
		}
	}
	raw := in.Scope
	if raw == nil {
		raw = map[string]any{"class_id": anchor.RootClassID, "project": origin.Project}
		if in.Global {
			delete(raw, "project")
		}
		if in.Lane && anchor.RaisedByProfile != "" {
			raw["raised_by_profile"] = anchor.RaisedByProfile
		}
	}
	scope, err := parseGuardScope(raw)
	if err != nil {
		return Guard{}, err
	}
	project := origin.Project
	if _, has := scope["project"]; !has {
		if !(in.Global && human) {
			return Guard{}, guardErr(GuardErrProjectRequired, "scope needs project unless a human generalizes it global")
		}
		project = "*"
	}
	if !scope.matches(anchor) {
		return Guard{}, guardErr(GuardErrScopeExcludes, "scope %s does not match its origin %s", scope.json(), anchor.ID)
	}
	replay, err := replayGuardTx(q, anchor, scope, in.Action, params, now)
	if err != nil {
		return Guard{}, err
	}
	var classID any
	if c, ok := scope["class_id"]; ok && len(c) == 1 {
		classID = c[0]
	}
	pj, _ := json.Marshal(params)
	rj, _ := json.Marshal(replay)
	id := newGuardID()
	expires := now.Add(in.ExpiresIn).UTC().Format(memoryTimeFmt)
	if _, err := q.Exec(`INSERT INTO compiled_guards (id, project, class_id, from_exception_id, point, action, action_params,
		scope_expr, specificity, mode, replay_json, created_by, source, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'shadow', ?, ?, ?, ?, ?)`,
		id, project, classID, origin.ID, in.Point, in.Action, string(pj), scope.json(), len(scope), string(rj),
		in.Caller, in.Source, ts, expires); err != nil {
		return Guard{}, fmt.Errorf("insert guard: %w", err)
	}
	if err := guardAuditTx(q, origin.Project, in.Caller, "guard_compile", id,
		fmt.Sprintf("guard %s compiled (%s/%s, shadow) from %s", guardShort(id), in.Point, in.Action, origin.ID),
		map[string]any{"scope": scope.json(), "replay": replay, "expires_at": expires}, ts); err != nil {
		return Guard{}, err
	}
	return getGuardTx(q, id)
}

// CompileResolution runs CompileResolutionTx in its own writer tx.
func (d *DB) CompileResolution(in GuardCompile) (Guard, error) {
	tx, err := d.beginWriterTx()
	if err != nil {
		return Guard{}, err
	}
	defer func() { _ = tx.Rollback() }()
	g, err := CompileResolutionTx(tx, in)
	if err != nil {
		return Guard{}, err
	}
	return g, tx.Commit()
}

const guardColumns = `id, project, COALESCE(class_id, ''), from_exception_id, point, action, action_params, scope_expr,
	specificity, mode, replay_json, shadow_hits, shadow_conflicts, live_hits, regressions, COALESCE(anchor_at, ''),
	created_by, COALESCE(challenged_by, ''), source, created_at, COALESCE(promoted_at, ''), COALESCE(last_hit_at, ''),
	expires_at, COALESCE(ended_at, ''), COALESCE(ended_reason, '')`

func scanGuard(sc interface{ Scan(...any) error }) (Guard, error) {
	var g Guard
	var params, scope, replay string
	err := sc.Scan(&g.ID, &g.Project, &g.ClassID, &g.FromExceptionID, &g.Point, &g.Action, &params, &scope,
		&g.Specificity, &g.Mode, &replay, &g.ShadowHits, &g.ShadowConflicts, &g.LiveHits, &g.Regressions, &g.AnchorAt,
		&g.CreatedBy, &g.ChallengedBy, &g.Source, &g.CreatedAt, &g.PromotedAt, &g.LastHitAt,
		&g.ExpiresAt, &g.EndedAt, &g.EndedReason)
	if err != nil {
		return g, err
	}
	_ = json.Unmarshal([]byte(params), &g.ActionParams)
	_ = json.Unmarshal([]byte(replay), &g.Replay)
	var raw map[string]any
	if err := json.Unmarshal([]byte(scope), &raw); err != nil {
		return g, fmt.Errorf("guard %s scope: %w", g.ID, err)
	}
	if g.Scope, err = parseGuardScope(raw); err != nil {
		return g, fmt.Errorf("guard %s scope: %w", g.ID, err)
	}
	return g, nil
}

func getGuardTx(q excQ, id string) (Guard, error) {
	g, err := scanGuard(q.QueryRow(`SELECT `+guardColumns+` FROM compiled_guards WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return g, guardErr(GuardErrNotFound, "guard %s not found", id)
	}
	return g, err
}

// GetGuard reads one guard.
func (d *DB) GetGuard(id string) (Guard, error) { return getGuardTx(d.ro(), id) }

// ---------------------------------------------------------------------------
// Evaluation at instance open (design §4.1, point = open)

// settingIntTx reads an int knob on q (the open path has no *DB): the same
// empty/unparsable -> def and clamp contract as SettingInt, without the WARN.
func settingIntTx(q excQ, key string, def, min, max int) int {
	var raw string
	_ = q.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	var v int
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &v); err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// budgetForTx is budgetFor read on q.
func budgetForTx(q excQ, kind, code string) (classBudget, bool) {
	var b classBudget
	var enabled int
	err := q.QueryRow(`SELECT intensity, period_s, enabled FROM class_budgets WHERE kind = ? AND reason_code IN (?, '*')
		ORDER BY reason_code = '*' LIMIT 1`, kind, code).Scan(&b.Intensity, &b.PeriodS, &enabled)
	if err != nil || enabled != 1 || b.Intensity <= 0 || b.PeriodS <= 0 {
		return classBudget{}, false
	}
	return b, true
}

// evaluateOpenGuardsTx runs every shadow/active open-point guard against the
// exception just inserted, inside the producer's writer tx: shadow guards
// record, and at most ONE active guard acts (highest specificity, newest
// promotion, lowest id). Two active winners of equal specificity with
// different actions act on neither and both take a conflict. Any error fails
// the producer's tx (design §0: same transaction).
func evaluateOpenGuardsTx(q excQ, excID, at string) error {
	_, err := evaluateGuardsTx(q, GuardPointOpen, excID, at)
	return err
}

// evaluateGuardsTx is the evaluation at one point; it returns the guard that
// acted (nil when none did). The ladder point runs it on the systemic at
// match_precedent (S2b).
func evaluateGuardsTx(q excQ, point, excID, at string) (*Guard, error) {
	f, err := excFactsTx(q, excID)
	if err != nil {
		return nil, err
	}
	if prot, err := protectedTx(q, f); err != nil || prot {
		return nil, err
	}
	rows, err := q.Query(`SELECT `+guardColumns+` FROM compiled_guards
		WHERE point = ? AND mode IN ('shadow', 'active') AND (class_id IN (?, ?) OR class_id IS NULL)
		  AND project IN (?, '*') AND expires_at > ?`, point, f.ClassID, f.RootClassID, f.Project, at)
	if err != nil {
		return nil, fmt.Errorf("guard candidates: %w", err)
	}
	var matched []Guard
	for rows.Next() {
		g, err := scanGuard(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if g.Scope.matches(f) {
			matched = append(matched, g)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		return nil, nil
	}
	var active []Guard
	for _, g := range matched {
		if g.Mode == GuardModeActive {
			active = append(active, g)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		a, b := active[i], active[j]
		if a.Specificity != b.Specificity {
			return a.Specificity > b.Specificity
		}
		if a.PromotedAt != b.PromotedAt {
			return a.PromotedAt > b.PromotedAt
		}
		return a.ID < b.ID
	})
	actor := ""
	conflicted := map[string]bool{}
	if len(active) > 0 {
		actor = active[0].ID
		for _, g := range active[1:] {
			if g.Specificity == active[0].Specificity && (g.Action != active[0].Action || !sameParams(g.ActionParams, active[0].ActionParams)) {
				conflicted[g.ID], conflicted[active[0].ID] = true, true
			}
		}
		if conflicted[actor] {
			actor = ""
		}
	}
	massDemoted := ""
	if actor != "" && active[0].Action == GuardActionSuppress {
		mass, err := suppressMassTx(q, active[0], f, at)
		if err != nil {
			return nil, err
		}
		if mass {
			if err := demoteGuardTx(q, active[0], guardEndedDemotedMass, "system", at); err != nil {
				return nil, err
			}
			massDemoted, actor = actor, ""
		}
	}
	for _, g := range matched {
		acted := 0
		mode := g.Mode
		if g.ID == actor {
			acted = 1
		}
		if g.ID == massDemoted {
			mode = GuardModeShadow // this instance counts toward the budget again
		}
		if _, err := q.Exec(`INSERT INTO guard_hits (guard_id, exception_id, mode, acted, at) VALUES (?, ?, ?, ?, ?)`,
			g.ID, excID, mode, acted, at); err != nil {
			return nil, fmt.Errorf("guard hit: %w", err)
		}
		set := `shadow_hits = shadow_hits + 1`
		if acted == 1 {
			set = `live_hits = live_hits + 1`
		} else if mode == GuardModeActive {
			set = `shadow_hits = shadow_hits` // an outranked active guard records its would-have only
		}
		if conflicted[g.ID] {
			set += `, shadow_conflicts = shadow_conflicts + 1`
		}
		if _, err := q.Exec(`UPDATE compiled_guards SET `+set+`, last_hit_at = ? WHERE id = ?`, at, g.ID); err != nil {
			return nil, fmt.Errorf("guard counters: %w", err)
		}
	}
	for id := range conflicted {
		g, err := getGuardTx(q, id)
		if err != nil {
			return nil, err
		}
		if g.Mode == GuardModeActive {
			if err := demoteGuardTx(q, g, guardEndedDemotedConflct, "system", at); err != nil {
				return nil, err
			}
		}
	}
	if actor == "" {
		return nil, nil
	}
	return &active[0], nil
}

func sameParams(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// suppressMassTx is the suppress regression backstop (design §6): acting on
// this instance would put the guard's suppressed instances inside one budget
// period above 2x the class intensity, i.e. the known friction is escalating.
func suppressMassTx(q excQ, g Guard, f excFacts, at string) (bool, error) {
	b, ok := budgetForTx(q, f.Kind, f.ReasonCode)
	if !ok {
		return false, nil
	}
	t, err := time.Parse(memoryTimeFmt, at)
	if err != nil {
		return false, nil
	}
	since := t.Add(-time.Duration(b.PeriodS) * time.Second).UTC().Format(memoryTimeFmt)
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM guard_hits h JOIN exceptions e ON e.id = h.exception_id
		WHERE h.guard_id = ? AND h.acted = 1 AND e.opened_at >= ?`, g.ID, since).Scan(&n); err != nil {
		return false, err
	}
	return n+1 > 2*b.Intensity, nil
}

// demoteGuardTx returns an active guard to shadow (never a silent flap): the
// regression is counted, the shadow evidence restarts from zero so a
// re-promotion needs fresh clean hits, and an audit event records why.
func demoteGuardTx(q excQ, g Guard, reason, actor, at string) error {
	res, err := q.Exec(`UPDATE compiled_guards SET mode = 'shadow', regressions = regressions + 1, shadow_hits = 0,
		shadow_conflicts = 0, anchor_at = NULL, ended_reason = ?, ended_at = ? WHERE id = ? AND mode = 'active'`, reason, at, g.ID)
	if err != nil {
		return fmt.Errorf("demote guard: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	return guardAuditTx(q, g.Project, actor, "guard_demote", g.ID,
		fmt.Sprintf("guard %s demoted to shadow: %s", guardShort(g.ID), reason),
		map[string]any{"reason": reason, "regressions": g.Regressions + 1, "live_hits": g.LiveHits,
			"created_by": g.CreatedBy, "challenged_by": g.ChallengedBy}, at)
}

// ---------------------------------------------------------------------------
// Settling (design §5.1)

// settleGuardHitsTx settles the pending hits of the exceptions selected by
// where (a clause over exceptions aliased e) whose outcome is now known, with
// the replay rule. A shadow conflict bumps shadow_conflicts; a conflict on a
// hit an active guard acted on is an override, i.e. a regression.
func settleGuardHitsTx(q excQ, now, where string, args ...any) error {
	rows, err := q.Query(`SELECT h.guard_id, h.exception_id, h.acted FROM guard_hits h JOIN exceptions e ON e.id = h.exception_id
		WHERE h.verdict = 'pending' AND (`+where+`)`, args...)
	if err != nil {
		return fmt.Errorf("pending guard hits: %w", err)
	}
	type hit struct {
		guard, exc string
		acted      int
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.guard, &h.exc, &h.acted); err != nil {
			_ = rows.Close()
			return err
		}
		hits = append(hits, h)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range hits {
		g, err := getGuardTx(q, h.guard)
		if err != nil {
			return err
		}
		f, err := excFactsTx(q, h.exc)
		if err != nil {
			return err
		}
		v, err := guardVerdictTx(q, g.Action, g.ActionParams, f, true)
		if err != nil {
			return err
		}
		if v == verdictUndecided {
			continue
		}
		if _, err := q.Exec(`UPDATE guard_hits SET verdict = ? WHERE guard_id = ? AND exception_id = ?`, v, h.guard, h.exc); err != nil {
			return fmt.Errorf("settle guard hit: %w", err)
		}
		if v != verdictConflict {
			continue
		}
		if h.acted == 1 {
			if _, err := q.Exec(`UPDATE compiled_guards SET regressions = regressions + 1 WHERE id = ?`, g.ID); err != nil {
				return err
			}
			g.Regressions++
			if g.Mode == GuardModeActive && g.Regressions >= settingIntTx(q, settingGuardDemoteRegressions, defaultGuardDemoteRegressions, 1, 100) {
				if err := demoteGuardTx(q, g, guardEndedDemoted, "system", now); err != nil {
					return err
				}
			}
		} else if _, err := q.Exec(`UPDATE compiled_guards SET shadow_conflicts = shadow_conflicts + 1 WHERE id = ?`, g.ID); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Promotion and renewal (design §5.2, §6)

// PromoteGuard moves a shadow guard to active. Refused unless the caller is
// neither its creator nor the origin's resolver, it has >= guard_promote_hits
// shadow hits with 0 shadow conflicts, 0 replay conflicts (a fresh replay is
// recomputed when the stored one had any), and it is unexpired.
func (d *DB) PromoteGuard(id, caller string, now time.Time) (Guard, error) {
	promoteHits := d.SettingInt(settingGuardPromoteHits, defaultGuardPromoteHits, 1, 1000)
	ts := now.UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return Guard{}, err
	}
	defer func() { _ = tx.Rollback() }()
	g, err := getGuardTx(tx, id)
	if err != nil {
		return Guard{}, err
	}
	origin, err := excFactsTx(tx, g.FromExceptionID)
	if err != nil {
		return Guard{}, err
	}
	resolver := originResolverTx(tx, origin)
	switch {
	case g.Mode != GuardModeShadow || g.ExpiresAt <= ts:
		return Guard{}, guardErr(GuardErrPromoteRefused, "guard is %s (expires %s)", g.Mode, g.ExpiresAt)
	case caller == "" || strings.EqualFold(caller, g.CreatedBy) || (resolver != "" && strings.EqualFold(caller, resolver)):
		return Guard{}, guardErr(GuardErrPromoteRefused, "promotion needs an agent other than the creator and the origin's resolver")
	case g.ShadowHits < promoteHits:
		return Guard{}, guardErr(GuardErrPromoteRefused, "shadow_hits %d < %d", g.ShadowHits, promoteHits)
	case g.ShadowConflicts > 0:
		return Guard{}, guardErr(GuardErrPromoteRefused, "shadow_conflicts %d > 0", g.ShadowConflicts)
	}
	replay := g.Replay
	if replay.Conflicts > 0 {
		if replay, err = replayGuardTx(tx, origin, g.Scope, g.Action, g.ActionParams, now); err != nil {
			return Guard{}, err
		}
		if replay.Conflicts > 0 {
			return Guard{}, guardErr(GuardErrPromoteRefused, "replay has %d conflicts", replay.Conflicts)
		}
	}
	rj, _ := json.Marshal(replay)
	res, err := tx.Exec(`UPDATE compiled_guards SET mode = 'active', challenged_by = ?, promoted_at = ?, anchor_at = ?,
		regressions = 0, replay_json = ?, ended_at = NULL, ended_reason = NULL WHERE id = ? AND mode = 'shadow'`,
		caller, ts, ts, string(rj), id)
	if err != nil {
		return Guard{}, fmt.Errorf("promote guard: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Guard{}, guardErr(GuardErrPromoteRefused, "guard %s changed concurrently", id)
	}
	if err := guardAuditTx(tx, g.Project, caller, "guard_promote", id,
		fmt.Sprintf("guard %s promoted to active by %s", guardShort(id), caller),
		map[string]any{"shadow_hits": g.ShadowHits, "created_by": g.CreatedBy}, ts); err != nil {
		return Guard{}, err
	}
	if err := tx.Commit(); err != nil {
		return Guard{}, err
	}
	return d.GetGuard(id)
}

// RenewGuard extends an active guard by expiresIn (<= 90d from now). Refused
// for its creator, after any regression since promotion, or with no live hit.
// Expiry never auto-renews.
func (d *DB) RenewGuard(id, caller string, expiresIn time.Duration, now time.Time) (Guard, error) {
	ts := now.UTC().Format(memoryTimeFmt)
	switch {
	case expiresIn < guardMinExpiry:
		return Guard{}, guardErr(GuardErrExpiryRequired, "expires_in %s is below 1d", expiresIn)
	case expiresIn > guardMaxExpiry:
		return Guard{}, guardErr(GuardErrExpiryTooLong, "expires_in %s is above 90d", expiresIn)
	}
	tx, err := d.beginWriterTx()
	if err != nil {
		return Guard{}, err
	}
	defer func() { _ = tx.Rollback() }()
	g, err := getGuardTx(tx, id)
	if err != nil {
		return Guard{}, err
	}
	switch {
	case g.Mode != GuardModeActive || g.ExpiresAt <= ts:
		return Guard{}, guardErr(GuardErrRenewRefused, "guard is %s (expires %s)", g.Mode, g.ExpiresAt)
	case caller == "" || strings.EqualFold(caller, g.CreatedBy):
		return Guard{}, guardErr(GuardErrRenewRefused, "renewal needs an agent other than the creator")
	case g.Regressions > 0:
		return Guard{}, guardErr(GuardErrRenewRefused, "%d regressions since promotion", g.Regressions)
	case g.LiveHits < 1:
		return Guard{}, guardErr(GuardErrRenewRefused, "no live hit")
	}
	expires := now.Add(expiresIn).UTC().Format(memoryTimeFmt)
	if _, err := tx.Exec(`UPDATE compiled_guards SET expires_at = ? WHERE id = ? AND mode = 'active'`, expires, id); err != nil {
		return Guard{}, err
	}
	if err := guardAuditTx(tx, g.Project, caller, "guard_renew", id,
		fmt.Sprintf("guard %s renewed to %s by %s", guardShort(id), expires, caller), nil, ts); err != nil {
		return Guard{}, err
	}
	if err := tx.Commit(); err != nil {
		return Guard{}, err
	}
	return d.GetGuard(id)
}

// ---------------------------------------------------------------------------
// Lifecycle sweep (design §6)

// GuardSweep reports what one sweep changed (S2 turns it into messages).
type GuardSweep struct {
	Demoted, Expired, Retired []string
}

// SweepGuards settles every pending hit whose outcome is now known, counts
// anchored recurrences of active fix guards (regression), demotes, expires and
// retires. One writer tx for settling and one per guard that changes; a quiet
// sweep writes nothing.
func (d *DB) SweepGuards(now time.Time) (GuardSweep, error) {
	var out GuardSweep
	ts := now.UTC().Format(memoryTimeFmt)
	var pending int
	if err := d.ro().QueryRow(`SELECT COUNT(*) FROM guard_hits WHERE verdict = 'pending'`).Scan(&pending); err != nil {
		return out, fmt.Errorf("guard sweep: %w", err)
	}
	if pending > 0 {
		if err := d.inGuardTx(func(tx *writerTx) error { return settleGuardHitsTx(tx, ts, `1 = 1`) }); err != nil {
			return out, err
		}
	}
	grace := d.SettingDuration(settingGuardRegressionGrace, defaultGuardRegressionGrace, 0, 30*24*time.Hour)
	retireAfter := d.SettingDuration(settingGuardRetireAfter, defaultGuardRetireAfter, time.Hour, 365*24*time.Hour)
	demoteAt := d.SettingInt(settingGuardDemoteRegressions, defaultGuardDemoteRegressions, 1, 100)
	rows, err := d.ro().Query(`SELECT ` + guardColumns + ` FROM compiled_guards WHERE mode IN ('shadow', 'active') ORDER BY id`)
	if err != nil {
		return out, fmt.Errorf("guard sweep: %w", err)
	}
	var live []Guard
	for rows.Next() {
		g, err := scanGuard(rows)
		if err != nil {
			_ = rows.Close()
			return out, err
		}
		live = append(live, g)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	retireBefore := now.Add(-retireAfter).UTC().Format(memoryTimeFmt)
	for _, g := range live {
		switch {
		case g.ExpiresAt <= ts:
			if err := d.endGuard(g, GuardModeExpired, guardEndedExpired, ts); err != nil {
				return out, err
			}
			out.Expired = append(out.Expired, g.ID)
		case laterTS(g.LastHitAt, laterTS(g.CreatedAt, g.PromotedAt)) < retireBefore:
			if err := d.endGuard(g, GuardModeRetired, guardEndedRetiredNoHits, ts); err != nil {
				return out, err
			}
			out.Retired = append(out.Retired, g.ID)
		case g.Mode == GuardModeActive && g.Point == GuardPointLadder && g.AnchorAt != "":
			demoted, err := d.countRecurrences(g, grace, demoteAt, ts)
			if err != nil {
				return out, err
			}
			if demoted {
				out.Demoted = append(out.Demoted, g.ID)
			}
		}
	}
	return out, nil
}

func (d *DB) inGuardTx(fn func(tx *writerTx) error) error {
	tx, err := d.beginWriterTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) endGuard(g Guard, mode, reason, ts string) error {
	return d.inGuardTx(func(tx *writerTx) error {
		res, err := tx.Exec(`UPDATE compiled_guards SET mode = ?, ended_at = ?, ended_reason = ? WHERE id = ? AND mode = ?`,
			mode, ts, reason, g.ID, g.Mode)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return guardAuditTx(tx, g.Project, "system", "guard_"+mode, g.ID,
			fmt.Sprintf("guard %s %s (%s)", guardShort(g.ID), mode, reason), nil, ts)
	})
}

// countRecurrences applies the anchored-recurrence rule to an active ladder
// guard whose origin decision was a fix: every matching systemic opened after
// anchor_at + grace is a regression (the repair did not take). The count is
// derived from the anchor, so re-sweeping never double counts.
func (d *DB) countRecurrences(g Guard, grace time.Duration, demoteAt int, ts string) (bool, error) {
	var reason string
	if err := d.ro().QueryRow(`SELECT COALESCE(resolution_reason, '') FROM exceptions WHERE id = ?`, g.FromExceptionID).Scan(&reason); err != nil {
		return false, nil
	}
	if reason != "fixed" {
		return false, nil
	}
	anchor, err := time.Parse(memoryTimeFmt, g.AnchorAt)
	if err != nil {
		return false, nil
	}
	after := anchor.Add(grace).UTC().Format(memoryTimeFmt)
	cands, err := excFactsListTx(d.ro(), `e.source_kind = ? AND e.opened_at > ? AND e.id <> ?`, excSourceClassBudget, after, g.FromExceptionID)
	if err != nil {
		return false, err
	}
	n := 0
	for _, f := range cands {
		if g.Scope.matches(f) {
			n++
		}
	}
	if n <= g.Regressions {
		return false, nil
	}
	demoted := n >= demoteAt
	stored := n
	if demoted {
		stored = n - 1 // demoteGuardTx records the last one
	}
	err = d.inGuardTx(func(tx *writerTx) error {
		if _, err := tx.Exec(`UPDATE compiled_guards SET regressions = ? WHERE id = ? AND mode = 'active'`, stored, g.ID); err != nil {
			return err
		}
		if !demoted {
			return nil
		}
		g.Regressions = stored
		return demoteGuardTx(tx, g, guardEndedDemoted, "system", ts)
	})
	return demoted, err
}
