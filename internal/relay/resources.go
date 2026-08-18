package relay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// CCAR-G2 — relay content catalogs as MCP RESOURCES.
//
// Agents connect and immediately see the task board, the agent roster, the
// boards, and the memory/decisions index as read-only MCP resources — no
// exploratory tool call, and NOT counted against the tool-schema budget (a
// separate MCP capability). Every catalog is scoped to the connection's
// ?project=; a connection with no project gets a hint payload, never another
// project's data. Read-only: resources never mutate, so they sit outside the
// guardIdentity write boundary.
//
// Compact by design — the point is to CUT context pressure, so each catalog is a
// bounded index (ids + a little metadata), not full record dumps. An agent reads
// the index, then uses the existing tools for detail/actions.
const (
	resourceTasksURI   = "relay://tasks"
	resourceAgentsURI  = "relay://agents"
	resourceBoardsURI  = "relay://boards"
	resourceMemoryURI  = "relay://memory"
	resourceCatalogCap = 100 // per-catalog row cap keeps a resource read bounded
)

// RegisterResources wires the read-only catalog resources onto the MCP server.
// Called once from relay.New after the server is constructed.
func (h *Handlers) RegisterResources(srv *server.MCPServer) {
	srv.AddResource(
		mcp.NewResource(resourceTasksURI, "Task board",
			mcp.WithResourceDescription("Read-only index of active (non-done) tasks in the connection's project: id, title, status, priority, assignee, profile, board. Scoped to ?project=."),
			mcp.WithMIMEType("application/json")),
		h.resourceTasks)
	srv.AddResource(
		mcp.NewResource(resourceAgentsURI, "Agent roster",
			mcp.WithResourceDescription("Read-only roster of agents registered in the connection's project: name, role, status, profile, reports_to. Scoped to ?project=."),
			mcp.WithMIMEType("application/json")),
		h.resourceAgents)
	srv.AddResource(
		mcp.NewResource(resourceBoardsURI, "Task boards",
			mcp.WithResourceDescription("Read-only list of task boards in the connection's project: id, name, slug, description. Scoped to ?project=."),
			mcp.WithMIMEType("application/json")),
		h.resourceBoards)
	srv.AddResource(
		mcp.NewResource(resourceMemoryURI, "Memory & decisions index",
			mcp.WithResourceDescription("Read-only index of the project's accepted decisions (full) plus a compact memory index (key/scope/layer/updated_at, no values). Scoped to ?project=."),
			mcp.WithMIMEType("application/json")),
		h.resourceMemory)
}

// jsonResource marshals a catalog payload into a single JSON text resource. A
// marshal failure surfaces as an error (the client sees a resource read error,
// never a silent empty catalog).
func jsonResource(uri string, payload any) ([]mcp.ResourceContents, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal resource %s: %w", uri, err)
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{URI: uri, MIMEType: "application/json", Text: string(b)},
	}, nil
}

// unscopedCatalog is the payload returned when a connection carries no
// ?project= — a hint, not another project's data.
func unscopedCatalog(uri string) ([]mcp.ResourceContents, error) {
	return jsonResource(uri, map[string]any{
		"project": "",
		"note":    "connect with ?project=<name> (or pass project on tool calls) to scope this catalog",
		"items":   []any{},
	})
}

func (h *Handlers) resourceTasks(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	project := ProjectFromContext(ctx)
	if project == "" {
		return unscopedCatalog(req.Params.URI)
	}
	tasks, err := h.db.ListTasks(project, "active", "", "", "", "", resourceCatalogCap, false)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	rows := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		row := map[string]any{
			"id":          t.ID,
			"title":       t.Title,
			"status":      t.Status,
			"priority":    t.Priority,
			"assigned_to": t.AssignedTo,
			"profile":     t.ProfileSlug,
			"board_id":    t.BoardID,
		}
		// Surface the linked PR (PR-link S1) when present, so an agent sees a
		// task's PR in the catalog without a get_task round-trip.
		if t.PRNumber != nil {
			row["pr_number"] = *t.PRNumber
		}
		if t.PRState != nil {
			row["pr_state"] = *t.PRState
		}
		if t.PRURL != nil {
			row["pr_url"] = *t.PRURL
		}
		rows = append(rows, row)
	}
	return jsonResource(req.Params.URI, map[string]any{
		"project": project,
		"count":   len(rows),
		"tasks":   rows,
	})
}

func (h *Handlers) resourceAgents(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	project := ProjectFromContext(ctx)
	if project == "" {
		return unscopedCatalog(req.Params.URI)
	}
	agents, err := h.db.ListAgents(project)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	rows := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, map[string]any{
			"name":       a.Name,
			"role":       a.Role,
			"status":     a.Status,
			"profile":    a.ProfileSlug,
			"reports_to": a.ReportsTo,
		})
	}
	return jsonResource(req.Params.URI, map[string]any{
		"project": project,
		"count":   len(rows),
		"agents":  rows,
	})
}

func (h *Handlers) resourceBoards(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	project := ProjectFromContext(ctx)
	if project == "" {
		return unscopedCatalog(req.Params.URI)
	}
	boards, err := h.db.ListBoards(project)
	if err != nil {
		return nil, fmt.Errorf("list boards: %w", err)
	}
	rows := make([]map[string]any, 0, len(boards))
	for _, b := range boards {
		rows = append(rows, map[string]any{
			"id":          b.ID,
			"name":        b.Name,
			"slug":        b.Slug,
			"description": b.Description,
		})
	}
	return jsonResource(req.Params.URI, map[string]any{
		"project": project,
		"count":   len(rows),
		"boards":  rows,
	})
}

func (h *Handlers) resourceMemory(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	project := ProjectFromContext(ctx)
	if project == "" {
		return unscopedCatalog(req.Params.URI)
	}
	decisions, err := h.db.ListDecisions(project)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	decRows := make([]map[string]any, 0, len(decisions))
	for _, d := range decisions {
		decRows = append(decRows, map[string]any{
			"key":     d.Key,
			"value":   d.Value,
			"tags":    d.Tags,
			"updated": d.UpdatedAt,
		})
	}
	// Compact memory INDEX only (keys/scope/layer/updated) — never the values, to
	// keep the catalog small. Project-scoped memories; agent-private memories are
	// not enumerated here (they belong to their owner's get/search path).
	mems, err := h.db.ListMemories(project, "project", "", nil, resourceCatalogCap)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	memRows := make([]map[string]any, 0, len(mems))
	for _, m := range mems {
		memRows = append(memRows, map[string]any{
			"key":     m.Key,
			"scope":   m.Scope,
			"layer":   m.Layer,
			"status":  m.Status,
			"updated": m.UpdatedAt,
		})
	}
	return jsonResource(req.Params.URI, map[string]any{
		"project":      project,
		"decisions":    decRows,
		"memory_index": memRows,
	})
}

// resourceCatalogURIs is the set of catalog URIs, for tests and docs.
var resourceCatalogURIs = []string{
	resourceTasksURI, resourceAgentsURI, resourceBoardsURI, resourceMemoryURI,
}
