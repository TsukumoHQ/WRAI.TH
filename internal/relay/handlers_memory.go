package relay

import (
	"agent-relay/internal/db"
	"agent-relay/internal/models"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// roundImportance trims the derived salience score to 3 decimals so compact
// list/search rows stay token-light (the raw float can run 17 digits).
func roundImportance(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func (h *Handlers) HandleSetMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	key := req.GetString("key", "")
	if key == "" {
		return toolResultError("key is required"), nil
	}
	value := req.GetString("value", "")
	if value == "" {
		return toolResultError("value is required"), nil
	}
	scope := req.GetString("scope", "project")
	confidence := req.GetString("confidence", "stated")
	layer := req.GetString("layer", "behavior")
	tags := req.GetStringSlice("tags", nil)
	tagsJSON := db.TagsToJSON(tags)
	upsert := req.GetBool("upsert", true)
	validFrom := req.GetString("valid_from", "")
	validUntil := req.GetString("valid_until", "")

	// Memory Protocol v2 write-side guard (DEC-governance-memory-protocol-2),
	// opt-in per project (DEC-governance-enforcement-1) — read before the write
	// so a violation never lands. Checked on the raw (pre-normalize) tagsJSON,
	// same as SetMemory would receive it.
	if h.db.ProjectRequiresMemoryDiscipline(project) {
		if verr := db.ValidateMemoryWrite(project, key, tagsJSON, layer, validUntil, time.Now().UTC()); verr != nil {
			return toolResultError(verr.Error()), nil
		}
	}

	// Causal context (DEC-wraith-memory-causal-1): an explicit based_on wins,
	// else the id this agent's last get_memory of the key returned, else none
	// (legacy last-writer-wins).
	basedOn, causal := req.GetString("based_on", ""), "arg"
	if basedOn == "" {
		causal = "none"
		if cached := h.memReads.lookup(project, agent, scope, key); cached != "" {
			basedOn, causal = cached, "cache"
		}
	}
	mem, err := h.db.SetMemoryWith(project, agent, key, value, tagsJSON, scope, confidence, layer, upsert, db.SetMemoryOpts{BasedOn: basedOn})
	if errors.Is(err, db.ErrBasedOnMismatch) {
		if causal == "arg" {
			return validationError(CodeInvalidArgument, err.Error()), nil
		}
		// A cached read that no longer resolves degrades to legacy, never to
		// a refused write or a false conflict.
		causal = "none"
		mem, err = h.db.SetMemoryWith(project, agent, key, value, tagsJSON, scope, confidence, layer, upsert, db.SetMemoryOpts{})
	}
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to set memory: %v", err)), nil
	}
	outcome := memoryWriteOutcome(mem)
	log.Printf("[memory] causal=%s outcome=%s project=%s agent=%s key=%s", causal, outcome, project, agent, key)

	// Optional temporal validity window (T5). Stamped after the memory exists so
	// the caller can set an expiry at write time; past valid_until reads as stale.
	if validFrom != "" || validUntil != "" {
		if verr := h.db.SetMemoryValidity(project, agent, key, mem.Scope, validFrom, validUntil); verr != nil {
			return toolResultError(fmt.Sprintf("failed to set validity window: %v", verr)), nil
		}
		// Reflect the CANONICAL stored window + effective status by re-reading,
		// rather than echoing the raw caller input — the DB normalizes valid_from/
		// valid_until to memoryTimeFmt, so the response must match what was stored
		// (bf4dc48d). Falls back to the pre-read mem if the re-read finds nothing.
		if fresh, ferr := h.db.GetMemory(project, agent, key, mem.Scope); ferr == nil && len(fresh) > 0 {
			mem = &fresh[0]
		}
	}

	result := map[string]any{
		"memory": mem,
	}
	if h.db.ProjectRequiresMemoryDiscipline(project) {
		if warning := db.MemoryValueWarning(value); warning != "" {
			result["warning"] = warning
		}
	}
	result["causal"] = causal
	action := "set"
	if mem.ConflictWith != nil {
		result["conflict"] = true
		result["message"] = fmt.Sprintf("Conflict detected: key '%s' already exists with a different value. Both versions preserved. Use resolve_conflict to pick the truth.", key)
		action = "conflict"
		h.routeMemoryConflict(result, project, agent, key, scope, layer, causal, upsert, mem)
	}
	h.events.Emit(MCPEvent{Type: "memory", Action: action, Agent: agent, Project: project, Label: key})

	return h.resultJSONTracked(project, agent, "set_memory", result)
}

func (h *Handlers) HandleGetMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	key := req.GetString("key", "")
	if key == "" {
		return toolResultError("key is required"), nil
	}
	scope := req.GetString("scope", "")

	memories, err := h.db.GetMemory(project, agent, key, scope)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to get memory: %v", err)), nil
	}
	if memories == nil {
		memories = []models.Memory{}
	}
	// Remember what this agent read (in memory, no DB write) so its next
	// set_memory of the key carries causal context without a based_on arg.
	if len(memories) == 1 {
		h.memReads.record(project, agent, memories[0].Scope, key, memories[0].ID)
	}
	// Consumption capture (design 54e529d8): buffered, flushed off the read path.
	h.recalls.record(project, agent, memoryIDs(memories))

	result := map[string]any{
		"key":      key,
		"count":    len(memories),
		"memories": memories,
	}
	if len(memories) > 1 {
		result["conflict"] = true
		result["message"] = "Multiple values exist for this key. Use resolve_conflict to pick the truth."
	}

	// Never return an unexplained empty result for a key that once existed: if
	// no live/stale memory matched, surface the archived tombstone (who/when/why)
	// so recall of a deleted/superseded key explains itself (T5 invariant).
	if len(memories) == 0 {
		archived, aerr := h.db.GetMemoryIncludingArchived(project, agent, key, scope)
		if aerr == nil && len(archived) > 0 {
			result["memories"] = archived
			result["count"] = len(archived)
			result["archived"] = true
			result["message"] = fmt.Sprintf("Key %q has no live value — it was archived (see status/archived_at/archived_by/archived_reason).", key)
		}
	}

	return h.resultJSONTracked(project, agent, "get_memory", result)
}

// memoryWriteOutcome classifies what SetMemoryWith did, for the [memory] log.
func memoryWriteOutcome(mem *models.Memory) string {
	switch {
	case mem.ConflictWith != nil:
		return "sibling"
	case mem.CreatedAt != mem.UpdatedAt:
		return "noop" // convergent write: existing row touched, nothing inserted
	case mem.Supersedes != nil:
		return "fast-forward"
	default:
		return "fresh"
	}
}

// routeMemoryConflict reports a sibling: the live row it conflicts with and its
// author go in the result, event:memory-conflict goes to the notifier, and for
// a based_on mismatch the current author gets a relay message (fyi P2, or a P1
// decide message for constraints/decision layer keys). No message on a
// self-race or for agent-scope memories.
func (h *Handlers) routeMemoryConflict(result map[string]any, project, agent, key, scope, layer, causal string, upsert bool, mem *models.Memory) {
	current, err := h.db.GetMemoryByID(*mem.ConflictWith)
	if err != nil || current == nil {
		return
	}
	result["conflict_with"] = current.ID
	result["current_author"] = current.AgentName
	preview, _ := truncatePreview(current.Value, msgContentPreview)
	result["current_value"] = preview
	h.events.EmitSemantic("event:memory-conflict", project, agent, map[string]any{
		"key": key, "scope": scope, "current_id": current.ID, "sibling_id": mem.ID,
		"writer": agent, "current_author": current.AgentName,
	})
	if causal == "none" || !upsert || scope == "agent" || current.AgentName == agent {
		return
	}
	msgType, priority, action := "fyi", "P2", "none"
	if isDoctrineLayer(current.Layer) || isDoctrineLayer(layer) {
		msgType, priority, action = "notification", "P1", "decide"
	}
	content := fmt.Sprintf("%s wrote memory %q (%s scope) based on an older version than yours. Your value (%s) and theirs (%s) are both live; use resolve_conflict to pick the truth.",
		agent, key, scope, current.ID, mem.ID)
	meta, _ := json.Marshal(map[string]string{"memory_key": key, "current_id": current.ID, "sibling_id": mem.ID})
	if _, _, err := h.db.InsertMessageWithDeliveries(project, "relay", current.AgentName, msgType, "memory conflict: "+key, content, string(meta),
		priority, -1, nil, nil, []string{current.AgentName}, action); err != nil {
		log.Printf("[memory] conflict notice to %s failed: %v", current.AgentName, err)
	}
}

func isDoctrineLayer(layer string) bool { return layer == "constraints" || layer == "decision" }

// HandleRemember records an ADR-style decision (TSU-51). Decisions are project
// memories (layer="decision"); the accepted set is surfaced at session start so
// agents stop re-litigating settled calls.
func (h *Handlers) HandleRemember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	decision := req.GetString("decision", "")
	if decision == "" {
		return toolResultError("decision is required (the settled rule, one line)"), nil
	}
	rationale := req.GetString("rationale", "")
	area := req.GetString("area", "")
	tags := req.GetStringSlice("tags", nil)
	supersedes := req.GetString("supersedes", "")
	dependsOn := req.GetStringSlice("depends_on", nil)

	if h.db.ProjectRequiresMemoryDiscipline(project) {
		if hint := db.ValidateRememberArea(area); hint != "" {
			return toolResultError(fmt.Sprintf("memory discipline (project %q): area. %s", project, hint)), nil
		}
	}

	mem, err := h.db.RememberDecision(project, agent, area, decision, rationale, tags, supersedes, dependsOn)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to remember decision: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "memory", Action: "decision", Agent: agent, Project: project, Label: mem.Key})
	return h.resultJSONTracked(project, agent, "remember", map[string]any{"decision": mem})
}

// HandleRecallDecisions returns the project's accepted (non-superseded) decisions.
func (h *Handlers) HandleRecallDecisions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	decs, err := h.db.ListDecisions(project)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to recall decisions: %v", err)), nil
	}
	h.recalls.record(project, agent, memoryIDs(decs))
	return h.resultJSONTracked(project, agent, "recall_decisions", map[string]any{"decisions": decs, "count": len(decs)})
}

func (h *Handlers) HandleSearchMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	query := req.GetString("query", "")
	if query == "" {
		return toolResultError("query is required"), nil
	}
	scope := req.GetString("scope", "")
	tags := req.GetStringSlice("tags", nil)
	limit := clampLimit(req.GetInt("limit", 20))
	includeStale := req.GetBool("include_stale", false)
	// rank="mempalace" opts into ranked recall (relevance+recency+importance).
	// Any other value (incl. default "") keeps the pure-FTS bm25 order.
	ranked := req.GetString("rank", "") == "mempalace"

	// Truncate values for compact response
	compact := func(m *models.Memory) map[string]any {
		val := m.Value
		if len(val) > 300 {
			val = val[:300] + "..."
		}
		row := map[string]any{
			"id":         m.ID,
			"key":        m.Key,
			"value":      val,
			"tags":       m.Tags,
			"scope":      m.Scope,
			"agent_name": m.AgentName,
			"confidence": m.Confidence,
			"version":    m.Version,
			"updated_at": m.UpdatedAt,
			"conflict":   m.ConflictWith != nil,
			"status":     m.Status,
			"importance": roundImportance(m.Importance),
		}
		if m.ValidUntil != nil {
			row["valid_until"] = *m.ValidUntil
		}
		return row
	}

	if ranked {
		results, err := h.db.SearchMemoryRanked(project, agent, query, tags, scope, limit, includeStale)
		if err != nil {
			return toolResultError(fmt.Sprintf("failed to search memories: %v", err)), nil
		}
		rows := make([]map[string]any, len(results))
		served := make([]string, len(results))
		for i := range results {
			row := compact(&results[i].Memory)
			row["rank_score"] = roundImportance(results[i].RankScore)
			rows[i] = row
			served[i] = results[i].ID
		}
		h.recalls.record(project, agent, served)
		return h.resultJSONTracked(project, agent, "search_memory", map[string]any{
			"query":         query,
			"count":         len(rows),
			"include_stale": includeStale,
			"rank":          "mempalace",
			"memories":      rows,
		})
	}

	memories, err := h.db.SearchMemory(project, agent, query, tags, scope, limit, includeStale)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to search memories: %v", err)), nil
	}
	if memories == nil {
		memories = []models.Memory{}
	}

	truncated := make([]map[string]any, len(memories))
	for i := range memories {
		truncated[i] = compact(&memories[i])
	}
	h.recalls.record(project, agent, memoryIDs(memories))

	return h.resultJSONTracked(project, agent, "search_memory", map[string]any{
		"query":         query,
		"count":         len(truncated),
		"include_stale": includeStale,
		"memories":      truncated,
	})
}

func (h *Handlers) HandleListMemories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	scope := req.GetString("scope", "")
	agentFilter := req.GetString("agent", "")
	tags := req.GetStringSlice("tags", nil)
	limit := clampLimit(req.GetInt("limit", 50))
	includeStale := req.GetBool("include_stale", false)

	// Bug fix: scope=agent must be filtered by the calling agent to prevent leaking
	// other agents' private memories. If no explicit agent filter, use the caller's identity.
	if scope == "agent" && agentFilter == "" {
		agentFilter = resolveAgent(ctx, req)
	}

	memories, err := h.db.ListMemories(project, scope, agentFilter, tags, limit, includeStale)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to list memories: %v", err)), nil
	}
	if memories == nil {
		memories = []models.Memory{}
	}

	// Truncate values for compact response
	truncated := make([]map[string]any, len(memories))
	for i, m := range memories {
		val := m.Value
		if len(val) > 200 {
			val = val[:200] + "..."
		}
		row := map[string]any{
			"id":         m.ID,
			"key":        m.Key,
			"value":      val,
			"tags":       m.Tags,
			"scope":      m.Scope,
			"project":    m.Project,
			"agent_name": m.AgentName,
			"confidence": m.Confidence,
			"version":    m.Version,
			"updated_at": m.UpdatedAt,
			"conflict":   m.ConflictWith != nil,
			"status":     m.Status,
			"importance": roundImportance(m.Importance),
		}
		if m.ValidUntil != nil {
			row["valid_until"] = *m.ValidUntil
		}
		truncated[i] = row
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(memories))
		for i, m := range memories {
			val, _ := truncated[i]["value"].(string)
			rows[i] = []string{m.Key, m.Scope, m.Status, m.AgentName, m.Confidence, m.Tags, val, m.UpdatedAt}
		}
		table := renderTable([]string{"key", "scope", "status", "agent", "confidence", "tags", "value", "updated_at"}, rows)
		return h.resultTextTracked(project, "", "list_memories", fmt.Sprintf("%d memories\n%s", len(memories), table))
	}

	return h.resultJSONTracked(project, "", "list_memories", map[string]any{
		"count":         len(truncated),
		"include_stale": includeStale,
		"memories":      truncated,
	})
}

func (h *Handlers) HandleDeleteMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	key := req.GetString("key", "")
	if key == "" {
		return toolResultError("key is required"), nil
	}
	scope := req.GetString("scope", "project")
	reason := req.GetString("reason", "")

	// `agent` is the OPTIONAL target author (agent scope only): the admin arm that
	// lets an executive — or anyone clearing a dead agent's leftovers — archive
	// another author's agent-scope memory (a self-delete resolves agent_name to
	// the caller, which cannot reach a departed agent's rows). Default: the caller
	// itself, i.e. the ordinary self-delete.
	targetAuthor := strings.TrimSpace(req.GetString("agent", ""))
	if targetAuthor == "" {
		targetAuthor = agent
	}
	// Agent names are stored lowercase (register folds them), and the agent-scope
	// UPDATE matches agent_name exactly. Fold the caller-supplied target to the
	// canonical form so a mixed-case `agent` param (e.g. "Frontend-Lead") still
	// resolves to the row it names instead of silently matching nothing — the
	// authz walk already folds case, so an unfolded store target would let the
	// permission check pass yet archive nothing.
	targetAuthor = strings.ToLower(targetAuthor)
	if !strings.EqualFold(targetAuthor, agent) {
		// Cross-author delete. Only agent scope carries an agent_name dimension;
		// on project/global the `agent` param would silently do nothing, so refuse
		// loudly rather than pretend.
		if scope != "agent" {
			return toolResultError(fmt.Sprintf(
				"the `agent` target (%s) only applies to scope=agent — %s memories have no per-author dimension", targetAuthor, scope)), nil
		}
		// Gate: permitted iff the caller is an executive, OR the target author is
		// no longer a live agent (inactive/deactivated/deleted, or never registered
		// — e.g. an anonymous author). A non-executive aimed at another LIVE agent
		// is refused loudly, naming both (founder silent-failure theme).
		caller, _ := h.db.GetAgent(project, strings.ToLower(agent))
		callerIsExec := caller != nil && caller.IsExecutive
		if !callerIsExec && !targetAuthorIsDead(h, project, targetAuthor) {
			return permissionError(CodeForbidden, fmt.Sprintf(
				"delete_memory: %s cannot archive the agent-scope memory of the LIVE agent %s — only an executive may target another active agent's memory (the target must be inactive/deactivated otherwise)",
				agent, targetAuthor)), nil
		}
	}

	if err := h.db.DeleteMemoryAs(project, agent, targetAuthor, key, scope, reason); err != nil {
		return toolResultError(fmt.Sprintf("failed to delete memory: %v", err)), nil
	}

	return h.resultJSONTracked(project, agent, "delete_memory", map[string]any{
		"deleted":       true,
		"key":           key,
		"scope":         scope,
		"target_author": targetAuthor,
		"archived_by":   agent,
		"archived":      true,
		"note":          "soft-deleted: archived with a tombstone (who/when/why + target author), not removed. Recall still surfaces it flagged.",
	})
}

// targetAuthorIsDead reports whether the named author is no longer a live agent,
// so its leftover agent-scope memories are fair game for any caller to reap. Dead
// = never registered in this project (GetAgent nil — e.g. an "anonymous" author),
// or deactivated/removed (status inactive/deleted). An active or merely sleeping
// agent is LIVE and its memory needs executive sign-off to touch. Names fold case
// (agents are stored lowercase).
func targetAuthorIsDead(h *Handlers, project, author string) bool {
	ag, err := h.db.GetAgent(project, strings.ToLower(author))
	if err != nil || ag == nil {
		return true
	}
	return ag.Status == "inactive" || ag.Status == "deleted"
}

func (h *Handlers) HandleResolveConflict(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	key := req.GetString("key", "")
	if key == "" {
		return toolResultError("key is required"), nil
	}
	chosenValue := req.GetString("chosen_value", "")
	if chosenValue == "" {
		return toolResultError("chosen_value is required"), nil
	}
	scope := req.GetString("scope", "project")

	winner, err := h.db.ResolveConflict(project, agent, key, chosenValue, scope)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to resolve conflict: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "memory", Action: "resolve", Agent: agent, Project: project, Label: key})

	return h.resultJSONTracked(project, agent, "resolve_conflict", map[string]any{
		"resolved": true,
		"memory":   winner,
	})
}

// memoryIDs returns the row ids (= versions) of memories, for recall capture.
func memoryIDs(mems []models.Memory) []string {
	ids := make([]string, len(mems))
	for i := range mems {
		ids[i] = mems[i].ID
	}
	return ids
}
