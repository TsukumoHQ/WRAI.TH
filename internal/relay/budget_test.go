package relay

import (
	"bytes"
	"encoding/json"
	"log"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/models"
)

func msg(id, priority, content string) models.Message {
	return models.Message{
		ID:       id,
		From:     "a",
		To:       "b",
		Priority: priority,
		Subject:  "test",
		Content:  content,
		Metadata: "{}",
	}
}

func TestApplyBudgetP0Bypass(t *testing.T) {
	msgs := []models.Message{
		msg("1", "P0", "critical"),
		msg("2", "P2", "normal"),
	}
	// Budget too small for both — P0 must still be included
	result := applyBudget(msgs, nil, 50)
	found := false
	for _, m := range result {
		if m.ID == "1" {
			found = true
		}
	}
	if !found {
		t.Error("P0 message should always be included")
	}
}

func TestApplyBudgetEmptyTags(t *testing.T) {
	score := jaccard(nil, nil)
	if score != 0 {
		t.Errorf("expected 0 for empty tags, got %f", score)
	}
}

func TestJaccardSimilarity(t *testing.T) {
	a := map[string]bool{"database": true, "auth": true, "api": true}
	b := map[string]bool{"database": true, "frontend": true, "auth": true}
	// intersection=2, union=4 → 0.5
	j := jaccard(a, b)
	if j < 0.49 || j > 0.51 {
		t.Errorf("expected ~0.5, got %f", j)
	}
}

func TestApplyBudgetRespectsLimit(t *testing.T) {
	msgs := []models.Message{
		msg("1", "P2", "aaaa"),
		msg("2", "P2", "bbbb"),
		msg("3", "P2", "cccc"),
	}
	// Each message is roughly 20 bytes. Budget for ~2.
	b := messageBytes(msgs[0])
	result := applyBudget(msgs, nil, b*2+1)
	if len(result) > 2 {
		t.Errorf("expected at most 2 messages, got %d", len(result))
	}
}

func TestApplyBudgetZeroBudget(t *testing.T) {
	msgs := []models.Message{msg("1", "P2", "hello")}
	result := applyBudget(msgs, nil, 0)
	if len(result) != 1 {
		t.Error("zero budget should return all messages (no filtering)")
	}
}

func TestExtractTags(t *testing.T) {
	tags := extractTags(`{"tags":["db","auth"],"other":"x"}`)
	if len(tags) != 2 || !tags["db"] || !tags["auth"] {
		t.Errorf("expected {db, auth}, got %v", tags)
	}

	empty := extractTags(`{"no_tags": true}`)
	if len(empty) != 0 {
		t.Error("expected empty tags")
	}
}

// captureBudgetJournal runs fn with the std logger redirected and returns
// every decoded "[budget]" journal line it emitted.
func captureBudgetJournal(t *testing.T, fn func()) []budgetJournal {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	fn()
	var out []budgetJournal
	for _, line := range strings.Split(buf.String(), "\n") {
		i := strings.Index(line, "[budget] ")
		if i < 0 {
			continue
		}
		var j budgetJournal
		if err := json.Unmarshal([]byte(line[i+len("[budget] "):]), &j); err != nil {
			t.Fatalf("journal line is not JSON: %q: %v", line, err)
		}
		out = append(out, j)
	}
	return out
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// AC1: "" and out-of-range priorities score exactly like P3, never above P1.
func TestUtilityUnknownPriorityScoresAsP3(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339Nano)
	score := func(p string) float64 {
		m := msg("x", p, "c")
		m.CreatedAt = ts
		return utility(m, nil, now)
	}
	p0, p1, p3 := score("P0"), score("P1"), score("P3")
	for _, p := range []string{"", "P4", "urgent", "p1"} {
		got := score(p)
		if !approx(got, p3) {
			t.Errorf("priority %q scored %f, want P3 score %f", p, got, p3)
		}
		if got >= p1 || got >= p0 {
			t.Errorf("priority %q scored %f, must be below P1 %f and P0 %f", p, got, p1, p0)
		}
	}
}

// AC1: final order uses the priority index (P0<P1<P2<P3<unknown), not a
// string compare where "" would sort before "P0".
func TestApplyBudgetOrdersByPriorityIndex(t *testing.T) {
	msgs := []models.Message{
		msg("unknown", "", "c"),
		msg("p1", "P1", "c"),
		msg("p0", "P0", "c"),
		msg("p3", "P3", "c"),
	}
	var result []models.Message
	captureBudgetJournal(t, func() { result = applyBudget(msgs, nil, 1<<20) })
	var ids []string
	for _, m := range result {
		ids = append(ids, m.ID)
	}
	if got, want := strings.Join(ids, ","), "p0,p1,p3,unknown"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

// AC2: RFC3339Nano and the legacy layout both parse; unparseable -> 0;
// future -> age 0 -> 1.
func TestFreshnessParsing(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	hourAgo := now.Add(-time.Hour)
	cases := []struct {
		name, createdAt string
		want            float64
	}{
		{"rfc3339nano", hourAgo.Format(time.RFC3339Nano), 0.5},
		{"rfc3339 offset", hourAgo.In(time.FixedZone("CEST", 2*3600)).Format(time.RFC3339), 0.5},
		{"legacy layout", hourAgo.Format(legacyCreatedAtLayout), 0.5},
		{"unparseable", "not-a-date", 0},
		{"empty", "", 0},
		{"future", now.Add(time.Hour).Format(time.RFC3339Nano), 1},
	}
	for _, c := range cases {
		if got := freshness(c.createdAt, now); !approx(got, c.want) {
			t.Errorf("%s: freshness(%q) = %f, want %f", c.name, c.createdAt, got, c.want)
		}
	}
}

// AC2 via utility: an unparseable created_at no longer earns full freshness.
func TestUtilityUnparseableCreatedAtNotFresh(t *testing.T) {
	now := time.Now().UTC()
	bad := msg("x", "P3", "c")
	bad.CreatedAt = "garbage"
	if got := utility(bad, nil, now); got != 0 {
		t.Errorf("P3 with unparseable created_at scored %f, want 0", got)
	}
}

// AC3: with no tags on either side the tag term is dropped and weights
// renormalise to 0.875/0.125, so a fresh P0 reaches exactly 1.0.
func TestUtilityNoTagRenormalisation(t *testing.T) {
	now := time.Now().UTC()
	fresh := msg("x", "P0", "c")
	fresh.CreatedAt = now.Format(time.RFC3339Nano)

	agentTags := map[string]bool{"db": true}
	if got := utility(fresh, nil, now); !approx(got, 1.0) {
		t.Errorf("no agent tags: fresh P0 scored %f, want 1.0", got)
	}
	if got := utility(fresh, agentTags, now); !approx(got, 1.0) {
		t.Errorf("no message tags: fresh P0 scored %f, want 1.0", got)
	}

	// Max over every priority in the no-tag case is 1.0 and nothing exceeds it.
	maxScore := 0.0
	for _, p := range []string{"P0", "P1", "P2", "P3", ""} {
		m := msg("x", p, "c")
		m.CreatedAt = now.Format(time.RFC3339Nano)
		if s := utility(m, nil, now); s > maxScore {
			maxScore = s
		}
	}
	if !approx(maxScore, 1.0) {
		t.Errorf("max no-tag score = %f, want 1.0", maxScore)
	}

	// With tags on both sides the original 0.7/0.2/0.1 weights still apply.
	tagged := fresh
	tagged.Metadata = `{"tags":["db"]}`
	if got := utility(tagged, agentTags, now); !approx(got, 1.0) {
		t.Errorf("full tag match: fresh P0 scored %f, want 1.0", got)
	}
	tagged.Metadata = `{"tags":["ui"]}`
	if got := utility(tagged, agentTags, now); !approx(got, 0.8) {
		t.Errorf("disjoint tags: fresh P0 scored %f, want 0.8", got)
	}
}

// AC4: exactly one structured journal line per call with the candidate ids,
// selected ids, per-message score and bytes, and budget used/max.
func TestApplyBudgetJournalLine(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	msgs := []models.Message{
		msg("p0", "P0", "critical"),
		msg("p1", "P1", "aaaa"),
		msg("p3", "P3", "bbbb"),
	}
	for i := range msgs {
		msgs[i].CreatedAt = now
	}
	// Room for P0 + one more.
	maxBytes := messageBytes(msgs[0]) + messageBytes(msgs[1])

	lines := captureBudgetJournal(t, func() { applyBudget(msgs, nil, maxBytes) })
	if len(lines) != 1 {
		t.Fatalf("got %d [budget] lines, want exactly 1", len(lines))
	}
	j := lines[0]
	if got := strings.Join(j.Candidates, ","); got != "p0,p1,p3" {
		t.Errorf("candidates = %s, want p0,p1,p3", got)
	}
	if got := strings.Join(j.Selected, ","); got != "p0,p1" {
		t.Errorf("selected = %s, want p0,p1", got)
	}
	if j.BudgetMax != maxBytes || j.BudgetUsed != maxBytes {
		t.Errorf("budget used/max = %d/%d, want %d/%d", j.BudgetUsed, j.BudgetMax, maxBytes, maxBytes)
	}
	if len(j.Scores) != 3 {
		t.Fatalf("scores has %d entries, want 3", len(j.Scores))
	}
	for i, s := range j.Scores {
		if s.ID != msgs[i].ID || s.Priority != msgs[i].Priority {
			t.Errorf("scores[%d] = %+v, want id %s priority %s", i, s, msgs[i].ID, msgs[i].Priority)
		}
		if s.Bytes != messageBytes(msgs[i]) {
			t.Errorf("scores[%d].bytes = %d, want %d", i, s.Bytes, messageBytes(msgs[i]))
		}
		if s.Score < 0 || s.Score > 1 {
			t.Errorf("scores[%d].score = %f, want within [0,1]", i, s.Score)
		}
	}
	if !(j.Scores[0].Score > j.Scores[1].Score && j.Scores[1].Score > j.Scores[2].Score) {
		t.Errorf("scores not ordered P0>P1>P3: %+v", j.Scores)
	}
}

// AC4: the early-return paths (P0 over budget, no budget) still journal once;
// an empty inbox journals nothing.
func TestApplyBudgetJournalEdgePaths(t *testing.T) {
	msgs := []models.Message{msg("p0", "P0", "critical"), msg("p2", "P2", "n")}

	lines := captureBudgetJournal(t, func() { applyBudget(msgs, nil, 1) })
	if len(lines) != 1 || strings.Join(lines[0].Selected, ",") != "p0" {
		t.Errorf("P0-over-budget: lines = %+v, want one line selecting p0", lines)
	}

	lines = captureBudgetJournal(t, func() { applyBudget(msgs, nil, 0) })
	if len(lines) != 1 || strings.Join(lines[0].Selected, ",") != "p0,p2" || lines[0].BudgetMax != 0 {
		t.Errorf("no-budget: lines = %+v, want one line selecting p0,p2 with budget_max 0", lines)
	}

	lines = captureBudgetJournal(t, func() { applyBudget(nil, nil, 100) })
	if len(lines) != 0 {
		t.Errorf("empty input: got %d [budget] lines, want 0", len(lines))
	}
}
