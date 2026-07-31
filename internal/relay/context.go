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
	ToolsModeFull      = "full"      // opt-in: every tool schema served on tools/list (~10k tokens)
	ToolsModeDiscovery = "discovery" // default: progressive disclosure, only discover_tools + call_tool (~430 tokens)
)

// HTTPContextFunc extracts the project from the ?project= query parameter
// and the optional ?agent= fallback, injecting both into the request context.
// Agent identity is primarily set via register_agent + the "as" param on tool calls.
// Progressive tool disclosure (discover_tools + call_tool) is the default — it
// cuts the tools/list init payload from ~10k tokens to ~430. Pass ?tools=full to
// serve every tool schema on tools/list for vanilla list-driven MCP clients.
// Note: dispatch is unaffected by the mode — every tool stays callable by name
// in either mode (see toolsModeFilter), so clients that invoke tools directly
// keep working under the default.
func HTTPContextFunc(ctx context.Context, r *http.Request) context.Context {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "anonymous"
	}
	project := NormalizeProject(r.URL.Query().Get("project"))
	if project == "" {
		project = "default"
	}
	if r.URL.Query().Get("tools") == ToolsModeFull {
		ctx = context.WithValue(ctx, toolsModeKey, ToolsModeFull)
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
// Defaults to discovery (progressive disclosure) when unset.
func ToolsModeFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(toolsModeKey).(string); ok {
		return v
	}
	return ToolsModeDiscovery
}
