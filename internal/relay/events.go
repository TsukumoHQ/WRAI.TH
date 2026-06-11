package relay

import (
	"sync"
	"time"
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
	Type     string         `json:"type"`               // event group OR semantic event name
	Action   string         `json:"action"`             // specific action (visual events)
	Agent    string         `json:"agent"`              // agent that triggered it
	Project  string         `json:"project"`            // project scope
	Target   string         `json:"target,omitempty"`   // target agent/profile (for dispatch, team ops)
	Label    string         `json:"label,omitempty"`    // short label (task title, memory key, etc.)
	Semantic map[string]any `json:"semantic,omitempty"` // tiny payload for the notifications evaluator
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
// visual event stream.
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

// EventBus broadcasts MCP events to SSE subscribers.
type EventBus struct {
	mu   sync.RWMutex
	subs map[chan MCPEvent]struct{}
}

func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[chan MCPEvent]struct{})}
}

func (b *EventBus) Emit(evt MCPEvent) {
	evt.TS = time.Now().UnixMilli()
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- evt:
		default: // drop if slow
		}
	}
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
