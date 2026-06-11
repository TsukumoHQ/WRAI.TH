// Package connector defines the task-source abstraction that lets the relay run
// either fully native (the no-op connector owns nothing external) or against an
// external SSOT like Linear. The interface is intentionally tiny: one inbound
// path (verified webhook → mirror upsert), one outbound path (the agent's owned
// → In Review transition + comment), one reconcile path (heal missed webhooks),
// and a coarse state mapper.
package connector

// TaskEvent is a semantic event produced by Ingest that the relay emits onto the
// EventBus (where the notifications evaluator turns it into a launch, etc.).
// Keeping it a plain value decouples the connector from the relay's EventBus.
type TaskEvent struct {
	Type    string         // semantic event name, e.g. "task.in_progress"
	Project string         // relay project scope
	Agent   string         // actor/assignee for the event
	Payload map[string]any // tiny payload ({agent, task_id, linear_key, title, line, ...})
}

// TaskConnector is the seam between the relay's task core and a task source.
// Native mode uses Noop (no external I/O); Linear mode uses the linear package.
type TaskConnector interface {
	// Ingest verifies a provider webhook (HMAC over the raw body + freshness),
	// upserts the mirror, and returns the semantic events to emit. A verification
	// failure returns an error and no events. Self-authored echoes are dropped
	// (anti-loop) and return no events without error.
	Ingest(payload []byte, sig string) (events []TaskEvent, err error)

	// PushInReview performs the agent's single owned write-back: move the issue to
	// the team's In Review state and post a comment. Bounded retries inside.
	PushInReview(linearIssueID, comment string) error

	// ReconcileCycle pulls the active cycle's issues and upserts the mirror,
	// healing missed webhooks. Returns the number of rows upserted.
	ReconcileCycle(project string) (upserted int, err error)

	// MapState maps a provider state TYPE (backlog/unstarted/started/completed/
	// canceled) to the relay's coarse status. Robust across teams (type, not name).
	MapState(providerState string) string

	// Active reports whether the connector performs external I/O. Noop is always
	// inactive; Linear is active only when configured.
	Active() bool
}

// Noop is the native-mode connector: the relay owns everything and there is no
// external system. Every method is a safe no-op so call sites need no branching.
type Noop struct{}

func (Noop) Ingest(_ []byte, _ string) ([]TaskEvent, error) { return nil, nil }
func (Noop) PushInReview(_ string, _ string) error          { return nil }
func (Noop) ReconcileCycle(_ string) (int, error)           { return 0, nil }
func (Noop) Active() bool                                   { return false }

// MapState mirrors the Linear type→status mapping so native callers that happen
// to hold a provider type get a sensible answer; native tasks don't use it.
func (Noop) MapState(providerState string) string { return MapStateType(providerState) }

// MapStateType maps a Linear workflow-state TYPE to the relay's coarse status.
// Shared by Noop and the Linear connector so the mapping has one definition.
func MapStateType(stateType string) string {
	switch stateType {
	case "backlog", "unstarted", "triage":
		return "pending"
	case "started":
		return "in-progress"
	case "completed":
		return "done"
	case "canceled", "cancelled":
		return "cancelled"
	default:
		return "pending"
	}
}
