package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-relay/internal/models"
)

// bootJournalFixtureBudget is small enough that the fixture overflows it:
// the P0 bypasses, the P1s fit, the P2/P3 tail is omitted.
const bootJournalFixtureBudget = 900

func bootJournalFixture() []models.Message {
	task := "task-42"
	conv := "conv-7"
	return []models.Message{
		{ID: "m-p2-old", From: "cto", Subject: "fyi", Type: "notification", Priority: "P2", CreatedAt: "2026-09-25T10:00:00.000000Z", Content: strings.Repeat("old p2 body ", 20)},
		{ID: "m-p1-new", From: "qa", Subject: "verdict", Type: "notification", Priority: "P1", CreatedAt: "2026-09-25T12:00:00.000000Z", Content: "approved", TaskID: &task},
		{ID: "m-p0", From: "ops", Subject: "outage", Type: "notification", Priority: "P0", CreatedAt: "2026-09-25T09:00:00.000000Z", Content: strings.Repeat("fire ", 100)},
		{ID: "m-p1-old", From: "cto", Subject: "dispatch", Type: "task", Priority: "P1", CreatedAt: "2026-09-25T11:00:00.000000Z", Content: strings.Repeat("do the thing é ", 30), ConversationID: &conv},
		{ID: "m-p3", From: "bot", Subject: "digest", Type: "notification", Priority: "P3", CreatedAt: "2026-09-25T12:30:00.000000Z", Content: strings.Repeat("noise ", 50)},
		{ID: "m-unknown", From: "legacy", Subject: "", Type: "", Priority: "", CreatedAt: "2026-09-25T12:45:00.000000Z", Content: "no priority"},
	}
}

// bootJournalGolden is projectMessages(bootJournalFixture(), 900) captured on
// main BEFORE the journal was added (task 409fb2e8). The journal is log-only:
// the session_context projection must stay byte-identical.
const bootJournalGolden = `[{"id":"m-p0","from":"ops","subject":"outage","type":"notification","priority":"P0","content_preview":"fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire fire "},{"id":"m-p1-new","from":"qa","subject":"verdict","type":"notification","priority":"P1","task_id":"task-42","content_preview":"approved"},{"id":"m-p1-old","from":"cto","subject":"dispatch","type":"task","priority":"P1","conversation_id":"conv-7","content_preview":"do the thing é do the thing é do the thing é do the thing é do the thing é do the thing é do the thing é do the thing é do the thing é do the thing é "}]`

func TestProjectMessages_OutputByteIdenticalWithJournal(t *testing.T) {
	var out []MessageSummary
	captureBudgetJournal(t, func() {
		out = projectMessages(bootJournalFixture(), bootJournalFixtureBudget, "wraith-backend")
	})
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != bootJournalGolden {
		t.Fatalf("projection changed:\n got %s\nwant %s", got, bootJournalGolden)
	}
}

// The boot path emits exactly one [budget] line in the applyBudget journal
// shape plus path=session_context and the agent, with at least one omission.
func TestProjectMessages_JournalLine(t *testing.T) {
	fixture := bootJournalFixture()
	var out []MessageSummary
	lines := captureBudgetJournal(t, func() {
		out = projectMessages(fixture, bootJournalFixtureBudget, "wraith-backend")
	})
	if len(lines) != 1 {
		t.Fatalf("got %d [budget] lines, want exactly 1", len(lines))
	}
	j := lines[0]
	if j.Path != "session_context" || j.Agent != "wraith-backend" {
		t.Errorf("path/agent = %q/%q, want session_context/wraith-backend", j.Path, j.Agent)
	}
	if got, want := strings.Join(j.Candidates, ","), "m-p2-old,m-p1-new,m-p0,m-p1-old,m-p3,m-unknown"; got != want {
		t.Errorf("candidates = %s, want %s", got, want)
	}
	if got, want := strings.Join(j.Selected, ","), "m-p0,m-p1-new,m-p1-old"; got != want {
		t.Errorf("selected = %s, want %s", got, want)
	}
	if got, want := strings.Join(j.Omitted, ","), "m-p2-old,m-p3,m-unknown"; got != want {
		t.Errorf("omitted = %s, want %s", got, want)
	}
	// Selected ids are exactly the projected ids, in order.
	var projected []string
	for _, s := range out {
		projected = append(projected, s.ID)
	}
	if strings.Join(projected, ",") != strings.Join(j.Selected, ",") {
		t.Errorf("selected %v != projected %v", j.Selected, projected)
	}
	if j.BudgetMax != bootJournalFixtureBudget {
		t.Errorf("budget_max = %d, want %d", j.BudgetMax, bootJournalFixtureBudget)
	}

	// Per-message bytes match the projected summary sizes; used = sum(selected).
	byID := map[string]models.Message{}
	for _, m := range fixture {
		byID[m.ID] = m
	}
	selected := map[string]bool{}
	for _, id := range j.Selected {
		selected[id] = true
	}
	if len(j.Scores) != len(fixture) {
		t.Fatalf("scores has %d entries, want %d", len(j.Scores), len(fixture))
	}
	used := 0
	for _, s := range j.Scores {
		m, ok := byID[s.ID]
		if !ok {
			t.Fatalf("scores has unknown id %q", s.ID)
		}
		if want := messageSummaryBytes(summarizeMessage(m)); s.Bytes != want {
			t.Errorf("%s bytes = %d, want %d", s.ID, s.Bytes, want)
		}
		if s.Priority != m.Priority {
			t.Errorf("%s priority = %q, want %q", s.ID, s.Priority, m.Priority)
		}
		if s.Score != nil {
			t.Errorf("%s has a score; projectMessages ranks without one", s.ID)
		}
		if selected[s.ID] {
			used += s.Bytes
		}
	}
	if j.BudgetUsed != used {
		t.Errorf("budget_used = %d, want %d", j.BudgetUsed, used)
	}
	if j.BudgetUsed > j.BudgetMax {
		t.Errorf("budget_used %d exceeds budget_max %d without a P0 overflow", j.BudgetUsed, j.BudgetMax)
	}
}

// No candidates, no line (empty inbox at boot stays silent).
func TestProjectMessages_EmptyNoJournal(t *testing.T) {
	lines := captureBudgetJournal(t, func() { projectMessages(nil, sessionUnreadBudget, "a") })
	if len(lines) != 0 {
		t.Errorf("empty input: got %d [budget] lines, want 0", len(lines))
	}
}
