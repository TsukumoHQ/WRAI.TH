package relay

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// DEC-wraith-linkage-1 slice B (task fde40c25): send_message takes an optional,
// validated task_id, and reply_to is resolved softly — malformed refused,
// well-formed but purged accepted and reported unresolved.

func linkageSendSetup(t *testing.T) (*Handlers, string) {
	t.Helper()
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b"}))
	task, err := h.db.DispatchTask("p1", "lead", "cto", "linkage", "", "P1", nil, nil, db.TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch task: %v", err)
	}
	return h, task.ID
}

func inboxCount(t *testing.T, h *Handlers, agent string) int {
	t.Helper()
	msgs, err := h.db.GetInbox("p1", agent, false, 100)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	return len(msgs)
}

func captureLinkageLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	fn()
	return buf.String()
}

func TestSendLinkage(t *testing.T) {
	t.Run("TaskIDArgStored", func(t *testing.T) {
		h, taskID := linkageSendSetup(t)
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{
			"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "s", "content": "about the task",
			"task_id": taskID, "metadata": `{"tag":"x"}`,
		}))
		id := parseJSON(t, res)["id"].(string)
		msg, err := h.db.GetMessage(id)
		if err != nil || msg == nil {
			t.Fatalf("get message: %v", err)
		}
		if msg.TaskID == nil || *msg.TaskID != taskID {
			t.Fatalf("stored task_id = %v, want %s", msg.TaskID, taskID)
		}
		if !strings.Contains(msg.Metadata, `"tag":"x"`) {
			t.Fatalf("caller metadata lost: %s", msg.Metadata)
		}
	})

	t.Run("UnknownTaskIDRejectedNothingPersisted", func(t *testing.T) {
		h, _ := linkageSendSetup(t)
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{
			"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "s", "content": "c",
			"task_id": "00000000-0000-0000-0000-000000000000",
		}))
		if msg := expectError(t, res); !strings.Contains(msg, "unknown task_id") {
			t.Fatalf("want 'unknown task_id' error, got %s", msg)
		}
		if n := inboxCount(t, h, "bot-b"); n != 0 {
			t.Fatalf("rejected send persisted %d message(s)", n)
		}
	})

	t.Run("TaskIDConflictRejected", func(t *testing.T) {
		h, taskID := linkageSendSetup(t)
		other, err := h.db.DispatchTask("p1", "lead", "cto", "other", "", "P1", nil, nil, db.TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{
			"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "s", "content": "c",
			"task_id": taskID, "metadata": `{"task_id":"` + other.ID + `"}`,
		}))
		if msg := expectError(t, res); !strings.Contains(msg, "task_id conflicts with metadata.task_id") {
			t.Fatalf("want conflict error, got %s", msg)
		}
		if n := inboxCount(t, h, "bot-b"); n != 0 {
			t.Fatalf("rejected send persisted %d message(s)", n)
		}
	})

	t.Run("MalformedReplyToRejected", func(t *testing.T) {
		h, _ := linkageSendSetup(t)
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{
			"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "s", "content": "c",
			"type": "response", "reply_to": "not-a-message-id",
		}))
		if msg := expectError(t, res); !strings.Contains(msg, "not a well-formed message id") {
			t.Fatalf("want malformed reply_to error, got %s", msg)
		}
		if n := inboxCount(t, h, "bot-b"); n != 0 {
			t.Fatalf("rejected send persisted %d message(s)", n)
		}
	})

	t.Run("DanglingReplyToAcceptedUnresolved", func(t *testing.T) {
		h, _ := linkageSendSetup(t)
		purged := "11111111-2222-3333-4444-555555555555"
		var res map[string]any
		out := captureLinkageLog(t, func() {
			r, _ := h.HandleSendMessage(ctx, call(map[string]any{
				"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "s", "content": "late answer",
				"type": "response", "reply_to": purged,
			}))
			res = parseJSON(t, r)
		})
		if v, ok := res["reply_to_resolved"].(bool); !ok || v {
			t.Fatalf("reply_to_resolved = %v, want false", res["reply_to_resolved"])
		}
		if res["reply_to"] != purged {
			t.Fatalf("reply_to stored as %v, want %s", res["reply_to"], purged)
		}
		if _, ok := res["task_id"]; ok {
			t.Fatalf("nothing may be derived from a dangling parent, got task_id %v", res["task_id"])
		}
		if n := strings.Count(out, "[linkage] reply_to="+purged+" state=unknown"); n != 1 {
			t.Fatalf("want exactly 1 [linkage] line, got %d: %q", n, out)
		}
	})

	t.Run("ResolvedReplyToReported", func(t *testing.T) {
		h, _ := linkageSendSetup(t)
		parent := parseJSON(t, mustSend(t, h, map[string]any{
			"project": "p1", "as": "bot-b", "to": "bot-a", "subject": "q", "content": "?", "type": "question",
		}))["id"].(string)
		res := parseJSON(t, mustSend(t, h, map[string]any{
			"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "re", "content": "!", "type": "response", "reply_to": parent,
		}))
		if v, ok := res["reply_to_resolved"].(bool); !ok || !v {
			t.Fatalf("reply_to_resolved = %v, want true", res["reply_to_resolved"])
		}
	})

	t.Run("NoArgBehaviourUnchanged", func(t *testing.T) {
		h, _ := linkageSendSetup(t)
		res := parseJSON(t, mustSend(t, h, map[string]any{
			"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "s", "content": "plain",
		}))
		for _, k := range []string{"reply_to_resolved", "task_id"} {
			if _, ok := res[k]; ok {
				t.Fatalf("plain send result gained field %q: %v", k, res)
			}
		}
		if id, _ := res["id"].(string); id == "" {
			t.Fatalf("plain send result lost its id: %v", res)
		}
	})

	t.Run("CrossProjectReplyToNotValidated", func(t *testing.T) {
		h := testHandlers(t)
		_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto-a", "is_executive": true}))
		_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p2", "name": "cto-b", "is_executive": true}))
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{
			"project": "p1", "as": "cto-a", "to": "cto-b", "target_project": "p2", "subject": "s", "content": "c",
			"type": "response", "reply_to": "remote-message-ref",
		}))
		parseJSON(t, res) // fails the test on an error result
	})
}

func mustSend(t *testing.T, h *Handlers, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, _ := h.HandleSendMessage(ctx, call(args))
	return res
}

// The web-UI response path (W9) applies the same soft reply_to rule.
func TestUserResponseLinkage(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	if w := doAPI(r, "POST", "/user-response", `{"project":"p1","to":"bot-a","content":"yes","reply_to":"bogus"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed reply_to: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	w := doAPI(r, "POST", "/user-response", `{"project":"p1","to":"bot-a","content":"yes","reply_to":"11111111-2222-3333-4444-555555555555"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("dangling reply_to: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if v, ok := decodeJSON(t, w)["reply_to_resolved"].(bool); !ok || v {
		t.Fatalf("reply_to_resolved = %v, want false", v)
	}
}
