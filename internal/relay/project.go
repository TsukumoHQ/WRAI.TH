package relay

import (
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
	if len(m.Content) > msgContentPreview {
		s.ContentPreview = m.Content[:msgContentPreview]
		s.ContentTruncated = true
	} else {
		s.ContentPreview = m.Content
	}
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
	for _, m := range sorted {
		s := summarizeMessage(m)
		b := messageSummaryBytes(s)
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
		if len(t.Description) > taskDescPreview {
			s.DescPreview = t.Description[:taskDescPreview]
			s.DescTruncated = true
		} else {
			s.DescPreview = t.Description
		}
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
	for _, t := range sorted {
		s := summarizeTask(t)
		b := taskSummaryBytes(s)
		// P0 always bypasses the budget
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
