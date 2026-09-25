package relay

import (
	"strings"
	"testing"
)

// DEC-wraith-tombstones-1 T2 (task 93b6f1cf): the reply_to probe logs every
// non-live ref and never rejects the send.
func TestReplyToProbe(t *testing.T) {
	t.Run("ProbeLogsNeverRejects", func(t *testing.T) {
		h, _ := linkageSendSetup(t)
		unknown := "11111111-2222-3333-4444-555555555555"
		var res map[string]any
		out := captureLinkageLog(t, func() {
			res = parseJSON(t, mustSend(t, h, map[string]any{
				"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "re", "content": "late",
				"type": "response", "reply_to": unknown,
			}))
		})
		if n := strings.Count(out, "[linkage] reply_to="+unknown+" state=unknown"); n != 1 {
			t.Fatalf("want exactly 1 probe line, got %d: %q", n, out)
		}
		if v, ok := res["reply_to_resolved"].(bool); !ok || v {
			t.Fatalf("reply_to_resolved = %v, want false for an unknown parent", res["reply_to_resolved"])
		}
		id, _ := res["id"].(string)
		inbox, err := h.db.GetInbox("p1", "bot-b", false, 50)
		if err != nil {
			t.Fatalf("inbox: %v", err)
		}
		delivered := false
		for _, m := range inbox {
			delivered = delivered || m.ID == id
		}
		if !delivered {
			t.Fatalf("probed send has no delivery row")
		}
	})

	t.Run("LiveParentNotProbed", func(t *testing.T) {
		h, _ := linkageSendSetup(t)
		parent := parseJSON(t, mustSend(t, h, map[string]any{
			"project": "p1", "as": "bot-b", "to": "bot-a", "subject": "q", "content": "?", "type": "question",
		}))["id"].(string)
		out := captureLinkageLog(t, func() {
			_ = mustSend(t, h, map[string]any{
				"project": "p1", "as": "bot-a", "to": "bot-b", "subject": "re", "content": "!", "type": "response", "reply_to": parent,
			})
		})
		if strings.Contains(out, "[linkage] reply_to=") {
			t.Fatalf("live parent must not be probed: %q", out)
		}
	})
}
