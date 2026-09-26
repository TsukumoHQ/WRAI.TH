package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// Consumption T2 (design 54e529d8 §4.3, §5; ruling f97023b7): claim/complete
// stamp the knowledge basis a task ran on, and ONE read-only tool,
// who_consumed, answers which agents (and their active tasks) hold which
// version of a key, or with self=true the caller's own basis.

// take removes and returns one agent's buffered recall ids (nil when none), so
// a basis stamp can write them in its own tx.
func (b *recallBuffer) take(project, agent string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := db.RecallKey{Project: project, Agent: agent}
	set := b.ids[k]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	delete(b.ids, k)
	b.n -= len(out)
	return out
}

// basisUnknown is the basis a result carries when its stamp failed: the task
// transition stands (ruling f97023b7 OQ3), the stamp is best-effort.
const basisUnknown = "unknown"

// stampBasis writes the basis stamp for taskIDs after a successful transition,
// draining the agent's buffered recalls into it. A failed stamp puts the
// recalls back for the next flush and returns "unknown".
func (h *Handlers) stampBasis(project, agent, event string, taskIDs []string) string {
	if len(taskIDs) == 0 {
		return ""
	}
	recalls := h.recalls.take(project, agent)
	basis, err := h.db.StampTaskBasis(project, agent, event, taskIDs, recalls)
	if err != nil {
		h.recalls.record(project, agent, recalls)
		consumptionCaptureErrors.Add(1)
		log.Printf("consumption: %s stamp %s/%s: %v", event, project, agent, err)
		return basisUnknown
	}
	return basis
}

// withBasis renders a task result with its basis token added, keeping every
// field of the task JSON the tool returned before.
func withBasis(task any, basis string) any {
	raw, err := json.Marshal(task)
	if err != nil {
		return task
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return task
	}
	m["basis"] = basis
	return m
}

func whoConsumedTool() mcp.Tool {
	return mcp.NewTool(
		"who_consumed",
		mcp.WithDescription("Who holds which version of a memory key (current=false: stale), with active tasks. self=true: your basis."),
		asParam,
		projectParam,
		mcp.WithString("key"),
		mcp.WithString("scope"),
		mcp.WithBoolean("self"),
		mcp.WithBoolean("active_only", mcp.Description("default true")),
	)
}

// HandleWhoConsumed is read-only (RO pool): it never records a recall.
func (h *Handlers) HandleWhoConsumed(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	if req.GetBool("self", false) {
		head, mems, err := h.db.ContextBasis(project, agent)
		if err != nil {
			return toolResultError(fmt.Sprintf("who_consumed: %v", err)), nil
		}
		if head == nil {
			return h.resultJSONTracked(project, agent, "who_consumed", map[string]any{"basis": nil, "memories": []db.BasisMemory{}})
		}
		if mems == nil {
			mems = []db.BasisMemory{}
		}
		var rev any
		if head.KnowledgeRev.Valid {
			rev = head.KnowledgeRev.Int64
		}
		return h.resultJSONTracked(project, agent, "who_consumed", map[string]any{
			"basis": head.Basis(), "snapshot_id": head.SnapshotID, "kind": head.Kind,
			"knowledge_rev": rev, "created_at": head.CreatedAt, "memories": mems,
		})
	}
	key := req.GetString("key", "")
	if key == "" {
		return validationError(CodeInvalidArgument, "key is required unless self=true"), nil
	}
	consumers, err := h.db.WhoConsumed(project, key, req.GetString("scope", ""), req.GetBool("active_only", true))
	if err != nil {
		return toolResultError(fmt.Sprintf("who_consumed: %v", err)), nil
	}
	return h.resultJSONTracked(project, agent, "who_consumed", map[string]any{"key": key, "holders": consumers})
}
