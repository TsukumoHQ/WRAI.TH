package relay

import "github.com/mark3labs/mcp-go/mcp"

// asParam is added to every tool that uses agent identity.
var asParam = mcp.WithString("as", mcp.Description("Act as this agent (overrides the identity from the connection URL)."))

// projectParam is added to every tool that needs project scoping.
// It allows overriding the default ?project= from the URL,
// so agents can switch projects without changing the MCP connection.
var projectParam = mcp.WithString("project", mcp.Description("Project namespace (overrides the connection URL default)."))

// formatParam is the shared output-format selector for list/get tools that can
// render either a compact markdown table (default, ~half the tokens) or JSON.
var formatParam = mcp.WithString("format", mcp.Description("'md' (default, markdown table — ~half the tokens) or 'json'"), mcp.Enum("md", "json"))

func whoamiTool() mcp.Tool {
	return mcp.NewTool(
		"whoami",
		mcp.WithDescription("Find your Claude Code session ID. Generate a salt (3+ random words), write it in your conversation, then call this with it — the relay greps ~/.claude transcripts for the salt. Use the returned session_id in register_agent."),
		mcp.WithString("salt", mcp.Description("Unique string of 3+ random words you just wrote in your conversation (e.g. 'purple-falcon-nebula')."), mcp.Required()),
	)
}

func registerAgentTool() mcp.Tool {
	return mcp.NewTool(
		"register_agent",
		mcp.WithDescription("Register an agent (once per agent at startup; re-registering updates it). Returns session_context: profile, tasks, unread messages, conversations.\n\nOn re-register, identity fields you OMIT (reports_to, profile_slug, is_executive, session_id) are PRESERVED, not cleared; role/description/interest_tags/max_context_bytes always update.\n\nis_executive=true auto-creates the 'leadership' admin team and adds the agent, enabling broadcast (send_message to='*'). Broadcast is open until the first team exists, then requires admin-team membership."),
		projectParam,
		mcp.WithString("name", mcp.Description("Unique agent name (e.g. 'backend'). Re-register same name to update. To rename: register new name, deactivate_agent the old."), mcp.Required()),
		mcp.WithString("role", mcp.Description("Role description (e.g. 'FastAPI backend developer')")),
		mcp.WithString("description", mcp.Description("What this agent is currently working on")),
		mcp.WithString("reports_to", mcp.Description("Manager agent name (org hierarchy)")),
		mcp.WithBoolean("is_executive", mcp.Description("Executive flag (crown in UI)")),
		mcp.WithString("profile_slug", mcp.Description("Profile archetype this agent runs")),
		mcp.WithString("session_id", mcp.Description("Claude Code session ID ($CLAUDE_SESSION_ID) for activity tracking")),
		mcp.WithString("cwd", mcp.Description("Worktree dir ($PWD). Stable identity key: lets a SessionStart hook re-bind the rotated session_id after /clear.")),
		mcp.WithString("interest_tags", mcp.Description("JSON array of tags for context budget filtering (e.g. '[\"database\",\"auth\"]')")),
		mcp.WithNumber("max_context_bytes", mcp.Description("Max bytes for budget-pruned inbox (default 16384)")),
	)
}

func sendMessageTool() mcp.Tool {
	return mcp.NewTool(
		"send_message",
		mcp.WithDescription("Send a message to an agent. to='*' broadcasts (requires admin team; executives have it). to='team:<slug>' messages a team. conversation_id targets a conversation instead."),
		asParam,
		projectParam,
		mcp.WithString("to", mcp.Description("Recipient agent name, or '*' for broadcast. Ignored when conversation_id is set."), mcp.Required()),
		mcp.WithString("type",
			mcp.Description("Message type"),
			mcp.Enum("question", "response", "notification", "code-snippet", "task", "user_question"),
		),
		mcp.WithString("subject", mcp.Description("Subject line"), mcp.Required()),
		mcp.WithString("content", mcp.Description("Message body"), mcp.Required()),
		mcp.WithString("reply_to", mcp.Description("Message ID to reply to (threading)")),
		mcp.WithString("metadata", mcp.Description("JSON string of additional metadata")),
		mcp.WithString("conversation_id", mcp.Description("Send to a conversation instead of a single agent")),
		mcp.WithString("priority",
			mcp.Description("P0=interrupt, P1=steering, P2=advisory (default), P3=info"),
			mcp.Enum("P0", "P1", "P2", "P3"),
		),
		mcp.WithNumber("ttl_seconds", mcp.Description("Seconds before expiry (default 14400 = 4h, 0 = never). Expired messages leave the inbox.")),
		mcp.WithString("target_project", mcp.Description("Cross-project DM: deliver to this agent name in target_project. Both sender and recipient must be is_executive. Message lives in the target project; metadata records the source.")),
	)
}

func getInboxTool() mcp.Tool {
	return mcp.NewTool(
		"get_inbox",
		mcp.WithDescription("Get an agent's inbox: messages sent to them or broadcast (excluding their own broadcasts)."),
		asParam,
		projectParam,
		mcp.WithBoolean("unread_only", mcp.Description("Only unread (default true)")),
		mcp.WithNumber("limit", mcp.Description("Max messages (default 10)")),
		mcp.WithBoolean("full_content", mcp.Description("Full content instead of 300-char truncation (default false)")),
		mcp.WithBoolean("apply_budget", mcp.Description("Prune by priority, tag relevance and freshness to fit the agent's max_context_bytes (default false)")),
		mcp.WithString("min_priority", mcp.Description("Minimum priority (e.g. 'P1' returns P0+P1)"), mcp.Enum("P0", "P1", "P2", "P3")),
		mcp.WithString("from", mcp.Description("Filter by sender")),
		mcp.WithString("since", mcp.Description("Only messages after this ISO timestamp")),
		mcp.WithBoolean("exclude_broadcasts", mcp.Description("Exclude broadcasts (default false)")),
		formatParam,
	)
}

func ackDeliveryTool() mcp.Tool {
	return mcp.NewTool(
		"ack_delivery",
		mcp.WithDescription("Acknowledge a message delivery (surfaced → acknowledged). Use the delivery_id from get_inbox."),
		mcp.WithString("delivery_id", mcp.Description("Delivery ID to acknowledge"), mcp.Required()),
	)
}

func getThreadTool() mcp.Tool {
	return mcp.NewTool(
		"get_thread",
		mcp.WithDescription("Get the message thread containing the given message (up to 200 messages). Content is preview-truncated by default; pass full_content=true for untruncated bodies."),
		projectParam,
		mcp.WithString("message_id", mcp.Description("Any message ID in the thread"), mcp.Required()),
		mcp.WithBoolean("full_content", mcp.Description("Full content instead of 300-char truncation (default false)")),
		formatParam,
	)
}

func listAgentsTool() mcp.Tool {
	return mcp.NewTool(
		"list_agents",
		mcp.WithDescription("List registered agents and their status."),
		projectParam,
		formatParam,
	)
}

func markReadTool() mcp.Tool {
	return mcp.NewTool(
		"mark_read",
		mcp.WithDescription("Mark messages as read."),
		asParam,
		projectParam,
		mcp.WithArray("message_ids",
			mcp.Description("Message IDs to mark as read"),
			mcp.WithStringItems(),
		),
		mcp.WithString("conversation_id", mcp.Description("Mark a whole conversation read (alternative to message_ids)")),
	)
}

func createConversationTool() mcp.Tool {
	return mcp.NewTool(
		"create_conversation",
		mcp.WithDescription("Create a multi-agent conversation. All members see messages sent to it."),
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
		mcp.WithDescription("Get a conversation's messages, chronological."),
		asParam,
		projectParam,
		mcp.WithString("conversation_id", mcp.Description("Conversation ID"), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Max messages (default 50)")),
		mcp.WithString("format", mcp.Description("'full' (default), 'compact' (metadata only), 'digest' (metadata + first 200 chars)")),
		mcp.WithBoolean("full_content", mcp.Description("With format 'full', return untruncated content (default true)")),
	)
}

func inviteToConversationTool() mcp.Tool {
	return mcp.NewTool(
		"invite_to_conversation",
		mcp.WithDescription("Add an agent to a conversation."),
		asParam,
		projectParam,
		mcp.WithString("conversation_id", mcp.Description("Conversation ID"), mcp.Required()),
		mcp.WithString("agent_name", mcp.Description("Agent to invite"), mcp.Required()),
	)
}

func leaveConversationTool() mcp.Tool {
	return mcp.NewTool(
		"leave_conversation",
		mcp.WithDescription("Leave a conversation."),
		asParam,
		projectParam,
		mcp.WithString("conversation_id", mcp.Description("Conversation ID"), mcp.Required()),
	)
}

func archiveConversationTool() mcp.Tool {
	return mcp.NewTool(
		"archive_conversation",
		mcp.WithDescription("Archive a conversation (removed from everyone's list)."),
		asParam,
		projectParam,
		mcp.WithString("conversation_id", mcp.Description("Conversation ID"), mcp.Required()),
	)
}

// --- Memory tools ---

func setMemoryTool() mcp.Tool {
	return mcp.NewTool(
		"set_memory",
		mcp.WithDescription("Store knowledge in persistent memory. Default upsert: overwrites existing value (old version archived). upsert=false enables conflict detection — both versions kept, resolve with resolve_conflict."),
		asParam,
		projectParam,
		mcp.WithString("key", mcp.Description("Memory key (e.g. 'auth-header-format')"), mcp.Required()),
		mcp.WithString("value", mcp.Description("The knowledge to store"), mcp.Required()),
		mcp.WithArray("tags", mcp.Description("Tags for search/filtering"), mcp.WithStringItems()),
		mcp.WithString("scope",
			mcp.Description("'agent' (private), 'project' (team-shared), 'global' (cross-project)"),
			mcp.Enum("agent", "project", "global"),
		),
		mcp.WithString("confidence",
			mcp.Description("How obtained"),
			mcp.Enum("stated", "inferred", "observed"),
		),
		mcp.WithString("layer",
			mcp.Description("'constraints' (hard rules), 'behavior' (adaptable defaults), 'context' (ephemeral)"),
			mcp.Enum("constraints", "behavior", "context"),
		),
		mcp.WithBoolean("upsert", mcp.Description("true (default): overwrite. false: flag a conflict if value differs.")),
	)
}

func rememberTool() mcp.Tool {
	return mcp.NewTool(
		"remember",
		mcp.WithDescription("Record a SETTLED decision (ADR-style) so the team stops re-debating it. Stored as a project decision; the accepted set is surfaced at session start. Enforces dedup-or-supersede: a near-identical decision in the same area is rejected unless you pass `supersedes`."),
		asParam,
		projectParam,
		mcp.WithString("decision", mcp.Description("The settled rule, one line (e.g. 'POST hook events to the relay; no file-drop watcher')"), mcp.Required()),
		mcp.WithString("rationale", mcp.Description("Why, one line")),
		mcp.WithString("area", mcp.Description("Component/area it governs (e.g. 'ingest/hooks') — groups the DEC key")),
		mcp.WithArray("tags", mcp.Description("Extra tags for search"), mcp.WithStringItems()),
		mcp.WithString("supersedes", mcp.Description("DEC id this replaces (archives the prior decision)")),
	)
}

func recallDecisionsTool() mcp.Tool {
	return mcp.NewTool(
		"recall_decisions",
		mcp.WithDescription("List the project's accepted decisions (the live, non-superseded set). Read before re-litigating a settled call."),
		asParam,
		projectParam,
	)
}

func getMemoryTool() mcp.Tool {
	return mcp.NewTool(
		"get_memory",
		mcp.WithDescription("Get a memory by key. Scope cascade: agent → project → global. On conflict, returns ALL values with provenance."),
		asParam,
		projectParam,
		mcp.WithString("key", mcp.Description("Memory key"), mcp.Required()),
		mcp.WithString("scope",
			mcp.Description("Specific scope (skips cascade)"),
			mcp.Enum("agent", "project", "global"),
		),
	)
}

func searchMemoryTool() mcp.Tool {
	return mcp.NewTool(
		"search_memory",
		mcp.WithDescription("Full-text search across memories. Ranked results with provenance and confidence. Cross-scope by default (respects agent privacy)."),
		asParam,
		projectParam,
		mcp.WithString("query", mcp.Description("Full-text search query"), mcp.Required()),
		mcp.WithArray("tags", mcp.Description("Filter by tags"), mcp.WithStringItems()),
		mcp.WithString("scope",
			mcp.Description("Limit to a scope"),
			mcp.Enum("agent", "project", "global"),
		),
		mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
	)
}

func listMemoriesTool() mcp.Tool {
	return mcp.NewTool(
		"list_memories",
		mcp.WithDescription("Browse memories with filtering. Shows key, truncated value, tags, provenance."),
		asParam,
		projectParam,
		mcp.WithString("scope",
			mcp.Description("Filter by scope"),
			mcp.Enum("agent", "project", "global"),
		),
		mcp.WithArray("tags", mcp.Description("Filter by tags"), mcp.WithStringItems()),
		mcp.WithString("agent", mcp.Description("Filter by author")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 50)")),
		formatParam,
	)
}

func deleteMemoryTool() mcp.Tool {
	return mcp.NewTool(
		"delete_memory",
		mcp.WithDescription("Soft-delete a memory (archived, never hard-deleted). Author or same scope only."),
		asParam,
		projectParam,
		mcp.WithString("key", mcp.Description("Memory key to archive"), mcp.Required()),
		mcp.WithString("scope",
			mcp.Description("Scope of the memory"),
			mcp.Enum("agent", "project", "global"),
		),
	)
}

func resolveConflictTool() mcp.Tool {
	return mcp.NewTool(
		"resolve_conflict",
		mcp.WithDescription("Resolve a memory conflict by choosing one value (existing or new). The rejected version is archived with resolution metadata."),
		asParam,
		projectParam,
		mcp.WithString("key", mcp.Description("Conflicted memory key"), mcp.Required()),
		mcp.WithString("chosen_value", mcp.Description("Value to keep"), mcp.Required()),
		mcp.WithString("scope",
			mcp.Description("Scope of the conflict"),
			mcp.Enum("agent", "project", "global"),
		),
	)
}

// --- Profile tools ---

func registerProfileTool() mcp.Tool {
	return mcp.NewTool(
		"register_profile",
		mcp.WithDescription("Create or update a profile — an identity card for an agent role (name, role, advertised skills). Discoverable via find_profiles."),
		projectParam,
		mcp.WithString("slug", mcp.Description("Unique profile identifier (e.g. 'backend')"), mcp.Required()),
		mcp.WithString("name", mcp.Description("Display name"), mcp.Required()),
		mcp.WithString("role", mcp.Description("Role description")),
		mcp.WithString("skills", mcp.Description("Skill objects, JSON string or array: [{\"id\":\"...\",\"name\":\"...\",\"tags\":[...]}]")),
	)
}

func getProfileTool() mcp.Tool {
	return mcp.NewTool(
		"get_profile",
		mcp.WithDescription("Get a profile archetype by slug — name, role, and skills."),
		projectParam,
		mcp.WithString("slug", mcp.Description("Profile slug"), mcp.Required()),
	)
}

func listProfilesTool() mcp.Tool {
	return mcp.NewTool(
		"list_profiles",
		mcp.WithDescription("List all profiles in a project."),
		projectParam,
	)
}

func findProfilesTool() mcp.Tool {
	return mcp.NewTool(
		"find_profiles",
		mcp.WithDescription("Find profiles whose skills match a tag."),
		projectParam,
		mcp.WithString("skill_tag", mcp.Description("Skill tag (e.g. 'database')"), mcp.Required()),
	)
}

// --- Task tools ---

func dispatchTaskTool() mcp.Tool {
	return mcp.NewTool(
		"dispatch_task",
		mcp.WithDescription("Dispatch a task to a profile (state 'pending', claimable by agents running it). profile='human' for tasks needing human action (auto-created on first use). Without board_id, assigned to the first board (a 'backlog' board is auto-created if none)."),
		asParam,
		projectParam,
		mcp.WithString("profile", mcp.Description("Profile slug to dispatch to"), mcp.Required()),
		mcp.WithString("title", mcp.Description("Task title"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Detailed description")),
		mcp.WithString("priority",
			mcp.Description("Task priority"),
			mcp.Enum("P0", "P1", "P2", "P3"),
		),
		mcp.WithString("parent_task_id", mcp.Description("Parent task ID (subtasks)")),
		mcp.WithString("board_id", mcp.Description("Board to assign to")),
	)
}

func claimTaskTool() mcp.Tool {
	return mcp.NewTool(
		"claim_task",
		mcp.WithDescription("Claim a pending task → 'accepted'."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
	)
}

func startTaskTool() mcp.Tool {
	return mcp.NewTool(
		"start_task",
		mcp.WithDescription("Start a task → 'in-progress'. Can skip 'accepted'."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
	)
}

func commentTool() mcp.Tool {
	return mcp.NewTool(
		"comment",
		mcp.WithDescription("Comment on a task. On a Linear-mirrored task the comment is posted to the Linear issue (Linear is the source of truth); otherwise it is saved as a local progress note."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithString("body", mcp.Description("Comment text"), mcp.Required()),
	)
}

func reviewTaskTool() mcp.Tool {
	return mcp.NewTool(
		"review_task",
		mcp.WithDescription("Mark a task as in-review → 'in-review' (the agent's 'PR up' signal). Stamps in_review_at. In Linear mode, also moves the issue to In Review and posts the optional comment."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithString("comment", mcp.Description("Optional note (PR link / result) posted to Linear on the In Review transition")),
	)
}

func completeTaskTool() mcp.Tool {
	return mcp.NewTool(
		"complete_task",
		mcp.WithDescription("Complete a task with a result → 'done'."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithString("result", mcp.Description("Task output/result")),
	)
}

func blockTaskTool() mcp.Tool {
	return mcp.NewTool(
		"block_task",
		mcp.WithDescription("Mark a task blocked with a reason. Notifies the dispatcher (and the parent task's dispatcher if any)."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithString("reason", mcp.Description("Why blocked")),
	)
}

func resumeTaskTool() mcp.Tool {
	return mcp.NewTool(
		"resume_task",
		mcp.WithDescription("Move a blocked task back to 'in-progress'. Fires task.resumed."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
	)
}

func cancelTaskTool() mcp.Tool {
	return mcp.NewTool(
		"cancel_task",
		mcp.WithDescription("Cancel a task from any state. Notifies the dispatcher."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithString("reason", mcp.Description("Why cancelled")),
	)
}

func getTaskTool() mcp.Tool {
	return mcp.NewTool(
		"get_task",
		mcp.WithDescription("Get full task details, optionally with subtask chain."),
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithBoolean("include_subtasks", mcp.Description("Include subtasks, max depth 3 (default false)")),
	)
}

func listTasksTool() mcp.Tool {
	return mcp.NewTool(
		"list_tasks",
		mcp.WithDescription("List tasks sorted by priority. status='active' = all non-done/cancelled."),
		asParam,
		projectParam,
		mcp.WithString("status",
			mcp.Description("Filter by status ('active' = all non-done/cancelled)"),
			mcp.Enum("pending", "accepted", "in-progress", "done", "blocked", "cancelled", "active"),
		),
		mcp.WithString("profile", mcp.Description("Filter by profile slug")),
		mcp.WithString("priority",
			mcp.Description("Filter by priority"),
			mcp.Enum("P0", "P1", "P2", "P3"),
		),
		mcp.WithString("assigned_to", mcp.Description("Filter by assignee")),
		mcp.WithString("board_id", mcp.Description("Filter by board")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 50)")),
		mcp.WithBoolean("include_archived", mcp.Description("Include archived (default false)")),
		formatParam,
	)
}

func batchCompleteTasksTool() mcp.Tool {
	return mcp.NewTool(
		"batch_complete_tasks",
		mcp.WithDescription("Complete multiple tasks at once."),
		asParam,
		projectParam,
		mcp.WithString("tasks", mcp.Description("JSON array: [{\"task_id\":\"...\",\"result\":\"...\"}]. result optional."), mcp.Required()),
	)
}

func batchDispatchTasksTool() mcp.Tool {
	return mcp.NewTool(
		"batch_dispatch_tasks",
		mcp.WithDescription("Dispatch multiple tasks at once."),
		asParam,
		projectParam,
		mcp.WithString("tasks", mcp.Description("JSON array: [{\"profile\":\"...\",\"title\":\"...\",\"description\":\"...\",\"priority\":\"P2\",\"board_id\":\"...\"}]. Only profile and title required."), mcp.Required()),
	)
}

func moveTaskTool() mcp.Tool {
	return mcp.NewTool(
		"move_task",
		mcp.WithDescription("Move a task to a different board."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithString("board_id", mcp.Description("New board ID (empty string unassigns)")),
	)
}

// --- Boards ---

func createBoardTool() mcp.Tool {
	return mcp.NewTool(
		"create_board",
		mcp.WithDescription("Create a task board. Returns the board with its ID."),
		asParam,
		projectParam,
		mcp.WithString("name", mcp.Description("Display name"), mcp.Required()),
		mcp.WithString("slug", mcp.Description("Slug, unique per project (e.g. 'sprint-1')"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Description")),
	)
}

func listBoardsTool() mcp.Tool {
	return mcp.NewTool(
		"list_boards",
		mcp.WithDescription("List task boards."),
		projectParam,
	)
}

func archiveBoardTool() mcp.Tool {
	return mcp.NewTool(
		"archive_board",
		mcp.WithDescription("Archive a board and all its tasks (hidden from listings, data preserved)."),
		asParam,
		projectParam,
		mcp.WithString("board_id", mcp.Description("Board ID"), mcp.Required()),
	)
}

func deleteBoardTool() mcp.Tool {
	return mcp.NewTool(
		"delete_board",
		mcp.WithDescription("Permanently delete a board (must be archived first). Tasks are NOT deleted."),
		asParam,
		projectParam,
		mcp.WithString("board_id", mcp.Description("Board ID (archived)"), mcp.Required()),
	)
}

func updateTaskTool() mcp.Tool {
	return mcp.NewTool(
		"update_task",
		mcp.WithDescription("Update task fields without changing status (assignee/claim/history preserved). progress_note appends a timestamped update shown in the web UI — use it to signal progress on long tasks."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithString("title", mcp.Description("New title")),
		mcp.WithString("description", mcp.Description("New description")),
		mcp.WithString("priority", mcp.Description("New priority"), mcp.Enum("P0", "P1", "P2", "P3")),
		mcp.WithString("board_id", mcp.Description("Move to board")),
		mcp.WithString("progress_note", mcp.Description("Short progress note (does not change status)")),
	)
}

func archiveTasksTool() mcp.Tool {
	return mcp.NewTool(
		"archive_tasks",
		mcp.WithDescription("Archive done/cancelled tasks (soft-delete, never hard-deleted). Keeps boards manageable."),
		asParam,
		projectParam,
		mcp.WithString("status", mcp.Description("'done', 'cancelled', or empty for both"), mcp.Enum("done", "cancelled", "")),
		mcp.WithString("board_id", mcp.Description("Only this board (empty = all)")),
	)
}

// --- File locks ---
// Dormant: the lock tools are disabled in toolset.go (v2 dropped advisory
// locks in favour of worktree isolation). Kept so they can be re-enabled by
// uncommenting their registrations. nolint:unused until then.

//nolint:unused
func claimFilesTool() mcp.Tool {
	return mcp.NewTool(
		"claim_files",
		mcp.WithDescription("Declare files you are editing. Broadcasts a steering message; other agents should avoid them."),
		asParam,
		projectParam,
		mcp.WithString("file_paths", mcp.Description("JSON array of file paths (e.g. '[\"src/auth.go\"]')"), mcp.Required()),
		mcp.WithNumber("ttl_seconds", mcp.Description("Claim duration (default 1800 = 30min)")),
	)
}

//nolint:unused
func releaseFilesTool() mcp.Tool {
	return mcp.NewTool(
		"release_files",
		mcp.WithDescription("Release claimed files. Broadcasts an info message."),
		asParam,
		projectParam,
		mcp.WithString("file_paths", mcp.Description("JSON array of paths (must match a previous claim)"), mcp.Required()),
	)
}

//nolint:unused
func listLocksTool() mcp.Tool {
	return mcp.NewTool(
		"list_locks",
		mcp.WithDescription("List active file locks: which agent holds which files."),
		projectParam,
	)
}

// --- Agent lifecycle ---

func deactivateAgentTool() mcp.Tool {
	return mcp.NewTool(
		"deactivate_agent",
		mcp.WithDescription("Deactivate an agent (gone from list_agents, no more messages). Reactivate via register_agent. For temporary pause use sleep_agent."),
		projectParam,
		mcp.WithString("name", mcp.Description("Agent name"), mcp.Required()),
	)
}

func deleteAgentTool() mcp.Tool {
	return mcp.NewTool(
		"delete_agent",
		mcp.WithDescription("Soft-delete an agent (hidden from UI, kept in DB). Restore via register_agent."),
		projectParam,
		mcp.WithString("name", mcp.Description("Agent name"), mcp.Required()),
	)
}

func sleepAgentTool() mcp.Tool {
	return mcp.NewTool(
		"sleep_agent",
		mcp.WithDescription("Put an agent to sleep (visible, status='sleeping', messages still queued). Wake via register_agent."),
		asParam,
		projectParam,
	)
}

// --- Project lifecycle ---

func deleteProjectTool() mcp.Tool {
	return mcp.NewTool(
		"delete_project",
		mcp.WithDescription("Permanently delete a project and ALL its data (agents, tasks, messages, memories, boards). Irreversible."),
		mcp.WithString("project", mcp.Description("Project name"), mcp.Required()),
	)
}

// --- Project onboarding ---

func createProjectTool() mcp.Tool {
	return mcp.NewTool(
		"create_project",
		mcp.WithDescription("Set up a new project — the FIRST tool to call. Creates the project and returns an onboarding plan you execute step by step as the setup agent: analyze the codebase, store knowledge, create the org, profiles, and board."),
		mcp.WithString("name", mcp.Description("Project name (lowercase, no spaces)"), mcp.Required()),
		mcp.WithString("description", mcp.Description("One-line description")),
		mcp.WithString("cwd", mcp.Description("Absolute path to the project root")),
		mcp.WithBoolean("interactive", mcp.Description("Wait for user approval at each phase (default false = auto)")),
	)
}

// --- Soul RAG ---

func queryContextTool() mcp.Tool {
	return mcp.NewTool(
		"query_context",
		mcp.WithDescription("Query context for a task: ranked memories + completed task results. Use at boot or before starting work."),
		asParam,
		projectParam,
		mcp.WithString("query", mcp.Description("What context do you need? (e.g. 'supabase migration patterns')"), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Max results (default 10)")),
	)
}

// --- Session context ---

func getSessionContextTool() mcp.Tool {
	return mcp.NewTool(
		"get_session_context",
		mcp.WithDescription("Everything an agent needs in one call: profile, pending tasks, unread messages, conversations, relevant memories. Use at boot instead of 5-8 separate calls."),
		asParam,
		projectParam,
		mcp.WithString("profile_slug", mcp.Description("Profile to load (default: auto-detected from registration)")),
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
		mcp.WithString("slug", mcp.Description("Unique slug"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Description")),
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
		mcp.WithDescription("Create a team. Teams control messaging permissions and group agents."),
		asParam,
		projectParam,
		mcp.WithString("name", mcp.Description("Team name"), mcp.Required()),
		mcp.WithString("slug", mcp.Description("Unique team slug within the project"), mcp.Required()),
		mcp.WithString("description", mcp.Description("Description")),
		mcp.WithString("type", mcp.Description("'regular' (default), 'admin' (unrestricted broadcast), 'bot'")),
		mcp.WithString("org_id", mcp.Description("Organization ID")),
		mcp.WithString("parent_team_id", mcp.Description("Parent team ID (nested hierarchy)")),
	)
}

func listTeamsTool() mcp.Tool {
	return mcp.NewTool(
		"list_teams",
		mcp.WithDescription("List teams with their members."),
		asParam,
		projectParam,
	)
}

func addTeamMemberTool() mcp.Tool {
	return mcp.NewTool(
		"add_team_member",
		mcp.WithDescription("Add an agent to a team."),
		asParam,
		projectParam,
		mcp.WithString("team", mcp.Description("Team slug"), mcp.Required()),
		mcp.WithString("agent_name", mcp.Description("Agent to add"), mcp.Required()),
		mcp.WithString("role", mcp.Description("'admin', 'lead', 'member' (default), 'observer'")),
	)
}

func removeTeamMemberTool() mcp.Tool {
	return mcp.NewTool(
		"remove_team_member",
		mcp.WithDescription("Remove an agent from a team (soft remove)."),
		asParam,
		projectParam,
		mcp.WithString("team", mcp.Description("Team slug"), mcp.Required()),
		mcp.WithString("agent_name", mcp.Description("Agent to remove"), mcp.Required()),
	)
}

func getTeamInboxTool() mcp.Tool {
	return mcp.NewTool(
		"get_team_inbox",
		mcp.WithDescription("Get messages sent to a team (to='team:slug'). Content is preview-truncated by default; pass full_content=true for untruncated bodies."),
		asParam,
		projectParam,
		mcp.WithString("team", mcp.Description("Team slug"), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Max messages (default 50)")),
		mcp.WithBoolean("full_content", mcp.Description("Full content instead of 300-char truncation (default false)")),
		formatParam,
	)
}

func addNotifyChannelTool() mcp.Tool {
	return mcp.NewTool(
		"add_notify_channel",
		mcp.WithDescription("Allow this agent to message the target agent outside team boundaries."),
		asParam,
		projectParam,
		mcp.WithString("target", mcp.Description("Target agent name"), mcp.Required()),
	)
}
