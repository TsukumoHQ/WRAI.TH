package relay

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// --- Test helpers ---

func testHandlers(t *testing.T) *Handlers {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.NewTestDB(dbPath)
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mcpSrv := server.NewMCPServer("test", "0.0.0")
	registry := NewSessionRegistry(mcpSrv)
	events := NewEventBus()
	return NewHandlers(database, registry, nil, events)
}

func call(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: args},
	}
}

func parseJSON(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	if result.IsError {
		tc := result.Content[0].(mcp.TextContent)
		t.Fatalf("unexpected error result: %s", tc.Text)
	}
	tc := result.Content[0].(mcp.TextContent)
	var data map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &data); err != nil {
		t.Fatalf("parse result json: %v\nraw: %s", err, tc.Text)
	}
	return data
}

func expectError(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if !result.IsError {
		t.Fatal("expected error result, got success")
	}
	return result.Content[0].(mcp.TextContent).Text
}

var ctx = context.Background()

// --- Agent Tests ---

func TestRegisterAgent(t *testing.T) {
	h := testHandlers(t)

	res, err := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj",
		"name":    "bot-a",
		"role":    "developer",
	}))
	if err != nil {
		t.Fatal(err)
	}
	data := parseJSON(t, res)
	agent := data["agent"].(map[string]any)
	if agent["name"] != "bot-a" {
		t.Errorf("expected bot-a, got %v", agent["name"])
	}
	if agent["role"] != "developer" {
		t.Errorf("expected developer, got %v", agent["role"])
	}
	sessionCtx := data["session_context"].(map[string]any)
	if sessionCtx["is_respawn"] != false {
		t.Error("expected is_respawn=false for new agent")
	}
}

func TestRegisterAgentRespawn(t *testing.T) {
	h := testHandlers(t)

	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj",
		"name":    "bot-a",
		"role":    "developer",
	}))

	res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj",
		"name":    "bot-a",
		"role":    "updated-role",
	}))
	data := parseJSON(t, res)
	sessionCtx := data["session_context"].(map[string]any)
	if sessionCtx["is_respawn"] != true {
		t.Error("expected is_respawn=true for respawn")
	}
}

// TestRegisterAgentRespawnPreservesProfileSlug reproduces the production bug end-to-end:
// the orchestrator registers an agent WITH profile_slug, then the agent's own in-pane
// /relay register (per skill/relay.md — never passes profile_slug) re-registers without it.
// The slug must survive so list_tasks / session_context can route the agent's tasks.
func TestRegisterAgentRespawnPreservesProfileSlug(t *testing.T) {
	h := testHandlers(t)

	// Orchestrator registers WITH profile_slug.
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{
		"project":      "test-proj",
		"name":         "bot-a",
		"role":         "developer",
		"profile_slug": "backend",
	}))

	// Agent self-registers WITHOUT profile_slug (key omitted entirely).
	res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj",
		"name":    "bot-a",
		"role":    "developer",
	}))
	data := parseJSON(t, res)
	agent := data["agent"].(map[string]any)
	if agent["profile_slug"] != "backend" {
		t.Fatalf("profile_slug must survive omitted re-register, got %v", agent["profile_slug"])
	}
}

// TestRegisterAgentRespawnPreservesReportsTo ensures org hierarchy survives a respawn
// that omits reports_to.
func TestRegisterAgentRespawnPreservesReportsTo(t *testing.T) {
	h := testHandlers(t)

	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj", "name": "lead", "role": "lead",
	}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj", "name": "bot-a", "role": "dev", "reports_to": "lead",
	}))

	res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj", "name": "bot-a", "role": "dev",
	}))
	data := parseJSON(t, res)
	agent := data["agent"].(map[string]any)
	if agent["reports_to"] != "lead" {
		t.Fatalf("reports_to must survive omitted re-register, got %v", agent["reports_to"])
	}
}

// TestRegisterAgentRespawnPreservesIsExecutive ensures the executive flag (and its
// leadership-team membership) survives a respawn that omits is_executive.
func TestRegisterAgentRespawnPreservesIsExecutive(t *testing.T) {
	h := testHandlers(t)

	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj", "name": "cto", "role": "lead", "is_executive": true,
	}))

	res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj", "name": "cto", "role": "lead",
	}))
	data := parseJSON(t, res)
	agent := data["agent"].(map[string]any)
	if agent["is_executive"] != true {
		t.Fatalf("is_executive must survive omitted re-register, got %v", agent["is_executive"])
	}
}

// TestRegisterAgentExplicitSlugStillUpdates ensures preserve-on-omit does not block a
// legitimate slug change when profile_slug IS provided on re-register.
func TestRegisterAgentExplicitSlugStillUpdates(t *testing.T) {
	h := testHandlers(t)

	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj", "name": "bot-a", "role": "dev", "profile_slug": "backend",
	}))
	res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj", "name": "bot-a", "role": "dev", "profile_slug": "frontend",
	}))
	data := parseJSON(t, res)
	agent := data["agent"].(map[string]any)
	if agent["profile_slug"] != "frontend" {
		t.Fatalf("explicit profile_slug must update, got %v", agent["profile_slug"])
	}
}

func TestRegisterAgentMissingName(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "test-proj",
	}))
	expectError(t, res)
}

func TestListAgents(t *testing.T) {
	h := testHandlers(t)

	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "qa"}))

	res, _ := h.HandleListAgents(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	data := parseJSON(t, res)
	if data["count"].(float64) != 2 {
		t.Errorf("expected 2 agents, got %v", data["count"])
	}
}

func TestDeactivateAgent(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	res, _ := h.HandleDeactivateAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	data := parseJSON(t, res)
	if data["deactivated"] != true {
		t.Error("expected deactivated=true")
	}

	// Agent should not appear in list (inactive excluded from default list)
	listRes, _ := h.HandleListAgents(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	listData := parseJSON(t, listRes)
	// inactive agents ARE shown in list (status IN active, sleeping, inactive)
	agents := listData["agents"].([]any)
	if len(agents) != 1 {
		t.Errorf("expected 1 agent (inactive shown), got %d", len(agents))
	}
}

func TestDeleteAgent(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	res, _ := h.HandleDeleteAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	data := parseJSON(t, res)
	if data["deleted"] != true {
		t.Error("expected deleted=true")
	}

	// Deleted agents should NOT appear in list
	listRes, _ := h.HandleListAgents(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	listData := parseJSON(t, listRes)
	if listData["count"].(float64) != 0 {
		t.Errorf("expected 0 agents after delete, got %v", listData["count"])
	}
}

func TestSleepAgent(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	res, _ := h.HandleSleepAgent(ctx, call(map[string]any{"project": "p1", "as": "bot-a"}))
	data := parseJSON(t, res)
	if data["status"] != "sleeping" {
		t.Errorf("expected status=sleeping, got %v", data["status"])
	}
}

// --- Message Tests ---

func TestSendAndGetInbox(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "qa"}))

	// Send message
	sendRes, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1",
		"as":      "bot-a",
		"to":      "bot-b",
		"type":    "notification",
		"subject": "test",
		"content": "hello bot-b",
	}))
	msg := parseJSON(t, sendRes)
	if msg["to"] != "bot-b" {
		t.Errorf("expected to=bot-b, got %v", msg["to"])
	}
	if msg["id"] == nil {
		t.Errorf("expected message id in response")
	}

	// Check inbox
	inboxRes, _ := h.HandleGetInbox(ctx, call(map[string]any{"format": "json",
		"project":     "p1",
		"as":          "bot-b",
		"unread_only": true,
	}))
	inbox := parseJSON(t, inboxRes)
	if inbox["count"].(float64) != 1 {
		t.Errorf("expected 1 message in inbox, got %v", inbox["count"])
	}
}

func TestSendMessageMissingContent(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1",
		"as":      "bot-a",
		"to":      "bot-b",
	}))
	expectError(t, res)
}

func TestSendMessageMissingTo(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1",
		"as":      "bot-a",
		"content": "hello",
	}))
	expectError(t, res)
}

// TestDMReplyPathGrant is the TSU-75 guarantee: receiving a DM opens a scoped
// reply-path back to the sender, so an agent can always answer a message even
// across the team auth-wall — and the grant does NOT open it to anyone else.
func TestDMReplyPathGrant(t *testing.T) {
	h := testHandlers(t)
	// alice = executive → admin team (can message anyone; also makes hasTeams true
	// so the permission check is enforced). bob/carol are plain agents.
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "alice", "is_executive": true}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bob"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "carol"}))

	send := func(from, to string) *mcp.CallToolResult {
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{
			"project": "p1", "as": from, "to": to, "content": "hi",
		}))
		return res
	}

	// Auth-wall: bob cannot DM alice before she opens a channel.
	if res := send("bob", "alice"); !res.IsError {
		t.Fatal("expected bob→alice blocked before any contact")
	}
	// alice (admin) DMs bob — opens a scoped reply-path.
	if res := send("alice", "bob"); res.IsError {
		t.Fatalf("alice→bob should be allowed (admin): %s", expectError(t, res))
	}
	// bob can now reply to alice.
	if res := send("bob", "alice"); res.IsError {
		t.Fatalf("bob→alice should be allowed via reply-path: %s", expectError(t, res))
	}
	// Scoped: bob still cannot DM carol (carol never messaged bob).
	if res := send("bob", "carol"); !res.IsError {
		t.Fatal("expected bob→carol still blocked — reply-path must be scoped to the sender")
	}
}

func TestMarkRead(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "qa"}))

	sendRes, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "hello",
	}))
	msg := parseJSON(t, sendRes)
	msgID := msg["id"].(string)

	// Mark read
	readRes, _ := h.HandleMarkRead(ctx, call(map[string]any{
		"project":     "p1",
		"as":          "bot-b",
		"message_ids": []any{msgID},
	}))
	readData := parseJSON(t, readRes)
	if readData["marked_read"].(float64) != 1 {
		t.Errorf("expected 1 marked read, got %v", readData["marked_read"])
	}

	// Inbox should be empty (unread_only)
	inboxRes, _ := h.HandleGetInbox(ctx, call(map[string]any{"format": "json",
		"project": "p1", "as": "bot-b", "unread_only": true,
	}))
	inbox := parseJSON(t, inboxRes)
	if inbox["count"].(float64) != 0 {
		t.Errorf("expected 0 unread after mark read, got %v", inbox["count"])
	}
}

// TestInboxNonDestructivePeek is the core TSU-73 guarantee: fetching the inbox
// must NOT consume messages. Two consecutive unread peeks both return the
// message; only an explicit mark_read clears it. Previously the fetch auto-
// surfaced (queued→surfaced) and unread filtered queued-only, so the message
// dropped out of the unread view after a single read → silent loss.
func TestInboxNonDestructivePeek(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b"}))
	_, _ = h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "hello",
	}))

	unreadCount := func() float64 {
		res, _ := h.HandleGetInbox(ctx, call(map[string]any{
			"format": "json", "project": "p1", "as": "bot-b", "unread_only": true,
		}))
		return parseJSON(t, res)["count"].(float64)
	}

	if c := unreadCount(); c != 1 {
		t.Fatalf("first peek: expected 1 unread, got %v", c)
	}
	if c := unreadCount(); c != 1 {
		t.Fatalf("second peek: message vanished after first fetch (read-on-fetch loss), got %v unread", c)
	}

	// Grab the id, then explicit mark_read must clear it from unread.
	res, _ := h.HandleGetInbox(ctx, call(map[string]any{
		"format": "json", "project": "p1", "as": "bot-b", "unread_only": true,
	}))
	id := parseJSON(t, res)["messages"].([]any)[0].(map[string]any)["id"].(string)
	_, _ = h.HandleMarkRead(ctx, call(map[string]any{
		"project": "p1", "as": "bot-b", "message_ids": []any{id},
	}))
	if c := unreadCount(); c != 0 {
		t.Fatalf("after mark_read: expected 0 unread, got %v", c)
	}
}

// TestInboxTruncationP0AndFirstRead covers the TSU-73 truncation safety nets:
// a long message is delivered in full on its FIRST surfacing (then preview-
// truncated on later peeks), and a P0 is NEVER truncated.
func TestInboxTruncationP0AndFirstRead(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b"}))
	long := strings.Repeat("x", 500)

	contentByPriority := func(prio string) string {
		res, _ := h.HandleGetInbox(ctx, call(map[string]any{
			"format": "json", "project": "p1", "as": "bot-b", "unread_only": true,
		}))
		for _, mm := range parseJSON(t, res)["messages"].([]any) {
			m := mm.(map[string]any)
			if m["priority"] == prio {
				return m["content"].(string)
			}
		}
		return ""
	}

	// Long P2: full on first surfacing, truncated (300 + "...") afterwards.
	_, _ = h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "priority": "P2", "content": long,
	}))
	if c := contentByPriority("P2"); len(c) != 500 {
		t.Fatalf("first read of long P2 should be full (500), got %d", len(c))
	}
	if c := contentByPriority("P2"); len(c) != 303 {
		t.Fatalf("second read of long P2 should be truncated to 303, got %d", len(c))
	}

	// Long P0: never truncated, on first read or any later peek.
	_, _ = h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "priority": "P0", "content": long,
	}))
	if c := contentByPriority("P0"); len(c) != 500 {
		t.Fatalf("P0 first read should be full (500), got %d", len(c))
	}
	if c := contentByPriority("P0"); len(c) != 500 {
		t.Fatalf("P0 must NEVER truncate, even after surfacing, got %d", len(c))
	}
}

func TestMarkReadMissingIDs(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleMarkRead(ctx, call(map[string]any{
		"project": "p1", "as": "bot-b",
	}))
	expectError(t, res)
}

func TestGetThread(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "qa"}))

	// Send original
	res1, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "original",
	}))
	msg1 := parseJSON(t, res1)
	msg1ID := msg1["id"].(string)

	// Reply
	_, _ = h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-b", "to": "bot-a", "content": "reply", "reply_to": msg1ID,
	}))

	// Get thread (json format — default is now a markdown table)
	threadRes, _ := h.HandleGetThread(ctx, call(map[string]any{"message_id": msg1ID, "format": "json"}))
	thread := parseJSON(t, threadRes)
	if thread["count"].(float64) != 2 {
		t.Errorf("expected 2 messages in thread, got %v", thread["count"])
	}
}

// --- Conversation Tests ---

func TestConversationLifecycle(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "qa"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-c", "role": "pm"}))

	// Create conversation
	createRes, _ := h.HandleCreateConversation(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "title": "Test conv", "members": []any{"bot-a", "bot-b"},
	}))
	createData := parseJSON(t, createRes)
	conv := createData["conversation"].(map[string]any)
	convID := conv["id"].(string)

	// List conversations
	listRes, _ := h.HandleListConversations(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a",
	}))
	listData := parseJSON(t, listRes)
	if listData["count"].(float64) != 1 {
		t.Errorf("expected 1 conversation, got %v", listData["count"])
	}

	// Send message to conversation
	_, _ = h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "", "content": "hello conv",
		"conversation_id": convID,
	}))

	// Get conversation messages
	msgsRes, _ := h.HandleGetConversationMessages(ctx, call(map[string]any{
		"as": "bot-a", "conversation_id": convID,
	}))
	msgsData := parseJSON(t, msgsRes)
	if msgsData["count"].(float64) != 1 {
		t.Errorf("expected 1 message, got %v", msgsData["count"])
	}

	// Invite bot-c
	inviteRes, _ := h.HandleInviteToConversation(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "conversation_id": convID, "agent_name": "bot-c",
	}))
	inviteData := parseJSON(t, inviteRes)
	if inviteData["invited"] != "bot-c" {
		t.Errorf("expected invited=bot-c, got %v", inviteData["invited"])
	}

	// bot-b leaves
	leaveRes, _ := h.HandleLeaveConversation(ctx, call(map[string]any{
		"project": "p1", "as": "bot-b", "conversation_id": convID,
	}))
	leaveData := parseJSON(t, leaveRes)
	if leaveData["left"] != "bot-b" {
		t.Errorf("expected left=bot-b, got %v", leaveData["left"])
	}

	// Archive
	archiveRes, _ := h.HandleArchiveConversation(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "conversation_id": convID,
	}))
	archiveData := parseJSON(t, archiveRes)
	if archiveData["archived"] != true {
		t.Error("expected archived=true")
	}
}

func TestCreateConversationMissingTitle(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleCreateConversation(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "members": []any{"bot-b"},
	}))
	expectError(t, res)
}

func TestCreateConversationMissingMembers(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleCreateConversation(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "title": "test",
	}))
	expectError(t, res)
}

func TestConversationNonMemberCantRead(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "qa"}))

	createRes, _ := h.HandleCreateConversation(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "title": "private", "members": []any{"bot-a", "bot-b"},
	}))
	conv := parseJSON(t, createRes)["conversation"].(map[string]any)
	convID := conv["id"].(string)

	// bot-c (not a member) tries to read
	res, _ := h.HandleGetConversationMessages(ctx, call(map[string]any{
		"as": "bot-c", "conversation_id": convID,
	}))
	expectError(t, res)
}

// --- Task Tests ---

func TestTaskLifecycle(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	// Dispatch
	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "Fix bug", "description": "fix the thing",
	}))
	dispatchBody := parseJSON(t, dispatchRes)
	task := dispatchBody["task"].(map[string]any)
	taskID := task["id"].(string)
	if task["status"] != "pending" {
		t.Errorf("expected pending, got %v", task["status"])
	}

	// Claim
	claimRes, _ := h.HandleClaimTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": taskID,
	}))
	claimed := parseJSON(t, claimRes)
	if claimed["status"] != "accepted" {
		t.Errorf("expected accepted, got %v", claimed["status"])
	}

	// Start
	startRes, _ := h.HandleStartTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": taskID,
	}))
	started := parseJSON(t, startRes)
	if started["status"] != "in-progress" {
		t.Errorf("expected in-progress, got %v", started["status"])
	}

	// Complete
	completeRes, _ := h.HandleCompleteTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": taskID, "result": "done!",
	}))
	completed := parseJSON(t, completeRes)
	if completed["status"] != "done" {
		t.Errorf("expected done, got %v", completed["status"])
	}
}

func TestTaskBlock(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "task1",
	}))
	task := parseJSON(t, dispatchRes)["task"].(map[string]any)
	taskID := task["id"].(string)

	_, _ = h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID}))
	_, _ = h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID}))

	blockRes, _ := h.HandleBlockTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": taskID, "reason": "waiting for API",
	}))
	blocked := parseJSON(t, blockRes)
	if blocked["status"] != "blocked" {
		t.Errorf("expected blocked, got %v", blocked["status"])
	}
}

func TestTaskCancel(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "to cancel",
	}))
	task := parseJSON(t, dispatchRes)["task"].(map[string]any)
	taskID := task["id"].(string)

	cancelRes, _ := h.HandleCancelTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": taskID, "reason": "not needed",
	}))
	cancelled := parseJSON(t, cancelRes)
	if cancelled["status"] != "cancelled" {
		t.Errorf("expected cancelled, got %v", cancelled["status"])
	}
}

func TestGetTask(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "get me",
	}))
	task := parseJSON(t, dispatchRes)["task"].(map[string]any)
	taskID := task["id"].(string)

	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{
		"project": "p1", "task_id": taskID,
	}))
	got := parseJSON(t, getRes)
	if got["title"] != "get me" {
		t.Errorf("expected 'get me', got %v", got["title"])
	}
}

func TestGetTaskNotFound(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleGetTask(ctx, call(map[string]any{
		"project": "p1", "task_id": "nonexistent",
	}))
	expectError(t, res)
}

func TestListTasks(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	_, _ = h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "profile": "dev", "title": "task1"}))
	_, _ = h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "profile": "dev", "title": "task2"}))
	_, _ = h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "profile": "qa", "title": "task3"}))

	// List all
	res, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	data := parseJSON(t, res)
	if data["count"].(float64) != 3 {
		t.Errorf("expected 3 tasks, got %v", data["count"])
	}

	// Filter by profile
	res2, _ := h.HandleListTasks(ctx, call(map[string]any{"format": "json", "project": "p1", "profile": "dev"}))
	data2 := parseJSON(t, res2)
	if data2["count"].(float64) != 2 {
		t.Errorf("expected 2 dev tasks, got %v", data2["count"])
	}
}

func TestArchiveTasks(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "to archive",
	}))
	task := parseJSON(t, dispatchRes)["task"].(map[string]any)
	taskID := task["id"].(string)

	// Complete it first
	_, _ = h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID}))
	_, _ = h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID}))
	_, _ = h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": taskID}))

	// Archive done tasks
	archiveRes, _ := h.HandleArchiveTasks(ctx, call(map[string]any{
		"project": "p1", "status": "done",
	}))
	// ArchiveTasks returns plain text, not JSON
	tc := archiveRes.Content[0].(mcp.TextContent)
	if tc.Text == "" {
		t.Error("expected archive result text")
	}
}

func TestDispatchTaskMissingFields(t *testing.T) {
	h := testHandlers(t)
	// Missing profile
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "title": "test",
	}))
	expectError(t, res)

	// Missing title
	res2, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev",
	}))
	expectError(t, res2)
}

// --- Memory Tests ---

func TestMemorySetAndGet(t *testing.T) {
	h := testHandlers(t)

	// Set
	setRes, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "api_url", "value": "https://example.com",
	}))
	setData := parseJSON(t, setRes)
	mem := setData["memory"].(map[string]any)
	if mem["key"] != "api_url" {
		t.Errorf("expected key=api_url, got %v", mem["key"])
	}

	// Get
	getRes, _ := h.HandleGetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "api_url",
	}))
	getData := parseJSON(t, getRes)
	if getData["count"].(float64) != 1 {
		t.Errorf("expected 1 memory, got %v", getData["count"])
	}
}

func TestMemorySetMissingFields(t *testing.T) {
	h := testHandlers(t)
	// Missing key
	res, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "value": "test",
	}))
	expectError(t, res)

	// Missing value
	res2, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "test",
	}))
	expectError(t, res2)
}

func TestMemorySearch(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "deploy_url", "value": "the production deploy URL is https://prod.example.com",
	}))
	_, _ = h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "db_host", "value": "database host is db.internal",
	}))

	res, _ := h.HandleSearchMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "query": "deploy",
	}))
	// FTS5 may not be available in test builds — if error, that's expected
	if res.IsError {
		t.Skip("FTS5 not available in this build")
	}
	data := parseJSON(t, res)
	if data["count"].(float64) < 1 {
		t.Errorf("expected at least 1 search result, got %v", data["count"])
	}
}

func TestMemoryList(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "k1", "value": "v1",
	}))
	_, _ = h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-b", "key": "k2", "value": "v2",
	}))

	res, _ := h.HandleListMemories(ctx, call(map[string]any{"format": "json",
		"project": "p1",
	}))
	data := parseJSON(t, res)
	if data["count"].(float64) != 2 {
		t.Errorf("expected 2 memories, got %v", data["count"])
	}
}

func TestMemoryDelete(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "temp", "value": "to delete",
	}))

	res, _ := h.HandleDeleteMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "temp",
	}))
	data := parseJSON(t, res)
	if data["deleted"] != true {
		t.Error("expected deleted=true")
	}

	// Should be gone
	getRes, _ := h.HandleGetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "temp",
	}))
	getData := parseJSON(t, getRes)
	if getData["count"].(float64) != 0 {
		t.Errorf("expected 0 memories after delete, got %v", getData["count"])
	}
}

// --- Profile Tests ---

func TestProfileLifecycle(t *testing.T) {
	h := testHandlers(t)

	// Register
	regRes, _ := h.HandleRegisterProfile(ctx, call(map[string]any{
		"project": "p1", "slug": "backend", "name": "Backend Dev", "role": "developer",
	}))
	profile := parseJSON(t, regRes)
	if profile["slug"] != "backend" {
		t.Errorf("expected slug=backend, got %v", profile["slug"])
	}

	// Get
	getRes, _ := h.HandleGetProfile(ctx, call(map[string]any{
		"project": "p1", "slug": "backend",
	}))
	got := parseJSON(t, getRes)
	if got["name"] != "Backend Dev" {
		t.Errorf("expected name=Backend Dev, got %v", got["name"])
	}

	// List
	listRes, _ := h.HandleListProfiles(ctx, call(map[string]any{
		"project": "p1",
	}))
	listData := parseJSON(t, listRes)
	if listData["count"].(float64) != 1 {
		t.Errorf("expected 1 profile, got %v", listData["count"])
	}
}

func TestProfileNotFound(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleGetProfile(ctx, call(map[string]any{
		"project": "p1", "slug": "nonexistent",
	}))
	expectError(t, res)
}

func TestRegisterProfileMissingFields(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleRegisterProfile(ctx, call(map[string]any{
		"project": "p1", "name": "test",
	}))
	expectError(t, res)

	res2, _ := h.HandleRegisterProfile(ctx, call(map[string]any{
		"project": "p1", "slug": "test",
	}))
	expectError(t, res2)
}

func TestFindProfiles(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterProfile(ctx, call(map[string]any{
		"project": "p1", "slug": "backend", "name": "Backend Dev", "role": "dev",
		"skills": `[{"tag": "golang"}]`,
	}))
	_, _ = h.HandleRegisterProfile(ctx, call(map[string]any{
		"project": "p1", "slug": "frontend", "name": "Frontend Dev", "role": "dev",
		"skills": `[{"tag": "react"}]`,
	}))

	res, _ := h.HandleFindProfiles(ctx, call(map[string]any{
		"project": "p1", "skill_tag": "golang",
	}))
	data := parseJSON(t, res)
	if data["count"].(float64) != 1 {
		t.Errorf("expected 1 profile with golang tag, got %v", data["count"])
	}
}

// --- Board Tests ---

func TestBoardLifecycle(t *testing.T) {
	h := testHandlers(t)

	// Create
	createRes, _ := h.HandleCreateBoard(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "name": "Sprint 1", "slug": "sprint-1",
	}))
	board := parseJSON(t, createRes)
	boardID := board["id"].(string)
	if board["name"] != "Sprint 1" {
		t.Errorf("expected Sprint 1, got %v", board["name"])
	}

	// List (returns array, not map)
	listRes, _ := h.HandleListBoards(ctx, call(map[string]any{"project": "p1"}))
	tc := listRes.Content[0].(mcp.TextContent)
	var boardList []any
	if err := json.Unmarshal([]byte(tc.Text), &boardList); err != nil {
		t.Fatalf("parse board list: %v", err)
	}
	if len(boardList) != 1 {
		t.Errorf("expected 1 board, got %d", len(boardList))
	}

	// Archive (returns plain text)
	archiveRes, _ := h.HandleArchiveBoard(ctx, call(map[string]any{
		"project": "p1", "board_id": boardID,
	}))
	if archiveRes.IsError {
		t.Errorf("archive board failed: %v", archiveRes.Content)
	}

	// Delete (returns plain text)
	deleteRes, _ := h.HandleDeleteBoard(ctx, call(map[string]any{
		"project": "p1", "board_id": boardID,
	}))
	if deleteRes.IsError {
		t.Errorf("delete board failed: %v", deleteRes.Content)
	}
}

func TestCreateBoardMissingFields(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleCreateBoard(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "name": "test",
	}))
	expectError(t, res)
}

// --- Org / Team Tests ---

func TestOrgAndTeamLifecycle(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	// Create org
	orgRes, _ := h.HandleCreateOrg(ctx, call(map[string]any{
		"name": "Acme Corp", "slug": "acme",
	}))
	org := parseJSON(t, orgRes)
	orgID := org["id"].(string)

	// List orgs
	listOrgRes, _ := h.HandleListOrgs(ctx, call(map[string]any{}))
	orgs := parseJSON(t, listOrgRes)
	if orgs["count"].(float64) != 1 {
		t.Errorf("expected 1 org, got %v", orgs["count"])
	}

	// Create team
	teamRes, _ := h.HandleCreateTeam(ctx, call(map[string]any{
		"project": "p1", "name": "Backend Team", "slug": "backend", "org_id": orgID,
	}))
	team := parseJSON(t, teamRes)
	if team["name"] != "Backend Team" {
		t.Errorf("expected Backend Team, got %v", team["name"])
	}

	// List teams
	listTeamRes, _ := h.HandleListTeams(ctx, call(map[string]any{"project": "p1"}))
	teams := parseJSON(t, listTeamRes)
	if teams["count"].(float64) != 1 {
		t.Errorf("expected 1 team, got %v", teams["count"])
	}

	// Add member (uses team slug, not ID)
	addRes, _ := h.HandleAddTeamMember(ctx, call(map[string]any{
		"project": "p1", "team": "backend", "agent_name": "bot-a", "role": "lead",
	}))
	addData := parseJSON(t, addRes)
	if addData["added"] != true {
		t.Error("expected added=true")
	}

	// Remove member (uses team slug)
	removeRes, _ := h.HandleRemoveTeamMember(ctx, call(map[string]any{
		"project": "p1", "team": "backend", "agent_name": "bot-a",
	}))
	removeData := parseJSON(t, removeRes)
	if removeData["removed"] != true {
		t.Error("expected removed=true")
	}
}

// --- Memory Conflict Tests ---

func TestMemoryConflictAndResolve(t *testing.T) {
	h := testHandlers(t)

	// Agent A sets a memory
	_, _ = h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "db_host", "value": "localhost",
	}))

	// Agent B sets the same key with different value -> conflict (upsert=false)
	setRes, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-b", "key": "db_host", "value": "prod-db.internal", "upsert": false,
	}))
	setData := parseJSON(t, setRes)
	if setData["conflict"] != true {
		t.Error("expected conflict=true")
	}

	// Get should show multiple values
	getRes, _ := h.HandleGetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "db_host",
	}))
	getData := parseJSON(t, getRes)
	if getData["count"].(float64) < 2 {
		t.Errorf("expected at least 2 conflicting memories, got %v", getData["count"])
	}

	// Resolve conflict
	resolveRes, _ := h.HandleResolveConflict(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "db_host", "chosen_value": "prod-db.internal",
	}))
	resolveData := parseJSON(t, resolveRes)
	if resolveData["resolved"] != true {
		t.Error("expected resolved=true")
	}

	// After resolve, should be 1 value
	getRes2, _ := h.HandleGetMemory(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "db_host",
	}))
	getData2 := parseJSON(t, getRes2)
	if getData2["count"].(float64) != 1 {
		t.Errorf("expected 1 memory after resolve, got %v", getData2["count"])
	}
}

func TestResolveConflictMissingFields(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleResolveConflict(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "chosen_value": "x",
	}))
	expectError(t, res)

	res2, _ := h.HandleResolveConflict(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "key": "k",
	}))
	expectError(t, res2)
}

// --- Team Inbox Tests ---

func TestTeamInbox(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	_, _ = h.HandleCreateTeam(ctx, call(map[string]any{
		"project": "p1", "name": "Dev Team", "slug": "dev",
	}))
	_, _ = h.HandleAddTeamMember(ctx, call(map[string]any{
		"project": "p1", "team": "dev", "agent_name": "bot-a",
	}))

	// Send message to team
	_, _ = h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "team:dev", "content": "team message",
	}))

	// Get team inbox (json format — default is now a markdown table)
	inboxRes, _ := h.HandleGetTeamInbox(ctx, call(map[string]any{
		"project": "p1", "team": "dev", "format": "json",
	}))
	data := parseJSON(t, inboxRes)
	if data["count"].(float64) != 1 {
		t.Errorf("expected 1 team message, got %v", data["count"])
	}
}

func TestTeamInboxMissingTeam(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleGetTeamInbox(ctx, call(map[string]any{"project": "p1"}))
	expectError(t, res)
}

func TestTeamInboxNotFound(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleGetTeamInbox(ctx, call(map[string]any{
		"project": "p1", "team": "nonexistent",
	}))
	expectError(t, res)
}

// --- Notify Channel Tests ---

func TestAddNotifyChannel(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleAddNotifyChannel(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "target": "bot-b",
	}))
	data := parseJSON(t, res)
	if data["added"] != true {
		t.Error("expected added=true")
	}
}

func TestAddNotifyChannelMissingTarget(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleAddNotifyChannel(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a",
	}))
	expectError(t, res)
}

// --- Message Broadcast Tests ---

func TestSendBroadcastMessage(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "qa"}))

	res, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "*", "content": "broadcast!",
	}))
	msg := parseJSON(t, res)
	if msg["to"] != "*" {
		t.Errorf("expected to=*, got %v", msg["to"])
	}

	// bot-b should see it in inbox
	inboxRes, _ := h.HandleGetInbox(ctx, call(map[string]any{"format": "json",
		"project": "p1", "as": "bot-b", "unread_only": true,
	}))
	inbox := parseJSON(t, inboxRes)
	if inbox["count"].(float64) != 1 {
		t.Errorf("expected 1 broadcast message, got %v", inbox["count"])
	}
}

func TestSendConversationShorthand(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "qa"}))

	// Create conversation
	createRes, _ := h.HandleCreateConversation(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "title": "test", "members": []any{"bot-a", "bot-b"},
	}))
	conv := parseJSON(t, createRes)["conversation"].(map[string]any)
	convID := conv["id"].(string)

	// Send using "to": "conversation:<id>" shorthand
	sendRes, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "conversation:" + convID, "content": "shorthand!",
	}))
	if sendRes.IsError {
		t.Fatalf("send via shorthand failed: %v", sendRes.Content)
	}
}

// --- Task with subtasks ---

func TestTaskSubtaskCompletion(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))

	// Dispatch parent
	parentRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "Parent task",
	}))
	parent := parseJSON(t, parentRes)["task"].(map[string]any)
	parentID := parent["id"].(string)

	// Dispatch subtask
	subRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "Subtask 1",
		"parent_task_id": parentID,
	}))
	sub := parseJSON(t, subRes)["task"].(map[string]any)
	subID := sub["id"].(string)

	// Complete subtask flow
	_, _ = h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": subID}))
	_, _ = h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": subID}))
	completeRes, _ := h.HandleCompleteTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": subID, "result": "done",
	}))
	completed := parseJSON(t, completeRes)
	if completed["status"] != "done" {
		t.Errorf("expected done, got %v", completed["status"])
	}

	// Get parent with subtasks
	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{
		"project": "p1", "task_id": parentID, "include_subtasks": true,
	}))
	parentData := parseJSON(t, getRes)
	if parentData["title"] != "Parent task" {
		t.Errorf("expected 'Parent task', got %v", parentData["title"])
	}
}

// --- Validation Tests (cross-cutting) ---

func TestResolveProjectDefault(t *testing.T) {
	ctx := context.Background()
	req := call(map[string]any{})
	if p := resolveProject(ctx, req); p != "default" {
		t.Errorf("expected 'default', got %s", p)
	}
}

func TestResolveAgentDefault(t *testing.T) {
	ctx := context.Background()
	req := call(map[string]any{})
	if a := resolveAgent(ctx, req); a != "anonymous" {
		t.Errorf("expected 'anonymous', got %s", a)
	}
}

func TestOptionalString(t *testing.T) {
	if optionalString("") != nil {
		t.Error("expected nil for empty string")
	}
	if *optionalString("hello") != "hello" {
		t.Error("expected 'hello'")
	}
}

func TestCreateProject(t *testing.T) {
	h := testHandlers(t)

	// New project returns onboarding prompt
	res, err := h.HandleCreateProject(ctx, call(map[string]any{"name": "test-app"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected success")
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Project Setup") {
		t.Error("expected onboarding prompt with 'Project Setup' header")
	}
	// Phase 1 must wire the hooks (the relay is blind without them) and must NOT
	// reference the removed vault tools.
	if !strings.Contains(text, "agent-relay hooks install") {
		t.Error("expected onboarding to install the activity/identity hooks")
	}
	if strings.Contains(text, "search_vault") || strings.Contains(text, "register_vault") {
		t.Error("onboarding must not reference the removed vault tools")
	}
	if !strings.Contains(text, "test-app") {
		t.Error("expected project name in prompt")
	}
	if !strings.Contains(text, "Phase 7") {
		t.Error("expected all 7 phases")
	}
	if !strings.Contains(text, "--dangerously-skip-permissions") {
		t.Error("expected spawn commands with skip-permissions flag")
	}
	if !strings.Contains(text, "send_message") {
		t.Error("expected worker ping-ready instruction")
	}

	// Second call with agents already registered should return already_configured
	h.db.EnsureProject("test-app")
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{
		"name": "cto", "project": "test-app", "role": "lead",
	}))
	res2, err := h.HandleCreateProject(ctx, call(map[string]any{"name": "test-app"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data := parseJSON(t, res2)
	if data["status"] != "already_configured" {
		t.Errorf("expected already_configured, got %v", data["status"])
	}
}

func TestCreateProjectValidation(t *testing.T) {
	h := testHandlers(t)

	// Missing name
	res, err := h.HandleCreateProject(ctx, call(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msg := expectError(t, res)
	if !strings.Contains(msg, "name is required") {
		t.Errorf("expected 'name is required', got: %s", msg)
	}
}

func TestOnboardingPromptBoardBranch(t *testing.T) {
	// Native mode (default): the relay owns the board — create_board + dispatch_task.
	native := buildOnboardingPrompt("p", "", "", false, false)
	if !strings.Contains(native, "create_board") {
		t.Error("native onboarding must create a relay board")
	}
	if !strings.Contains(native, "dispatch_task({") {
		t.Error("native onboarding must dispatch tasks on the relay board")
	}
	if strings.Contains(native, "Linear-SSOT mode") {
		t.Error("native onboarding must not mention Linear-SSOT mode")
	}

	// Linear-SSOT mode: Linear owns the board — no relay board, no dispatch_task.
	linear := buildOnboardingPrompt("p", "", "", false, true)
	if !strings.Contains(linear, "Linear-SSOT mode") {
		t.Error("Linear onboarding must announce Linear-SSOT mode")
	}
	if !strings.Contains(linear, "linear_routing") {
		t.Error("Linear onboarding must point at the linear_routing map")
	}
	if strings.Contains(linear, "create_board") {
		t.Error("Linear onboarding must NOT create a relay board")
	}
	if strings.Contains(linear, "dispatch_task({") {
		t.Error("Linear onboarding must NOT emit a dispatch_task call (orphan native task)")
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{10, 10},      // normal — unchanged
		{200, 200},    // at the cap
		{201, 200},    // over the cap — clamped
		{100000, 200}, // pathological dump — clamped
		{0, 0},        // 0 preserved (default/unbounded semantics)
		{-1, -1},      // negative preserved
	}
	for _, c := range cases {
		if got := clampLimit(c.in); got != c.want {
			t.Errorf("clampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
