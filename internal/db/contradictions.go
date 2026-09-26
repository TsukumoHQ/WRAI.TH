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
