package relay

import (
	"strings"
	"testing"
)

// TestSendMessageKnownNonAgentRecipientsStillAccepted backs AC2: the unknown-
// recipient guard must NOT swallow the recipients that are valid today without
// being agent rows — '*' broadcast, a team channel, and the console operator
// inbox addressed as either "user" or its twin "human". Each must still create
// the message (and its deliveries), exactly as before the guard.
func TestSendMessageKnownNonAgentRecipientsStillAccepted(t *testing.T) {
	h := testHandlers(t)
	// is_executive so the sender may broadcast ('*' needs admin-team membership);
	// the unknown-recipient guard under test is orthogonal to that permission.
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "sender", "is_executive": true}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "peer"}))
	_, _ = h.HandleCreateTeam(ctx, call(map[string]any{"project": "p1", "name": "Dev Team", "slug": "dev"}))
	_, _ = h.HandleAddTeamMember(ctx, call(map[string]any{"project": "p1", "team": "dev", "agent_name": "peer"}))

	cases := []struct {
		name string
		to   string
	}{
		{"broadcast", "*"},
		{"team", "team:dev"},
		{"user", "user"},
		{"human", "human"},
	}
	for _, c := range cases {
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{
			"project": "p1", "as": "sender", "to": c.to, "content": "hi " + c.name,
		}))
		if res.IsError {
			t.Fatalf("send to %q (%s) must be accepted, got error: %s", c.to, c.name, expectError(t, res))
		}
		msg := parseJSON(t, res)
		if msg["id"] == nil || msg["id"] == "" {
			t.Fatalf("send to %q must return a persisted message id, got %v", c.to, msg)
		}
	}

	// The broadcast actually reached the registered peer (a delivery was made).
	inbox, _ := h.HandleGetInbox(ctx, call(map[string]any{"project": "p1", "as": "peer", "format": "json"}))
	data := parseJSON(t, inbox)
	if msgs, _ := data["messages"].([]any); len(msgs) == 0 {
		t.Fatalf("broadcast + team send should have delivered to 'peer', inbox empty: %v", data)
	}
}

// TestExtPR152DiffScope backs AC3. The build/diff-scope/author-preserved half is
// enforced at the gate; the testable half is requirement (iii): the "did you
// mean" suggestion on an unknown recipient must be drawn from the CALLER's
// project roster only, never leaking agents registered in another project.
func TestExtPR152DiffScope(t *testing.T) {
	h := testHandlers(t)
	// Sender + an executive so hasTeams is set and the guard path is live.
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "is_executive": true}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "sender"}))
	// A distinctively named agent that exists ONLY in another project. p1 holds
	// no name within Levenshtein 2 of it, so a correct, project-scoped
	// suggestion yields NO "Did you mean" clause — but if suggestions wrongly
	// drew from p2's roster, this exact name (distance 0) would be offered.
	const foreign = "zzforeignpeer"
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p2", "name": foreign}))

	res, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "sender", "to": foreign, "content": "leak?",
	}))
	if !res.IsError {
		t.Fatalf("send to a recipient that exists only in another project must be rejected in p1")
	}
	errStr := expectError(t, res)
	if !strings.Contains(errStr, foreign) {
		t.Fatalf("rejection should name the unknown recipient %q, got: %s", foreign, errStr)
	}
	if strings.Contains(errStr, "Did you mean") {
		t.Fatalf("cross-project leak: p1 rejection offered a suggestion drawn from p2's roster: %s", errStr)
	}
}
