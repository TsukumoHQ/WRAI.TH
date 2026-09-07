package relay

import (
	"testing"

	"agent-relay/internal/db"
)

// One operator identity end to end (founder P1): the human operator is reachable
// and admin-privileged under the canonical "human" as well as the legacy "user".
// One test per acceptance criterion.

// AC1: with teams configured, an agent reaches "human" via both the REST and MCP
// send paths exactly like "user"; a send to an unrelated agent still 403s.
func TestOperatorReachableAsHuman(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.CreateTeam("ops", "ops", "p1", "", "", nil, nil) // HasTeams => guard engages
	for _, to := range []string{"human", "user"} {
		if w := doAPI(r, "POST", "/messages", `{"project":"p1","from":"bot-a","to":"`+to+`","content":"hi"}`); w.Code != 200 {
			t.Fatalf("REST bot-a->%s = %d %s", to, w.Code, w.Body.String())
		}
		if res, _ := r.Handlers.HandleSendMessage(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "to": to, "content": "hi"})); res.IsError {
			t.Fatalf("MCP bot-a->%s errored: %s", to, expectError(t, res))
		}
	}
	if w := doAPI(r, "POST", "/messages", `{"project":"p1","from":"bot-a","to":"bot-z","content":"hi"}`); w.Code != 403 {
		t.Fatalf("bot-a->unrelated should 403, got %d", w.Code)
	}
}

// AC2: a transition driven by "human" takes the admin force-path (an otherwise
// invalid done->in-progress move is allowed) exactly like "user"; a regular
// agent name is still refused.
func TestOperatorForcePathHuman(t *testing.T) {
	r := testRelay(t)
	task, err := r.DB.DispatchTask("p1", "dev", "boss", "t", "", "P2", nil, nil, db.TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := r.DB.CompleteTask(task.ID, "human", "p1", nil); err != nil { // force -> done
		t.Fatalf("human force to done: %v", err)
	}
	if _, err := r.DB.StartTask(task.ID, "human", "p1"); err != nil { // done -> in-progress (invalid, forced)
		t.Fatalf("human force done->in-progress: %v", err)
	}
	if _, err := r.DB.CompleteTask(task.ID, "human", "p1", nil); err != nil {
		t.Fatalf("reset to done: %v", err)
	}
	if _, err := r.DB.StartTask(task.ID, "bot-a", "p1"); err == nil {
		t.Fatalf("regular agent done->in-progress must be refused")
	}
}

// AC3: apiPostMemory with no agent_name defaults to "human"; resolveTargets keeps
// "human" and "user" pointed at the same delivery target.
func TestOperatorDefaultsHuman(t *testing.T) {
	r := testRelay(t)
	if w := doAPI(r, "POST", "/memories", `{"project":"p1","key":"k1","value":"v1"}`); w.Code != 200 {
		t.Fatalf("post memory = %d %s", w.Code, w.Body.String())
	}
	got, err := r.DB.GetMemory("p1", "human", "k1", "project")
	if err != nil || len(got) == 0 {
		t.Fatalf("memory not stored under human (err=%v n=%d)", err, len(got))
	}
	n := &Notifier{}
	if h, u := n.resolveTargets("p1", "human", nil), n.resolveTargets("p1", "user", nil); len(h) != 1 || len(u) != 1 || h[0] != u[0] {
		t.Fatalf("resolveTargets human=%v user=%v, want equal single target", h, u)
	}
}
