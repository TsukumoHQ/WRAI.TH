package relay

import (
	"sync"
	"time"

	"agent-relay/internal/models"
)

// MCPEvent represents an event on the bus. It carries both the legacy visual
// fields (consumed by the canvas) and an optional semantic payload consumed by
// the notifications evaluator.
//
// Semantic events use a dotted Type (e.g. "task.in_progress", "cycle.digest",
// "event:<custom>") and populate Semantic with a tiny, low-token payload
// ({agent, task_id, linear_key, title, line, ...}). Visual events leave
// Semantic nil and the canvas ignores Types it does not recognize.
type MCPEvent struct {
	Type     string         `json:"type"`               // event group (memory, task, ...) OR semantic event name (task.claimed, cycle.digest, event:<custom>)
	Action   string         `json:"action"`             // specific action: set, search, dispatch, claim, complete, block, etc.
	Agent    string         `json:"agent"`              // agent that triggered it
	Project  string         `json:"project"`            // project scope
	Target   string         `json:"target,omitempty"`   // target agent/profile (for dispatch, team ops)
	Label    string         `json:"label,omitempty"`    // short label (task title, memory key, etc.)
	Semantic map[string]any `json:"semantic,omitempty"` // tiny payload for the notifications evaluator ({agent, task_id, linear_key, title, ...}); nil for plain visual events
	TS       int64          `json:"ts"`                 // unix ms
}

// Semantic event Type constants consumed by the notifications evaluator.
const (
	EvTaskDispatched = "task.dispatched"
	EvTaskClaimed    = "task.claimed"
	EvTaskInProgress = "task.in_progress"
	EvTaskBlocked    = "task.blocked"
	EvTaskInReview   = "task.in_review"
	EvTaskDone       = "task.done"
	EvCycleDigest    = "cycle.digest"
)

// EmitSemantic publishes a semantic event onto the bus with a tiny payload.
// It is a thin convenience over Emit that sets Type + Semantic, so the
// notifications evaluator can key off clean event names independent of the
// visual event stream. Task lifecycle transitions go through emitTaskEvent
// instead (one event carrying both the visual action and the payload);
// EmitSemantic is for bus-only events: cycle.digest and event:<custom>.
func (b *EventBus) EmitSemantic(eventType, project, agent string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	b.Emit(MCPEvent{
		Type:     eventType,
		Agent:    agent,
		Project:  project,
		Semantic: payload,
	})
}

// eventHistorySize is the fixed-size ring buffer for /api/events/recent.
// Keeps the last N MCP events so the UI can display recent activity even
// without an open SSE subscription (cold load, page refresh, etc.).
const eventHistorySize = 500

// EventBus broadcasts MCP events to SSE subscribers and retains a bounded
// history of recent events for HTTP polling.
type EventBus struct {
	mu      sync.RWMutex
	subs    map[chan MCPEvent]struct{}
	history []MCPEvent // ring buffer, newest last
}

func NewEventBus() *EventBus {
	return &EventBus{
		subs:    make(map[chan MCPEvent]struct{}),
		history: make([]MCPEvent, 0, eventHistorySize),
	}
}

func (b *EventBus) Emit(evt MCPEvent) {
	evt.TS = time.Now().UnixMilli()
	b.mu.Lock()
	// Append + trim to ring size
	b.history = append(b.history, evt)
	if len(b.history) > eventHistorySize {
		b.history = b.history[len(b.history)-eventHistorySize:]
	}
	subs := b.subs
	b.mu.Unlock()

	for ch := range subs {
		select {
		case ch <- evt:
		default: // drop if slow
		}
	}
}

// Recent returns up to limit most-recent events (newest first) optionally
// filtered by project. Limit 0 or negative uses the full buffer.
func (b *EventBus) Recent(project string, limit int) []MCPEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]MCPEvent, 0, len(b.history))
	// Walk newest → oldest
	for i := len(b.history) - 1; i >= 0; i-- {
		e := b.history[i]
		if project != "" && e.Project != project {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// emitTaskEvent emits a semantic task lifecycle event on the shared bus.
// name is the full semantic event name ("task.dispatched", "task.claimed",
// "task.in_progress", "task.blocked", "task.in_review", "task.done"); action is
// the short visual action the canvas already understands. The minimal payload
// {agent, task_id, linear_key, title} is attached for the notifications engine.
func emitTaskEvent(events *EventBus, name, action, project string, t *models.Task, extra ...map[string]any) {
	if events == nil || t == nil {
		return
	}
	semantic := map[string]any{
		"agent":   agentForEvent(t),
		"task_id": t.ID,
		"title":   t.Title,
	}
	if t.LinearKey != nil && *t.LinearKey != "" {
		semantic["linear_key"] = *t.LinearKey
	}
	for _, m := range extra {
		for k, v := range m {
			semantic[k] = v
		}
	}
	events.Emit(MCPEvent{
		Type:     name,
		Action:   action,
		Agent:    t.DispatchedBy,
		Project:  project,
		Label:    t.Title,
		Semantic: semantic,
	})
}

// agentForEvent returns the most relevant agent for a task event: the assignee
// if claimed, else the dispatcher.
func agentForEvent(t *models.Task) string {
	if t.AssignedTo != nil && *t.AssignedTo != "" {
		return *t.AssignedTo
	}
	return t.DispatchedBy
}

func (b *EventBus) Subscribe() chan MCPEvent {
	ch := make(chan MCPEvent, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *EventBus) Unsubscribe(ch chan MCPEvent) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
	close(ch)
}
