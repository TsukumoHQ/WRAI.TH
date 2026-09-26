package db

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Gated contradiction detection (design 8d107daa, ruling cto-tsukumo ed744dee).
// Four structural gates run before any judge: subject overlap (G1), the scope
// lattice (G2), validity overlap (G3) and layer compatibility (G4). A pair that
// passes all four is a candidate: a duplicate or a contradiction, which only a
// claiming agent tells apart. A narrower scope, a decision over a constraint,
// or a disjoint validity window is an exception, recorded as an implicit
// precedence edge (declared=0), never a conflict. No model runs anywhere, the
// write path gains at most one read-only query (the D1 hint), and no write is
// ever refused.

// Settings.
const (
	SettingContradictionMode   = "contradiction_mode"
	ContradictionModeOff       = "off"
	ContradictionModeDetect    = "detect"
	settingContradictionCursor = "contradiction_cursor_rev"
	// settingContradictionBackfill marks the one-shot audited pass over the
	// live HIGH memories that predate detection (ruling OQ3).
	settingContradictionBackfill = "contradiction_backfill_v1"
	settingOverlapMin            = "contradiction_overlap_min"
	settingConflictLeaseTTL      = "conflict_lease_ttl"
)

// Conflict states (the lease pattern of task_lease.go).
const (
	ConflictDetected  = "detected"
	ConflictClaimed   = "claimed"
	ConflictResolved  = "resolved"
	ConflictEscalated = "escalated"
)

// Knowledge edge kinds and the rules that write them.
const (
	KEdgeOverrides  = "overrides"
	KEdgeAmends     = "amends"
	KEdgeSupersedes = "supersedes"
	KEdgeDuplicates = "duplicates"

	ruleScopeRank        = "scope_rank"
	ruleLayerPrecedence  = "layer_precedence"
	ruleDisjointValidity = "disjoint_validity"
)

const (
	// overlapMinDefault is G1's value word-Jaccard floor (ruling OQ1).
	overlapMinDefault = 0.35
	// overlapTopK is how many FTS5 results of N's rarest value tokens a peer
	// must be among to pass G1 on value overlap.
	overlapTopK = 5
	// overlapQueryTokens bounds the FTS5 query to N's rarest value tokens.
	overlapQueryTokens = 12
	// conflictLeaseDefault is conflict_lease_ttl's default.
	conflictLeaseDefault = 2 * time.Hour
	// escalateAfterClaims is B3's "two failed claims" rule.
	escalateAfterClaims = 2
	// contradictionBatch bounds the knowledge_log rows one tick reads.
	contradictionBatch = 200
	// detectorAgent authors the rows the sweeper writes.
	detectorAgent = "relay-sweeper"

	excSourceContradiction = "contradiction"
)

func migrateContradictions(conn *sql.DB) {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS knowledge_edges (
		src_memory_id TEXT NOT NULL,
		dst_memory_id TEXT NOT NULL,
		kind          TEXT NOT NULL,
		declared      INTEGER NOT NULL,
		rule          TEXT NOT NULL,
		created_by    TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		PRIMARY KEY (src_memory_id, dst_memory_id, kind)
	) WITHOUT ROWID`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_knowledge_edges_dst ON knowledge_edges(dst_memory_id)`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS knowledge_conflicts (
		id                   TEXT PRIMARY KEY,
		project              TEXT NOT NULL,
		members              TEXT NOT NULL,
		members_hash         TEXT NOT NULL UNIQUE,
		kind                 TEXT NOT NULL,
		gate_evidence        TEXT NOT NULL,
		detected_by          TEXT NOT NULL,
		state                TEXT NOT NULL,
		lease_holder         TEXT,
		lease_expires_at     TEXT,
		failed_claims        INTEGER NOT NULL DEFAULT 0,
		resolution           TEXT,
		resolution_memory_id TEXT,
		resolved_by          TEXT,
		rationale            TEXT,
		audit                TEXT,
		created_at           TEXT NOT NULL,
		resolved_at          TEXT
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_knowledge_conflicts_open ON knowledge_conflicts(state, project)
		WHERE state IN ('detected', 'claimed')`)
	// invalidated_by is read by dedicated queries only; memorySelectCols is
	// untouched (the column-count lockstep rule).
	ensureColumns(conn, "memories", map[string]string{"invalidated_by": "TEXT"})
	// Term document frequencies, to query FTS5 with N's rarest value tokens.
	_, _ = conn.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts_vocab USING fts5vocab(memories_fts, row)`)
	// D2 starts at the current log head; history is covered once by the
	// audited backfill pass instead of a replay.
	_, _ = conn.Exec(`INSERT OR IGNORE INTO settings (key, value)
		SELECT ?, CAST(COALESCE(MAX(rev), 0) AS TEXT) FROM knowledge_log`, settingContradictionCursor)
}

// kmem is one memory as the gates see it.
type kmem struct {
	ID, Key, Value, Tags, Scope, Project, Agent, Layer string
	ValidFrom, ValidUntil, CreatedAt                   string
}

const kmemCols = `m.id, m.key, m.value, m.tags, m.scope, m.project, m.agent_name, m.layer,
	COALESCE(m.valid_from, ''), COALESCE(m.valid_until, ''), m.created_at`

func scanKMem(s interface{ Scan(...any) error }) (kmem, error) {
	var m kmem
	err := s.Scan(&m.ID, &m.Key, &m.Value, &m.Tags, &m.Scope, &m.Project, &m.Agent, &m.Layer,
		&m.ValidFrom, &m.ValidUntil, &m.CreatedAt)
	return m, err
}

// isHighLayer: only constraints and decisions can conflict (G4).
func isHighLayer(layer string) bool {
	return layer == "constraints" || layer == "decision" || layer == "decisions"
}

func isDecisionLayer(layer string) bool { return layer == "decision" || layer == "decisions" }

// liveHighClause filters live HIGH-layer memories (alias m).
const liveHighClause = `m.archived_at IS NULL AND m.status != 'stale' AND (m.valid_until IS NULL OR m.valid_until > ?)
	AND m.layer IN ('constraints', 'decision', 'decisions')`

// --- G1: subject overlap -------------------------------------------------

var (
	valueTokenRe    = regexp.MustCompile(`[a-z][a-z0-9]{3,}`)
	valueStopTokens = map[string]bool{}
)

func init() {
	for _, w := range strings.Fields("the a an of to for in on and or is are be with no not must never always use via per") {
		valueStopTokens[w] = true
	}
}

// valueTokens is the G1 word set: lowercase words of >= 4 characters starting
// with a letter, stop-words removed (the tokenization the §1 replay measured).
func valueTokens(v string) map[string]bool {
	out := map[string]bool{}
	for _, t := range valueTokenRe.FindAllString(strings.ToLower(v), -1) {
		if !valueStopTokens[t] {
			out[t] = true
		}
	}
	return out
}

// gateText is the text G1 compares: a decision's decision + rationale (its
// JSON field names would make every pair of decisions overlap), else the value.
func gateText(m kmem) string {
	if isDecisionLayer(m.Layer) {
		var dv DecisionValue
		if json.Unmarshal([]byte(m.Value), &dv) == nil && dv.Decision != "" {
			return dv.Decision + " " + dv.Rationale
		}
	}
	return m.Value
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// declaredSubject is the normalized `subject:<x>` tag, or "".
func declaredSubject(tagsJSON string) string {
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	for _, t := range tags {
		if s, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(t)), "subject:"); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// decisionArea is a decision's normalized area. The catch-all "general" (the
// default of an omitted area) names no subject, so it is never a G1 signal:
// DEC-general-1..9 share an area and nothing else.
func decisionArea(m kmem) string {
	if !isDecisionLayer(m.Layer) {
		return ""
	}
	var dv DecisionValue
	if json.Unmarshal([]byte(m.Value), &dv) != nil {
		return ""
	}
	a := strings.ToLower(strings.TrimSpace(dv.Area))
	if a == "general" {
		return ""
	}
	return a
}

// g1Signal decides G1 for one pair. inTopK says whether M was among N's FTS5
// top-K; sim is the value word-Jaccard. The key string is never a signal.
func g1Signal(n, m kmem, inTopK bool, sim, minSim float64) string {
	if s := declaredSubject(n.Tags); s != "" && s == declaredSubject(m.Tags) {
		return "subject"
	}
	if a := decisionArea(n); a != "" && a == decisionArea(m) {
		return "area"
	}
	if inTopK && sim >= minSim {
		return "value"
	}
	return ""
}

// --- G2: scope lattice ----------------------------------------------------

// scopeRank orders agent < team < project < org < global (team and org are
// reserved ranks: no memory uses them yet).
func scopeRank(scope string) int {
	switch scope {
	case "agent":
		return 0
	case "team":
		return 1
	case "project":
		return 2
	case "org":
		return 3
	case "global":
		return 4
	}
	return 2
}

// extentsMeet reports whether the extents where n and m apply intersect.
func extentsMeet(n, m kmem) bool {
	if n.Scope == "global" || m.Scope == "global" {
		return true
	}
	if n.Project != m.Project {
		return false
	}
	if n.Scope == "agent" && m.Scope == "agent" {
		return n.Agent == m.Agent
	}
	return true
}

// sameKey: the same key in the same scope (and agent) is the sibling path
// (D3), not a cross-key overlap.
func sameKey(n, m kmem) bool {
	if n.Key != m.Key || n.Scope != m.Scope {
		return false
	}
	switch n.Scope {
	case "global":
		return true
	case "agent":
		return n.Project == m.Project && n.Agent == m.Agent
	}
	return n.Project == m.Project
}

// --- G3: validity ---------------------------------------------------------

func memTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := parseAnyMemoryTime(s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// window is [valid_from (default created_at), valid_until); ok=false = unbounded.
func (m kmem) window() (from time.Time, until time.Time, bounded bool) {
	from, _ = memTime(m.ValidFrom)
	if from.IsZero() {
		from, _ = memTime(m.CreatedAt)
	}
	until, bounded = memTime(m.ValidUntil)
	return from, until, bounded
}

func windowsOverlap(n, m kmem) bool {
	nf, nu, nb := n.window()
	mf, mu, mb := m.window()
	if nb && !nu.After(mf) {
		return false
	}
	if mb && !mu.After(nf) {
		return false
	}
	return true
}

// --- the verdict ----------------------------------------------------------

// pairVerdict is what the gates decide for one pair.
type pairVerdict struct {
	Candidate bool
	// Edge is an exception to record (Candidate=false): Src precedes Dst.
	Edge                *knowledgeEdge
	Signal              string
	Similarity          float64
	Evidence            map[string]any
	DeclaredExceptionOf string // non-empty: an agent-declared edge already exempts the pair
}

type knowledgeEdge struct {
	Src, Dst, Kind, Rule string
}

// evaluatePair runs G2-G4 on a pair that passed G1 (signal non-empty).
// declared reports an agent-declared overrides/amends edge between the two.
func evaluatePair(n, m kmem, signal string, sim float64, declared bool) (pairVerdict, bool) {
	v := pairVerdict{Signal: signal, Similarity: sim}
	if signal == "" || !extentsMeet(n, m) || sameKey(n, m) {
		return v, false
	}
	if !isHighLayer(n.Layer) || !isHighLayer(m.Layer) {
		return v, false // G4: behavior / context / state never conflict
	}
	ev := map[string]any{
		"g1": map[string]any{"signal": signal, "similarity": round3(sim)},
		"g2": map[string]any{"ranks": []string{n.Scope, m.Scope}},
	}
	v.Evidence = ev
	// G2: declared exception, then the scope lattice.
	if declared {
		v.DeclaredExceptionOf = "declared_edge"
		return v, true
	}
	if rn, rm := scopeRank(n.Scope), scopeRank(m.Scope); rn != rm {
		src, dst := n, m
		if rm < rn {
			src, dst = m, n
		}
		v.Edge = &knowledgeEdge{Src: src.ID, Dst: dst.ID, Kind: KEdgeOverrides, Rule: ruleScopeRank}
		return v, true
	}
	// G3: disjoint windows are a temporal succession.
	if !windowsOverlap(n, m) {
		older, newer := n, m
		if nf, _, _ := n.window(); func() bool { mf, _, _ := m.window(); return mf.Before(nf) }() {
			older, newer = m, n
		}
		v.Edge = &knowledgeEdge{Src: older.ID, Dst: newer.ID, Kind: KEdgeAmends, Rule: ruleDisjointValidity}
		ev["g3"] = map[string]any{"windows": "disjoint"}
		return v, true
	}
	ev["g3"] = map[string]any{"windows": "overlap"}
	// G4: a decision defeats a constraint (default superiority).
	nd, md := isDecisionLayer(n.Layer), isDecisionLayer(m.Layer)
	if nd != md {
		src, dst := n, m
		if md {
			src, dst = m, n
		}
		v.Edge = &knowledgeEdge{Src: src.ID, Dst: dst.ID, Kind: KEdgeOverrides, Rule: ruleLayerPrecedence}
		return v, true
	}
	ev["g4"] = map[string]any{"layers": []string{n.Layer, m.Layer}}
	v.Candidate = true
	return v, true
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

// --- candidate search (RO) --------------------------------------------------

// Overlap is one live memory a write overlaps (the D1 hint and the sweep).
type Overlap struct {
	Key        string  `json:"key"`
	Scope      string  `json:"scope"`
	MemoryID   string  `json:"memory_id"`
	Similarity float64 `json:"similarity"`
	// Relation: duplicate_candidate (same rank, layer, validity: resolve it),
	// overrides (the peer takes precedence here), narrower (this write takes
	// precedence over the peer), amends (disjoint validity: a succession).
	Relation string `json:"relation"`

	verdict pairVerdict
	peer    kmem
}

func (d *DB) overlapMin() float64 {
	raw := d.GetSetting(settingOverlapMin)
	if raw == "" {
		return overlapMinDefault
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f != f {
		warnSettingOnce(settingOverlapMin, raw, "unparsable as float", overlapMinDefault)
		return overlapMinDefault
	}
	return min(max(f, 0.1), 0.95)
}

// peerScopeClause restricts peers (alias m) to the memories whose extent
// meets n's.
func peerScopeClause(n kmem) (string, []any) {
	switch n.Scope {
	case "global":
		return "1 = 1", nil
	case "agent":
		return `(m.scope = 'global' OR (m.project = ? AND (m.scope <> 'agent' OR m.agent_name = ?)))`, []any{n.Project, n.Agent}
	}
	return `(m.scope = 'global' OR m.project = ?)`, []any{n.Project}
}

// rarestTokens orders N's value tokens by document frequency (ascending),
// then length, and keeps the first overlapQueryTokens.
func rarestTokens(q excQ, toks map[string]bool) []string {
	list := make([]string, 0, len(toks))
	for t := range toks {
		list = append(list, t)
	}
	freq := map[string]int{}
	if len(list) > 0 {
		ph, args := inPlaceholders(list)
		if rows, err := q.Query(`SELECT term, doc FROM memories_fts_vocab WHERE term IN (`+ph+`)`, args...); err == nil {
			for rows.Next() {
				var term string
				var doc int
				if rows.Scan(&term, &doc) == nil {
					freq[term] = doc
				}
			}
			_ = rows.Close()
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if freq[list[i]] != freq[list[j]] {
			return freq[list[i]] < freq[list[j]]
		}
		if len(list[i]) != len(list[j]) {
			return len(list[i]) > len(list[j])
		}
		return list[i] < list[j]
	})
	if len(list) > overlapQueryTokens {
		list = list[:overlapQueryTokens]
	}
	return list
}

// declaredEdgeBetween reports an agent-declared overrides/amends edge between
// a and b, in either direction.
func declaredEdgeBetween(q excQ, a, b string) bool {
	var one int
	err := q.QueryRow(`SELECT 1 FROM knowledge_edges WHERE declared = 1 AND kind IN ('overrides', 'amends')
		AND ((src_memory_id = ? AND dst_memory_id = ?) OR (src_memory_id = ? AND dst_memory_id = ?)) LIMIT 1`, a, b, b, a).Scan(&one)
	return err == nil
}

// overlaps finds n's live HIGH peers that pass G1 and runs G2-G4 on each.
// Read-only: every query runs on q (the RO pool for the D1 hint).
func overlaps(q excQ, n kmem, minSim float64, now string) ([]Overlap, error) {
	scope, sargs := peerScopeClause(n)
	peers := map[string]kmem{}
	inTopK := map[string]bool{}
	collect := func(query string, args []any, topK bool) error {
		rows, err := q.Query(query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			m, err := scanKMem(rows)
			if err != nil {
				return err
			}
			if m.ID == n.ID {
				continue
			}
			peers[m.ID] = m
			if topK {
				inTopK[m.ID] = true
			}
		}
		return rows.Err()
	}
	base := `SELECT ` + kmemCols + ` FROM memories m WHERE ` + liveHighClause + ` AND ` + scope + ` AND m.id <> ?`
	baseArgs := append(append([]any{now}, sargs...), n.ID)

	// Value overlap: N's rarest tokens against the value column, top-K by BM25.
	toks := valueTokens(gateText(n))
	if rare := rarestTokens(q, toks); len(rare) > 0 {
		quoted := make([]string, len(rare))
		for i, t := range rare {
			quoted[i] = `"` + t + `"`
		}
		match := "value : (" + strings.Join(quoted, " OR ") + ")"
		ftsQ := `SELECT ` + kmemCols + ` FROM memories m JOIN memories_fts f ON m.rowid = f.rowid
			WHERE ` + liveHighClause + ` AND ` + scope + ` AND m.id <> ? AND memories_fts MATCH ?
			ORDER BY rank LIMIT ?`
		if err := collect(ftsQ, append(append([]any{}, baseArgs...), match, overlapTopK), true); err != nil {
			return nil, fmt.Errorf("overlap fts: %w", err)
		}
	}
	// Declared subject.
	if s := declaredSubject(n.Tags); s != "" {
		if err := collect(base+` AND lower(m.tags) LIKE ?`, append(append([]any{}, baseArgs...), `%"subject:`+s+`"%`), false); err != nil {
			return nil, fmt.Errorf("overlap subject: %w", err)
		}
	}
	// Decision area.
	if a := decisionArea(n); a != "" {
		if err := collect(base+` AND m.layer IN ('decision', 'decisions') AND json_valid(m.value) AND lower(json_extract(m.value, '$.area')) = ?`,
			append(append([]any{}, baseArgs...), a), false); err != nil {
			return nil, fmt.Errorf("overlap area: %w", err)
		}
	}

	ids := make([]string, 0, len(peers))
	for id := range peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Overlap
	for _, id := range ids {
		m := peers[id]
		sim := jaccard(toks, valueTokens(gateText(m)))
		signal := g1Signal(n, m, inTopK[id], sim, minSim)
		v, ok := evaluatePair(n, m, signal, sim, signal != "" && declaredEdgeBetween(q, n.ID, m.ID))
		if !ok || v.DeclaredExceptionOf != "" {
			continue
		}
		rel := "duplicate_candidate"
		if e := v.Edge; e != nil {
			switch {
			case e.Kind == KEdgeAmends:
				rel = "amends"
			case e.Src == n.ID:
				rel = "narrower"
			default:
				rel = "overrides"
			}
		}
		out = append(out, Overlap{Key: m.Key, Scope: m.Scope, MemoryID: m.ID, Similarity: round3(sim), Relation: rel, verdict: v, peer: m})
	}
	return out, nil
}

// loadLiveKMem loads one memory if it is live and HIGH-layer.
func loadLiveKMem(q excQ, id, now string) (kmem, bool, error) {
	m, err := scanKMem(q.QueryRow(`SELECT `+kmemCols+` FROM memories m WHERE m.id = ? AND `+liveHighClause, id, now))
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

// OverlapHint is the D1 write-time hint for a fresh HIGH write: the live
// memories it overlaps, and a supersede suggestion when one is a duplicate
// candidate. Read-only (the RO pool, one FTS5 query plus bounded lookups); it
// never refuses and never writes. Empty for other layers.
func (d *DB) OverlapHint(memoryID string) ([]Overlap, string, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	n, ok, err := loadLiveKMem(d.ro(), memoryID, now)
	if err != nil || !ok {
		return nil, "", err
	}
	ov, err := overlaps(d.ro(), n, d.overlapMin(), now)
	if err != nil {
		return nil, "", err
	}
	suggestion := ""
	best := -1.0
	for _, o := range ov {
		if o.Relation == "duplicate_candidate" && o.Similarity > best {
			best = o.Similarity
			suggestion = fmt.Sprintf("same subject as %s: re-write with key=%s and based_on=%s to supersede it", o.Key, o.Key, o.MemoryID)
		}
	}
	return ov, suggestion, nil
}

// --- the D2 sweep -------------------------------------------------------------

// ContradictionReport counts one tick's work.
type ContradictionReport struct {
	Scanned, Conflicts, Edges, Expired, Escalated int
	// Backfill is set on the tick that ran the one-shot audited pass: every
	// pair it recorded, for the audit log.
	Backfill []string
}

func membersHash(ids []string) string {
	h := sha1.Sum([]byte(strings.Join(ids, "|")))
	return hex.EncodeToString(h[:])
}

// coveredTx drops the peers already grouped with n in a conflict row, in any
// state: a resolved not_a_conflict is never re-detected, and a re-run or a
// concurrent tick adds nothing.
func coveredTx(q excQ, nID string, peers []string) ([]string, error) {
	var out []string
	for _, p := range peers {
		var one int
		err := q.QueryRow(`SELECT 1 FROM knowledge_conflicts c
			WHERE EXISTS (SELECT 1 FROM json_each(c.members) WHERE value = ?)
			  AND EXISTS (SELECT 1 FROM json_each(c.members) WHERE value = ?) LIMIT 1`, nID, p).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			out = append(out, p)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// recordTx writes n's gate results: the implicit edges (INSERT OR IGNORE on
// the natural key) and at most one conflict row for n and its uncovered
// candidates (members_hash UNIQUE). Returns what it wrote.
func recordTx(q excQ, n kmem, ov []Overlap, now string) (conflicts, edges int, pairs []string, err error) {
	var cands []string
	evidence := map[string]any{}
	project := n.Project
	if n.Scope == "global" {
		project = "*"
	}
	for _, o := range ov {
		if e := o.verdict.Edge; e != nil {
			res, err := q.Exec(`INSERT OR IGNORE INTO knowledge_edges (src_memory_id, dst_memory_id, kind, declared, rule, created_by, created_at)
				VALUES (?, ?, ?, 0, ?, ?, ?)`, e.Src, e.Dst, e.Kind, e.Rule, detectorAgent, now)
			if err != nil {
				return 0, 0, nil, fmt.Errorf("knowledge edge: %w", err)
			}
			if k, _ := res.RowsAffected(); k == 1 {
				edges++
				pairs = append(pairs, fmt.Sprintf("edge %s %s -> %s (%s)", e.Kind, keyOf(n, o, e.Src), keyOf(n, o, e.Dst), e.Rule))
			}
			continue
		}
		if o.verdict.Candidate {
			cands = append(cands, o.MemoryID)
			evidence[o.MemoryID] = o.verdict.Evidence
			if o.peer.Scope == "global" {
				project = "*"
			}
		}
	}
	if len(cands) == 0 {
		return 0, edges, pairs, nil
	}
	cands, err = coveredTx(q, n.ID, cands)
	if err != nil || len(cands) == 0 {
		return 0, edges, pairs, err
	}
	members := append([]string{n.ID}, cands...)
	sort.Strings(members)
	mj, _ := json.Marshal(members)
	ej, _ := json.Marshal(map[string]any{"detected_for": n.ID, "pairs": evidence})
	res, err := q.Exec(`INSERT OR IGNORE INTO knowledge_conflicts (id, project, members, members_hash, kind, gate_evidence, detected_by, state, created_at)
		VALUES (?, ?, ?, ?, 'candidate', ?, ?, ?, ?)`,
		uuid.New().String(), project, string(mj), membersHash(members), string(ej), detectorAgent, ConflictDetected, now)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("knowledge conflict: %w", err)
	}
	if k, _ := res.RowsAffected(); k == 1 {
		conflicts = 1
		keys := []string{n.Key}
		for _, o := range ov {
			for _, c := range cands {
				if o.MemoryID == c {
					keys = append(keys, o.Key)
				}
			}
		}
		pairs = append(pairs, "conflict "+strings.Join(keys, " <-> "))
	}
	return conflicts, edges, pairs, nil
}

func keyOf(n kmem, o Overlap, id string) string {
	if id == n.ID {
		return n.Key
	}
	return o.Key
}

// EvaluateContradictions is the D2 tick: lease expiry, escalation after two
// failed claims, the one-shot audited backfill (first run only), then the
// cursor sweep over knowledge_log. Idempotent and concurrency-safe: the cursor
// advances by CAS inside each hit's tx, and members_hash / the edge key make
// every write insert-once.
func (d *DB) EvaluateContradictions(now time.Time) (ContradictionReport, error) {
	var rep ContradictionReport
	ts := now.UTC().Format(memoryTimeFmt)
	n, err := d.expireConflictLeases(ts)
	rep.Expired = n
	if err != nil {
		return rep, err
	}
	if rep.Escalated, err = d.escalateConflicts(ts); err != nil {
		return rep, err
	}
	if d.GetSetting(settingContradictionBackfill) == "" {
		if err := d.backfillContradictions(ts, &rep); err != nil {
			return rep, err
		}
	}
	return rep, d.sweepContradictions(ts, &rep)
}

func (d *DB) contradictionCursor() string {
	c := d.GetSetting(settingContradictionCursor)
	if c == "" {
		return "0"
	}
	return c
}

func (d *DB) sweepContradictions(now string, rep *ContradictionReport) error {
	cursor := d.contradictionCursor()
	from, _ := strconv.ParseInt(cursor, 10, 64)
	rows, err := d.ro().Query(`SELECT rev, COALESCE(memory_id, ''), op, layer FROM knowledge_log
		WHERE rev > ? ORDER BY rev LIMIT ?`, from, contradictionBatch)
	if err != nil {
		return fmt.Errorf("contradiction scan: %w", err)
	}
	type change struct {
		rev           int64
		id, op, layer string
	}
	var changes []change
	for rows.Next() {
		var c change
		if err := rows.Scan(&c.rev, &c.id, &c.op, &c.layer); err != nil {
			_ = rows.Close()
			return err
		}
		changes = append(changes, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Only writes that put a (new) live row in a HIGH layer are checked.
	relevant := func(c change) (kmem, []Overlap, bool, error) {
		if c.id == "" || !isHighLayer(c.layer) || (c.op != opSet && c.op != opSupersede && c.op != opSibling && c.op != opConflict) {
			return kmem{}, nil, false, nil
		}
		m, ok, err := loadLiveKMem(d.ro(), c.id, now)
		if err != nil || !ok {
			return kmem{}, nil, false, err
		}
		ov, err := overlaps(d.ro(), m, d.overlapMin(), now)
		if err != nil {
			return kmem{}, nil, false, err
		}
		return m, ov, len(ov) > 0, nil
	}
	for i, c := range changes {
		rep.Scanned++
		m, ov, hit, err := relevant(c)
		if err != nil {
			return err
		}
		if !hit {
			if i == len(changes)-1 {
				if _, err := d.advanceContradictionCursor(cursor, c.rev); err != nil {
					return err
				}
			}
			continue
		}
		ok, err := d.recordHit(m, ov, cursor, c.rev, now, rep)
		if err != nil {
			return err
		}
		if !ok {
			return nil // another tick owns this range
		}
		cursor = strconv.FormatInt(c.rev, 10)
	}
	return nil
}

func (d *DB) advanceContradictionCursor(prev string, to int64) (bool, error) {
	res, err := d.writerExec(`UPDATE settings SET value = ? WHERE key = ? AND value = ?`,
		strconv.FormatInt(to, 10), settingContradictionCursor, prev)
	if err != nil {
		return false, fmt.Errorf("contradiction cursor: %w", err)
	}
	k, _ := res.RowsAffected()
	return k == 1, nil
}

// recordHit writes one changed memory's results and moves the cursor to rev,
// in one writer tx. ok=false: the cursor moved (another tick won).
func (d *DB) recordHit(m kmem, ov []Overlap, prevCursor string, rev int64, now string, rep *ContradictionReport) (bool, error) {
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, fmt.Errorf("contradiction begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE settings SET value = ? WHERE key = ? AND value = ?`, strconv.FormatInt(rev, 10), settingContradictionCursor, prevCursor)
	if err != nil {
		return false, fmt.Errorf("contradiction cursor: %w", err)
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return false, nil
	}
	c, e, _, err := recordTx(tx, m, ov, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	rep.Conflicts += c
	rep.Edges += e
	return true, nil
}

// backfillContradictions is the one-shot audited pass over every live HIGH
// memory that predates detection (ruling OQ3). Each memory with a hit is one
// writer tx; the marker is written last, so a crash re-runs it, and the
// insert-once keys keep that idempotent. Every recorded pair is logged and
// returned for the audit.
func (d *DB) backfillContradictions(now string, rep *ContradictionReport) error {
	rows, err := d.ro().Query(`SELECT `+kmemCols+` FROM memories m WHERE `+liveHighClause+` ORDER BY m.created_at, m.id`, now)
	if err != nil {
		return fmt.Errorf("contradiction backfill: %w", err)
	}
	var all []kmem
	for rows.Next() {
		m, err := scanKMem(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, m)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	minSim := d.overlapMin()
	for _, m := range all {
		ov, err := overlaps(d.ro(), m, minSim, now)
		if err != nil {
			return err
		}
		if len(ov) == 0 {
			continue
		}
		tx, err := d.beginWriterTx()
		if err != nil {
			return err
		}
		c, e, pairs, err := recordTx(tx, m, ov, now)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		rep.Conflicts += c
		rep.Edges += e
		rep.Backfill = append(rep.Backfill, pairs...)
	}
	for _, p := range rep.Backfill {
		log.Printf("[contradictions] backfill: %s", p)
	}
	d.SetSetting(settingContradictionBackfill, fmt.Sprintf("%s memories=%d recorded=%d", now, len(all), len(rep.Backfill)))
	return nil
}

// --- claim / expire / escalate --------------------------------------------------

func (d *DB) conflictLeaseTTL() time.Duration {
	return d.SettingDuration(settingConflictLeaseTTL, conflictLeaseDefault, 10*time.Minute, 48*time.Hour)
}

// ClaimConflict leases a detected conflict to agent: a CAS on state and lease,
// so of two claimers exactly one wins. False = someone else holds it or it is
// no longer detected.
func (d *DB) ClaimConflict(id, agent string, now time.Time) (bool, error) {
	ts := now.UTC().Format(memoryTimeFmt)
	res, err := d.writerExec(`UPDATE knowledge_conflicts SET state = ?, lease_holder = ?, lease_expires_at = ?
		WHERE id = ? AND state = ? AND (lease_expires_at IS NULL OR lease_expires_at < ?)`,
		ConflictClaimed, agent, now.Add(d.conflictLeaseTTL()).UTC().Format(memoryTimeFmt), id, ConflictDetected, ts)
	if err != nil {
		return false, fmt.Errorf("claim conflict: %w", err)
	}
	k, _ := res.RowsAffected()
	return k == 1, nil
}

// ReleaseConflict gives a claimed conflict back unresolved ("cannot"): it
// counts as a failed claim.
func (d *DB) ReleaseConflict(id, agent string) (bool, error) {
	res, err := d.writerExec(`UPDATE knowledge_conflicts SET state = ?, lease_holder = NULL, lease_expires_at = NULL,
		failed_claims = failed_claims + 1 WHERE id = ? AND state = ? AND lease_holder = ?`,
		ConflictDetected, id, ConflictClaimed, agent)
	if err != nil {
		return false, fmt.Errorf("release conflict: %w", err)
	}
	k, _ := res.RowsAffected()
	return k == 1, nil
}

// expireConflictLeases returns claimed conflicts whose lease lapsed unresolved
// to detected, counting a failed claim each.
func (d *DB) expireConflictLeases(now string) (int, error) {
	res, err := d.writerExec(`UPDATE knowledge_conflicts SET state = ?, lease_holder = NULL, lease_expires_at = NULL,
		failed_claims = failed_claims + 1 WHERE state = ? AND lease_expires_at < ?`, ConflictDetected, ConflictClaimed, now)
	if err != nil {
		return 0, fmt.Errorf("expire conflict leases: %w", err)
	}
	k, _ := res.RowsAffected()
	return int(k), nil
}

// escalateConflicts moves each detected conflict with two failed claims to
// escalated and opens one exception (kind contradiction) in the same tx. The
// class-budget ladder owns it from there; nothing messages a human directly.
func (d *DB) escalateConflicts(now string) (int, error) {
	rows, err := d.ro().Query(`SELECT id, project, members, failed_claims FROM knowledge_conflicts
		WHERE state = ? AND failed_claims >= ?`, ConflictDetected, escalateAfterClaims)
	if err != nil {
		return 0, fmt.Errorf("escalate conflicts: %w", err)
	}
	type due struct {
		id, project, members string
		failed               int
	}
	var list []due
	for rows.Next() {
		var c due
		if err := rows.Scan(&c.id, &c.project, &c.members, &c.failed); err != nil {
			_ = rows.Close()
			return 0, err
		}
		list = append(list, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, c := range list {
		tx, err := d.beginWriterTx()
		if err != nil {
			return n, err
		}
		res, err := tx.Exec(`UPDATE knowledge_conflicts SET state = ? WHERE id = ? AND state = ? AND failed_claims >= ?`,
			ConflictEscalated, c.id, ConflictDetected, escalateAfterClaims)
		if err != nil {
			_ = tx.Rollback()
			return n, err
		}
		if k, _ := res.RowsAffected(); k == 0 {
			_ = tx.Rollback()
			continue
		}
		var members []string
		_ = json.Unmarshal([]byte(c.members), &members)
		if _, err := openExceptionTx(tx, exceptionOpen{
			Project: c.project, SourceKind: excSourceContradiction, SourceRef: c.id, RaisedBy: detectorAgent,
			Code: "unresolved_after_claims", Kind: "contradiction", Retry: "non_retryable",
			Template: "knowledge conflict unresolved after failed claims",
			Evidence: map[string]any{"conflict_id": c.id, "members": members, "failed_claims": c.failed},
			At:       now,
		}); err != nil {
			_ = tx.Rollback()
			return n, err
		}
		if err := tx.Commit(); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// --- resolution, reversal and sampled audits (T2, §4.3-§5) ---------------------

// Conflict kinds.
const (
	ConflictKindCandidate = "candidate"
	ConflictKindSibling   = "sibling"
)

// Resolutions (§4.3). merge, supersede and reject_new archive; the rest
// archive nothing.
const (
	ResolutionMerge             = "merge"
	ResolutionSupersede         = "supersede"
	ResolutionRejectNew         = "reject_new"
	ResolutionScopeSplit        = "scope_split"
	ResolutionBothValidTemporal = "both_valid_temporal"
	ResolutionOverrideDeclared  = "override_declared"
	ResolutionNotAConflict      = "not_a_conflict"
)

var resolutions = map[string]bool{
	ResolutionMerge: true, ResolutionSupersede: true, ResolutionRejectNew: true, ResolutionScopeSplit: true,
	ResolutionBothValidTemporal: true, ResolutionOverrideDeclared: true, ResolutionNotAConflict: true,
}

func archivingResolution(r string) bool {
	return r == ResolutionMerge || r == ResolutionSupersede || r == ResolutionRejectNew
}

// Audit outcomes (§5), in knowledge_conflicts.audit and gate_evidence.audits.
const (
	AuditSampled  = "sampled"
	AuditUpheld   = "upheld"
	AuditReverted = "reverted"
)

const (
	settingAuditRate    = "contradiction_audit_rate"
	settingPrecisionMin = "contradiction_precision_min"
	settingArchiveSeq   = "contradiction_archive_seq"
	// Sampling: 1 in auditRateEarly while fewer than auditEarlyUntil audits
	// have an outcome, then 1 in auditRateLate.
	auditRateEarly  = 3
	auditRateLate   = 10
	auditEarlyUntil = 30
	// The precision gate: below precisionMinDefault over >= precisionMinAudits
	// audits in precisionWindow, a (profile, layer)'s archiving resolutions
	// need a second agent's concurrence.
	precisionMinDefault = 0.8
	precisionMinAudits  = 5
	precisionWindow     = 90 * 24 * time.Hour
)

// Refusals. Nothing is written when one is returned.
var (
	ErrConflictUnknown     = errors.New("unknown conflict")
	ErrConflictNotHeld     = errors.New("you do not hold this conflict's lease: claim it first")
	ErrConflictResolution  = errors.New("invalid resolution or keep for this conflict")
	ErrConflictNotResolved = errors.New("conflict is not resolved")
	ErrRevertNotAllowed    = errors.New("revert is for the resolver's lead, an executive, or the auditor")
	ErrConcurrenceSelf     = errors.New("a proposal needs a second agent's concurrence, not the proposer's")
)

// ConflictMember is one member of a conflict as list shows it.
type ConflictMember struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Scope   string `json:"scope"`
	Layer   string `json:"layer"`
	Agent   string `json:"agent_name"`
	Preview string `json:"value_preview"`
	Live    bool   `json:"live"`
}

// ConflictView is one conflict row as list and resolve return it.
type ConflictView struct {
	ID           string           `json:"id"`
	Project      string           `json:"project"`
	Kind         string           `json:"kind"`
	State        string           `json:"state"`
	Members      []ConflictMember `json:"members"`
	LeaseHolder  string           `json:"lease_holder,omitempty"`
	FailedClaims int              `json:"failed_claims,omitempty"`
	Resolution   string           `json:"resolution,omitempty"`
	ResolvedBy   string           `json:"resolved_by,omitempty"`
	Audit        string           `json:"audit,omitempty"`
	Proposal     map[string]any   `json:"proposal,omitempty"`
	CreatedAt    string           `json:"created_at"`
}

// conflictPreviewLen bounds a member's value preview in list results.
const conflictPreviewLen = 160

// ListConflicts returns project's conflicts (its own and '*'), open ones
// (detected, claimed) unless state names one, oldest first. Read-only.
func (d *DB) ListConflicts(project, state string, limit int) ([]ConflictView, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	where := `state IN ('detected', 'claimed')`
	args := []any{project}
	if state != "" {
		where = `state = ?`
		args = []any{state, project}
	}
	rows, err := d.ro().Query(`SELECT id, project, kind, state, members, COALESCE(lease_holder, ''), failed_claims,
			COALESCE(resolution, ''), COALESCE(resolved_by, ''), COALESCE(audit, ''), gate_evidence, created_at
		FROM knowledge_conflicts WHERE `+where+` AND project IN (?, '*') ORDER BY created_at, id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	var out []ConflictView
	var membersJSON []string
	for rows.Next() {
		var v ConflictView
		var mj, ev string
		if err := rows.Scan(&v.ID, &v.Project, &v.Kind, &v.State, &mj, &v.LeaseHolder, &v.FailedClaims,
			&v.Resolution, &v.ResolvedBy, &v.Audit, &ev, &v.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if p, ok := parseEvidence(ev)["proposal"].(map[string]any); ok {
			v.Proposal = p
		}
		out = append(out, v)
		membersJSON = append(membersJSON, mj)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		var ids []string
		_ = json.Unmarshal([]byte(membersJSON[i]), &ids)
		for _, id := range ids {
			m := ConflictMember{ID: id}
			var value string
			var archived sql.NullString
			if err := d.ro().QueryRow(`SELECT key, scope, layer, agent_name, value, archived_at FROM memories WHERE id = ?`, id).
				Scan(&m.Key, &m.Scope, &m.Layer, &m.Agent, &value, &archived); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			m.Live = !archived.Valid && m.Key != ""
			if r := []rune(value); len(r) > conflictPreviewLen {
				value = string(r[:conflictPreviewLen]) + "…"
			}
			m.Preview = value
			out[i].Members = append(out[i].Members, m)
		}
	}
	return out, nil
}

// OpenConflictKinds maps each of ids that is a member of an open conflict
// (detected or claimed) to that conflict's kind (sibling wins over
// candidate). One read-only query; nil for no ids.
func (d *DB) OpenConflictKinds(ids []string) map[string]string {
	if len(ids) == 0 {
		return nil
	}
	ph, args := inPlaceholders(ids)
	rows, err := d.ro().Query(`SELECT j.value, c.kind FROM knowledge_conflicts c, json_each(c.members) j
		WHERE c.state IN ('detected', 'claimed') AND j.value IN (`+ph+`)`, args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, kind string
		if rows.Scan(&id, &kind) == nil && out[id] != ConflictKindSibling {
			out[id] = kind
		}
	}
	return out
}

func parseEvidence(s string) map[string]any {
	ev := map[string]any{}
	_ = json.Unmarshal([]byte(s), &ev)
	return ev
}

func evidenceString(ev map[string]any, k string) string {
	s, _ := ev[k].(string)
	return s
}

// conflictMem is a member row as a resolution reads it on the writer tx.
type conflictMem struct {
	ID, Project, Scope, Key, Agent, Layer, Status string
	ValidFrom, ValidUntil, ArchivedBy, Reason     sql.NullString
	CreatedAt                                     string
	Live                                          bool
}

func loadConflictMembers(q excQ, ids []string) ([]conflictMem, error) {
	var out []conflictMem
	for _, id := range ids {
		var m conflictMem
		var archivedAt sql.NullString
		err := q.QueryRow(`SELECT id, project, scope, key, agent_name, layer, status, valid_from, valid_until, archived_by,
				archived_reason, created_at, archived_at FROM memories WHERE id = ?`, id).
			Scan(&m.ID, &m.Project, &m.Scope, &m.Key, &m.Agent, &m.Layer, &m.Status, &m.ValidFrom, &m.ValidUntil,
				&m.ArchivedBy, &m.Reason, &m.CreatedAt, &archivedAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m.Live = !archivedAt.Valid
		out = append(out, m)
	}
	return out, nil
}

// prior is what a resolution changed on one memory, so revert restores the
// exact row: archived members lose archived_* and get their status back;
// stamped members (both_valid_temporal) get their valid_until back.
func (m conflictMem) prior(archived bool) map[string]any {
	return map[string]any{"archived": archived, "status": m.Status, "valid_until": nullStr(m.ValidUntil),
		"archived_by": nullStr(m.ArchivedBy), "archived_reason": nullStr(m.Reason)}
}

func nullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

// archiveForConflictTx archives one live member with invalidated_by = cid and
// logs its one-row retraction (class retraction: coherence reaches the
// member's consumers). extra is the legacy path's validity close ("" = none).
func archiveForConflictTx(tx *writerTx, m conflictMem, cid, by, reason, now string, closeValidity bool) error {
	q := `UPDATE memories SET archived_at = ?, archived_by = ?, archived_reason = ?, status = 'archived', invalidated_by = ?`
	args := []any{now, by, reason, cid}
	if closeValidity {
		q += `, ` + closeValidityClause
		args = append(args, now, now)
	}
	res, err := tx.Exec(q+` WHERE id = ? AND archived_at IS NULL`, append(args, m.ID)...)
	if err != nil {
		return fmt.Errorf("archive member %s: %w", m.ID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("archive member %s: moved concurrently", m.ID)
	}
	if _, _, err := appendKnowledgeTx(tx, knowledgeEntry{
		Project: m.Project, Scope: m.Scope, Key: m.Key, PrevMemoryID: m.ID, Op: opRetract, Layer: m.Layer, Agent: by, At: now,
	}, m.Agent); err != nil {
		return err
	}
	return nil
}

// agentProfile is name's profile in project (any project for '*').
func agentProfile(q excQ, project, name string) string {
	var p string
	_ = q.QueryRow(`SELECT COALESCE(profile_slug, '') FROM agents WHERE lower(name) = lower(?) AND (project = ? OR ? = '*')
		ORDER BY status = 'active' DESC LIMIT 1`, name, project, project).Scan(&p)
	return p
}

// agentLead is name's reports_to in project (any project for '*').
func agentLead(q excQ, project, name string) string {
	var lead string
	_ = q.QueryRow(`SELECT COALESCE(reports_to, '') FROM agents WHERE lower(name) = lower(?) AND (project = ? OR ? = '*')
		ORDER BY status = 'active' DESC LIMIT 1`, name, project, project).Scan(&lead)
	return lead
}

func isActiveExecutive(q excQ, project, name string) bool {
	var one int
	err := q.QueryRow(`SELECT 1 FROM agents WHERE lower(name) = lower(?) AND is_executive = 1 AND status = 'active'
		AND (project = ? OR ? = '*') LIMIT 1`, name, project, project).Scan(&one)
	return err == nil
}

// pickAuditor names the audit's target: the resolver's lead when its profile
// differs from the resolver's, else an active executive of another profile.
// ("", "") when there is none.
func pickAuditor(q excQ, project, resolver, resolverProfile string) (name, profile string) {
	if lead := agentLead(q, project, resolver); lead != "" {
		if p := agentProfile(q, project, lead); p != "" && p != resolverProfile {
			return strings.ToLower(lead), p
		}
	}
	_ = q.QueryRow(`SELECT name, profile_slug FROM agents WHERE is_executive = 1 AND status = 'active'
		AND (project = ? OR ? = '*') AND lower(name) <> lower(?) AND COALESCE(profile_slug, '') NOT IN ('', ?)
		ORDER BY name LIMIT 1`, project, project, resolver, resolverProfile).Scan(&name, &profile)
	return name, profile
}

// precisionTx is (profile, layer)'s audit precision since since, and the audit
// count it is over.
func precisionTx(q excQ, profile, layer, since string) (float64, int, error) {
	var upheld, reverted int
	err := q.QueryRow(`SELECT
			COALESCE(SUM(CASE WHEN json_extract(a.value, '$.outcome') = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN json_extract(a.value, '$.outcome') = ? THEN 1 ELSE 0 END), 0)
		FROM knowledge_conflicts c, json_each(c.gate_evidence, '$.audits') a
		WHERE json_extract(a.value, '$.profile') = ? AND json_extract(a.value, '$.layer') = ? AND json_extract(a.value, '$.at') >= ?`,
		AuditUpheld, AuditReverted, profile, layer, since).Scan(&upheld, &reverted)
	if err != nil {
		return 0, 0, err
	}
	n := upheld + reverted
	if n == 0 {
		return 1, 0, nil
	}
	return float64(upheld) / float64(n), n, nil
}

func (d *DB) precisionMin() float64 {
	raw := d.GetSetting(settingPrecisionMin)
	if raw == "" {
		return precisionMinDefault
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f != f {
		warnSettingOnce(settingPrecisionMin, raw, "unparsable as float", precisionMinDefault)
		return precisionMinDefault
	}
	return min(max(f, 0), 1)
}

// sampleAuditTx advances the archiving-resolution sequence and says whether
// this one is audited: 1 in contradiction_audit_rate when set, else 1 in 3
// while fewer than 30 audits have an outcome, then 1 in 10. The first
// archiving resolution is always sampled.
func sampleAuditTx(q excQ, rateSetting string) (bool, error) {
	var seq int64
	if err := q.QueryRow(`INSERT INTO settings (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT) RETURNING CAST(value AS INTEGER)`,
		settingArchiveSeq).Scan(&seq); err != nil {
		return false, fmt.Errorf("audit sequence: %w", err)
	}
	rate := auditRateLate
	if n, err := strconv.Atoi(rateSetting); err == nil && n >= 1 {
		rate = n
	} else {
		var audited int
		if err := q.QueryRow(`SELECT COUNT(*) FROM knowledge_conflicts c, json_each(c.gate_evidence, '$.audits') a
			WHERE json_extract(a.value, '$.outcome') IN (?, ?)`, AuditUpheld, AuditReverted).Scan(&audited); err != nil {
			return false, err
		}
		if audited < auditEarlyUntil {
			rate = auditRateEarly
		}
	}
	return (seq-1)%int64(rate) == 0, nil
}

// ConflictResolution is one resolve call.
type ConflictResolution struct {
	ID, Agent, Resolution, Keep, Rationale string
	Now                                    time.Time
}

// ConflictOutcome is what a resolve or revert did.
type ConflictOutcome struct {
	ConflictID string   `json:"conflict_id"`
	State      string   `json:"state"`
	Resolution string   `json:"resolution,omitempty"`
	Kept       string   `json:"kept,omitempty"`
	Archived   []string `json:"archived,omitempty"`
	Restored   []string `json:"restored,omitempty"`
	// Proposed: the resolver's (profile, layer) is below the precision gate,
	// so nothing was archived; a second agent must claim and resolve the same
	// way to concur.
	Proposed bool   `json:"proposed,omitempty"`
	Audit    string `json:"audit,omitempty"`
	Auditor  string `json:"auditor,omitempty"`
	// AuditorProfile is the profile an audit ticket goes to (sampled only).
	AuditorProfile string `json:"-"`
	Project        string `json:"-"`
}

// ResolveKnowledgeConflict applies a resolution (§4.3) in one writer tx, by
// the lease holder only. Archiving resolutions archive with invalidated_by =
// the conflict id and log one retraction per archived member; edges carry
// rule resolution:<id>; prior member state goes to gate_evidence.prior so
// revert restores the exact rows. Below the precision gate (§5) an archiving
// resolution becomes a proposal: the conflict returns to detected (no failed
// claim) and a second agent's matching resolve concurs. On a resolved,
// sampled conflict, the auditor profile's matching resolve upholds it.
func (d *DB) ResolveKnowledgeConflict(in ConflictResolution) (*ConflictOutcome, error) {
	if !resolutions[in.Resolution] {
		return nil, ErrConflictResolution
	}
	agent := strings.ToLower(in.Agent)
	now := in.Now.UTC().Format(memoryTimeFmt)
	rateSetting := d.GetSetting(settingAuditRate)
	precisionMin := d.precisionMin()
	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, fmt.Errorf("resolve conflict begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var project, state, membersJSON, evJSON, holder, expires, resolution, keptID, audit string
	err = tx.QueryRow(`SELECT project, state, members, gate_evidence, COALESCE(lease_holder, ''), COALESCE(lease_expires_at, ''),
			COALESCE(resolution, ''), COALESCE(resolution_memory_id, ''), COALESCE(audit, '')
		FROM knowledge_conflicts WHERE id = ?`, in.ID).
		Scan(&project, &state, &membersJSON, &evJSON, &holder, &expires, &resolution, &keptID, &audit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConflictUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("resolve conflict: %w", err)
	}
	ev := parseEvidence(evJSON)
	out := &ConflictOutcome{ConflictID: in.ID, Resolution: in.Resolution, Project: project}

	// Uphold: the auditor profile concurs with a sampled resolution.
	if state == ConflictResolved {
		a, _ := ev["audit"].(map[string]any)
		if audit != AuditSampled || a == nil || in.Resolution != resolution ||
			agentProfile(tx, project, agent) != evidenceString(a, "profile") || evidenceString(a, "profile") == "" {
			return nil, ErrConflictNotHeld
		}
		appendAudit(ev, AuditUpheld, agent, now)
		ej, _ := json.Marshal(ev)
		res, err := tx.Exec(`UPDATE knowledge_conflicts SET audit = ?, gate_evidence = ? WHERE id = ? AND state = ? AND audit = ?`,
			AuditUpheld, string(ej), in.ID, ConflictResolved, AuditSampled)
		if err != nil {
			return nil, fmt.Errorf("uphold: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, ErrConflictNotHeld
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		out.State, out.Kept, out.Audit = ConflictResolved, keptID, AuditUpheld
		return out, nil
	}
	if state != ConflictClaimed || !strings.EqualFold(holder, agent) || expires < now {
		return nil, ErrConflictNotHeld
	}

	var ids []string
	_ = json.Unmarshal([]byte(membersJSON), &ids)
	members, err := loadConflictMembers(tx, ids)
	if err != nil {
		return nil, err
	}
	var live []conflictMem
	for _, m := range members {
		if m.Live {
			live = append(live, m)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		return live[i].CreatedAt < live[j].CreatedAt || (live[i].CreatedAt == live[j].CreatedAt && live[i].ID < live[j].ID)
	})
	var keep *conflictMem
	for i := range live {
		if live[i].ID == in.Keep {
			keep = &live[i]
		}
	}

	// What the resolution archives, stamps and links.
	var archive, stamp []conflictMem
	type edge struct{ src, dst, kind string }
	var edges []edge
	switch in.Resolution {
	case ResolutionMerge, ResolutionSupersede:
		if keep == nil || len(live) < 2 {
			return nil, ErrConflictResolution
		}
		for _, m := range live {
			if m.ID != keep.ID {
				archive = append(archive, m)
				edges = append(edges, edge{keep.ID, m.ID, KEdgeSupersedes})
			}
		}
	case ResolutionRejectNew:
		if len(live) < 2 {
			return nil, ErrConflictResolution
		}
		archive = []conflictMem{live[len(live)-1]}
		keep = nil
	case ResolutionScopeSplit, ResolutionOverrideDeclared:
		if keep == nil {
			return nil, ErrConflictResolution
		}
		for _, m := range live {
			if m.ID != keep.ID {
				edges = append(edges, edge{keep.ID, m.ID, KEdgeOverrides})
			}
		}
	case ResolutionBothValidTemporal:
		if len(live) < 2 {
			return nil, ErrConflictResolution
		}
		newest := live[len(live)-1]
		keep = &newest
		for _, m := range live[:len(live)-1] {
			stamp = append(stamp, m)
			edges = append(edges, edge{newest.ID, m.ID, KEdgeAmends})
		}
	case ResolutionNotAConflict:
		ev["distinct"] = true
	}
	if keep != nil {
		out.Kept = keep.ID
	}

	// The precision gate and its concurrence.
	profile := agentProfile(tx, project, agent)
	layer := ""
	for _, m := range archive {
		if layer == "" || layerDurability(m.Layer) > layerDurability(layer) {
			layer = m.Layer
		}
	}
	if archivingResolution(in.Resolution) {
		prop, _ := ev["proposal"].(map[string]any)
		concurs := prop != nil && evidenceString(prop, "resolution") == in.Resolution && evidenceString(prop, "keep") == in.Keep
		if concurs && strings.EqualFold(evidenceString(prop, "by"), agent) {
			return nil, ErrConcurrenceSelf
		}
		if concurs {
			ev["concurred_by"] = agent
			delete(ev, "proposal")
		} else {
			p, n, err := precisionTx(tx, profile, layer, in.Now.Add(-precisionWindow).UTC().Format(memoryTimeFmt))
			if err != nil {
				return nil, fmt.Errorf("precision: %w", err)
			}
			if n >= precisionMinAudits && p < precisionMin {
				ev["proposal"] = map[string]any{"by": agent, "profile": profile, "layer": layer, "resolution": in.Resolution,
					"keep": in.Keep, "rationale": in.Rationale, "precision": round3(p), "audits": n, "at": now}
				ej, _ := json.Marshal(ev)
				res, err := tx.Exec(`UPDATE knowledge_conflicts SET state = ?, lease_holder = NULL, lease_expires_at = NULL, gate_evidence = ?
					WHERE id = ? AND state = ? AND lease_holder = ?`, ConflictDetected, string(ej), in.ID, ConflictClaimed, holder)
				if err != nil {
					return nil, fmt.Errorf("propose: %w", err)
				}
				if n, _ := res.RowsAffected(); n != 1 {
					return nil, ErrConflictNotHeld
				}
				if err := tx.Commit(); err != nil {
					return nil, err
				}
				out.State, out.Proposed = ConflictDetected, true
				return out, nil
			}
		}
	}

	// Apply: archive, stamp, link. Every change is recorded for revert.
	priors := map[string]any{}
	reason := "conflict:" + in.Resolution
	for _, m := range archive {
		priors[m.ID] = m.prior(true)
		if err := archiveForConflictTx(tx, m, in.ID, agent, reason, now, false); err != nil {
			return nil, err
		}
		out.Archived = append(out.Archived, m.ID)
	}
	for _, m := range stamp {
		until := keep.CreatedAt
		if keep.ValidFrom.Valid && keep.ValidFrom.String != "" {
			until = keep.ValidFrom.String
		}
		if m.ValidUntil.Valid && m.ValidUntil.String != "" && m.ValidUntil.String <= until {
			continue // already ends before its successor starts
		}
		priors[m.ID] = m.prior(false)
		if _, err := tx.Exec(`UPDATE memories SET valid_until = ? WHERE id = ? AND archived_at IS NULL`, until, m.ID); err != nil {
			return nil, fmt.Errorf("stamp %s: %w", m.ID, err)
		}
		if _, _, err := appendKnowledgeTx(tx, knowledgeEntry{
			Project: m.Project, Scope: m.Scope, Key: m.Key, MemoryID: m.ID, Op: opValidity, Layer: m.Layer,
			Narrowing: true, Agent: agent, At: now,
		}, m.Agent); err != nil {
			return nil, err
		}
	}
	rule := "resolution:" + in.ID
	for _, e := range edges {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO knowledge_edges (src_memory_id, dst_memory_id, kind, declared, rule, created_by, created_at)
			VALUES (?, ?, ?, 1, ?, ?, ?)`, e.src, e.dst, e.kind, rule, agent, now); err != nil {
			return nil, fmt.Errorf("resolution edge: %w", err)
		}
	}
	ev["prior"] = priors
	ev["resolved"] = map[string]any{"by": agent, "profile": profile, "layer": layer, "at": now}
	auditCol := sql.NullString{}
	if archivingResolution(in.Resolution) {
		sampled, err := sampleAuditTx(tx, rateSetting)
		if err != nil {
			return nil, err
		}
		if sampled {
			name, p := pickAuditor(tx, project, agent, profile)
			ev["audit"] = map[string]any{"auditor": name, "profile": p, "at": now}
			auditCol = sql.NullString{String: AuditSampled, Valid: true}
			out.Audit, out.Auditor, out.AuditorProfile = AuditSampled, name, p
		}
	}
	ej, _ := json.Marshal(ev)
	res, err := tx.Exec(`UPDATE knowledge_conflicts SET state = ?, resolution = ?, resolution_memory_id = ?, resolved_by = ?, rationale = ?,
			resolved_at = ?, audit = ?, gate_evidence = ?, lease_holder = NULL, lease_expires_at = NULL
		WHERE id = ? AND state = ? AND lease_holder = ?`,
		ConflictResolved, in.Resolution, knNull(out.Kept), agent, knNull(in.Rationale), now, auditCol, string(ej),
		in.ID, ConflictClaimed, holder)
	if err != nil {
		return nil, fmt.Errorf("resolve conflict: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrConflictNotHeld
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("resolve conflict commit: %w", err)
	}
	out.State = ConflictResolved
	return out, nil
}

// appendAudit records one audit outcome of the conflict's current resolution
// in gate_evidence.audits, keyed by the resolver's (profile, layer).
func appendAudit(ev map[string]any, outcome, by, now string) {
	r, _ := ev["resolved"].(map[string]any)
	entry := map[string]any{"outcome": outcome, "by": by, "at": now,
		"resolver": evidenceString(r, "by"), "profile": evidenceString(r, "profile"), "layer": evidenceString(r, "layer")}
	list, _ := ev["audits"].([]any)
	ev["audits"] = append(list, entry)
}

// RevertConflict undoes a resolution (§4.3 Reversal), by the resolver's
// lead, an active executive, or the auditor profile: every invalidated_by =
// id row is restored exactly (archived_* cleared, status back, invalidated_by
// kept for history) with one knowledge_log set row each, stamped windows get
// their valid_until back, the resolution's edges are deleted, and the
// conflict returns to detected with audit = reverted. The only place that
// un-archives.
func (d *DB) RevertConflict(id, by string, now time.Time) (*ConflictOutcome, error) {
	by = strings.ToLower(by)
	ts := now.UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, fmt.Errorf("revert conflict begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var project, state, evJSON, resolvedBy string
	err = tx.QueryRow(`SELECT project, state, gate_evidence, COALESCE(resolved_by, '') FROM knowledge_conflicts WHERE id = ?`, id).
		Scan(&project, &state, &evJSON, &resolvedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConflictUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("revert conflict: %w", err)
	}
	if state != ConflictResolved {
		return nil, ErrConflictNotResolved
	}
	ev := parseEvidence(evJSON)
	a, _ := ev["audit"].(map[string]any)
	auditorProfile := evidenceString(a, "profile")
	allowed := isActiveExecutive(tx, project, by) ||
		(resolvedBy != "" && strings.EqualFold(agentLead(tx, project, resolvedBy), by)) ||
		(auditorProfile != "" && agentProfile(tx, project, by) == auditorProfile)
	if !allowed {
		return nil, ErrRevertNotAllowed
	}
	out := &ConflictOutcome{ConflictID: id, State: ConflictDetected, Audit: AuditReverted, Project: project}
	priors, _ := ev["prior"].(map[string]any)
	rows, err := tx.Query(`SELECT id FROM memories WHERE invalidated_by = ? AND archived_at IS NOT NULL ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	var restore []string
	for rows.Next() {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			_ = rows.Close()
			return nil, err
		}
		restore = append(restore, mid)
	}
	_ = rows.Close()
	members, err := loadConflictMembers(tx, restore)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		p, _ := priors[m.ID].(map[string]any)
		status := evidenceString(p, "status")
		if status == "" {
			status = "live"
		}
		if _, err := tx.Exec(`UPDATE memories SET archived_at = NULL, archived_by = ?, archived_reason = ?, status = ?, valid_until = ?
			WHERE id = ? AND invalidated_by = ?`, p["archived_by"], p["archived_reason"], status, p["valid_until"], m.ID, id); err != nil {
			return nil, fmt.Errorf("restore %s: %w", m.ID, err)
		}
		if _, _, err := appendKnowledgeTx(tx, knowledgeEntry{
			Project: m.Project, Scope: m.Scope, Key: m.Key, MemoryID: m.ID, Op: opSet, Layer: m.Layer, Agent: by, At: ts,
		}, m.Agent); err != nil {
			return nil, err
		}
		out.Restored = append(out.Restored, m.ID)
	}
	// Stamped windows (both_valid_temporal) of still-live members.
	var stamped []string
	for mid, raw := range priors {
		if p, _ := raw.(map[string]any); p != nil && p["archived"] == false {
			stamped = append(stamped, mid)
		}
	}
	sort.Strings(stamped)
	stampedMems, err := loadConflictMembers(tx, stamped)
	if err != nil {
		return nil, err
	}
	for _, m := range stampedMems {
		if !m.Live {
			continue
		}
		p, _ := priors[m.ID].(map[string]any)
		if _, err := tx.Exec(`UPDATE memories SET valid_until = ? WHERE id = ? AND archived_at IS NULL`, p["valid_until"], m.ID); err != nil {
			return nil, fmt.Errorf("unstamp %s: %w", m.ID, err)
		}
		if _, _, err := appendKnowledgeTx(tx, knowledgeEntry{
			Project: m.Project, Scope: m.Scope, Key: m.Key, MemoryID: m.ID, Op: opValidity, Layer: m.Layer, Agent: by, At: ts,
		}, m.Agent); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM knowledge_edges WHERE rule = ?`, "resolution:"+id); err != nil {
		return nil, fmt.Errorf("revert edges: %w", err)
	}
	appendAudit(ev, AuditReverted, by, ts)
	for _, k := range []string{"prior", "resolved", "distinct", "concurred_by", "audit"} {
		delete(ev, k)
	}
	ej, _ := json.Marshal(ev)
	res, err := tx.Exec(`UPDATE knowledge_conflicts SET state = ?, audit = ?, gate_evidence = ?, resolution = NULL, resolution_memory_id = NULL,
			resolved_by = NULL, rationale = NULL, resolved_at = NULL, lease_holder = NULL, lease_expires_at = NULL
		WHERE id = ? AND state = ?`, ConflictDetected, AuditReverted, string(ej), id, ConflictResolved)
	if err != nil {
		return nil, fmt.Errorf("revert conflict: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrConflictNotResolved
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("revert conflict commit: %w", err)
	}
	return out, nil
}

// recordSiblingTx tracks a same-key sibling (§4.4): the open sibling conflict
// that already holds live grows by sibling, else a new one opens with both.
// It replaces writing conflict_with.
func recordSiblingTx(q excQ, project, scope, key, agent, live, sibling, op, now string) error {
	cp := project
	if scope == "global" {
		cp = "*"
	}
	var cid, membersJSON string
	err := q.QueryRow(`SELECT c.id, c.members FROM knowledge_conflicts c
		WHERE c.kind = ? AND c.state IN ('detected', 'claimed') AND EXISTS (SELECT 1 FROM json_each(c.members) WHERE value = ?)
		ORDER BY c.created_at LIMIT 1`, ConflictKindSibling, live).Scan(&cid, &membersJSON)
	if err == nil {
		var members []string
		_ = json.Unmarshal([]byte(membersJSON), &members)
		members = append(members, sibling)
		sort.Strings(members)
		mj, _ := json.Marshal(members)
		_, err = q.Exec(`UPDATE knowledge_conflicts SET members = ?, members_hash = ? WHERE id = ?`, string(mj), membersHash(members), cid)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	members := []string{live, sibling}
	sort.Strings(members)
	mj, _ := json.Marshal(members)
	ej, _ := json.Marshal(map[string]any{"key": key, "scope": scope, "op": op})
	_, err = q.Exec(`INSERT OR IGNORE INTO knowledge_conflicts (id, project, members, members_hash, kind, gate_evidence, detected_by, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.New().String(), cp, string(mj), membersHash(members), ConflictKindSibling, string(ej), agent, ConflictDetected, now)
	return err
}
