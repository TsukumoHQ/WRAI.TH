package relay

import (
	"regexp"
	"strings"
	"testing"
)

// W12 (4a4a9913): agents quote message times from the server, never
// hand-typed — every message read carries the server sent_at.

var rfc3339UTC = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

func w12Send(t *testing.T) (*Handlers, string) {
	t.Helper()
	h := testHandlers(t)
	for _, n := range []string{"alice", "bob"} {
		_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": n}))
	}
	res, _ := h.HandleSendMessage(ctx, call(map[string]any{"project": "p1", "as": "alice", "to": "bob", "subject": "hello", "content": "body"}))
	if res.IsError {
		t.Fatalf("send: %s", expectError(t, res))
	}
	return h, parseJSON(t, res)["id"].(string)
}

func TestGetInbox_SentAt(t *testing.T) {
	h, _ := w12Send(t)
	res, _ := h.HandleGetInbox(ctx, call(map[string]any{"project": "p1", "as": "bob", "format": "json"}))
	msgs := parseJSON(t, res)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if s, _ := msgs[0].(map[string]any)["sent_at"].(string); !rfc3339UTC.MatchString(s) {
		t.Errorf("get_inbox json sent_at = %q, want RFC3339 UTC", s)
	}

	res, _ = h.HandleGetInbox(ctx, call(map[string]any{"project": "p1", "as": "bob"}))
	md := resultText(res)
	lines := strings.Split(md, "\n")
	var header, row []string
	for _, l := range lines {
		if strings.HasPrefix(l, "|id|") {
			header = strings.Split(l, "|")
		} else if header != nil && strings.HasPrefix(l, "|") && !strings.HasPrefix(l, "|---") {
			row = strings.Split(l, "|")
			break
		}
	}
	col := -1
	for i, c := range header {
		if c == "sent_at" {
			col = i
		}
	}
	if col < 0 || row == nil || !rfc3339UTC.MatchString(row[col]) {
		t.Errorf("get_inbox md: no RFC3339 UTC sent_at column (header %v, row %v)", header, row)
	}
}

func TestSessionContext_UnreadPreviewSentAt(t *testing.T) {
	h, _ := w12Send(t)
	res, _ := h.HandleGetSessionContext(ctx, call(map[string]any{"project": "p1", "as": "bob", "session_context": "minimal"}))
	unread, _ := parseJSON(t, res)["unread_messages"].([]any)
	if len(unread) != 1 {
		t.Fatalf("want 1 unread preview, got %v", unread)
	}
	if s, _ := unread[0].(map[string]any)["sent_at"].(string); !rfc3339UTC.MatchString(s) {
		t.Errorf("session_context unread preview sent_at = %q, want RFC3339 UTC", s)
	}
}

func TestGetMessage_CreatedAtUnchanged(t *testing.T) {
	h, id := w12Send(t)
	stored, err := h.db.GetMessage(id)
	if err != nil || stored == nil {
		t.Fatalf("stored message: %v", err)
	}
	res, _ := h.HandleGetMessage(ctx, call(map[string]any{"project": "p1", "as": "bob", "id": id}))
	if got := parseJSON(t, res)["created_at"]; got != stored.CreatedAt {
		t.Errorf("get_message created_at = %v, want unchanged %q", got, stored.CreatedAt)
	}
}
