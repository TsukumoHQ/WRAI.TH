package db

import (
	"testing"
	"time"
)

func TestValidateMemoryKey(t *testing.T) {
	cases := []struct {
		key      string
		wantHint bool
	}{
		{"wraith-serve-wedge-fix", false},
		{"niwa-ledger-seam", false},
		{"agent-checkpoint-1", true},
		{"CHECKPOINT-foo", true},
		{"resume-state", true},
		{"wraith-tickets-vague2", false},
		{"wraith-t2-2026-08-18", true},
		{"foo-2026-08", true},
	}
	for _, c := range cases {
		got := ValidateMemoryKey(c.key)
		if (got != "") != c.wantHint {
			t.Errorf("ValidateMemoryKey(%q) = %q, wantHint=%v", c.key, got, c.wantHint)
		}
	}
}

func TestValidateMemoryTags(t *testing.T) {
	if hint := ValidateMemoryTags(`["wraith","serve-wedge"]`); hint != "" {
		t.Errorf("expected no hint for populated tags, got %q", hint)
	}
	if hint := ValidateMemoryTags(`[]`); hint == "" {
		t.Error("expected a hint for empty tags array")
	}
	if hint := ValidateMemoryTags(``); hint == "" {
		t.Error("expected a hint for empty tagsJSON string")
	}
	if hint := ValidateMemoryTags(`[""]`); hint == "" {
		t.Error("expected a hint for a tags array of only blank strings")
	}
}

func TestValidateMemoryValidUntil(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	if hint := ValidateMemoryValidUntil("behavior", "", now); hint != "" {
		t.Errorf("non-context layer should never require valid_until, got %q", hint)
	}
	if hint := ValidateMemoryValidUntil("constraints", "", now); hint != "" {
		t.Errorf("non-context layer should never require valid_until, got %q", hint)
	}
	if hint := ValidateMemoryValidUntil("context", "", now); hint == "" {
		t.Error("expected a hint for layer=context with no valid_until")
	}
	if hint := ValidateMemoryValidUntil("context", "not-a-date", now); hint == "" {
		t.Error("expected a hint for an unparseable valid_until")
	}
	within := now.Add(3 * 24 * time.Hour).Format(time.RFC3339)
	if hint := ValidateMemoryValidUntil("context", within, now); hint != "" {
		t.Errorf("valid_until 3 days out should be accepted, got %q", hint)
	}
	tooFar := now.Add(10 * 24 * time.Hour).Format(time.RFC3339)
	if hint := ValidateMemoryValidUntil("context", tooFar, now); hint == "" {
		t.Error("expected a hint for valid_until more than 7 days out")
	}
	exactly7 := now.Add(7 * 24 * time.Hour).Format(time.RFC3339)
	if hint := ValidateMemoryValidUntil("context", exactly7, now); hint != "" {
		t.Errorf("valid_until exactly 7 days out should be accepted, got %q", hint)
	}
	// DB-canonical format must also parse (SetMemoryValidity normalizes to this).
	canonical := now.Add(1 * 24 * time.Hour).Format(memoryTimeFmt)
	if hint := ValidateMemoryValidUntil("context", canonical, now); hint != "" {
		t.Errorf("valid_until in memoryTimeFmt should parse, got %q", hint)
	}
}

func TestValidateRememberArea(t *testing.T) {
	if hint := ValidateRememberArea("wraith/memory"); hint != "" {
		t.Errorf("expected no hint for a real area, got %q", hint)
	}
	if hint := ValidateRememberArea("general"); hint == "" {
		t.Error("expected a hint for area=general")
	}
	if hint := ValidateRememberArea("General"); hint == "" {
		t.Error("expected area=general rejection to be case-insensitive")
	}
	if hint := ValidateRememberArea(" general "); hint == "" {
		t.Error("expected area=general rejection to trim whitespace")
	}
}

func TestMemoryValueWarning(t *testing.T) {
	short := "a rule-first lede under the limit"
	if w := MemoryValueWarning(short); w != "" {
		t.Errorf("expected no warning for a %d-char value, got %q", len(short), w)
	}
	long := make([]byte, 601)
	for i := range long {
		long[i] = 'a'
	}
	if w := MemoryValueWarning(string(long)); w == "" {
		t.Error("expected a warning for a 601-char value")
	}
	exactly600 := make([]byte, 600)
	for i := range exactly600 {
		exactly600[i] = 'a'
	}
	if w := MemoryValueWarning(string(exactly600)); w != "" {
		t.Errorf("expected no warning at exactly 600 chars, got %q", w)
	}
}

func TestValidateMemoryWrite(t *testing.T) {
	now := time.Now().UTC()

	if err := ValidateMemoryWrite("tsukumo", "wraith-serve-wedge-fix", `["wraith"]`, "behavior", "", now); err != nil {
		t.Errorf("expected a clean write to pass, got %v", err)
	}

	err := ValidateMemoryWrite("tsukumo", "agent-checkpoint-42", `["wraith"]`, "behavior", "", now)
	if err == nil {
		t.Fatal("expected a checkpoint-key rejection")
	}
	var de *MemoryDisciplineError
	if !asMemoryDisciplineError(err, &de) || de.Field != "key" {
		t.Errorf("expected a key-field MemoryDisciplineError, got %v", err)
	}

	// Key check runs before the tags check — first violation wins.
	err = ValidateMemoryWrite("tsukumo", "agent-checkpoint-42", `[]`, "behavior", "", now)
	if err == nil || !asMemoryDisciplineError(err, &de) || de.Field != "key" {
		t.Errorf("expected key violation to win over tags violation, got %v", err)
	}

	err = ValidateMemoryWrite("tsukumo", "wraith-serve-wedge-fix", `[]`, "behavior", "", now)
	if err == nil || !asMemoryDisciplineError(err, &de) || de.Field != "tags" {
		t.Errorf("expected a tags-field MemoryDisciplineError, got %v", err)
	}

	err = ValidateMemoryWrite("tsukumo", "wraith-serve-wedge-fix", `["wraith"]`, "context", "", now)
	if err == nil || !asMemoryDisciplineError(err, &de) || de.Field != "valid_until" {
		t.Errorf("expected a valid_until-field MemoryDisciplineError, got %v", err)
	}
}

func asMemoryDisciplineError(err error, target **MemoryDisciplineError) bool {
	de, ok := err.(*MemoryDisciplineError)
	if ok {
		*target = de
	}
	return ok
}
