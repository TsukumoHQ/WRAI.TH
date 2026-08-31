package db

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// checkpointKeyPattern flags a memory key that looks like a checkpoint/resume
// artifact rather than a durable fact: the literal words "checkpoint"/"resume",
// or a dated stamp (e.g. "...-2026-08-22..."). Checkpoints belong in resume
// files (~/.agentd/resume/) or trovex, per Memory Protocol v2 R4 — a relay
// memory that only makes sense with a date attached is a snapshot, not a rule.
var checkpointKeyPattern = regexp.MustCompile(`(?i)checkpoint|resume|20\d{2}-\d{2}(-\d{2})?`)

// MemoryDisciplineError is returned when a memory write violates Memory
// Protocol v2 (DEC-governance-memory-protocol-2) on a project that opted in
// (ProjectRequiresMemoryDiscipline). One error per violated field, with a hint
// pointing at the fix — never a bare rejection.
type MemoryDisciplineError struct {
	Project string
	Field   string
	Hint    string
}

func (e *MemoryDisciplineError) Error() string {
	return fmt.Sprintf("memory discipline (project %q): %s. %s", e.Project, e.Field, e.Hint)
}

// ValidateMemoryKey rejects a checkpoint/resume/dated-looking key. Returns ""
// when the key is fine.
func ValidateMemoryKey(key string) string {
	if checkpointKeyPattern.MatchString(key) {
		return "checkpoints/dated snapshots don't belong in relay memory — write a resume file (~/.agentd/resume/<agent>.md) or a trovex doc instead, and if this really is a durable rule, key it '<lane>-<topic-slug>' with no date in it"
	}
	return ""
}

// ValidateMemoryTags rejects an empty tag set. tagsJSON is the JSON-array form
// already normalized by the caller ("[]" when the caller passed none). Returns
// "" when at least one non-blank tag is present.
func ValidateMemoryTags(tagsJSON string) string {
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	for _, t := range tags {
		if strings.TrimSpace(t) != "" {
			return ""
		}
	}
	return "at least 1 tag is required — use the lane+topic convention, e.g. [\"wraith\", \"serve-wedge\"]"
}

// maxContextValidity is Memory Protocol v2 R3's bound on a layer=context
// memory's valid_until: ephemeral facts must expire soon, or they're really a
// constraint (durable) or a checkpoint (doesn't belong here at all).
const maxContextValidity = 7 * 24 * time.Hour

// ValidateMemoryValidUntil enforces R3 for layer="context" writes: valid_until
// is required and must be no more than 7 days out. now is injected for
// testability. Returns "" when layer isn't "context" (no requirement) or the
// window is fine.
func ValidateMemoryValidUntil(layer, validUntil string, now time.Time) string {
	if layer != "context" {
		return ""
	}
	if strings.TrimSpace(validUntil) == "" {
		return "layer=context requires valid_until (an ISO-8601 UTC timestamp ≤7 days out) — ephemeral facts must expire; a fact meant to last belongs in layer=constraints or layer=behavior instead"
	}
	t, err := parseAnyMemoryTime(validUntil)
	if err != nil {
		return fmt.Sprintf("valid_until %q is not a parseable ISO-8601 UTC timestamp", validUntil)
	}
	if t.After(now.Add(maxContextValidity)) {
		return fmt.Sprintf("valid_until %q is more than 7 days out — layer=context must expire soon; use layer=constraints/behavior for anything longer-lived", validUntil)
	}
	return ""
}

// parseAnyMemoryTime accepts either the DB's canonical memoryTimeFmt or plain
// RFC3339, since callers (the set_memory tool) pass a caller-supplied
// ISO-8601 string that hasn't gone through the DB's own normalization yet.
func parseAnyMemoryTime(s string) (time.Time, error) {
	if t, err := time.Parse(memoryTimeFmt, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// ValidateRememberArea rejects the catch-all area="general" on a decision —
// R6: remember() records settled, specific rulings, and a "general" area is a
// sure sign the decision has no real home. Returns "" when area is fine.
func ValidateRememberArea(area string) string {
	if strings.EqualFold(strings.TrimSpace(area), "general") {
		return `area "general" isn't a real area — name the subsystem/lane this decision governs (e.g. "wraith/memory", "niwa/ledger-seam")`
	}
	return ""
}

// MemoryValueWarning flags (but never blocks) a value over 600 chars, so the
// rule-first, ≤200-char lede convention (Memory Protocol v2 R1) stays visible
// as feedback without breaking a legitimately longer write. Returns "" when
// the value is within bounds.
func MemoryValueWarning(value string) string {
	if len(value) > 600 {
		return fmt.Sprintf("value is %d chars — Memory Protocol v2 wants a rule-first lede (≤200 chars carries the rule); consider trimming, or moving long-form detail to a trovex doc and keeping just the pointer here", len(value))
	}
	return ""
}

// ValidateMemoryWrite runs every Memory Protocol v2 write-side check for
// set_memory and returns the first violation found (key, then tags, then the
// valid_until/layer pairing) as a *MemoryDisciplineError, or nil when the
// write is clean. Callers gate this behind ProjectRequiresMemoryDiscipline —
// it does not check the flag itself, so it stays a pure, easily-testable rule.
func ValidateMemoryWrite(project, key, tagsJSON, layer, validUntil string, now time.Time) error {
	if hint := ValidateMemoryKey(key); hint != "" {
		return &MemoryDisciplineError{Project: project, Field: "key", Hint: hint}
	}
	if hint := ValidateMemoryTags(tagsJSON); hint != "" {
		return &MemoryDisciplineError{Project: project, Field: "tags", Hint: hint}
	}
	if hint := ValidateMemoryValidUntil(layer, validUntil, now); hint != "" {
		return &MemoryDisciplineError{Project: project, Field: "valid_until", Hint: hint}
	}
	return nil
}
