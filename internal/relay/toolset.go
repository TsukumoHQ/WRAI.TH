package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registeredTool pairs a ServerTool with the category used by discover_tools.
type registeredTool struct {
	server.ServerTool
	category string
}

// toolCategories drives the discover_tools enum and description. Order matters
// for display; keep summaries to one line — they are the discovery-mode index.
var toolCategories = []struct{ name, summary string }{
	{"session", "identity + boot: whoami, register_agent, get_session_context, query_context"},
	{"messaging", "send/receive: send_message, get_inbox, ack_delivery, get_thread, mark_read, get_team_inbox"},
	{"conversations", "multi-agent threads: create, list, get messages, invite, leave, archive"},
	{"tasks", "task lifecycle: dispatch, claim, start, review, complete, block, resume, cancel, get, list, update, move, archive, batch ops"},
	{"boards", "task boards: create, list, archive, delete"},
	{"memory", "persistent knowledge: set, get, search, list, delete, resolve_conflict"},
	{"profiles", "role archetypes: register, get, list, find by skill"},
	{"agents", "agent lifecycle: list, deactivate, delete, sleep"},
	{"teams", "teams + orgs: create, list, members, notify channels"},
	{"projects", "project lifecycle: create_project, delete_project"},
}

// toolRegistry is the single source of truth for every MCP tool the relay
// serves: relay.New registers from it, discover_tools/call_tool index it,
// and the schema budget test measures it.
func (h *Handlers) toolRegistry() []registeredTool {
	tools := []registeredTool{
		{server.ServerTool{Tool: whoamiTool(), Handler: h.HandleWhoami}, "session"},
		{server.ServerTool{Tool: registerAgentTool(), Handler: h.HandleRegisterAgent}, "session"},
		{server.ServerTool{Tool: getSessionContextTool(), Handler: h.HandleGetSessionContext}, "session"},
		{server.ServerTool{Tool: queryContextTool(), Handler: h.HandleQueryContext}, "session"},

		{server.ServerTool{Tool: sendMessageTool(), Handler: h.HandleSendMessage}, "messaging"},
		{server.ServerTool{Tool: getInboxTool(), Handler: h.HandleGetInbox}, "messaging"},
		{server.ServerTool{Tool: ackDeliveryTool(), Handler: h.HandleAckDelivery}, "messaging"},
		{server.ServerTool{Tool: getThreadTool(), Handler: h.HandleGetThread}, "messaging"},
		{server.ServerTool{Tool: markReadTool(), Handler: h.HandleMarkRead}, "messaging"},
		{server.ServerTool{Tool: getTeamInboxTool(), Handler: h.HandleGetTeamInbox}, "messaging"},

		{server.ServerTool{Tool: createConversationTool(), Handler: h.HandleCreateConversation}, "conversations"},
		{server.ServerTool{Tool: listConversationsTool(), Handler: h.HandleListConversations}, "conversations"},
		{server.ServerTool{Tool: getConversationMessagesTool(), Handler: h.HandleGetConversationMessages}, "conversations"},
		{server.ServerTool{Tool: inviteToConversationTool(), Handler: h.HandleInviteToConversation}, "conversations"},
		{server.ServerTool{Tool: leaveConversationTool(), Handler: h.HandleLeaveConversation}, "conversations"},
		{server.ServerTool{Tool: archiveConversationTool(), Handler: h.HandleArchiveConversation}, "conversations"},

		{server.ServerTool{Tool: dispatchTaskTool(), Handler: h.HandleDispatchTask}, "tasks"},
		{server.ServerTool{Tool: claimTaskTool(), Handler: h.HandleClaimTask}, "tasks"},
		{server.ServerTool{Tool: startTaskTool(), Handler: h.HandleStartTask}, "tasks"},
		{server.ServerTool{Tool: reviewTaskTool(), Handler: h.HandleReviewTask}, "tasks"},
		{server.ServerTool{Tool: commentTool(), Handler: h.HandleComment}, "tasks"},
		{server.ServerTool{Tool: completeTaskTool(), Handler: h.HandleCompleteTask}, "tasks"},
		{server.ServerTool{Tool: blockTaskTool(), Handler: h.HandleBlockTask}, "tasks"},
		{server.ServerTool{Tool: resumeTaskTool(), Handler: h.HandleResumeTask}, "tasks"},
		{server.ServerTool{Tool: cancelTaskTool(), Handler: h.HandleCancelTask}, "tasks"},
		{server.ServerTool{Tool: getTaskTool(), Handler: h.HandleGetTask}, "tasks"},
		{server.ServerTool{Tool: listTasksTool(), Handler: h.HandleListTasks}, "tasks"},
		{server.ServerTool{Tool: updateTaskTool(), Handler: h.HandleUpdateTask}, "tasks"},
		{server.ServerTool{Tool: archiveTasksTool(), Handler: h.HandleArchiveTasks}, "tasks"},
		{server.ServerTool{Tool: moveTaskTool(), Handler: h.HandleMoveTask}, "tasks"},
		{server.ServerTool{Tool: batchCompleteTasksTool(), Handler: h.HandleBatchCompleteTasks}, "tasks"},
		{server.ServerTool{Tool: batchDispatchTasksTool(), Handler: h.HandleBatchDispatchTasks}, "tasks"},

		{server.ServerTool{Tool: createBoardTool(), Handler: h.HandleCreateBoard}, "boards"},
		{server.ServerTool{Tool: listBoardsTool(), Handler: h.HandleListBoards}, "boards"},
		{server.ServerTool{Tool: archiveBoardTool(), Handler: h.HandleArchiveBoard}, "boards"},
		{server.ServerTool{Tool: deleteBoardTool(), Handler: h.HandleDeleteBoard}, "boards"},

		{server.ServerTool{Tool: setMemoryTool(), Handler: h.HandleSetMemory}, "memory"},
		{server.ServerTool{Tool: getMemoryTool(), Handler: h.HandleGetMemory}, "memory"},
		{server.ServerTool{Tool: searchMemoryTool(), Handler: h.HandleSearchMemory}, "memory"},
		{server.ServerTool{Tool: listMemoriesTool(), Handler: h.HandleListMemories}, "memory"},
		{server.ServerTool{Tool: deleteMemoryTool(), Handler: h.HandleDeleteMemory}, "memory"},
		{server.ServerTool{Tool: resolveConflictTool(), Handler: h.HandleResolveConflict}, "memory"},
		{server.ServerTool{Tool: rememberTool(), Handler: h.HandleRemember}, "memory"},
		{server.ServerTool{Tool: recallDecisionsTool(), Handler: h.HandleRecallDecisions}, "memory"},

		{server.ServerTool{Tool: registerProfileTool(), Handler: h.HandleRegisterProfile}, "profiles"},
		{server.ServerTool{Tool: getProfileTool(), Handler: h.HandleGetProfile}, "profiles"},
		{server.ServerTool{Tool: listProfilesTool(), Handler: h.HandleListProfiles}, "profiles"},
		{server.ServerTool{Tool: findProfilesTool(), Handler: h.HandleFindProfiles}, "profiles"},

		{server.ServerTool{Tool: listAgentsTool(), Handler: h.HandleListAgents}, "agents"},
		{server.ServerTool{Tool: deactivateAgentTool(), Handler: h.HandleDeactivateAgent}, "agents"},
		{server.ServerTool{Tool: deleteAgentTool(), Handler: h.HandleDeleteAgent}, "agents"},
		{server.ServerTool{Tool: sleepAgentTool(), Handler: h.HandleSleepAgent}, "agents"},

		{server.ServerTool{Tool: createOrgTool(), Handler: h.HandleCreateOrg}, "teams"},
		{server.ServerTool{Tool: listOrgsTool(), Handler: h.HandleListOrgs}, "teams"},
		{server.ServerTool{Tool: createTeamTool(), Handler: h.HandleCreateTeam}, "teams"},
		{server.ServerTool{Tool: listTeamsTool(), Handler: h.HandleListTeams}, "teams"},
		{server.ServerTool{Tool: addTeamMemberTool(), Handler: h.HandleAddTeamMember}, "teams"},
		{server.ServerTool{Tool: removeTeamMemberTool(), Handler: h.HandleRemoveTeamMember}, "teams"},
		{server.ServerTool{Tool: addNotifyChannelTool(), Handler: h.HandleAddNotifyChannel}, "teams"},

		// File locks disabled: worktree isolation + merge conflicts replace
		// advisory locks. Re-enable by uncommenting if a workflow ever needs
		// cross-worktree file claims.
		// {server.ServerTool{Tool: claimFilesTool(), Handler: h.HandleClaimFiles}, "locks"},
		// {server.ServerTool{Tool: releaseFilesTool(), Handler: h.HandleReleaseFiles}, "locks"},
		// {server.ServerTool{Tool: listLocksTool(), Handler: h.HandleListLocks}, "locks"},

		{server.ServerTool{Tool: createProjectTool(), Handler: h.HandleCreateProject}, "projects"},
		{server.ServerTool{Tool: deleteProjectTool(), Handler: h.HandleDeleteProject}, "projects"},
	}
	// When RELAY_REQUIRE_REGISTERED is on, wrap the actor-attributed write tools
	// so an unregistered/anonymous identity is rejected before the write. Reads
	// and the bootstrap tools (register_agent, whoami, get_session_context …)
	// are never wrapped, so an agent can always register and orient first.
	if h.requireRegistered {
		for i := range tools {
			if mutatingTools[tools[i].Tool.Name] {
				tools[i].Handler = h.guardRegistered(tools[i].Handler)
			}
		}
	}
	return tools
}

// mutatingTools are the write tools gated by RELAY_REQUIRE_REGISTERED — those
// that record or act under an agent identity. Pure reads and identity bootstrap
// tools are intentionally absent so registration and orientation stay open.
var mutatingTools = map[string]bool{
	"send_message": true, "ack_delivery": true, "mark_read": true,
	"dispatch_task": true, "claim_task": true, "start_task": true,
	"review_task": true, "comment": true, "complete_task": true,
	"block_task": true, "resume_task": true, "cancel_task": true,
	"update_task": true, "archive_tasks": true, "move_task": true,
	"batch_complete_tasks": true, "batch_dispatch_tasks": true,
	"create_board": true, "archive_board": true, "delete_board": true,
	"set_memory": true, "delete_memory": true, "resolve_conflict": true,
	"remember":         true,
	"register_profile": true,
	"deactivate_agent": true, "delete_agent": true, "sleep_agent": true,
	"create_org": true, "create_team": true, "add_team_member": true,
	"remove_team_member": true, "add_notify_channel": true,
	"create_conversation": true, "invite_to_conversation": true,
	"leave_conversation": true, "archive_conversation": true,
	"create_project": true, "delete_project": true,
}

// guardRegistered wraps a tool handler to reject calls whose acting agent is
// anonymous or not registered in the project. Only installed when
// RELAY_REQUIRE_REGISTERED is on (see toolRegistry).
func (h *Handlers) guardRegistered(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		from := strings.ToLower(resolveAgent(ctx, req))
		project := ProjectFromContext(ctx)
		if from == "" || from == "anonymous" {
			return mcp.NewToolResultError("RELAY_REQUIRE_REGISTERED is on: no agent identity. Set ?agent=<name> on the MCP URL (or pass `as`) after calling register_agent."), nil
		}
		agent, err := h.db.GetAgent(project, from)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("identity check failed: %v", err)), nil
		}
		if agent == nil {
			return mcp.NewToolResultError(fmt.Sprintf("RELAY_REQUIRE_REGISTERED is on: agent %q is not registered in project %q — call register_agent first.", from, project)), nil
		}
		return next(ctx, req)
	}
}

// --- Discovery mode (progressive disclosure) ---
//
// Connecting with ?tools=discovery exposes only two tools (~600 bytes of
// schema instead of ~44KB): discover_tools returns the schemas for one
// category on demand, call_tool dispatches to any registered tool by name.

const discoveryToolName = "discover_tools"
const callToolName = "call_tool"

func discoverToolsTool() mcp.Tool {
	var lines []string
	var names []string
	for _, c := range toolCategories {
		lines = append(lines, fmt.Sprintf("- %s: %s", c.name, c.summary))
		names = append(names, c.name)
	}
	return mcp.NewTool(
		discoveryToolName,
		mcp.WithDescription("Get the tool schemas for one category. Then invoke them via call_tool(tool, args).\n\nCategories:\n"+strings.Join(lines, "\n")),
		mcp.WithString("category", mcp.Description("Category to load"), mcp.Enum(names...), mcp.Required()),
	)
}

func callToolTool() mcp.Tool {
	return mcp.NewTool(
		callToolName,
		mcp.WithDescription("Invoke a relay tool by name (see discover_tools for schemas)."),
		mcp.WithString("tool", mcp.Description("Tool name (e.g. 'send_message')"), mcp.Required()),
		mcp.WithObject("args", mcp.Description("Tool arguments object")),
	)
}

func (h *Handlers) HandleDiscoverTools(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	category := req.GetString("category", "")
	valid := false
	for _, c := range toolCategories {
		if c.name == category {
			valid = true
			break
		}
	}
	if !valid {
		return mcp.NewToolResultError(fmt.Sprintf("unknown category %q — call discover_tools without reading its description? Valid: see enum", category)), nil
	}

	type toolSchema struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema any    `json:"inputSchema"`
	}
	var out []toolSchema
	for _, rt := range h.toolRegistry() {
		if rt.category == category {
			out = append(out, toolSchema{rt.Tool.Name, rt.Tool.Description, rt.Tool.InputSchema})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	b, err := json.Marshal(map[string]any{"category": category, "tools": out})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal schemas: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

func (h *Handlers) HandleCallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("tool", "")
	if name == "" {
		return mcp.NewToolResultError("tool is required"), nil
	}

	var args map[string]any
	switch raw := req.GetArguments()["args"].(type) {
	case map[string]any:
		args = raw
	case string: // tolerate JSON-encoded args from less capable callers
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("args is a string but not valid JSON: %v", err)), nil
		}
	case nil:
		args = map[string]any{}
	default:
		return mcp.NewToolResultError("args must be an object"), nil
	}

	for _, rt := range h.toolRegistry() {
		if rt.Tool.Name == name {
			inner := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}}
			return rt.Handler(ctx, inner)
		}
	}
	return mcp.NewToolResultError(fmt.Sprintf("unknown tool %q — use discover_tools to browse categories", name)), nil
}

// toolsModeFilter hides the full toolset behind discover_tools/call_tool when
// the connection asked for ?tools=discovery, and hides the discovery pair in
// full mode. Call dispatch is unaffected — only tools/list is filtered.
// coreDiscoveryTools stay on tools/list even in discovery mode: the onboarding
// tools a list-driven client (Claude Code) must call BY NAME before it would
// ever think to discover_tools — without them, `create_project` 404s with
// "tool not found". Adding ~5 schemas keeps the init payload ~1.5k tokens (vs
// ~430 bare / ~11k full), so the token economy is preserved while project setup
// works out of the box. Everything else still flows through discover/call_tool.
var coreDiscoveryTools = map[string]bool{
	"create_project":      true,
	"delete_project":      true,
	"register_agent":      true,
	"whoami":              true,
	"get_session_context": true,
}

func toolsModeFilter(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	discovery := ToolsModeFromContext(ctx) == ToolsModeDiscovery
	filtered := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		isDiscoveryTool := t.Name == discoveryToolName || t.Name == callToolName
		keep := isDiscoveryTool == discovery
		if discovery && coreDiscoveryTools[t.Name] {
			keep = true // surface the onboarding core even in discovery mode
		}
		if keep {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
