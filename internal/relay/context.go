package relay

import (
	"context"
	"net/http"
)

type contextKey string

const agentNameKey contextKey = "agent_name"
const projectKey contextKey = "project_name"
const toolsModeKey contextKey = "tools_mode"

// Tools exposure modes (?tools= query parameter).
const (
	ToolsModeFull      = "full"      // default: every tool schema served on tools/list
	ToolsModeDiscovery = "discovery" // progressive disclosure: only discover_tools + call_tool
)

// HTTPContextFunc extracts the project from the ?project= query parameter
// and the optional ?agent= fallback, injecting both into the request context.
// Agent identity is primarily set via register_agent + the "as" param on tool calls.
// ?tools=discovery opts the connection into progressive tool disclosure.
func HTTPContextFunc(ctx context.Context, r *http.Request) context.Context {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "anonymous"
	}
	project := r.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	if r.URL.Query().Get("tools") == ToolsModeDiscovery {
		ctx = context.WithValue(ctx, toolsModeKey, ToolsModeDiscovery)
	}
	ctx = context.WithValue(ctx, agentNameKey, agent)
	return context.WithValue(ctx, projectKey, project)
}

// AgentFromContext retrieves the agent name from the context.
func AgentFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(agentNameKey).(string); ok {
		return v
	}
	return "anonymous"
}

// ProjectFromContext retrieves the project name from the context.
func ProjectFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(projectKey).(string); ok {
		return v
	}
	return "default"
}

// ToolsModeFromContext retrieves the tool exposure mode from the context.
func ToolsModeFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(toolsModeKey).(string); ok {
		return v
	}
	return ToolsModeFull
}
