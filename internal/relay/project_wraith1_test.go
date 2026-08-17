package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-relay/internal/models"
)

// WRAITH-1: active_conversations must be count-capped and title-truncated.
func TestProjectConversations(t *testing.T) {
	convs := make([]models.ConversationSummary, 20)
	for i := range convs {
		convs[i] = models.ConversationSummary{ID: string(rune('a' + i)), Title: strings.Repeat("x", 500)}
	}
	got := projectConversations(convs, sessionConversationMax)
	if len(got) != sessionConversationMax {
		t.Fatalf("want %d convs, got %d", sessionConversationMax, len(got))
	}
	if len(got[0].Title) > convTitlePreview+3 { // +ellipsis
		t.Errorf("title not truncated: %d bytes", len(got[0].Title))
	}
	// Under the cap: unchanged count.
	if n := len(projectConversations(convs[:3], sessionConversationMax)); n != 3 {
		t.Errorf("under-cap count changed: %d", n)
	}
}

// WRAITH-1: a profile's unbounded Skills JSON is bounded, staying valid JSON.
func TestCapJSONArrayAndProfile(t *testing.T) {
	// Small array: untouched.
	small := `[{"s":"go"},{"s":"rust"}]`
	if out, capped := capJSONArray(small, profileSkillsBudget); capped || out != small {
		t.Errorf("small array altered: %q capped=%v", out, capped)
	}

	// Big array: trimmed to whole elements, still valid JSON.
	var items []map[string]string
	for i := 0; i < 500; i++ {
		items = append(items, map[string]string{"skill": strings.Repeat("y", 20)})
	}
	bigBytes, _ := json.Marshal(items)
	out, capped := capJSONArray(string(bigBytes), profileSkillsBudget)
	if !capped {
		t.Fatal("big array should be capped")
	}
	if len(out) > profileSkillsBudget {
		t.Errorf("capped array over budget: %d", len(out))
	}
	var reparsed []map[string]string
	if err := json.Unmarshal([]byte(out), &reparsed); err != nil {
		t.Fatalf("capped array is not valid JSON: %v", err)
	}

	// Malformed skills → empty array, never a corrupt payload.
	if out, capped := capJSONArray(strings.Repeat("{", 3000), profileSkillsBudget); out != "[]" || !capped {
		t.Errorf("malformed skills should collapse to []: %q", out)
	}

	p := &models.Profile{ID: "1", Slug: "backend", Skills: string(bigBytes)}
	m := projectProfile(p)
	if m["skills_capped"] != true {
		t.Error("projectProfile should flag skills_capped for a big profile")
	}
}

// WRAITH-1: the global ceiling collapses conversations and flags payload_capped
// when the whole payload is still too big; leaves a small payload untouched.
func TestEnforceSessionPayloadCeiling(t *testing.T) {
	small := map[string]any{"active_conversations": []models.ConversationSummary{{ID: "a"}}}
	enforceSessionPayloadCeiling(small)
	if _, capped := small["payload_capped"]; capped {
		t.Error("small payload wrongly capped")
	}
	if n := len(small["active_conversations"].([]models.ConversationSummary)); n != 1 {
		t.Error("small payload conversations wrongly trimmed")
	}

	big := map[string]any{
		"unread_messages":      strings.Repeat("z", sessionPayloadCeiling+1000),
		"active_conversations": []models.ConversationSummary{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}
	enforceSessionPayloadCeiling(big)
	if big["payload_capped"] != true {
		t.Fatal("oversized payload not flagged")
	}
	if n := len(big["active_conversations"].([]models.ConversationSummary)); n != 0 {
		t.Errorf("conversations not collapsed: %d", n)
	}
	if big["active_conversations_omitted"].(int) != 3 {
		t.Errorf("omitted count wrong: %v", big["active_conversations_omitted"])
	}
}
