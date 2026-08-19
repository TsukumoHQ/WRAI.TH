package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildStatusPayload slots the three lists + note, caps them, and renders a
// readable body — never erroring on oversized input.
func TestBuildStatusPayloadSlotsAndRenders(t *testing.T) {
	content, metadata, blockers := buildStatusPayload(
		[]string{"shipped S4", " ", "wrote tests"}, // blank dropped
		[]string{"S3 typed status"},
		[]string{"none"},
		"  on track  ",
	)
	if blockers != 1 {
		t.Errorf("blockerCount = %d, want 1", blockers)
	}

	var p statusPayload
	if err := json.Unmarshal([]byte(metadata), &p); err != nil {
		t.Fatalf("metadata not valid JSON: %v", err)
	}
	if p.StatusSchema != statusSchema {
		t.Errorf("schema = %d, want %d", p.StatusSchema, statusSchema)
	}
	if len(p.Done) != 2 || p.Done[0] != "shipped S4" {
		t.Errorf("done = %v, want the two non-blank items", p.Done)
	}
	if p.Note != "on track" {
		t.Errorf("note = %q, want trimmed 'on track'", p.Note)
	}
	if p.Truncated != nil {
		t.Errorf("nothing oversized, truncated should be nil: %+v", p.Truncated)
	}
	for _, want := range []string{"STATUS", "DONE: shipped S4; wrote tests", "DOING: S3 typed status", "BLOCKERS: none", "NOTE: on track"} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q; got:\n%s", want, content)
		}
	}
}

// Accept-and-slot: over-cap item count and over-long note are clipped and
// recorded in truncated, never rejected.
func TestBuildStatusPayloadCaps(t *testing.T) {
	many := make([]string, statusMaxSlotItems+5)
	for i := range many {
		many[i] = "item"
	}
	longNote := strings.Repeat("z", statusMaxNoteBytes+50)

	_, metadata, _ := buildStatusPayload(many, nil, nil, longNote)
	var p statusPayload
	if err := json.Unmarshal([]byte(metadata), &p); err != nil {
		t.Fatalf("metadata invalid: %v", err)
	}
	if len(p.Done) != statusMaxSlotItems {
		t.Errorf("done kept %d, want cap %d", len(p.Done), statusMaxSlotItems)
	}
	if p.Truncated == nil || p.Truncated.Done != 5 {
		t.Errorf("expected 5 dropped done items recorded, got %+v", p.Truncated)
	}
	if len(p.Note) > statusMaxNoteBytes || p.Truncated == nil || !p.Truncated.Note {
		t.Errorf("note should be capped + flagged, got len=%d truncated=%+v", len(p.Note), p.Truncated)
	}
	// Empty slots serialize as [] (non-nil), so a consumer never nil-derefs.
	if p.Doing == nil || p.Blockers == nil {
		t.Errorf("empty slots must be [] not null: doing=%v blockers=%v", p.Doing, p.Blockers)
	}
}

// An all-empty status still renders a marker body, never blank.
func TestBuildStatusPayloadEmpty(t *testing.T) {
	content, _, blockers := buildStatusPayload(nil, nil, nil, "")
	if blockers != 0 {
		t.Errorf("blockers = %d, want 0", blockers)
	}
	if !strings.Contains(content, "STATUS") || !strings.Contains(content, "(no items)") {
		t.Errorf("empty status body = %q", content)
	}
}
