package relay

import (
	"encoding/json"
	"unicode/utf8"

	"agent-relay/internal/db"
	"agent-relay/internal/models"
)

// This file implements the paper's Def. 7 (Budget Projection):
//   M_boot = top-k(M_db, U, B_max), |M_boot| ≤ B_max
//
// Raw entities (Task, vault doc Content) are projected into compact summaries
// bounded by a byte budget before being injected into session_context or spawn
// prompts. The agent can always pay-for-what-you-use by calling
// get_task / get_vault_doc for full content.

// taskDescPreview is the byte ceiling for a single task's description preview
// in a projection. Full descriptions are fetched via get_task(id) on demand.
const taskDescPreview = 200

// sessionUnreadBudget bounds the total bytes spent on unread_messages in
// session_context. With msgContentPreview=300, ~15 messages fit; P0 messages
// bypass the budget. The rest are reported via unread_omitted + fetched on
// demand with get_inbox.
const sessionUnreadBudget = 6000

// msgContentPreview is the byte ceiling for a single unread message's content
// preview in session_context. Mirrors the 300-char cap get_inbox already applies
// (see handlers.go). Full bodies are fetched via get_inbox(full_content=true).
const msgContentPreview = 300

// budgetHardMultiplier caps the absolute size of a projection at this multiple
// of its soft byte budget. P0 messages/tasks and constraints-layer memories
// bypass the SOFT budget (they must surface even when it's blown), but they
// still obey this HARD ceiling — so a flood of P0 items (priority is
// caller-settable on send_message) from one agent cannot inflate every peer's
// boot payload without bound. Keeps the paper's invariant |M_boot| ≤ B_max real,
// with B_max = soft budget × budgetHardMultiplier. Items beyond the ceiling are
// dropped and surface via the *_omitted counters in session_context.
const budgetHardMultiplier = 2

// memValuePreview bounds a single memory's value preview in session_context.
// Full values are fetched via get_memory(key).
const memValuePreview = 400

// sessionMemoryBudget bounds the total bytes spent on relevant_memories in
// session_context. Constraints-layer memories bypass the budget (Def. 3:
// constraints > behavior > context — a constraint must never silently drop
// out of boot).
const sessionMemoryBudget = 6000

// truncatePreview cuts s to at most max bytes without splitting a UTF-8 rune
// (byte slicing on French text can cut an accent mid-sequence and produce
// invalid JSON payloads). Returns the cut string and whether truncation occurred.
func truncatePreview(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	// The last byte may be the start of a multi-byte rune whose tail was cut.
	if r, _ := utf8.DecodeLastRuneInString(cut); r == utf8.RuneError {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

// MessageSummary is the projected form of models.Message injected into
// session_context.unread_messages. The Content field is truncated to
// msgContentPreview; the full body is reachable via get_inbox(full_content=true)
// or get_thread(id). Heavy/structural fields (metadata, ttl) are dropped.
type MessageSummary struct {
	ID               string  `json:"id"`
	From             string  `json:"from"`
	Subject          string  `json:"subject,omitempty"`
	Type             string  `json:"type,omitempty"`
	Priority         string  `json:"priority,omitempty"`
	CreatedAt        string  `json:"created_at,omitempty"`
	TaskID           *string `json:"task_id,omitempty"`
	ConversationID   *string `json:"conversation_id,omitempty"`
	ReplyTo          *string `json:"reply_to,omitempty"`
	DeliveryID       *string `json:"delivery_id,omitempty"`
	ContentPreview   string  `json:"content_preview,omitempty"`
	ContentTruncated bool    `json:"content_truncated,omitempty"`
}

// summarizeMessage converts a Message into a MessageSummary with a bounded
// content preview.
func summarizeMessage(m models.Message) MessageSummary {
	s := MessageSummary{
		ID:             m.ID,
		From:           m.From,
		Subject:        m.Subject,
		Type:           m.Type,
		Priority:       m.Priority,
		CreatedAt:      m.CreatedAt,
		TaskID:         m.TaskID,
		ConversationID: m.ConversationID,
		ReplyTo:        m.ReplyTo,
		DeliveryID:     m.DeliveryID,
	}
	s.ContentPreview, s.ContentTruncated = truncatePreview(m.Content, msgContentPreview)
	return s
}

// messageSummaryBytes estimates the serialized size of a MessageSummary.
func messageSummaryBytes(s MessageSummary) int {
	n := len(s.ID) + len(s.From) + len(s.Subject) + len(s.Type) +
		len(s.Priority) + len(s.CreatedAt) + len(s.ContentPreview)
	if s.TaskID != nil {
		n += len(*s.TaskID)
	}
	if s.ConversationID != nil {
		n += len(*s.ConversationID)
	}
	if s.ReplyTo != nil {
		n += len(*s.ReplyTo)
	}
	if s.DeliveryID != nil {
		n += len(*s.DeliveryID)
	}
	// JSON structural overhead (quotes, commas, field names)
	n += 180
	return n
}

// projectMessages applies Def. 7 to unread messages: sort by priority (P0 first,
// then newest within priority), summarize each with a bounded content preview,
// then greedily select until maxBytes is reached. P0 messages bypass the budget
// (but are still content-truncated), mirroring projectTasks' P0-bypass rule.
func projectMessages(msgs []models.Message, maxBytes int) []MessageSummary {
	if len(msgs) == 0 {
		return []MessageSummary{}
	}

	// Stable insertion sort: P0 first, then created_at DESC within priority.
	sorted := make([]models.Message, len(msgs))
	copy(sorted, msgs)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0; j-- {
			a, b := sorted[j-1], sorted[j]
			if priorityRank(a.Priority) < priorityRank(b.Priority) {
				break
			}
			if priorityRank(a.Priority) == priorityRank(b.Priority) && a.CreatedAt >= b.CreatedAt {
				break
			}
			sorted[j-1], sorted[j] = b, a
		}
	}

	var out []MessageSummary
	used := 0
	hardCeil := 0
	if maxBytes > 0 {
		hardCeil = maxBytes * budgetHardMultiplier
	}
	for _, m := range sorted {
		s := summarizeMessage(m)
		b := messageSummaryBytes(s)
		// Hard ceiling caps the bypass flood; the first (most-important) P0
		// item always surfaces even under a tiny budget.
		if hardCeil > 0 && used+b > hardCeil && len(out) > 0 {
			continue
		}
		if m.Priority == "P0" {
			out = append(out, s)
			used += b
			continue
		}
		if maxBytes > 0 && used+b > maxBytes {
			continue
		}
		out = append(out, s)
		used += b
	}
	return out
}

// TaskSummary is the projected form of models.Task injected into session_context.
// Heavy fields (description, result, blocked_reason) are dropped or truncated;
// the full task is reachable via get_task(id).
type TaskSummary struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Priority      string  `json:"priority"`
	Status        string  `json:"status"`
	ProfileSlug   string  `json:"profile_slug,omitempty"`
	AssignedTo    *string `json:"assigned_to,omitempty"`
	DispatchedBy  string  `json:"dispatched_by,omitempty"`
	BoardID       *string `json:"board_id,omitempty"`
	DispatchedAt  string  `json:"dispatched_at,omitempty"`
	DescPreview   string  `json:"desc_preview,omitempty"`
	DescTruncated bool    `json:"desc_truncated,omitempty"`
}

// priorityRank returns the integer rank used for stable priority ordering.
// Lower = more important (P0=0). Unknown values sort last.
func priorityRank(p string) int {
	switch p {
	case "P0":
		return 0
	case "P1":
		return 1
	case "P2":
		return 2
	case "P3":
		return 3
	}
	return 4
}

// summarizeTask converts a Task into a TaskSummary with a bounded description preview.
func summarizeTask(t models.Task) TaskSummary {
	s := TaskSummary{
		ID:           t.ID,
		Title:        t.Title,
		Priority:     t.Priority,
		Status:       t.Status,
		ProfileSlug:  t.ProfileSlug,
		AssignedTo:   t.AssignedTo,
		DispatchedBy: t.DispatchedBy,
		BoardID:      t.BoardID,
		DispatchedAt: t.DispatchedAt,
	}
	if t.Description != "" {
		s.DescPreview, s.DescTruncated = truncatePreview(t.Description, taskDescPreview)
	}
	return s
}

// taskSummaryBytes estimates the serialized size of a TaskSummary (for budget accounting).
func taskSummaryBytes(s TaskSummary) int {
	n := len(s.ID) + len(s.Title) + len(s.Priority) + len(s.Status) +
		len(s.ProfileSlug) + len(s.DispatchedBy) + len(s.DispatchedAt) + len(s.DescPreview)
	if s.AssignedTo != nil {
		n += len(*s.AssignedTo)
	}
	if s.BoardID != nil {
		n += len(*s.BoardID)
	}
	// JSON structural overhead (quotes, commas, field names)
	n += 160
	return n
}

// projectTasks applies Def. 7 to a list of tasks: sort by priority (P0 first),
// summarize each, then greedily select until maxBytes is reached.
// P0 tasks always fit (they're surfaced even if the budget is blown — mirrors
// applyBudget's P0-bypass rule in internal/relay/budget.go).
func projectTasks(tasks []models.Task, maxBytes int) []TaskSummary {
	if len(tasks) == 0 {
		return []TaskSummary{}
	}

	// Stable sort: P0 first, then by dispatched_at DESC within priority.
	// insertion sort keeps it obvious and O(n²) is fine at our scale (≤100 items).
	sorted := make([]models.Task, len(tasks))
	copy(sorted, tasks)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0; j-- {
			a, b := sorted[j-1], sorted[j]
			if priorityRank(a.Priority) < priorityRank(b.Priority) {
				break
			}
			if priorityRank(a.Priority) == priorityRank(b.Priority) && a.DispatchedAt >= b.DispatchedAt {
				break
			}
			sorted[j-1], sorted[j] = b, a
		}
	}

	var out []TaskSummary
	used := 0
	hardCeil := 0
	if maxBytes > 0 {
		hardCeil = maxBytes * budgetHardMultiplier
	}
	for _, t := range sorted {
		s := summarizeTask(t)
		b := taskSummaryBytes(s)
		// Hard ceiling caps the bypass flood; the first (most-important) P0
		// item always surfaces even under a tiny budget.
		if hardCeil > 0 && used+b > hardCeil && len(out) > 0 {
			continue
		}
		// P0 bypasses the soft budget
		if t.Priority == "P0" {
			out = append(out, s)
			used += b
			continue
		}
		if maxBytes > 0 && used+b > maxBytes {
			continue
		}
		out = append(out, s)
		used += b
	}
	return out
}

// MemorySummary is the projected form of models.Memory injected into
// session_context.relevant_memories. Value is truncated to memValuePreview;
// the full value is reachable via get_memory(key).
type MemorySummary struct {
	Key            string `json:"key"`
	ValuePreview   string `json:"value_preview"`
	ValueTruncated bool   `json:"value_truncated,omitempty"`
	Scope          string `json:"scope"`
	Layer          string `json:"layer,omitempty"`
	Confidence     string `json:"confidence,omitempty"`
	AgentName      string `json:"agent_name,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

func summarizeMemory(m models.Memory) MemorySummary {
	s := MemorySummary{
		Key:        m.Key,
		Scope:      m.Scope,
		Layer:      m.Layer,
		Confidence: m.Confidence,
		AgentName:  m.AgentName,
		UpdatedAt:  m.UpdatedAt,
	}
	s.ValuePreview, s.ValueTruncated = truncatePreview(m.Value, memValuePreview)
	return s
}

func memorySummaryBytes(s MemorySummary) int {
	n := len(s.Key) + len(s.ValuePreview) + len(s.Scope) + len(s.Layer) +
		len(s.Confidence) + len(s.AgentName) + len(s.UpdatedAt)
	// JSON structural overhead (quotes, commas, field names)
	n += 140
	return n
}

// projectMemories applies Def. 7 to boot memories: summarize each with a
// bounded value preview, then greedily select until maxBytes is reached.
// Constraints-layer memories bypass the budget (mirrors the P0-bypass rule):
// a CTO constraint must always survive into the boot payload. The caller
// (ListBootMemories) already orders constraints first, then updated_at DESC.
// sessionDecisionMax bounds how many accepted decisions are injected at session
// start. Decisions are one-liners, so the count keeps the section bounded
// without a byte budget; the overflow count is surfaced and the rest are
// reachable via recall_decisions.
const sessionDecisionMax = 40

// DecisionSummary is the compact session-context form of an accepted decision.
type DecisionSummary struct {
	Key       string `json:"key"`
	Area      string `json:"area,omitempty"`
	Decision  string `json:"decision"`
	Rationale string `json:"rationale,omitempty"`
}

// projectDecisions decodes accepted decision memories into the compact boot form,
// capped at max.
func projectDecisions(decs []models.Memory, max int) []DecisionSummary {
	if max > 0 && len(decs) > max {
		decs = decs[:max]
	}
	out := make([]DecisionSummary, 0, len(decs))
	for _, m := range decs {
		s := DecisionSummary{Key: m.Key}
		var dv db.DecisionValue
		if json.Unmarshal([]byte(m.Value), &dv) == nil && dv.Decision != "" {
			s.Decision = dv.Decision
			s.Area = dv.Area
			s.Rationale = dv.Rationale
		} else {
			s.Decision = m.Value
		}
		out = append(out, s)
	}
	return out
}

func projectMemories(mems []models.Memory, maxBytes int) []MemorySummary {
	if len(mems) == 0 {
		return []MemorySummary{}
	}
	var out []MemorySummary
	used := 0
	hardCeil := 0
	if maxBytes > 0 {
		hardCeil = maxBytes * budgetHardMultiplier
	}
	for _, m := range mems {
		s := summarizeMemory(m)
		b := memorySummaryBytes(s)
		// Hard ceiling caps the bypass flood; the first (most-important)
		// constraints memory always surfaces even under a tiny budget.
		if hardCeil > 0 && used+b > hardCeil && len(out) > 0 {
			continue
		}
		if m.Layer == "constraints" {
			out = append(out, s)
			used += b
			continue
		}
		if maxBytes > 0 && used+b > maxBytes {
			continue
		}
		out = append(out, s)
		used += b
	}
	return out
}
