package relay

import "github.com/mark3labs/mcp-go/mcp"

// asParam is added to every tool that uses agent identity.
var asParam = mcp.WithString("as", mcp.Description("Act as this agent (overrides the default identity from the connection URL). Use this when managing multiple agents from a single session."))

// projectParam is added to every tool that needs project scoping.
// It allows overriding the default ?project= from the URL,
// so agents can switch projects without changing the MCP connection.
var projectParam = mcp.WithString("project", mcp.Description("Project namespace (overrides the default from the connection URL). Agents, messages, and conversations are isolated per project."))

func whoamiTool() mcp.Tool {
	return mcp.NewTool(
		"whoami",
		mcp.WithDescription("Identify your Claude Code session.\n\nHow it works: you generate a unique salt (3+ random words like 'purple-falcon-nebula'), include it in your message, then call this tool with that salt. The relay searches ~/.claude/ transcripts for the salt to find your session ID.\n\nNext step: use the returned session_id when calling register_agent."),
		mcp.WithString("salt", mcp.Description("A unique string you just generated (e.g. 'purple-falcon-nebula'). Must be 3+ random words that appear in your conversation transcript. The relay uses this to match your session."), mcp.Required()),
	)
}

func registerAgentTool() mcp.Tool {
	return mcp.NewTool(
		"register_agent",
		mcp.WithDescription("Register an agent with the relay. Call this once per agent at startup to announce their presence. Returns session_context with profile, tasks, unread messages, and conversations.\n\nRe-registering the same name+project is a respawn: role, description, interest_tags and max_context_bytes are updated, while identity fields you OMIT (reports_to, profile_slug, is_executive, session_id) are PRESERVED from the existing registration rather than cleared. This means a bare re-register won't drop a profile_slug set by your orchestrator. To clear these, use deactivate_agent / delete_agent / remove_team_member.\n\nIf is_executive=true (or already set), an 'admin' team ('leadership') is auto-created and the agent is added to it, enabling broadcast messages (send_message to='*')."),
		projectParam,
		mcp.WithString("name", mcp.Description("Unique agent name (e.g. 'lead', 'backend', 'frontend'). Re-registering the same name updates the agent. To rename, register the new name and call deactivate_agent on the old one."), mcp.Required()),
		mcp.WithString("role", mcp.Description("Agent role description (e.g. 'FastAPI backend developer')")),
		mcp.WithString("description", mcp.Description("What this agent is currently working on")),
		mcp.WithString("reports_to", mcp.Description("Name of the agent this one reports to (for org hierarchy)")),
		mcp.WithBoolean("is_executive", mcp.Description("Mark this agent as an executive (shows crown on canvas)")),
		mcp.WithString("profile_slug", mcp.Description("Profile archetype this agent runs (links to a registered profile)")),
		mcp.WithString("session_id", mcp.Description("Claude Code session ID ($CLAUDE_SESSION_ID) — used for activity tracking via hooks")),
		mcp.WithString("interest_tags", mcp.Description("JSON array of interest tags for context budget filtering (e.g. '[\"database\",\"auth\"]')")),
		mcp.WithNumber("max_context_bytes", mcp.Description("Max bytes for budget-pruned inbox (default: 16384)")),
	)
}

func sendMessageTool() mcp.Tool {
	return mcp.NewTool(
		"send_message",
		mcp.WithDescription("Send a message to another agent. Use '*' as recipient for broadcast (requires admin team membership — executives get this automatically). Use 'team:<slug>' to message a team. Set conversation_id to send to a conversation."),
		asParam,
		projectParam,
		mcp.WithString("to", mcp.Description("Recipient agent name, or '*' for broadcast. Ignored when conversation_id is set."), mcp.Required()),
		mcp.WithString("type",
			mcp.Description("Message type"),
			mcp.Enum("question", "response", "notification", "code-snippet", "task", "user_question"),
		),
		mcp.WithString("subject", mcp.Description("Message subject line"), mcp.Required()),
		mcp.WithString("content", mcp.Description("Message body content"), mcp.Required()),
		mcp.WithString("reply_to", mcp.Description("Message ID to reply to (for threading)")),
		mcp.WithString("metadata", mcp.Description("JSON string of additional metadata")),
		mcp.WithString("conversation_id", mcp.Description("Send message to a conversation instead of a single agent")),
		mcp.WithString("priority",
			mcp.Description("Message priority. P0=interrupt (critical), P1=steering (important), P2=advisory (default), P3=info (low). MACP aliases accepted."),
			mcp.Enum("P0", "P1", "P2", "P3", "interrupt", "steering", "advisory", "info"),
		),
		mcp.WithNumber("ttl_seconds", mcp.Description("Time-to-live in seconds (default: 14400 = 4h, 0 = never expires). Expired messages are excluded from inbox.")),
	)
}

func getInboxTool() mcp.Tool {
	return mcp.NewTool(
		"get_inbox",
		mcp.WithDescription("Get messages from an agent's inbox. Returns messages sent to them or broadcast (excluding their own broadcasts)."),
		asParam,
		projectParam,
		mcp.WithBoolean("unread_only", mcp.Description("Only return unread messages (default: true)")),
		mcp.WithNumber("limit", mcp.Description("Max number of messages to return (default: 10).")),
		mcp.WithBoolean("full_content", mcp.Description("Return full message content instead of truncating to 300 chars (default: false)")),
		mcp.WithBoolean("apply_budget", mcp.Description("Apply context budget pruning: filters messages by priority, tag relevance, and freshness to fit within agent's max_context_bytes (default: false)")),
		mcp.WithString("min_priority", mcp.Description("Minimum priority filter (e.g. 'P1' returns P0+P1 only). Priority is sorted lexically: P0 < P1 < P2 < P3."), mcp.Enum("P0", "P1", "P2", "P3")),
		mcp.WithString("from", mcp.Description("Filter by sender agent name")),
		mcp.WithString("since", mcp.Description("Only return messages created after this ISO timestamp (e.g. '2026-03-10T12:00:00Z')")),
		mcp.WithBoolean("exclude_broadcasts", mcp.Description("Exclude broadcast messages from results (default: false)")),
	)
}

func ackDeliveryTool() mcp.Tool {
	return mcp.NewTool(
		"ack_delivery",
		mcp.WithDescription("Acknowledge receipt of a message delivery. Transitions delivery state from surfaced to acknowledged. Use the delivery_id returned by get_inbox."),
		mcp.WithString("delivery_id", mcp.Description("Delivery ID to acknowledge"), mcp.Required()),
	)
}

func getThreadTool() mcp.Tool {
	return mcp.NewTool(
		"get_thread",
		mcp.WithDescription("Get a complete thread of messages starting from any message in the thread."),
		projectParam,
		mcp.WithString("message_id", mcp.Description("Any message ID in the thread"), mcp.Required()),
	)
}

func listAgentsTool() mcp.Tool {
	return mcp.NewTool(
		"list_agents",
		mcp.WithDescription("List all registered agents and their status."),
		projectParam,
	)
}

func markReadTool() mcp.Tool {
	return mcp.NewTool(
		"mark_read",
		mcp.WithDescription("Mark messages as read."),
		asParam,
		projectParam,
		mcp.WithArray("message_ids",
			mcp.Description("List of message IDs to mark as read"),
			mcp.WithStringItems(),
		),
		mcp.WithString("conversation_id", mcp.Description("Mark all messages in a conversation as read (alternative to message_ids)")),
	)
}

func createConversationTool() mcp.Tool {
	return mcp.NewTool(
		"create_conversation",
		mcp.WithDescription("Create a multi-agent conversation. All members will see messages sent to it."),
		asParam,
		projectParam,
		mcp.WithString("title", mcp.Description("Conversation title"), mcp.Required()),
		mcp.WithArray("members",
			mcp.Description("Agent names to include (you are added automatically)"),
			mcp.Required(),
			mcp.WithStringItems(),
		),
	)
}

func listConversationsTool() mcp.Tool {
	return mcp.NewTool(
		"list_conversations",
		mcp.WithDescription("List conversations you are a member of, with unread counts."),
		asParam,
		projectParam,
	)
}

func getConversationMessagesTool() mcp.Tool {
	return mcp.NewTool(
		"get_conversation_messages",
		mcp.WithDescription("Get messages from a conversation, ordered chronologically."),
		asParam,
		projectParam,
		mcp.WithString("conversation_id", mcp.Description("The conversation ID"), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Max number of messages to return (default: 50)")),
		mcp.WithString("format", mcp.Description("Response format: 'full' (default), 'compact' (metadata only: id, from, type, subject, timestamp), 'digest' (metadata + first 200 chars of content)")),
		mcp.WithBoolean("full_content", mcp.Description("When format is 'full', return complete message content without truncation (default: true)")),
	)
}

func inviteToConversationTool() mcp.Tool {
	return mcp.NewTool(
		"invite_to_conversation",
		mcp.WithDescription("Add an agent to an existing conversation."),
		asParam,
		projectParam,
		mcp.WithString("conversation_id", mcp.Description("The conversation ID"), mcp.Required()),
		mcp.WithString("agent_name", mcp.Description("Agent name to invite"), mcp.Required()),
	)
}

func leaveConversationTool() mcp.Tool {
	return mcp.NewTool(
		"leave_conversation",
		mcp.WithDescription("Leave a conversation. You will no longer see its messages."),
		asParam,
		projectParam,
		mcp.WithString("conversation_id", mcp.Description("The conversation ID"), mcp.Required()),
	)
}

func archiveConversationTool() mcp.Tool {
	return mcp.NewTool(
		"archive_conversation",
		mcp.WithDescription("Archive a conversation. It will no longer appear in anyone's list."),
		asParam,
		projectParam,
		mcp.WithString("conversation_id", mcp.Description("The conversation ID"), mcp.Required()),
	)
}

// --- Memory tools ---

func setMemoryTool() mcp.Tool {
	return mcp.NewTool(
		"set_memory",
		mcp.WithDescription("Store a piece of knowledge in persistent memory. By default uses upsert mode: silently overwrites existing values (archives old version). Set upsert=false to enable conflict detection (both versions preserved, use resolve_conflict to pick the truth)."),
		asParam,
		projectParam,
		mcp.WithString("key", mcp.Description("Memory key (e.g. 'auth-header-format', 'db-schema-version')"), mcp.Required()),
		mcp.WithString("value", mcp.Description("The knowledge to store"), mcp.Required()),
		mcp.WithArray("tags", mcp.Description("Categorization tags for search and filtering (e.g. ['auth', 'api'])"), mcp.WithStringItems()),
		mcp.WithString("scope",
			mcp.Description("Visibility scope: 'agent' (private), 'project' (shared with team), 'global' (cross-project)"),
			mcp.Enum("agent", "project", "global"),
		),
		mcp.WithString("confidence",
			mcp.Description("How this knowledge was obtained"),
			mcp.Enum("stated", "inferred", "observed"),
		),
		mcp.WithString("layer",
			mcp.Description("Memory layer: 'constraints' (hard rules, never override), 'behavior' (defaults, can adapt), 'context' (ephemeral, session-specific)"),
			mcp.Enum("constraints", "behavior", "context"),
		),
		mcp.WithBoolean("upsert", mcp.Description("When true (default), silently overwrites existing values. When false, flags a conflict if value differs.")),
	)
}

func getMemoryTool() mcp.Tool {
	return mcp.NewTool(
		"get_memory",
		mcp.WithDescription("Retrieve a memory by key. Searches with scope cascade: agent → project → global. If a conflict exists, returns ALL conflicting values with provenance so you can decide."),
		asParam,
		projectParam,
		mcp.WithString("key", mcp.Description("The memory key to look up"), mcp.Required()),
		mcp.WithString("scope",
			mcp.Description("Specific scope to search (skips cascade). Leave empty for automatic cascade."),
			mcp.Enum("agent", "project", "global"),
		),
	)
}

func searchMemoryTool() mcp.Tool {
	return mcp.NewTool(
		"search_memory",
		mcp.WithDescription("Full-text search across memories. Returns ranked results with provenance and confidence. Cross-scope search by default (respects agent privacy)."),
		asParam,
		projectParam,
		mcp.WithString("query", mcp.Description("Search query (full-text search)"), mcp.Required()),
		mcp.WithArray("tags", mcp.Description("Filter by tags"), mcp.WithStringItems()),
		mcp.WithString("scope",
			mcp.Description("Limit search to a specific scope"),
			mcp.Enum("agent", "project", "global"),
		),
		mcp.WithNumber("limit", mcp.Description("Max results to return (default: 20)")),
	)
}

func listMemoriesTool() mcp.Tool {
	return mcp.NewTool(
		"list_memories",
		mcp.WithDescription("Browse memories with filtering. Shows key, truncated value, tags, provenance. Useful for 'what does the team know about X?'"),
		asParam,
		projectParam,
		mcp.WithString("scope",
			mcp.Description("Filter by scope"),
			mcp.Enum("agent", "project", "global"),
		),
		mcp.WithArray("tags", mcp.Description("Filter by tags"), mcp.WithStringItems()),
		mcp.WithString("agent", mcp.Description("Filter by author agent name")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 50)")),
	)
}

func deleteMemoryTool() mcp.Tool {
	return mcp.NewTool(
		"delete_memory",
		mcp.WithDescription("Soft-delete a memory (archived, never hard deleted). Only the author or same scope can archive."),
		asParam,
		projectParam,
		mcp.WithString("key", mcp.Description("The memory key to archive"), mcp.Required()),
		mcp.WithString("scope",
			mcp.Description("Scope of the memory to delete"),
			mcp.Enum("agent", "project", "global"),
		),
	)
}

func resolveConflictTool() mcp.Tool {
	return mcp.NewTool(
		"resolve_conflict",
		mcp.WithDescription("Resolve a flagged memory conflict by choosing one value or providing a new one. The rejected version is archived with resolution metadata."),
		asParam,
		projectParam,
		mcp.WithString("key", mcp.Description("The conflicted memory key"), mcp.Required()),
		mcp.WithString("chosen_value", mcp.Description("The value to keep (can be one of the existing values or a new one)"), mcp.Required()),
		mcp.WithString("scope",
			mcp.Description("Scope where the conflict exists"),
			mcp.Enum("agent", "project", "global"),
		),
	)
}

// --- Profile tools ---

func registerProfileTool() mcp.Tool {
	return mcp.NewTool(
		"register_profile",
		mcp.WithDescription("Create or update a profile archetype. A profile defines a reusable agent role with a context pack (soul, skills, working style). Profiles are the 'executables' of the Agent OS — any spawn slot running this profile inherits all its knowledge."),
		projectParam,
		mcp.WithString("slug", mcp.Description("Unique profile identifier (e.g. 'backend', 'frontend', 'devops')"), mcp.Required()),
		mcp.WithString("name", mcp.Description("Display name for the profile"), mcp.Required()),
		mcp.WithString("role", mcp.Description("Role description")),
		mcp.WithString("context_pack", mcp.Description("Markdown blob: soul, skills, working style")),
		mcp.WithString("soul_keys", mcp.Description("Memory keys to load at boot. Accepts JSON string '[\"key1\",\"key2\"]' or native array.")),
		mcp.WithString("skills", mcp.Description("Skill objects. Accepts JSON string or native array. Format: [{\"id\":\"...\",\"name\":\"...\",\"tags\":[...]}]")),
		mcp.WithString("vault_paths", mcp.Description("Vault doc path patterns to auto-inject at boot. Accepts JSON string or native array. Supports globs: [\"guides/*.md\"]. {slug} is resolved to the profile slug.")),
		mcp.WithString("allowed_tools", mcp.Description("Tool patterns this profile can use. JSON array. Examples: [\"mcp__agent-relay__*\",\"Bash\",\"mcp__context7__*\"]. Default: all tools.")),
		mcp.WithNumber("pool_size", mcp.Description("Max concurrent spawns for this profile (default: 3). Set to 1 for singleton managers like CTO.")),
	)
}

func getProfileTool() mcp.Tool {
	return mcp.NewTool(
		"get_profile",
		mcp.WithDescription("Retrieve a profile with its full context pack and skills."),
		projectParam,
		mcp.WithString("slug", mcp.Description("Profile slug to retrieve"), mcp.Required()),
	)
}

func listProfilesTool() mcp.Tool {
	return mcp.NewTool(
		"list_profiles",
		mcp.WithDescription("List all available profiles in a project."),
		projectParam,
	)
}

func findProfilesTool() mcp.Tool {
	return mcp.NewTool(
		"find_profiles",
		mcp.WithDescription("Find profiles by skill tag. Returns profiles whose skills match the given tag."),
		projectParam,
		mcp.WithString("skill_tag", mcp.Description("Skill tag to search for (e.g. 'database', 'auth', 'frontend')"), mcp.Required()),
	)
}

// --- Task tools ---

func dispatchTaskTool() mcp.Tool {
	return mcp.NewTool(
		"dispatch_task",
		mcp.WithDescription("Dispatch a task to a profile archetype. Creates a task in 'pending' state for agents running that profile to claim.\n\nUse profile='human' for tasks that require human action (API keys, approvals, purchases). A 'human' profile is auto-created on first use.\n\nIf no board_id is provided, the task is auto-assigned to the first existing board (or a 'backlog' board is auto-created)."),
		asParam,
		projectParam,
		mcp.WithString("profile", mcp.Description("Profile slug to dispatch to"), mcp.Required()),
		mcp.WithString("title", mcp.Description("Task title"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Detailed task description")),
		mcp.WithString("priority",
			mcp.Description("Task priority"),
			mcp.Enum("P0", "P1", "P2", "P3"),
		),
		mcp.WithString("parent_task_id", mcp.Description("Parent task ID for subtasks")),
		mcp.WithString("board_id", mcp.Description("Board ID to assign this task to (from create_board)")),
		mcp.WithString("goal_id", mcp.Description("Goal ID to link this task to (from create_goal)")),
	)
}

func claimTaskTool() mcp.Tool {
	return mcp.NewTool(
		"claim_task",
		mcp.WithDescription("Claim a pending task. Transitions state to 'accepted'."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID to claim"), mcp.Required()),
	)
}

func startTaskTool() mcp.Tool {
	return mcp.NewTool(
		"start_task",
		mcp.WithDescription("Start working on a task. Transitions state to 'in-progress'. Can skip 'accepted' state."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID to start"), mcp.Required()),
	)
}

func completeTaskTool() mcp.Tool {
	return mcp.NewTool(
		"complete_task",
		mcp.WithDescription("Complete a task with a result. Transitions state to 'done'."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID to complete"), mcp.Required()),
		mcp.WithString("result", mcp.Description("Task output/result")),
	)
}

func blockTaskTool() mcp.Tool {
	return mcp.NewTool(
		"block_task",
		mcp.WithDescription("Mark a task as blocked with a reason. Triggers push notification to dispatcher. If task has a parent, notifies parent's dispatcher too."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID to block"), mcp.Required()),
		mcp.WithString("reason", mcp.Description("Why the task is blocked")),
	)
}

func cancelTaskTool() mcp.Tool {
	return mcp.NewTool(
		"cancel_task",
		mcp.WithDescription("Cancel a task from any state. Optionally provide a reason. Notifies the dispatcher."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID to cancel"), mcp.Required()),
		mcp.WithString("reason", mcp.Description("Why the task is being cancelled")),
	)
}

func getTaskTool() mcp.Tool {
	return mcp.NewTool(
		"get_task",
		mcp.WithDescription("Get full details of a task, optionally with its subtask chain."),
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithBoolean("include_subtasks", mcp.Description("Include subtask chain (max depth 3). Default: false")),
	)
}

func listTasksTool() mcp.Tool {
	return mcp.NewTool(
		"list_tasks",
		mcp.WithDescription("List tasks with filtering. Returns a task board view sorted by priority. Use status='active' to get all non-done/cancelled tasks."),
		asParam,
		projectParam,
		mcp.WithString("status",
			mcp.Description("Filter by status. Use 'active' for all non-done/cancelled tasks."),
			mcp.Enum("pending", "accepted", "in-progress", "done", "blocked", "cancelled", "active"),
		),
		mcp.WithString("profile", mcp.Description("Filter by profile slug")),
		mcp.WithString("priority",
			mcp.Description("Filter by priority"),
			mcp.Enum("P0", "P1", "P2", "P3"),
		),
		mcp.WithString("assigned_to", mcp.Description("Filter by assigned agent name")),
		mcp.WithString("board_id", mcp.Description("Filter by board ID")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 50)")),
		mcp.WithBoolean("include_archived", mcp.Description("Include archived tasks in results (default: false)")),
	)
}

func batchCompleteTasksTool() mcp.Tool {
	return mcp.NewTool(
		"batch_complete_tasks",
		mcp.WithDescription("Complete multiple tasks at once. Accepts an array of task IDs with optional results. More efficient than calling complete_task N times."),
		asParam,
		projectParam,
		mcp.WithString("tasks", mcp.Description("JSON array of objects: [{\"task_id\":\"...\",\"result\":\"...\"}]. Result is optional."), mcp.Required()),
	)
}

func batchDispatchTasksTool() mcp.Tool {
	return mcp.NewTool(
		"batch_dispatch_tasks",
		mcp.WithDescription("Dispatch multiple tasks at once. Accepts an array of task definitions. More efficient than calling dispatch_task N times."),
		asParam,
		projectParam,
		mcp.WithString("tasks", mcp.Description("JSON array of objects: [{\"profile\":\"...\",\"title\":\"...\",\"description\":\"...\",\"priority\":\"P2\",\"board_id\":\"...\",\"goal_id\":\"...\"}]. Only profile and title are required."), mcp.Required()),
	)
}

func moveTaskTool() mcp.Tool {
	return mcp.NewTool(
		"move_task",
		mcp.WithDescription("Move a task to a different board and/or goal. Shortcut for update_task when you only need to change board/goal assignment."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID to move"), mcp.Required()),
		mcp.WithString("board_id", mcp.Description("New board ID (use empty string to unassign from board)")),
		mcp.WithString("goal_id", mcp.Description("New goal ID (use empty string to unassign from goal)")),
	)
}

// --- Boards ---

func createBoardTool() mcp.Tool {
	return mcp.NewTool(
		"create_board",
		mcp.WithDescription("Create a task board for a project. Agents can then dispatch tasks to this board. Returns the board with its ID."),
		asParam,
		projectParam,
		mcp.WithString("name", mcp.Description("Board display name"), mcp.Required()),
		mcp.WithString("slug", mcp.Description("Board slug (unique per project, e.g. 'sprint-1', 'bugs')"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Board description")),
	)
}

func listBoardsTool() mcp.Tool {
	return mcp.NewTool(
		"list_boards",
		mcp.WithDescription("List all task boards for a project."),
		projectParam,
	)
}

func archiveBoardTool() mcp.Tool {
	return mcp.NewTool(
		"archive_board",
		mcp.WithDescription("Archive a task board and all its tasks. The board disappears from listings but data is preserved. Use this to clean up old sprint boards."),
		asParam,
		projectParam,
		mcp.WithString("board_id", mcp.Description("Board ID to archive"), mcp.Required()),
	)
}

func deleteBoardTool() mcp.Tool {
	return mcp.NewTool(
		"delete_board",
		mcp.WithDescription("Permanently delete an archived board. Only works on boards that have been archived first (safety check). Tasks are NOT deleted."),
		asParam,
		projectParam,
		mcp.WithString("board_id", mcp.Description("Board ID to delete (must be archived first)"), mcp.Required()),
	)
}

func updateTaskTool() mcp.Tool {
	return mcp.NewTool(
		"update_task",
		mcp.WithDescription("Update fields on an existing task without changing its status. Preserves assignee, claim, and progress history."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID to update"), mcp.Required()),
		mcp.WithString("title", mcp.Description("New title")),
		mcp.WithString("description", mcp.Description("New description")),
		mcp.WithString("priority", mcp.Description("New priority"), mcp.Enum("P0", "P1", "P2", "P3")),
		mcp.WithString("board_id", mcp.Description("Move to a different board")),
		mcp.WithString("goal_id", mcp.Description("Link to a different goal")),
	)
}

func archiveTasksTool() mcp.Tool {
	return mcp.NewTool(
		"archive_tasks",
		mcp.WithDescription("Archive completed/cancelled tasks to clean up boards. Soft-deletes tasks so they no longer appear in listings. Archived tasks are never hard-deleted. Use this to keep boards manageable."),
		asParam,
		projectParam,
		mcp.WithString("status", mcp.Description("Status to archive: 'done', 'cancelled', or empty for both"), mcp.Enum("done", "cancelled", "")),
		mcp.WithString("board_id", mcp.Description("Only archive tasks on this board (optional, empty = all boards)")),
	)
}

// --- Goals ---

func createGoalTool() mcp.Tool {
	return mcp.NewTool(
		"create_goal",
		mcp.WithDescription("Create a goal (objective) in the cascade hierarchy. Goals are NOT tasks — they don't appear in task boards. Goals flow: mission > project_goal > agent_goal. To create actionable work items, use dispatch_task() and link them to goals via goal_id. Goal progress is tracked by counting linked tasks."),
		asParam,
		projectParam,
		mcp.WithString("type",
			mcp.Description("Goal level in the cascade"),
			mcp.Enum("mission", "project_goal", "agent_goal"),
			mcp.Required(),
		),
		mcp.WithString("title", mcp.Description("Goal title"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Goal description")),
		mcp.WithString("parent_goal_id", mcp.Description("Parent goal ID (for cascading: mission > project_goal > agent_goal)")),
		mcp.WithString("owner_agent", mcp.Description("Agent name that owns this goal (typically for agent_goal type)")),
	)
}

func listGoalsTool() mcp.Tool {
	return mcp.NewTool(
		"list_goals",
		mcp.WithDescription("List goals with filtering and progress info."),
		projectParam,
		mcp.WithString("type",
			mcp.Description("Filter by goal type"),
			mcp.Enum("mission", "project_goal", "agent_goal"),
		),
		mcp.WithString("status",
			mcp.Description("Filter by status"),
			mcp.Enum("active", "completed", "paused"),
		),
		mcp.WithString("owner_agent", mcp.Description("Filter by owner agent")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 50)")),
	)
}

func getGoalTool() mcp.Tool {
	return mcp.NewTool(
		"get_goal",
		mcp.WithDescription("Get full goal details including ancestry chain, progress, and children."),
		projectParam,
		mcp.WithString("goal_id", mcp.Description("Goal ID"), mcp.Required()),
	)
}

func updateGoalTool() mcp.Tool {
	return mcp.NewTool(
		"update_goal",
		mcp.WithDescription("Update a goal's title, description, or status."),
		asParam,
		projectParam,
		mcp.WithString("goal_id", mcp.Description("Goal ID to update"), mcp.Required()),
		mcp.WithString("title", mcp.Description("New title")),
		mcp.WithString("description", mcp.Description("New description")),
		mcp.WithString("status",
			mcp.Description("New status"),
			mcp.Enum("active", "completed", "paused"),
		),
	)
}

func getGoalCascadeTool() mcp.Tool {
	return mcp.NewTool(
		"get_goal_cascade",
		mcp.WithDescription("Get the full goal hierarchy tree for a project with progress on each node."),
		projectParam,
	)
}

// --- Vault tools ---

func registerVaultTool() mcp.Tool {
	return mcp.NewTool(
		"register_vault",
		mcp.WithDescription("Register a vault (markdown docs folder) for a project. The relay indexes all .md files and watches for changes via fsnotify. One vault per project. Re-registering replaces the previous vault path.\n\nSuggested vault location: ~/.agent-relay/projects/<project-name>/vault/\n\nAfter registering, update your profiles' vault_paths to reference the new docs so they auto-inject at agent boot."),
		projectParam,
		mcp.WithString("path", mcp.Description("Absolute path to the vault directory (e.g. '/Users/me/my-org-docs')"), mcp.Required()),
	)
}

func searchVaultTool() mcp.Tool {
	return mcp.NewTool(
		"search_vault",
		mcp.WithDescription("Full-text search across indexed vault documents (markdown files). Returns matching docs with excerpts. Use get_vault_doc to retrieve full content after finding a match."),
		projectParam,
		mcp.WithString("query", mcp.Description("Search query (FTS5 syntax: plain words, OR, phrases in quotes)"), mcp.Required()),
		mcp.WithString("tags", mcp.Description("JSON array of tags to filter by (e.g. [\"guides\",\"decisions\"])")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 10)")),
	)
}

func getVaultDocTool() mcp.Tool {
	return mcp.NewTool(
		"get_vault_doc",
		mcp.WithDescription("Get the full content of a vault document by its path. Use search_vault first to find the right path."),
		projectParam,
		mcp.WithString("path", mcp.Description("Document path relative to vault root (e.g. 'guides/supabase-auth-config.md')"), mcp.Required()),
	)
}

func listVaultDocsTool() mcp.Tool {
	return mcp.NewTool(
		"list_vault_docs",
		mcp.WithDescription("List indexed vault documents with optional tag filtering. Returns metadata only (no content)."),
		projectParam,
		mcp.WithString("tags", mcp.Description("JSON array of tags to filter by")),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 100)")),
	)
}

// --- File locks ---

func claimFilesTool() mcp.Tool {
	return mcp.NewTool(
		"claim_files",
		mcp.WithDescription("Declare which files you are editing. Broadcasts a steering-priority message to all agents in the project. Other agents should avoid editing these files."),
		asParam,
		projectParam,
		mcp.WithString("file_paths", mcp.Description("JSON array of file paths being claimed (e.g. '[\"src/auth.go\",\"src/db.go\"]')"), mcp.Required()),
		mcp.WithNumber("ttl_seconds", mcp.Description("How long the claim lasts (default: 1800 = 30min)")),
	)
}

func releaseFilesTool() mcp.Tool {
	return mcp.NewTool(
		"release_files",
		mcp.WithDescription("Release previously claimed files. Broadcasts an info-priority message."),
		asParam,
		projectParam,
		mcp.WithString("file_paths", mcp.Description("JSON array of file paths to release (must match a previous claim)"), mcp.Required()),
	)
}

func listLocksTool() mcp.Tool {
	return mcp.NewTool(
		"list_locks",
		mcp.WithDescription("List all active file locks in a project. Shows which agent holds which files."),
		projectParam,
	)
}

// --- Agent lifecycle ---

func deactivateAgentTool() mcp.Tool {
	return mcp.NewTool(
		"deactivate_agent",
		mcp.WithDescription("Permanently deactivate an agent. It disappears from list_agents and stops receiving messages. To come back: call register_agent again. For temporary pause, use sleep_agent instead."),
		projectParam,
		mcp.WithString("name", mcp.Description("Agent name to deactivate"), mcp.Required()),
	)
}

func deleteAgentTool() mcp.Tool {
	return mcp.NewTool(
		"delete_agent",
		mcp.WithDescription("Soft-delete an agent. It disappears from the UI entirely but stays in the database. To restore: call register_agent again."),
		projectParam,
		mcp.WithString("name", mcp.Description("Agent name to delete"), mcp.Required()),
	)
}

func sleepAgentTool() mcp.Tool {
	return mcp.NewTool(
		"sleep_agent",
		mcp.WithDescription("Put an agent to sleep. It stays visible in list_agents (status='sleeping') but signals it's not actively working. Messages are still queued. Wake up by calling register_agent again."),
		asParam,
		projectParam,
	)
}

// --- Project lifecycle ---

func deleteProjectTool() mcp.Tool {
	return mcp.NewTool(
		"delete_project",
		mcp.WithDescription("Permanently delete a project and ALL its data (agents, tasks, messages, memories, boards, goals, etc). This is irreversible. Use to clean up empty or obsolete projects."),
		mcp.WithString("project", mcp.Description("Project name to delete"), mcp.Required()),
	)
}

// --- Project onboarding ---

func createProjectTool() mcp.Tool {
	return mcp.NewTool(
		"create_project",
		mcp.WithDescription("Set up a new project on the relay. This is the FIRST tool to call. It creates the project, analyzes your codebase, and returns a full onboarding plan that you must execute step by step — like a management game tutorial. You will become the setup agent: analyze the code, store knowledge, create the org (CTO + tech leads), set up the vault, profiles, goals, and board. Everything needed for multi-agent work."),
		mcp.WithString("name", mcp.Description("Project name (lowercase, no spaces — e.g. 'my-app', 'acme-api')"), mcp.Required()),
		mcp.WithString("description", mcp.Description("One-line description of the project")),
		mcp.WithString("cwd", mcp.Description("Absolute path to the project root directory (for vault setup)")),
		mcp.WithBoolean("interactive", mcp.Description("Interactive mode: present findings and wait for user approval at each phase instead of executing automatically. Default: false (auto mode).")),
	)
}

// --- Soul RAG ---

func queryContextTool() mcp.Tool {
	return mcp.NewTool(
		"query_context",
		mcp.WithDescription("Query relevant context for a task. Returns ranked memories and completed task results. Use this to load dynamic context at boot or before starting work."),
		asParam,
		projectParam,
		mcp.WithString("query", mcp.Description("What context do you need? (e.g. 'supabase migration patterns')"), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Max results (default: 10)")),
	)
}

// --- Session context ---

func getSessionContextTool() mcp.Tool {
	return mcp.NewTool(
		"get_session_context",
		mcp.WithDescription("Get everything an agent needs in one call: profile, pending tasks, unread messages, active conversations, and relevant memories. Use this at boot instead of making 5-8 separate calls."),
		asParam,
		projectParam,
		mcp.WithString("profile_slug", mcp.Description("Profile slug to load context for (optional, auto-detected from agent registration)")),
	)
}

// --- Teams + Orgs tools ---

func createOrgTool() mcp.Tool {
	return mcp.NewTool(
		"create_org",
		mcp.WithDescription("Create an organization. Orgs group teams across projects."),
		asParam,
		projectParam,
		mcp.WithString("name", mcp.Description("Organization name"), mcp.Required()),
		mcp.WithString("slug", mcp.Description("Unique slug (e.g. 'acme-corp')"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Organization description")),
	)
}

func listOrgsTool() mcp.Tool {
	return mcp.NewTool(
		"list_orgs",
		mcp.WithDescription("List all organizations."),
		asParam,
		projectParam,
	)
}

func createTeamTool() mcp.Tool {
	return mcp.NewTool(
		"create_team",
		mcp.WithDescription("Create a team within a project. Teams control messaging permissions and group agents. Types: 'regular' (default), 'admin' (unrestricted broadcast), 'bot'."),
		asParam,
		projectParam,
		mcp.WithString("name", mcp.Description("Team name"), mcp.Required()),
		mcp.WithString("slug", mcp.Description("Unique team slug within the project (e.g. 'frontend', 'comex')"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Team description")),
		mcp.WithString("type", mcp.Description("Team type: 'regular' (default), 'admin' (unrestricted messaging), 'bot'")),
		mcp.WithString("org_id", mcp.Description("Organization ID (optional)")),
		mcp.WithString("parent_team_id", mcp.Description("Parent team ID for nested team hierarchy (optional)")),
	)
}

func listTeamsTool() mcp.Tool {
	return mcp.NewTool(
		"list_teams",
		mcp.WithDescription("List all teams in a project with their members."),
		asParam,
		projectParam,
	)
}

func addTeamMemberTool() mcp.Tool {
	return mcp.NewTool(
		"add_team_member",
		mcp.WithDescription("Add an agent to a team. Roles: 'admin', 'lead', 'member' (default), 'observer'."),
		asParam,
		projectParam,
		mcp.WithString("team", mcp.Description("Team slug"), mcp.Required()),
		mcp.WithString("agent_name", mcp.Description("Agent name to add"), mcp.Required()),
		mcp.WithString("role", mcp.Description("Role in team: 'admin', 'lead', 'member' (default), 'observer'")),
	)
}

func removeTeamMemberTool() mcp.Tool {
	return mcp.NewTool(
		"remove_team_member",
		mcp.WithDescription("Remove an agent from a team (soft remove with left_at timestamp)."),
		asParam,
		projectParam,
		mcp.WithString("team", mcp.Description("Team slug"), mcp.Required()),
		mcp.WithString("agent_name", mcp.Description("Agent name to remove"), mcp.Required()),
	)
}

func getTeamInboxTool() mcp.Tool {
	return mcp.NewTool(
		"get_team_inbox",
		mcp.WithDescription("Get messages sent to a team (via to='team:slug' addressing)."),
		asParam,
		projectParam,
		mcp.WithString("team", mcp.Description("Team slug"), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Max messages (default: 50)")),
	)
}

func addNotifyChannelTool() mcp.Tool {
	return mcp.NewTool(
		"add_notify_channel",
		mcp.WithDescription("Add a notify channel — allows this agent to message the target agent even outside team boundaries."),
		asParam,
		projectParam,
		mcp.WithString("target", mcp.Description("Target agent name to allow messaging to"), mcp.Required()),
	)
}

// --- Spawn (fork/exec) tools ---

func spawnTool() mcp.Tool {
	return mcp.NewTool(
		"spawn",
		mcp.WithDescription("Spawn a child agent process (fork). Two modes:\n\n1. **Agent OS mode** (recommended): pass profile + cycle. The relay assembles the full context (identity, task, knowledge, rules, tools) from the DB. The agent opens its eyes knowing everything.\n\n2. **Legacy mode**: pass profile + prompt. Raw prompt is passed directly to claude.\n\nReturns immediately with child ID — executes asynchronously. Use list_children to monitor."),
		asParam,
		projectParam,
		mcp.WithString("profile", mcp.Description("Profile slug for the child agent (e.g. 'backend', 'cto'). Must match a registered profile."), mcp.Required()),
		mcp.WithString("cycle", mcp.Description("Cycle name (Agent OS mode). The relay loads the cycle prompt from the cycles table and assembles full context (identity, task, knowledge, rules). Examples: 'heartbeat-5min', 'execute-task', 'review-pr'.")),
		mcp.WithString("task_id", mcp.Description("Task ID to load into context (Agent OS mode). The spawned agent receives the full task details.")),
		mcp.WithString("prompt", mcp.Description("Raw prompt (legacy mode). Used only if 'cycle' is not set.")),
		mcp.WithString("ttl", mcp.Description("Max execution time (default: from cycle TTL or '10m'). Accepts Go duration format: '5m', '1h', '30s'.")),
	)
}

func killChildTool() mcp.Tool {
	return mcp.NewTool(
		"kill_child",
		mcp.WithDescription("Terminate a running child agent by ID. Sends SIGTERM to the subprocess."),
		asParam,
		projectParam,
		mcp.WithString("child_id", mcp.Description("Child agent ID (from spawn response)"), mcp.Required()),
	)
}

func listChildrenTool() mcp.Tool {
	return mcp.NewTool(
		"list_children",
		mcp.WithDescription("List spawned child agents. Shows running and recently finished children with their status, duration, and exit codes."),
		asParam,
		projectParam,
		mcp.WithString("status", mcp.Description("Filter by status: 'running', 'finished', 'killed', or 'all' (default: 'all')"),
			mcp.Enum("running", "finished", "killed", "all"),
		),
	)
}

// --- Schedule (crontab) tools ---

func scheduleTool() mcp.Tool {
	return mcp.NewTool(
		"schedule",
		mcp.WithDescription("Create or update a cron schedule. Two modes:\n\n1. **Agent OS mode**: set 'cycle' — the relay assembles full context at each trigger (profile + vault + memories + task).\n\n2. **Legacy mode**: set 'prompt' — raw prompt passed to claude each cycle.\n\nLike `crontab -e` for agents."),
		asParam,
		projectParam,
		mcp.WithString("name", mcp.Description("Schedule name (unique per agent, e.g. 'daily-review', '5min-check')"), mcp.Required()),
		mcp.WithString("cron_expr", mcp.Description("Cron expression (5-field: minute hour day month weekday). Examples: '*/5 * * * *' (every 5min), '0 9 * * *' (daily 9am), '0 */4 * * *' (every 4h)."), mcp.Required()),
		mcp.WithString("cycle", mcp.Description("Cycle name (Agent OS mode). Loads the cycle prompt from the cycles table and assembles full context. Examples: 'heartbeat-5min', 'heartbeat-1h'.")),
		mcp.WithString("prompt", mcp.Description("Raw prompt (legacy mode). Used only if 'cycle' is not set.")),
		mcp.WithString("ttl", mcp.Description("Max execution time per cycle (default: from cycle TTL or '10m')")),
	)
}

func unscheduleTool() mcp.Tool {
	return mcp.NewTool(
		"unschedule",
		mcp.WithDescription("Remove a cron schedule. The agent will no longer be triggered on this schedule."),
		asParam,
		projectParam,
		mcp.WithString("schedule_id", mcp.Description("Schedule ID to remove (from list_schedules)"), mcp.Required()),
	)
}

func listSchedulesTool() mcp.Tool {
	return mcp.NewTool(
		"list_schedules",
		mcp.WithDescription("List all cron schedules for an agent or project. Like `crontab -l`."),
		asParam,
		projectParam,
	)
}

func triggerCycleTool() mcp.Tool {
	return mcp.NewTool(
		"trigger_cycle",
		mcp.WithDescription("Manually trigger a scheduled cycle right now, without waiting for the cron timer. Like `systemctl start`."),
		asParam,
		projectParam,
		mcp.WithString("schedule_id", mcp.Description("Schedule ID to trigger (from list_schedules)"), mcp.Required()),
	)
}
