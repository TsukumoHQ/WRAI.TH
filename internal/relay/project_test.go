package relay

import (
	"strings"
	"testing"

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
