package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// knowledge_log (design af783f93, ruling cto-tsukumo af6e3a5f): every memory or
// decision write appends exactly ONE row, inside the transaction that makes the
// change, carrying a global monotonic revision and a typed change class. A
// per-(project, durability) clock records the last revision that changed
// knowledge at that durability or above, so staleness is one comparison. Reads
// never write. The log starts empty at deploy (no backfill: a class is never
// invented for history) and is compacted by key, not by count.

// Change classes, ordered: a higher rank propagates more eagerly.
const (
	ChangeEditorial  = "editorial"
	ChangeAdditive   = "additive"
	ChangeNarrowing  = "narrowing"
	ChangeBreaking   = "breaking"
	ChangeRetraction = "retraction"
)

var changeRank = map[string]int{
	ChangeEditorial:  0,
	ChangeAdditive:   1,
	ChangeNarrowing:  2,
	ChangeBreaking:   3,
	ChangeRetraction: 4,
}

// ErrInvalidChangeClass is returned (nothing written) for a declared class
// outside the enum. Retraction is never declarable: only a delete produces it.
var ErrInvalidChangeClass = errors.New("invalid change_class (want editorial|additive|narrowing|breaking)")

func validDeclaredClass(c string) bool {
	return c == "" || c == ChangeEditorial || c == ChangeAdditive || c == ChangeNarrowing || c == ChangeBreaking
}

// Durabilities (Salsa): how far a change to a layer propagates.
const (
	durabilityLow    = 0
	durabilityMedium = 1
	durabilityHigh   = 2
)

// layerDurability maps a memory layer to its durability. An unknown layer is
// MEDIUM: it errs toward propagating.
func layerDurability(layer string) int {
	switch layer {
	case "constraints", "decision", "decisions":
		return durabilityHigh
	case "context", "state":
		return durabilityLow
	default:
		return durabilityMedium
	}
}

// Log ops.
const (
	opSet       = "set"       // no live row before: fresh version
	opTouch     = "touch"     // convergent write: same value and metadata, updated_at only
	opSupersede = "supersede" // replaces the live row (archives it)
	opSibling   = "sibling"   // based_on mismatch: both stay live
	opConflict  = "conflict"  // upsert=false with a different value: both stay live, flagged
	opValidity  = "validity"  // valid_from / valid_until stamped on the live row(s)
	opRetract   = "retract"   // archived: the whole key, or one row when prev_memory_id is set
	opResolve   = "resolve"   // conflict resolved to one winner
)

func migrateKnowledgeLog(conn *sql.DB) {
	// rev is AUTOINCREMENT so a compacted revision is never handed out again.
	// author is the key's agent for agent scope ('' otherwise): agent-scope keys
	// of two agents are different keys, so compaction groups on it too.
	// live_ids is the key's full live set AFTER the write (usually one id), so
	// the latest row per key alone rebuilds current state: that is what keeps the
	// log state-complete once older rows of the key are compacted away.
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS knowledge_log (
		rev            INTEGER PRIMARY KEY AUTOINCREMENT,
		project        TEXT NOT NULL,
		scope          TEXT NOT NULL,
		key            TEXT NOT NULL,
		author         TEXT NOT NULL DEFAULT '',
		memory_id      TEXT,
		prev_memory_id TEXT,
		live_ids       TEXT NOT NULL DEFAULT '[]',
		op             TEXT NOT NULL,
		layer          TEXT NOT NULL,
		durability     INTEGER NOT NULL,
		change_class   TEXT NOT NULL,
		declared_class TEXT,
		causal         TEXT,
		agent          TEXT NOT NULL,
		created_at     TEXT NOT NULL
	)`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_knowledge_log_key ON knowledge_log(project, scope, key, rev)`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS knowledge_clock (
		project          TEXT NOT NULL,
		durability       INTEGER NOT NULL,
		last_changed_rev INTEGER NOT NULL,
		PRIMARY KEY (project, durability)
	) WITHOUT ROWID`)
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS knowledge_compaction (
		project       TEXT PRIMARY KEY,
		low_watermark INTEGER NOT NULL,
		compacted_at  TEXT NOT NULL
	) WITHOUT ROWID`)
}

// knowledgeProject is the log/clock project of a key: '*' for global scope, so
// one global change is one row and one clock, whoever wrote it.
func knowledgeProject(project, scope string) string {
	if scope == "global" {
		return "*"
	}
	return project
}

// knowledgeEntry is one write to log. Prev* describe the row the write replaced
// or changed (HasPrev=false for a fresh key). Validity entries set Narrowing.
type knowledgeEntry struct {
	Project, Scope, Key     string
	MemoryID, PrevMemoryID  string
	Op                      string
	Layer, PrevLayer        string
	Value, PrevValue        string
	HasPrev                 bool
	Declared, Causal, Agent string
	Narrowing               bool
	At                      string
}

// normalizedEqual is rule 2's "provably the same meaning": equal after the key
// normalization every write already gets, whitespace collapse and trim.
func normalizedEqual(a, b string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	return norm(a) == norm(b)
}

func maxClass(a, b string) string {
	if changeRank[b] > changeRank[a] {
		return b
	}
	return a
}

// effectiveClass applies the relay's override checks (design §3.3) in order:
//  1. op floor (retract, sibling/conflict/resolve, validity, touch);
//  2. normalized-equal value => editorial, whatever was declared, unless 3 fires;
//  3. a layer moving to a higher durability => at least breaking, lower => at
//     least narrowing;
//  4. the undeclared default is part of the floor: HIGH => breaking, else additive;
//  5. a declared class at or above the floor is kept, below it is raised.
//
// Only rule 2 (and a touch) can make a class editorial.
func effectiveClass(e knowledgeEntry) string {
	switch e.Op {
	case opRetract:
		return ChangeRetraction
	case opTouch:
		return ChangeEditorial
	}
	floor := ChangeEditorial
	switch e.Op {
	case opSibling, opConflict, opResolve:
		floor = ChangeBreaking
	case opValidity:
		if e.Narrowing {
			floor = ChangeNarrowing
		} else {
			floor = ChangeAdditive
		}
	}
	layerRaised := false
	if e.HasPrev && e.PrevLayer != "" && e.PrevLayer != e.Layer {
		switch dn, dp := layerDurability(e.Layer), layerDurability(e.PrevLayer); {
		case dn > dp:
			floor, layerRaised = maxClass(floor, ChangeBreaking), true
		case dn < dp:
			floor, layerRaised = maxClass(floor, ChangeNarrowing), true
		}
	}
	if e.Op == opSupersede && e.HasPrev && !layerRaised && normalizedEqual(e.Value, e.PrevValue) {
		return ChangeEditorial
	}
	if e.Op != opValidity {
		def := ChangeAdditive
		if layerDurability(e.Layer) == durabilityHigh {
			def = ChangeBreaking
		}
		floor = maxClass(floor, def)
	}
	if e.Declared != "" && changeRank[e.Declared] >= changeRank[floor] {
		return e.Declared
	}
	return floor
}

// liveIDsTx returns the key's live memory ids (oldest version first) on q.
func liveIDsTx(q knowledgeQ, project, scope, agent, key string) (string, error) {
	var rows *sql.Rows
	var err error
	switch scope {
	case "agent":
		rows, err = q.Query(`SELECT id FROM memories WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL ORDER BY version, id`, key, project, agent)
	case "project":
		rows, err = q.Query(`SELECT id FROM memories WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL ORDER BY version, id`, key, project)
	default:
		rows, err = q.Query(`SELECT id FROM memories WHERE key = ? AND scope = 'global' AND archived_at IS NULL ORDER BY version, id`, key)
	}
	if err != nil {
		return "", err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	b, _ := json.Marshal(ids)
	return string(b), nil
}

// knowledgeQ is the statement surface of a writer transaction.
type knowledgeQ interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func knNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// appendKnowledgeTx writes one log row for e on q (the write's own tx) and, for
// a non-editorial class, advances the clock of every durability at or below the
// row's. For agent scope the live set is read with e.Agent as the key's author.
func appendKnowledgeTx(q knowledgeQ, e knowledgeEntry, author string) (int64, string, error) {
	class := effectiveClass(e)
	live, err := liveIDsTx(q, e.Project, e.Scope, author, e.Key)
	if err != nil {
		return 0, "", fmt.Errorf("knowledge log live set: %w", err)
	}
	kp := knowledgeProject(e.Project, e.Scope)
	keyAuthor := ""
	if e.Scope == "agent" {
		keyAuthor = author
	}
	dur := layerDurability(e.Layer)
	res, err := q.Exec(`INSERT INTO knowledge_log
		(project, scope, key, author, memory_id, prev_memory_id, live_ids, op, layer, durability, change_class, declared_class, causal, agent, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		kp, e.Scope, e.Key, keyAuthor, knNull(e.MemoryID), knNull(e.PrevMemoryID), live, e.Op, e.Layer, dur,
		class, knNull(e.Declared), knNull(e.Causal), e.Agent, e.At)
	if err != nil {
		return 0, "", fmt.Errorf("knowledge log append: %w", err)
	}
	rev, err := res.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	if class != ChangeEditorial {
		vals := make([]string, 0, dur+1)
		args := make([]any, 0, 3*(dur+1))
		for dd := 0; dd <= dur; dd++ {
			vals = append(vals, "(?, ?, ?)")
			args = append(args, kp, dd, rev)
		}
		if _, err := q.Exec(`INSERT INTO knowledge_clock (project, durability, last_changed_rev) VALUES `+strings.Join(vals, ", ")+`
			ON CONFLICT(project, durability) DO UPDATE SET last_changed_rev = excluded.last_changed_rev`, args...); err != nil {
			return 0, "", fmt.Errorf("knowledge clock: %w", err)
		}
	}
	return rev, class, nil
}

// KnowledgeChange is one knowledge_log row as a reader sees it.
type KnowledgeChange struct {
	Rev          int64    `json:"rev"`
	Project      string   `json:"project"`
	Scope        string   `json:"scope"`
	Key          string   `json:"key"`
	Author       string   `json:"author,omitempty"`
	MemoryID     string   `json:"memory_id,omitempty"`
	PrevMemoryID string   `json:"prev_memory_id,omitempty"`
	LiveIDs      []string `json:"live_ids"`
	Op           string   `json:"op"`
	Layer        string   `json:"layer"`
	Durability   int      `json:"durability"`
	ChangeClass  string   `json:"change_class"`
	Declared     string   `json:"declared_class,omitempty"`
	Causal       string   `json:"causal,omitempty"`
	Agent        string   `json:"agent"`
	CreatedAt    string   `json:"created_at"`
}

// KnowledgeDelta is the answer to "what changed since rev R".
type KnowledgeDelta struct {
	HeadRev int64             `json:"head_rev"`
	Changes []KnowledgeChange `json:"changes"`
	// Compacted: sinceRev is below a compaction watermark of this project (or of
	// global). The delta is still state-complete (the latest row per key
	// survives) but intermediate versions are gone.
	Compacted bool `json:"compacted"`
}

// KnowledgeDelta returns the changes after sinceRev visible to project (its own
// keys plus global ones), on the read-only pool. Editorial rows are skipped
// unless includeEditorial: they change no meaning, but they can change a key's
// live id (a tags-only version), so a state rebuild asks for them.
func (d *DB) KnowledgeDelta(project string, sinceRev int64, includeEditorial bool) (*KnowledgeDelta, error) {
	out := &KnowledgeDelta{Changes: []KnowledgeChange{}}
	if err := d.ro().QueryRow(`SELECT COALESCE(MAX(rev), 0) FROM knowledge_log`).Scan(&out.HeadRev); err != nil {
		return nil, err
	}
	var wm int64
	if err := d.ro().QueryRow(`SELECT COALESCE(MAX(low_watermark), 0) FROM knowledge_compaction WHERE project IN (?, '*')`, project).Scan(&wm); err != nil {
		return nil, err
	}
	out.Compacted = sinceRev < wm
	q := `SELECT rev, project, scope, key, author, COALESCE(memory_id,''), COALESCE(prev_memory_id,''), live_ids, op, layer, durability,
		change_class, COALESCE(declared_class,''), COALESCE(causal,''), agent, created_at
		FROM knowledge_log WHERE rev > ? AND project IN (?, '*')`
	if !includeEditorial {
		q += ` AND change_class <> 'editorial'`
	}
	rows, err := d.ro().Query(q+` ORDER BY rev`, sinceRev, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c KnowledgeChange
		var live string
		if err := rows.Scan(&c.Rev, &c.Project, &c.Scope, &c.Key, &c.Author, &c.MemoryID, &c.PrevMemoryID, &live, &c.Op, &c.Layer,
			&c.Durability, &c.ChangeClass, &c.Declared, &c.Causal, &c.Agent, &c.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(live), &c.LiveIDs)
		out.Changes = append(out.Changes, c)
		// HeadRev and the rows are separate reads: a write between them must
		// not leave HeadRev below a returned rev (the next delta would repeat it).
		if c.Rev > out.HeadRev {
			out.HeadRev = c.Rev
		}
	}
	return out, rows.Err()
}

// CompactKnowledgeLog compacts one project's log (use "*" for global keys) in
// one writer tx. Rows younger than minLag are always kept. Older rows are kept
// when they are the latest row of their key, or breaking, narrowing or a
// retraction; the rest (superseded editorial/additive) are removed and the
// highest removed rev becomes the project's low watermark. Returns the number
// of rows removed.
func (d *DB) CompactKnowledgeLog(project string, minLag time.Duration, now time.Time) (int64, error) {
	cutoff := now.Add(-minLag).UTC().Format(memoryTimeFmt)
	tx, err := d.beginWriterTx()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	const victims = `FROM knowledge_log WHERE project = ? AND created_at < ?
		AND change_class IN ('editorial', 'additive')
		AND rev NOT IN (SELECT MAX(rev) FROM knowledge_log WHERE project = ? GROUP BY scope, key, author)`
	var maxRemoved sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(rev) `+victims, project, cutoff, project).Scan(&maxRemoved); err != nil {
		return 0, err
	}
	if !maxRemoved.Valid {
		return 0, tx.Commit()
	}
	res, err := tx.Exec(`DELETE `+victims, project, cutoff, project)
	if err != nil {
		return 0, fmt.Errorf("compact knowledge log: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`INSERT INTO knowledge_compaction (project, low_watermark, compacted_at) VALUES (?, ?, ?)
		ON CONFLICT(project) DO UPDATE SET low_watermark = MAX(low_watermark, excluded.low_watermark), compacted_at = excluded.compacted_at`,
		project, maxRemoved.Int64, now.UTC().Format(memoryTimeFmt)); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
