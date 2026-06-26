package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-relay/internal/models"
)

// DecisionValue is the structured payload stored in a decision memory's value
// (layer="decision"). The ADR record: the settled rule + why, its lifecycle
// status, the area it governs, and the prior decision it replaces.
type DecisionValue struct {
	Decision   string `json:"decision"`
	Rationale  string `json:"rationale,omitempty"`
	Status     string `json:"status"` // accepted | superseded | needs_review
	Area       string `json:"area,omitempty"`
	Supersedes string `json:"supersedes,omitempty"`
}

// decisionKeyArea sanitizes an area into the key segment (DEC-<area>-NNN): keys
// can't carry slashes/spaces, but the original area is preserved in the value.
func decisionKeyArea(area string) string {
	a := strings.ToLower(strings.TrimSpace(area))
	a = strings.NewReplacer("/", "-", " ", "-", "_", "-").Replace(a)
	if a == "" {
		a = "general"
	}
	return a
}

// nextDecisionSeq returns the next monotonic number for an area's DEC keys.
// Counts all (incl. archived) so numbers are never reused.
func (d *DB) nextDecisionSeq(project, keyArea string) int {
	var n int
	_ = d.ro().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE layer='decision' AND project=? AND key LIKE ?`,
		project, "DEC-"+keyArea+"-%",
	).Scan(&n)
	return n + 1
}

// RememberDecision records an ADR-style decision as a memory (layer="decision",
// scope="project" so the whole team reads it). key = DEC-<area>-NNN. Enforces
// dedup-or-supersede: an active decision in the same area with the same text is
// rejected unless `supersedes` is given; on supersede the named prior decision
// is archived so only the live set stays "accepted". Returns the new decision.
func (d *DB) RememberDecision(project, agent, area, decision, rationale string, tags []string, supersedes string) (*models.Memory, error) {
	decision = strings.TrimSpace(decision)
	if decision == "" {
		return nil, fmt.Errorf("decision text is required")
	}
	keyArea := decisionKeyArea(area)

	// Dedup-or-supersede: reject a near-identical active decision in this area
	// unless the caller explicitly supersedes one.
	if supersedes == "" {
		active, _ := d.GetMemoriesByLayer(project, "", "decision")
		for _, m := range active {
			var dv DecisionValue
			if json.Unmarshal([]byte(m.Value), &dv) != nil {
				continue
			}
			if decisionKeyArea(dv.Area) == keyArea && normalizeDecisionText(dv.Decision) == normalizeDecisionText(decision) {
				return nil, fmt.Errorf("near-duplicate of %s (%q) — pass supersedes=%q to replace it", m.Key, dv.Decision, m.Key)
			}
		}
	} else if err := d.archiveDecision(project, supersedes, agent); err != nil {
		return nil, err
	}

	key := fmt.Sprintf("DEC-%s-%d", keyArea, d.nextDecisionSeq(project, keyArea))
	val := DecisionValue{Decision: decision, Rationale: strings.TrimSpace(rationale), Status: "accepted", Area: strings.TrimSpace(area), Supersedes: supersedes}
	vj, _ := json.Marshal(val)

	// Tag with the area for search/filtering (plus any caller tags).
	allTags := append([]string{"decision", keyArea}, tags...)
	return d.SetMemory(project, agent, key, string(vj), TagsToJSON(allTags), "project", "stated", "decision", true)
}

// archiveDecision marks a prior decision superseded (archived → leaves the
// accepted set). Errors if the key isn't an active decision.
func (d *DB) archiveDecision(project, key, agent string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	res, err := d.conn.Exec(
		`UPDATE memories SET archived_at=?, archived_by=? WHERE project=? AND key=? AND layer='decision' AND archived_at IS NULL`,
		now, "superseded:"+agent, project, key,
	)
	if err != nil {
		return fmt.Errorf("archive superseded decision: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("supersedes %q: no active decision with that id", key)
	}
	return nil
}

// ListDecisions returns the accepted (active, non-superseded) decisions for a
// project — the bounded set injected at session start.
func (d *DB) ListDecisions(project string) ([]models.Memory, error) {
	return d.GetMemoriesByLayer(project, "", "decision")
}

func normalizeDecisionText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
