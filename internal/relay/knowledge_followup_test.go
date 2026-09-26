package relay

import (
	"strings"
	"testing"
)

// Knowledge follow-up (task 298ae9b3, deferred from knowledge S2 1edb1377):
// remember carries a declared change_class to the knowledge_log, and every
// boot hands the agent the current knowledge revision as its cursor.

func remember(t *testing.T, h *Handlers, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"project": "p1", "as": "a", "decision": "use sqlite", "area": "db"}
	for k, v := range extra {
		args[k] = v
	}
	res, _ := h.HandleRemember(ctx, call(args))
	return parseJSON(t, res)
}

func TestRememberChangeClass(t *testing.T) {
	h, _, _ := knowledgeHandlers(t)

	remember(t, h, map[string]any{"change_class": "breaking"})
	ch := deltaChanges(knowledgeDelta(t, h, 0))
	if len(ch) != 1 || ch[0]["layer"] != "decision" || ch[0]["declared_class"] != "breaking" || ch[0]["change_class"] != "breaking" {
		t.Fatalf("declared remember = %v, want one decision row declared breaking", ch)
	}
	head := knowledgeDelta(t, h, 0)["head_rev"]

	// Invalid value: INVALID_ARGUMENT, nothing written.
	res, _ := h.HandleRemember(ctx, call(map[string]any{"project": "p1", "as": "a", "decision": "use postgres", "area": "db2", "change_class": "retraction"}))
	if msg := expectError(t, res); !strings.Contains(msg, "INVALID_ARGUMENT") {
		t.Fatalf("invalid change_class: %s, want INVALID_ARGUMENT", msg)
	}
	if after := knowledgeDelta(t, h, 0)["head_rev"]; after != head {
		t.Fatalf("rejected remember wrote to the log: head %v -> %v", head, after)
	}

	// Omitted: today's default, an undeclared HIGH-layer write is breaking.
	remember(t, h, map[string]any{"decision": "cache reads for 5s", "area": "cache"})
	ch = deltaChanges(knowledgeDelta(t, h, 0))
	last := ch[len(ch)-1]
	if last["declared_class"] != nil || last["change_class"] != "breaking" {
		t.Fatalf("undeclared remember = %v, want no declared class and effective breaking", last)
	}
}

func TestSessionContextCarriesKnowledgeRev(t *testing.T) {
	h, _, _ := knowledgeHandlers(t)
	setKey(t, h, "k1", "v1", nil)
	remember(t, h, nil)
	head := knowledgeDelta(t, h, 0)["head_rev"].(float64)
	if head < 2 {
		t.Fatalf("fixture head_rev = %v, want >= 2", head)
	}
	for _, minimal := range []bool{false, true} {
		args := map[string]any{"project": "p1", "as": "a"}
		if minimal {
			args["session_context"] = "minimal"
		}
		res, _ := h.HandleGetSessionContext(ctx, call(args))
		sc := parseJSON(t, res)
		if rev, _ := sc["knowledge_rev"].(float64); rev != head {
			t.Fatalf("minimal=%v: knowledge_rev = %v, want %v", minimal, sc["knowledge_rev"], head)
		}
	}
	// Boot adds nothing to the knowledge log.
	if after := knowledgeDelta(t, h, 0)["head_rev"].(float64); after != head {
		t.Fatalf("boot wrote to the knowledge log: head %v -> %v", head, after)
	}
}
