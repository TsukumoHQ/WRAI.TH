package relay

import (
	"sort"

	"agent-relay/internal/models"

	"github.com/mark3labs/mcp-go/mcp"
)

// Compact mutation echoes (tokens L3, ffd35635). dispatch_task, update_task and
// cancel_task used to echo the whole task — description, goal, acceptance
// criteria, dod and a dozen timestamps the caller had just written or never
// reads — on every call. They now return the task's identity, its status and
// what the call changed; verbose=true restores the full record, and get_task
// always has it.

// verboseParam is the opt-out of the compact echo, shared by the three tools.
var verboseParam = mcp.WithBoolean("verbose", mcp.Description("Echo the full task (default: id, status, changed fields)"))

// taskEchoLongFields are text fields a compact echo names in "updated" but does
// not repeat: the caller just sent the value, and it is the bulk of the bytes.
var taskEchoLongFields = map[string]bool{
	"description": true, "goal": true, "acceptance_criteria": true, "dod": true, "progress_note": true,
}

// taskEchoBase is the part of every compact echo: who the task is and where it
// stands now.
func taskEchoBase(t *models.Task) map[string]any {
	e := map[string]any{
		"id":           t.ID,
		"status":       t.Status,
		"profile_slug": t.ProfileSlug,
	}
	if t.AssignedTo != nil {
		e["assigned_to"] = *t.AssignedTo
	}
	return e
}

// dispatchEcho is dispatch_task's compact echo: the values the relay decided
// (id, status, the resolved profile, priority, board, trace, readiness), not
// the ones the caller sent.
func dispatchEcho(t *models.Task) map[string]any {
	e := taskEchoBase(t)
	e["priority"] = t.Priority
	if t.BoardID != nil {
		e["board_id"] = *t.BoardID
	}
	if t.ParentTaskID != nil {
		e["parent_task_id"] = *t.ParentTaskID
	}
	if t.TraceID != nil {
		e["trace_id"] = *t.TraceID
	}
	if t.LinearKey != nil {
		e["linear_key"] = *t.LinearKey
	}
	if len(t.BlockedBy) > 0 {
		e["blocked_by"] = t.BlockedBy
	}
	if t.Ready != nil {
		e["ready"] = *t.Ready
	}
	return e
}

// updateEcho is update_task's compact echo: identity + status, the names of the
// fields this call set ("updated"), and the new value of each short one. Long
// text fields are named only.
func updateEcho(t *models.Task, args map[string]any) map[string]any {
	e := taskEchoBase(t)
	var updated []string
	for k := range args {
		if routingArgs[k] {
			continue
		}
		updated = append(updated, k)
	}
	sort.Strings(updated)
	e["updated"] = updated
	for _, k := range updated {
		if taskEchoLongFields[k] {
			continue
		}
		switch k {
		case "title":
			e["title"] = t.Title
		case "priority":
			e["priority"] = t.Priority
		case "board_id":
			if t.BoardID != nil {
				e["board_id"] = *t.BoardID
			}
		case "verify_cmd":
			if t.VerifyCmd != nil {
				e["verify_cmd"] = *t.VerifyCmd
			}
		case "parent_task_id":
			if t.ParentTaskID != nil {
				e["parent_task_id"] = *t.ParentTaskID
			}
		case "assigned_to", "profile_slug":
			// A reassign may move a claimed lease: the new holder needs the
			// grant to fence its terminal write.
			if t.ClaimedBy != nil {
				e["claimed_by"] = *t.ClaimedBy
			}
			if t.LeaseHolder != nil {
				e["lease_holder"] = *t.LeaseHolder
				e["lease_generation"] = t.LeaseGeneration
			}
			if t.LeaseTransfer != nil {
				e["lease_transfer"] = t.LeaseTransfer
			}
		case "blocked_by", "blocked_by_remove":
			e["blocked_by"] = t.BlockedBy
			if t.Ready != nil {
				e["ready"] = *t.Ready
			}
		}
	}
	return e
}

// routingArgs are call plumbing, never a field the call changed.
var routingArgs = map[string]bool{"as": true, "project": true, "task_id": true, "verbose": true}
