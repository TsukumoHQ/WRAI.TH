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

// sessionContextParam selects the boot-payload shape (WRAITH R1): 'full'
// (default) or 'minimal' (agent + tasks + unread only, <1.5 KB). Shared by
// register_agent and get_session_context.
var sessionContextParam = mcp.WithString("session_context", mcp.Description("'full' (default) or 'minimal' (lean boot payload)"), mcp.Enum("full", "minimal"))

func whoamiTool() mcp.Tool {
	return mcp.NewTool(
		"whoami",
		mcp.WithDescription("Find your Claude Code session ID. Write a salt (3+ random words) in your conversation, then call this with it — the relay greps ~/.claude transcripts for the salt, returns session_id for register_agent."),
		mcp.WithString("salt", mcp.Description("Unique string of 3+ random words you just wrote in your conversation (e.g. 'purple-falcon-nebula')."), mcp.Required()),
	)
}

func registerAgentTool() mcp.Tool {
	return mcp.NewTool(
		"register_agent",
		mcp.WithDescription("Register an agent (re-registering updates it). Returns session_context (profile, tasks, unread, conversations). On re-register, OMITTED identity fields (reports_to, profile_slug, is_executive, session_id) are PRESERVED. is_executive=true auto-creates the 'leadership' admin team for broadcast (to='*')."),
		projectParam,
		mcp.WithString("name", mcp.Description("Unique agent name (e.g. 'backend'). Re-register same name to update. To rename: register new name, deactivate_agent the old."), mcp.Required()),
		mcp.WithString("role", mcp.Description("Role (e.g. 'FastAPI backend developer')")),
		mcp.WithString("description", mcp.Description("What this agent is currently working on")),
		mcp.WithString("reports_to", mcp.Description("Manager agent name (org hierarchy)")),
		mcp.WithBoolean("is_executive", mcp.Description("Executive flag (crown in UI)")),
		mcp.WithBoolean("is_service", mcp.Description("Service/daemon identity (monitoring/QA): always eligible to send, exempt from the liveness gate. Preserved when omitted.")),
		mcp.WithString("profile_slug", mcp.Description("Profile archetype this agent runs")),
		mcp.WithString("session_id", mcp.Description("Claude Code session ID ($CLAUDE_SESSION_ID) for activity tracking")),
		mcp.WithString("cwd", mcp.Description("Worktree dir ($PWD). Stable identity key: a SessionStart hook re-binds the rotated session_id after /clear. Agents can share one cwd — each resolves by name; a rebind with NO name refuses to guess.")),
		mcp.WithString("interest_tags", mcp.Description("JSON array of tags for context budget filtering (e.g. '[\"database\",\"auth\"]')")),
		mcp.WithNumber("max_context_bytes", mcp.Description("Max bytes for budget-pruned inbox (default 16384)")),
		sessionContextParam,
	)
}

func sendMessageTool() mcp.Tool {
	return mcp.NewTool(
		"send_message",
		mcp.WithDescription("Send a message to an agent. to='*' broadcasts (requires admin team). to='team:<slug>' messages a team. conversation_id targets a conversation instead."),
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
		mcp.WithNumber("ttl_seconds", mcp.Description("Seconds before expiry (default 14400 = 4h, 0 = never).")),
		mcp.WithString("target_project", mcp.Description("Cross-project DM to this agent in target_project. Both sender and recipient must be is_executive. Message lives in target_project; metadata records the source.")),
		mcp.WithString("action_required",
			mcp.Description("Recipient action? 'none' = no-wake (stays in inbox), for FYIs/receipts/status. 'ask'/'do'/'decide' may wake per priority. Omit to derive from type. A question/blocker is never suppressed by 'none'."),
			mcp.Enum("ask", "do", "decide", "none"),
		),
		mcp.WithString("idempotency_key", mcp.Description("Optional retry key; same key returns the original message, no duplicate.")),
	)
}

func sendStatusTool() mcp.Tool {
	return mcp.NewTool(
		"send_status",
		mcp.WithDescription("Post a typed status (done/doing/blockers + note). No-wake: surfaced, never wakes. A blocker here is not an escalation — send a question to escalate."),
		asParam,
		projectParam,
		mcp.WithString("to", mcp.Description("Agent name, '*', or a conversation_id."), mcp.Required()),
		mcp.WithArray("done", mcp.Description("Completed items (up to 20)."), mcp.WithStringItems()),
		mcp.WithArray("doing", mcp.Description("In-progress items."), mcp.WithStringItems()),
		mcp.WithArray("blockers", mcp.Description("Blockers — surfaced, not woken."), mcp.WithStringItems()),
		mcp.WithString("note", mcp.Description("One short note.")),
		mcp.WithString("conversation_id", mcp.Description("Post to a conversation.")),
		mcp.WithString("priority", mcp.Description("P2 default; never wakes except P0."), mcp.Enum("P0", "P1", "P2", "P3")),
		mcp.WithNumber("ttl_seconds", mcp.Description("Expiry seconds (default 14400, 0=never).")),
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
		mcp.WithBoolean("full_content", mcp.Description("Full content, not 300-char truncated (default false)")),
		mcp.WithBoolean("apply_budget", mcp.Description("Prune by priority, tag relevance and freshness to fit max_context_bytes (default false)")),
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

func deliveryStatusTool() mcp.Tool {
	return mcp.NewTool(
		"delivery_status",
		mcp.WithDescription("Read a message's delivery/ack state (queued|surfaced|acknowledged|expired). Pass message_id OR agent. Read-only."),
		asParam,
		projectParam,
		mcp.WithString("message_id", mcp.Description("All deliveries of this message")),
		mcp.WithString("agent", mcp.Description("This recipient's deliveries")),
		mcp.WithNumber("limit", mcp.Description("Max rows (default 50)")),
	)
}

func deadletterTool() mcp.Tool {
	return mcp.NewTool(
		"deadletter",
		mcp.WithDescription("List messages that TTL-expired while still unread (so a P0/P1 never vanishes silently). Read-only; defaults to caller."),
		asParam,
		projectParam,
		mcp.WithString("agent", mcp.Description("Recipient (default caller)")),
		mcp.WithNumber("limit", mcp.Description("Max rows (default 50)")),
	)
}

func getThreadTool() mcp.Tool {
	return mcp.NewTool(
		"get_thread",
		mcp.WithDescription("Get the message thread containing the given message (up to 200 messages). Content is preview-truncated by default; pass full_content=true for untruncated bodies."),
		projectParam,
		mcp.WithString("message_id", mcp.Description("Any message ID in the thread"), mcp.Required()),
		mcp.WithBoolean("full_content", mcp.Description("Full content, not 300-char truncated (default false)")),
		formatParam,
	)
}

func getMessageTool() mcp.Tool {
	return mcp.NewTool(
		"get_message",
		mcp.WithDescription("Fetch a single message by ID with its FULL, untruncated content — the escape hatch when a preview in get_inbox / get_thread / get_session_context cut off the body. Accepts a full ID or an unambiguous ID prefix."),
		asParam,
		projectParam,
		mcp.WithString("id", mcp.Description("Message ID (full or an unambiguous prefix)"), mcp.Required()),
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

func isEligibleTool() mcp.Tool {
	return mcp.NewTool(
		"is_eligible",
		mcp.WithDescription("Read-only sender-eligibility check: {eligible, reason} for an agent without sending — the verdict send_message would give, so a client parks instead of hot-looping a failing send. Service identities always eligible."),
		asParam,
		projectParam,
		mcp.WithString("agent", mcp.Description("Agent name to check (default: yourself, i.e. the `as` identity)")),
	)
}

func identityCheckTool() mcp.Tool {
	return mcp.NewTool(
		"identity_check",
		mcp.WithDescription("Read-only: is a name wake-resolvable, or an unbound ghost that drops wakes? conflicting_agents lists other agents on this cwd — informational (normal for a co-located team), not a failure. Returns {registered, ghost, bound_uniquely, conflicting_agents, reason}."),
		asParam,
		projectParam,
		mcp.WithString("agent", mcp.Description("Name to check (default caller)")),
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
		mcp.WithDescription("Store knowledge in persistent memory. Default upsert overwrites (old version archived). upsert=false enables conflict detection — both versions kept, resolve with resolve_conflict."),
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
		mcp.WithString("valid_until", mcp.Description("Optional ISO-8601 UTC expiry; past it reads 'stale' (hidden unless include_stale).")),
	)
}

func rememberTool() mcp.Tool {
	return mcp.NewTool(
		"remember",
		mcp.WithDescription("Record a SETTLED decision (ADR-style), surfaced at session start. Dedup-or-supersede: a near-identical decision in the same area is rejected unless you pass `supersedes`."),
		asParam,
		projectParam,
		mcp.WithString("decision", mcp.Description("The settled rule, one line"), mcp.Required()),
		mcp.WithString("rationale", mcp.Description("Why, one line")),
		mcp.WithString("area", mcp.Description("Area it governs (e.g. 'ingest/hooks') — groups the DEC key")),
		mcp.WithArray("tags", mcp.Description("Extra tags for search"), mcp.WithStringItems()),
		mcp.WithString("supersedes", mcp.Description("DEC id this replaces (archives it)")),
		mcp.WithArray("depends_on", mcp.Description("DEC ids this rests on (graph edges)"), mcp.WithStringItems()),
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
		mcp.WithBoolean("include_stale", mcp.Description("Also return expired (stale) memories (default false).")),
		mcp.WithString("rank", mcp.Description("'mempalace' re-ranks by relevance+recency+importance; default is bm25 order.")),
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
		mcp.WithBoolean("include_stale", mcp.Description("Also list expired (stale) memories (default false).")),
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
		mcp.WithString("reason", mcp.Description("Tombstone 'why' (with who/when). Default 'deleted'.")),
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
		mcp.WithDescription("Dispatch a task to a profile (state 'pending', claimable by agents running it). profile='human' = human-action tasks. No board_id: auto-assigned on 0/1 boards ('backlog' if 0); refused (lists boards) if >1."),
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
		mcp.WithString("goal", mcp.Description("Typed ticket: one-line intent. Required where typed tickets are enforced, else optional.")),
		mcp.WithString("acceptance_criteria", mcp.Description("Typed ticket: JSON array of testable items, e.g. '[\"builds green\"]'. Review gate verdicts per item. Required where enforced.")),
		mcp.WithString("dod", mcp.Description("Typed ticket: definition of done. Required where enforced.")),
		mcp.WithString("verify_cmd", mcp.Description("Optional command the gate reviewer runs to validate this ticket; never required, even where typed tickets are enforced.")),
		mcp.WithBoolean("backlog", mcp.Description("Create in non-claimable 'backlog' (groomed; promote_task lifts it to pending). Default false.")),
		mcp.WithString("trace_id", mcp.Description("32-hex correlation id; auto-minted if omitted.")),
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

func promoteTaskTool() mcp.Tool {
	return mcp.NewTool(
		"promote_task",
		mcp.WithDescription("Promote a 'backlog' task → 'pending' (claimable), announced like a dispatch."),
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
		mcp.WithString("git_branch", mcp.Description("Branch the work sits on (review-gate git zone; stored opaquely)")),
		mcp.WithString("git_worktree", mcp.Description("Absolute path of the worktree holding the work")),
		mcp.WithString("git_target", mcp.Description("Branch the work should merge into")),
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

func reclaimTaskTool() mcp.Tool {
	return mcp.NewTool(
		"reclaim_task",
		mcp.WithDescription("Take over a DEAD holder's task (supervisor re-claim). Succeeds only when the lease expired OR the holder is deregistered/inactive; a live holder refuses (TASK_LEASE_HELD). On success the task moves to 'accepted' under the caller with a fresh lease."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
	)
}

func linkPrTool() mcp.Tool {
	return mcp.NewTool(
		"link_pr",
		mcp.WithDescription("Link a GitHub PR to a task (Linear-style): stores pr_url/pr_number/pr_repo/pr_state so its lifecycle syncs from GitHub. Additive + idempotent — omitted fields keep their value."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithNumber("pr_number", mcp.Description("GitHub PR number (the resolver key)")),
		mcp.WithString("pr_url", mcp.Description("PR html_url")),
		mcp.WithString("pr_repo", mcp.Description("Repo as owner/name (a task's PR may live in any repo)")),
		mcp.WithString("pr_state", mcp.Description("Last observed PR state: open | merged | closed")),
	)
}

func reconcilePrTool() mcp.Tool {
	return mcp.NewTool(
		"reconcile_pr",
		mcp.WithDescription("Poll-side PR convergence (write-back for relay://pr-reconcile): a gh-owning poller passes the observed pr_state to converge the task via the webhook's one-way map (open→in-review, merged→done, closed-unmerged→blocked), no-resurrect + idempotent."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID (a pr-reconcile candidate)"), mcp.Required()),
		mcp.WithString("pr_state", mcp.Description("Observed live PR state: open | merged | closed (closed = closed-unmerged)"), mcp.Required()),
		mcp.WithString("pr_url", mcp.Description("PR html_url (optional refresh)")),
		mcp.WithString("pr_repo", mcp.Description("Repo owner/name (optional refresh)")),
	)
}

func setRunTool() mcp.Tool {
	return mcp.NewTool(
		"set_run",
		mcp.WithDescription("Stamp the run zone on a PARENT task (changeset-per-run): integration_branch and/or a run_state advance (open→gating→merging→merged | blocked | amputated). The task is a container (groups slices, not claimable). Transition-enforced, idempotent."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Parent task ID (the run)"), mcp.Required()),
		mcp.WithString("integration_branch", mcp.Description("The run's shared integration branch (off the real target)")),
		mcp.WithString("run_state", mcp.Description("Advance the run lifecycle: open | gating | merging | merged | blocked | amputated")),
	)
}

func getRunTool() mcp.Tool {
	return mcp.NewTool(
		"get_run",
		mcp.WithDescription("Get a run: the PARENT task (with its run zone integration_branch/run_state) plus its subtask chain — the agent slices. The read for the changeset review surface."),
		projectParam,
		mcp.WithString("run_id", mcp.Description("Run (parent task) ID"), mcp.Required()),
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
			mcp.Description("Filter by status"),
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
		mcp.WithDescription("Dispatch multiple tasks at once. On projects that enforce typed tickets (e.g. niwa) each item must carry goal + acceptance_criteria + dod; an item missing any is skipped and reported in errors while the rest dispatch."),
		asParam,
		projectParam,
		mcp.WithString("tasks", mcp.Description("JSON array: [{\"profile\":\"...\",\"title\":\"...\",\"description\":\"...\",\"priority\":\"P2\",\"board_id\":\"...\",\"goal\":\"...\",\"acceptance_criteria\":[\"item1\",\"item2\"],\"dod\":\"...\",\"verify_cmd\":\"...\"}]. profile and title always required; goal/acceptance_criteria/dod required only where typed tickets are enforced; verify_cmd always optional."), mcp.Required()),
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
		mcp.WithDescription("Update task fields (not status). progress_note appends a timestamped note. goal/acceptance_criteria/dod/verify_cmd: dispatcher-only, audited. Reassign: assigned_to transfers a claimed lease + notifies the old doer; profile_slug alone on a claimed task refused. Unknown fields refused, not ignored."),
		asParam,
		projectParam,
		mcp.WithString("task_id", mcp.Description("Task ID"), mcp.Required()),
		mcp.WithString("title", mcp.Description("New title")),
		mcp.WithString("description", mcp.Description("New description")),
		mcp.WithString("priority", mcp.Description("New priority"), mcp.Enum("P0", "P1", "P2", "P3")),
		mcp.WithString("board_id", mcp.Description("Move to board")),
		mcp.WithString("assigned_to", mcp.Description("Reassign to this agent")),
		mcp.WithString("profile_slug", mcp.Description("Reassign to this profile")),
		mcp.WithString("progress_note", mcp.Description("Short progress note (does not change status)")),
		mcp.WithString("goal", mcp.Description("Intent")),
		mcp.WithString("acceptance_criteria", mcp.Description("JSON array")),
		mcp.WithString("dod", mcp.Description("Done bar")),
		mcp.WithString("verify_cmd", mcp.Description("Optional gate-reviewer validate command")),
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

func archiveProjectTool() mcp.Tool {
	return mcp.NewTool(
		"archive_project",
		mcp.WithDescription("Hide a project from listings (reversible, deletes nothing). Refused for Linear-backed projects."),
		mcp.WithString("project", mcp.Description("Project name"), mcp.Required()),
	)
}

func unarchiveProjectTool() mcp.Tool {
	return mcp.NewTool(
		"unarchive_project",
		mcp.WithDescription("Restore an archived project (zero data loss)."),
		mcp.WithString("project", mcp.Description("Project name"), mcp.Required()),
	)
}

// --- Project onboarding ---

func createProjectTool() mcp.Tool {
	return mcp.NewTool(
		"create_project",
		mcp.WithDescription("Set up a new project — the FIRST tool to call. Creates the project and returns an onboarding plan you execute as the setup agent: analyze the codebase, store knowledge, create the org, profiles, and board."),
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
		sessionContextParam,
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

func deleteTeamTool() mcp.Tool {
	return mcp.NewTool(
		"delete_team",
		mcp.WithDescription("Retire a team: removes the team, its memberships, and its inbox refs. To deprecate a channel, leave it memberless — its slug stays addressable but a send reaches nobody. Delivered messages untouched."),
		asParam,
		projectParam,
		mcp.WithString("team", mcp.Description("Team slug"), mcp.Required()),
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
		mcp.WithBoolean("full_content", mcp.Description("Full content, not 300-char truncated (default false)")),
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
