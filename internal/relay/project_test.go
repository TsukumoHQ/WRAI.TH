package relay

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-relay/internal/models"
)

func TestSummarizeTask_TruncatesLongDescription(t *testing.T) {
	longDesc := strings.Repeat("x", 5000)
	s := summarizeTask(models.Task{
		ID: "abc", Title: "t", Priority: "P2", Status: "pending",
		Description: longDesc,
	})
	if !s.DescTruncated {
		t.Fatal("expected desc_truncated=true for 5KB description")
	}
	if len(s.DescPreview) != taskDescPreview {
		t.Fatalf("desc_preview len: got %d, want %d", len(s.DescPreview), taskDescPreview)
	}
}

func TestSummarizeTask_ShortDescriptionKept(t *testing.T) {
	s := summarizeTask(models.Task{
		ID: "abc", Title: "t", Priority: "P2", Status: "pending",
		Description: "short",
	})
	if s.DescTruncated {
		t.Fatal("did not expect desc_truncated for short description")
	}
	if s.DescPreview != "short" {
		t.Fatalf("desc_preview: got %q", s.DescPreview)
	}
}

func TestProjectTasks_EnforcesBudget(t *testing.T) {
	var tasks []models.Task
	for i := 0; i < 50; i++ {
		tasks = append(tasks, models.Task{
			ID:          "task-id-" + strings.Repeat("z", 4),
			Title:       "title number " + strings.Repeat("t", 20),
			Priority:    "P2",
			Status:      "pending",
			Description: strings.Repeat("d", 10000),
		})
	}

	out := projectTasks(tasks, 2000)
	used := 0
	for _, s := range out {
		used += taskSummaryBytes(s)
	}
	if used > 2500 { // small slack for overhead computation
		t.Fatalf("budget exceeded: used %d > 2000+slack", used)
	}
}

func TestProjectTasks_P0AlwaysIncluded(t *testing.T) {
	tasks := []models.Task{
		{ID: "low1", Title: "low priority", Priority: "P3", Status: "pending"},
		{ID: "crit", Title: "critical", Priority: "P0", Status: "pending"},
	}
	// Budget=0 means "no budget" per projectTasks; use a tiny non-zero budget.
	out := projectTasks(tasks, 10)
	foundP0 := false
	for _, s := range out {
		if s.ID == "crit" {
			foundP0 = true
		}
	}
	if !foundP0 {
		t.Fatal("P0 task must bypass budget")
	}
}

func TestProjectTasks_SortsByPriority(t *testing.T) {
	tasks := []models.Task{
		{ID: "p3", Title: "p3", Priority: "P3", Status: "pending", DispatchedAt: "2026-01-01"},
		{ID: "p0", Title: "p0", Priority: "P0", Status: "pending", DispatchedAt: "2026-01-02"},
		{ID: "p1", Title: "p1", Priority: "P1", Status: "pending", DispatchedAt: "2026-01-03"},
	}
	out := projectTasks(tasks, 0)
	if len(out) != 3 {
		t.Fatalf("expected 3 out, got %d", len(out))
	}
	if out[0].ID != "p0" || out[1].ID != "p1" || out[2].ID != "p3" {
		t.Fatalf("wrong priority order: %s, %s, %s", out[0].ID, out[1].ID, out[2].ID)
	}
}

func TestSummarizeMessage_TruncatesLongContent(t *testing.T) {
	s := summarizeMessage(models.Message{
		ID: "m1", From: "prometheus", Priority: "P1",
		Content: strings.Repeat("digest ", 600), // ~4KB GlitchTip-style body
	})
	if !s.ContentTruncated {
		t.Fatal("expected content_truncated=true for verbose alert body")
	}
	if len(s.ContentPreview) != msgContentPreview {
		t.Fatalf("content_preview len: got %d, want %d", len(s.ContentPreview), msgContentPreview)
	}
}

func TestSummarizeMessage_ShortContentKept(t *testing.T) {
	s := summarizeMessage(models.Message{ID: "m1", From: "a", Content: "ping"})
	if s.ContentTruncated {
		t.Fatal("did not expect content_truncated for short body")
	}
	if s.ContentPreview != "ping" {
		t.Fatalf("content_preview: got %q", s.ContentPreview)
	}
}

// Acceptance: a boot payload with 10+ verbose unread P0/P1 stays small.
func TestProjectMessages_BoundsVerboseUnread(t *testing.T) {
	var msgs []models.Message
	for i := 0; i < 12; i++ {
		p := "P0"
		if i%2 == 0 {
			p = "P1"
		}
		msgs = append(msgs, models.Message{
			ID:        "msg-" + strings.Repeat("z", 8),
			From:      "alertmanager",
			Subject:   "ALERT",
			Priority:  p,
			CreatedAt: "2026-06-11T00:00:0" + string(rune('0'+i%10)) + "Z",
			Content:   strings.Repeat("Prometheus digest line. ", 200), // ~4.8KB each
		})
	}
	out := projectMessages(msgs, sessionUnreadBudget)
	total := 0
	for _, s := range out {
		total += messageSummaryBytes(s)
		if len(s.ContentPreview) > msgContentPreview {
			t.Fatalf("a preview exceeded cap: %d", len(s.ContentPreview))
		}
	}
	// 12 × 4.8KB raw = ~58KB. Projected must stay an order of magnitude smaller.
	if total > 16000 {
		t.Fatalf("projected unread payload too large: %d bytes", total)
	}
}

func TestProjectMessages_P0BypassesBudget(t *testing.T) {
	msgs := []models.Message{
		{ID: "low", From: "a", Priority: "P3", Content: "noise"},
		{ID: "crit", From: "a", Priority: "P0", Content: "fire"},
	}
	out := projectMessages(msgs, 10) // tiny budget
	found := false
	for _, s := range out {
		if s.ID == "crit" {
			found = true
		}
	}
	if !found {
		t.Fatal("P0 message must bypass the budget")
	}
}

func TestTruncatePreview_RuneSafe(t *testing.T) {
	// "é" is 2 bytes in UTF-8; a 5-byte cap on "aaaaé" (6 bytes) must not
	// keep the first byte of the é sequence.
	s, truncated := truncatePreview("aaaaé", 5)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if s != "aaaa" {
		t.Fatalf("got %q, want %q", s, "aaaa")
	}
	// Exact fit: no truncation.
	s, truncated = truncatePreview("aaaaé", 6)
	if truncated || s != "aaaaé" {
		t.Fatalf("got %q truncated=%v", s, truncated)
	}
	// French text heavy in accents stays valid UTF-8 at any cap.
	src := strings.Repeat("salaire médiane AFC très élevée ", 20)
	for max := 1; max < 64; max++ {
		cut, _ := truncatePreview(src, max)
		if !utf8.ValidString(cut) {
			t.Fatalf("invalid UTF-8 at max=%d: %q", max, cut)
		}
		if len(cut) > max {
			t.Fatalf("cut longer than max at max=%d", max)
		}
	}
}

func TestProjectMemories_ConstraintsBypassBudget(t *testing.T) {
	var mems []models.Memory
	// One constraints memory listed LAST with a huge value, after enough
	// behavior memories to blow the budget.
	for i := 0; i < 30; i++ {
		mems = append(mems, models.Memory{
			Key: "behavior-" + strings.Repeat("k", 10), Value: strings.Repeat("v", 400),
			Scope: "project", Layer: "behavior",
		})
	}
	mems = append(mems, models.Memory{
		Key: "cto-rule", Value: strings.Repeat("c", 2000),
		Scope: "project", Layer: "constraints",
	})

	out := projectMemories(mems, sessionMemoryBudget)

	found := false
	for _, s := range out {
		if s.Key == "cto-rule" {
			found = true
			if !s.ValueTruncated || len(s.ValuePreview) != memValuePreview {
				t.Fatalf("constraint value preview: len=%d truncated=%v", len(s.ValuePreview), s.ValueTruncated)
			}
		}
	}
	if !found {
		t.Fatal("constraints-layer memory dropped by budget — must bypass")
	}
	if len(out) >= len(mems) {
		t.Fatal("expected budget to drop some behavior memories")
	}
}

func TestProjectMemories_BudgetBound(t *testing.T) {
	var mems []models.Memory
	for i := 0; i < 50; i++ {
		mems = append(mems, models.Memory{
			Key: "k", Value: strings.Repeat("v", 400), Scope: "project", Layer: "behavior",
		})
	}
	out := projectMemories(mems, sessionMemoryBudget)
	used := 0
	for _, s := range out {
		used += memorySummaryBytes(s)
	}
	if used > sessionMemoryBudget {
		t.Fatalf("budget exceeded: %d > %d", used, sessionMemoryBudget)
	}
}

// TestProjectMessages_P0FloodObeysHardCeiling verifies the Def.7 hard ceiling:
// P0 messages bypass the soft budget but a flood cannot exceed
// soft*budgetHardMultiplier, so one agent can't inflate a peer's boot payload
// without bound. The single most-important P0 still always surfaces.
func TestProjectMessages_P0FloodObeysHardCeiling(t *testing.T) {
	var msgs []models.Message
	for i := 0; i < 200; i++ {
		msgs = append(msgs, models.Message{
			ID: "m", From: "attacker", Priority: "P0",
			Content: strings.Repeat("x", 400),
		})
	}
	const soft = 6000
	out := projectMessages(msgs, soft)

	total := 0
	for _, s := range out {
		total += messageSummaryBytes(s)
	}
	hardCeil := soft * budgetHardMultiplier
	// Allow one item of slack: the first item is always admitted even if it
	// alone would exceed the ceiling.
	if total > hardCeil+700 {
		t.Fatalf("P0 flood exceeded hard ceiling: %d > %d", total, hardCeil)
	}
	if len(out) >= len(msgs) {
		t.Fatalf("expected the hard ceiling to drop most of the %d P0 messages, kept %d", len(msgs), len(out))
	}
	if len(out) == 0 {
		t.Fatal("at least the most-important P0 must surface")
	}
}

// TestProjectTasks_P0FloodObeysHardCeiling mirrors the message test for tasks.
func TestProjectTasks_P0FloodObeysHardCeiling(t *testing.T) {
	var tasks []models.Task
	for i := 0; i < 200; i++ {
		tasks = append(tasks, models.Task{
			ID: "t", Title: "crit", Priority: "P0", Status: "pending",
			Description: strings.Repeat("x", 400),
		})
	}
	const soft = 8000
	out := projectTasks(tasks, soft)
	total := 0
	for _, s := range out {
		total += taskSummaryBytes(s)
	}
	if total > soft*budgetHardMultiplier+700 {
		t.Fatalf("P0 task flood exceeded hard ceiling: %d", total)
	}
	if len(out) >= len(tasks) || len(out) == 0 {
		t.Fatalf("hard ceiling should bound P0 tasks, kept %d of %d", len(out), len(tasks))
	}
}

func TestProjectDecisions_TruncatesAndByteBudgets(t *testing.T) {
	// The regression: agents write multi-line checkpoint/resume blobs as
	// decisions, so 40 of them dominated the boot payload. Build 40 fat ones.
	big := strings.Repeat("y", 3000)
	val := `{"decision":"` + big + `","rationale":"` + big + `","area":"ops","status":"accepted"}`
	decs := make([]models.Memory, 0, 40)
	for i := 0; i < 40; i++ {
		decs = append(decs, models.Memory{Key: "DEC-ops-x", Value: val, Layer: "decision"})
	}
	out := projectDecisions(decs, sessionDecisionMax)

	// Every field is preview-truncated (rune-safe helper, ≤ decisionPreview).
	total := 0
	for _, d := range out {
		if len(d.Decision) > decisionPreview {
			t.Fatalf("Decision not truncated: %d > %d", len(d.Decision), decisionPreview)
		}
		if len(d.Rationale) > decisionPreview {
			t.Fatalf("Rationale not truncated: %d > %d", len(d.Rationale), decisionPreview)
		}
		total += decisionSummaryBytes(d)
	}
	// The whole section is ENCODED-byte-bounded near the budget — NOT the raw
	// 40×6KB. Slack = one always-surfaced first entry over the budget.
	if total > sessionDecisionBudget+decisionPreview*2+256 {
		t.Fatalf("decisions[] not byte-bounded: %d encoded bytes", total)
	}
	// The first (most-recent) decision always surfaces.
	if len(out) == 0 {
		t.Fatal("expected at least the first decision to surface under budget")
	}

	// A raw (non-DecisionValue-JSON) value still lands truncated in Decision.
	raw := projectDecisions([]models.Memory{{Key: "DEC-raw", Value: strings.Repeat("z", 2000)}}, sessionDecisionMax)
	if len(raw) != 1 || len(raw[0].Decision) > decisionPreview {
		t.Fatalf("raw decision value not truncated: %+v", raw)
	}
}

// TestProjectDecisions_KeyVerbatimOrDropped proves the Key is a lookup handle:
// a normal key is surfaced byte-for-byte (so get_memory/supersedes resolve), and
// a pathologically oversized key is DROPPED — never surfaced truncated (which
// would return a dangling identifier).
func TestProjectDecisions_KeyVerbatimOrDropped(t *testing.T) {
	normalKey := "DEC-ops-3"
	hugeKey := "DEC-" + strings.Repeat("k", decisionKeyMax) // > decisionKeyMax
	mk := func(k string) models.Memory {
		return models.Memory{Key: k, Value: `{"decision":"d","area":"ops","status":"accepted"}`, Layer: "decision"}
	}
	out := projectDecisions([]models.Memory{mk(normalKey), mk(hugeKey)}, sessionDecisionMax)

	if len(out) != 1 {
		t.Fatalf("expected only the normal-key decision to surface, got %d: %+v", len(out), out)
	}
	if out[0].Key != normalKey {
		t.Fatalf("key not surfaced verbatim: got %q want %q", out[0].Key, normalKey)
	}
	// The oversized key must appear NOWHERE — not verbatim, not truncated.
	for _, d := range out {
		if strings.HasPrefix(d.Key, "DEC-k") {
			t.Fatalf("oversized key leaked (possibly truncated): %q", d.Key)
		}
	}
}

// TestProjectDecisions_CapAppliesAfterKeyFilter proves the count cap is spent on
// VALID decisions only: a run of oversized-key recent decisions must not consume
// the cap and starve a valid decision that follows, while capacity is unused.
func TestProjectDecisions_CapAppliesAfterKeyFilter(t *testing.T) {
	hugeKey := "DEC-" + strings.Repeat("k", decisionKeyMax)
	val := `{"decision":"d","area":"ops","status":"accepted"}`
	// max recent decisions all have oversized keys; the (max+1)th is valid.
	const max = 3
	decs := make([]models.Memory, 0, max+1)
	for i := 0; i < max; i++ {
		decs = append(decs, models.Memory{Key: hugeKey, Value: val, Layer: "decision"})
	}
	decs = append(decs, models.Memory{Key: "DEC-valid-1", Value: val, Layer: "decision"})

	out := projectDecisions(decs, max)
	if len(out) != 1 || out[0].Key != "DEC-valid-1" {
		t.Fatalf("valid decision starved by oversized-key run: %+v", out)
	}
}

// TestProjectDecisions_EncodedBudget_ControlChars proves the byte budget counts
// the ENCODED JSON size, not raw field lengths: escape-heavy decisions (quotes +
// newlines) roughly double on the wire, so a raw-len budget under-counts and lets
// decisions[] exceed the intended bound.
func TestProjectDecisions_EncodedBudget_ControlChars(t *testing.T) {
	// 3000 runes of `"` then `\n` — every byte becomes a 2-byte JSON escape.
	heavy := strings.Repeat("\"\n", 1500)
	valBytes, _ := json.Marshal(map[string]string{
		"decision": heavy, "rationale": heavy, "area": "ops", "status": "accepted",
	})
	decs := make([]models.Memory, 0, 40)
	for i := 0; i < 40; i++ {
		decs = append(decs, models.Memory{Key: "DEC-esc", Value: string(valBytes), Layer: "decision"})
	}
	out := projectDecisions(decs, sessionDecisionMax)

	encoded := 0
	raw := 0
	for _, d := range out {
		encoded += decisionSummaryBytes(d)
		raw += len(d.Key) + len(d.Area) + len(d.Decision) + len(d.Rationale)
	}
	if encoded > sessionDecisionBudget+decisionPreview*2+256 {
		t.Fatalf("encoded decisions[] exceeded budget: %d bytes", encoded)
	}
	// The whole point of the fix: encoded size materially exceeds raw for
	// escape-heavy content, so a raw-len budget would have over-admitted.
	if raw >= encoded {
		t.Fatalf("expected encoded > raw for escape-heavy decisions: raw=%d encoded=%d", raw, encoded)
	}
}

// TestProjectDecisions_OmittedReflectsByteTruncation proves the handler must
// compute decisions_omitted from the PROJECTED length: with fewer items than the
// count cap but more bytes than the budget, the old count-cap formula reports 0
// omitted while the projection actually drops entries.
func TestProjectDecisions_OmittedReflectsByteTruncation(t *testing.T) {
	big := strings.Repeat("y", 3000)
	val := `{"decision":"` + big + `","rationale":"` + big + `","area":"ops","status":"accepted"}`
	decs := make([]models.Memory, 0, 30) // 30 < sessionDecisionMax (40)
	for i := 0; i < 30; i++ {
		decs = append(decs, models.Memory{Key: "DEC-ops", Value: val, Layer: "decision"})
	}
	out := projectDecisions(decs, sessionDecisionMax)
	if len(out) >= len(decs) {
		t.Fatalf("expected byte budget to drop decisions: projected %d of %d", len(out), len(decs))
	}
	omitted := len(decs) - len(out) // handler's new formula
	countCapOmitted := 0            // old buggy formula: len<=cap → 0
	if len(decs) > sessionDecisionMax {
		countCapOmitted = len(decs) - sessionDecisionMax
	}
	if omitted <= countCapOmitted {
		t.Fatalf("projected-omitted (%d) must exceed count-cap-omitted (%d)", omitted, countCapOmitted)
	}
}
