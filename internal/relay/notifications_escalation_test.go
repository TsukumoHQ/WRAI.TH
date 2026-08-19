package relay

import (
	"testing"

	"agent-relay/internal/db"
	"agent-relay/internal/models"
)

// TestResolveManager_FallsBackToDispatcher is the fix for the "Blocked → manager"
// rule that failed ~64%: most workers have no reports_to, so the manager lookup
// drops and a blocked lane went silently unescalated. The target must fall back
// to the task's dispatcher so escalation reliably fires.
func TestResolveManager_FallsBackToDispatcher(t *testing.T) {
	database := newEscalationDB(t)
	n := &Notifier{db: database}

	// Worker with NO reports_to; the block event carries the dispatcher.
	mustRegister(t, database, "p1", "worker", nil)
	payload := map[string]any{"agent": "worker", "dispatched_by": "lead"}

	got := n.resolveTargets("p1", "manager", payload)
	if len(got) != 1 || got[0] != "lead" {
		t.Fatalf("manager must fall back to dispatcher, got %v", got)
	}

	// With reports_to set, the org-chart manager still wins over the fallback.
	mgr := "big-boss"
	mustRegister(t, database, "p1", "worker2", &mgr)
	got = n.resolveTargets("p1", "manager", map[string]any{"agent": "worker2", "dispatched_by": "lead"})
	if len(got) != 1 || got[0] != "big-boss" {
		t.Fatalf("reports_to must win when set, got %v", got)
	}

	// No manager AND no dispatcher in the payload → resolve dispatcher from the
	// task record (task_id only).
	task, err := database.DispatchTask("p1", "worker", "lead2", "blocked thing", "", "P1", nil, nil, db.TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got = n.resolveTargets("p1", "manager", map[string]any{"agent": "worker", "task_id": task.ID})
	if len(got) != 1 || got[0] != "lead2" {
		t.Fatalf("manager must resolve dispatcher from task record, got %v", got)
	}
}

// TestResolveDispatcher covers the auto-done 15/15-fail fix: the "dispatcher"
// target resolves from payload.dispatched_by, and from the task record when the
// emitter didn't carry it.
func TestResolveDispatcher(t *testing.T) {
	database := newEscalationDB(t)
	n := &Notifier{db: database}

	if got := n.resolveTargets("p1", "dispatcher", map[string]any{"dispatched_by": "cto"}); len(got) != 1 || got[0] != "cto" {
		t.Fatalf("dispatcher from payload, got %v", got)
	}

	task, err := database.DispatchTask("p1", "prof", "the-dispatcher", "t", "", "P2", nil, nil, db.TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := n.resolveTargets("p1", "dispatcher", map[string]any{"task_id": task.ID}); len(got) != 1 || got[0] != "the-dispatcher" {
		t.Fatalf("dispatcher from task record, got %v", got)
	}

	if got := n.resolveTargets("p1", "dispatcher", map[string]any{}); got != nil {
		t.Fatalf("unresolvable dispatcher must be nil, got %v", got)
	}
}

func TestActionRequiredFor(t *testing.T) {
	cases := []struct {
		name  string
		event string
		opts  ruleOpts
		want  string
	}{
		{"blocked force-wakes with do", EvTaskBlocked, ruleOpts{}, actionDo},
		{"done pinned no-wake", EvTaskDone, ruleOpts{}, actionNone},
		{"other events defer to derive", "event:lead-ready", ruleOpts{}, ""},
		{"digest defers to derive", EvCycleDigest, ruleOpts{}, ""},
		{"explicit opt overrides blocked default", EvTaskBlocked, ruleOpts{ActionRequired: "none"}, "none"},
		{"explicit opt overrides done default", EvTaskDone, ruleOpts{ActionRequired: "do"}, "do"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := actionRequiredFor(c.event, c.opts); got != c.want {
				t.Fatalf("actionRequiredFor(%q, %+v) = %q, want %q", c.event, c.opts, got, c.want)
			}
		})
	}
}

func TestParseOpts_ActionRequiredAndNoWake(t *testing.T) {
	if o := parseOpts(`{"action_required":"ask"}`); o.ActionRequired != "ask" {
		t.Fatalf("action_required not parsed: %+v", o)
	}
	if o := parseOpts(`{"no_wake":true}`); o.ActionRequired != actionNone {
		t.Fatalf("no_wake shorthand must map to none, got %+v", o)
	}
	if o := parseOpts(`{"no_wake":false}`); o.ActionRequired != "" {
		t.Fatalf("no_wake:false must not set a tag, got %+v", o)
	}
}

// TestDoMessage_SuppressesEmptyContent is the empty-wake fix: a rule whose line
// and title both render empty must NOT deliver a 0-char message that wakes the
// recipient. The delivery is recorded as "skipped", not "failed" (so the sweeper
// won't retry it toward the DLQ).
func TestDoMessage_SuppressesEmptyContent(t *testing.T) {
	h := testHandlers(t)
	n := NewNotifier(h.db, h.registry, h.events)
	mustRegister(t, h.db, "p1", "dest", nil)

	rule := models.NotificationRule{
		Project: "p1", Name: "empty", Event: EvCycleDigest,
		Match: "{}", Action: "message", Target: "dest",
	}
	// No line, no title, no template → empty content.
	_, rec := n.fireRule(rule, EvCycleDigest, "p1", map[string]any{"agent": "notifier"}, false)
	if rec.Outcome != "skipped" {
		t.Fatalf("empty content must be skipped, got outcome=%q err=%q", rec.Outcome, rec.Error)
	}
	msgs, err := h.db.GetInbox("p1", "dest", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("no message should be delivered for empty content, got %d", len(msgs))
	}
}

// TestDoMessage_BlockedWakesDoneDoesNot is the behavioral guarantee, asserted
// through the real wake predicate (UnreadCountForAgent): a task.blocked
// escalation WAKES its recipient (it is tagged "do", overriding the
// type→"none" derive that comms-discipline applies to every notifier message),
// while a P2 task.done announce does NOT wake.
func TestDoMessage_BlockedWakesDoneDoesNot(t *testing.T) {
	h := testHandlers(t)
	n := NewNotifier(h.db, h.registry, h.events)
	mustRegister(t, h.db, "p1", "boss", nil)
	mustRegister(t, h.db, "p1", "lead", nil)

	blocked := models.NotificationRule{
		Project: "p1", Name: "Blocked → manager", Event: EvTaskBlocked,
		Match: "{}", Action: "message", Target: "manager",
		Opts: `{"priority":"P1"}`,
	}
	// worker has no reports_to → escalation falls back to the dispatcher "boss".
	mustRegister(t, h.db, "p1", "worker", nil)
	n.fireRule(blocked, EvTaskBlocked, "p1", map[string]any{"agent": "worker", "line": "blocked: X", "dispatched_by": "boss"}, false)

	got, err := h.db.UnreadCountForAgent("p1", "boss")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("blocked escalation must wake the manager (unread=1), got %d", got)
	}

	done := models.NotificationRule{
		Project: "p1", Name: "auto-done", Event: EvTaskDone,
		Match: "{}", Action: "message", Target: "dispatcher",
		Opts: `{"priority":"P2"}`,
	}
	n.fireRule(done, EvTaskDone, "p1", map[string]any{"agent": "w", "line": "done: X", "dispatched_by": "lead"}, false)

	got, err = h.db.UnreadCountForAgent("p1", "lead")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("done announce must NOT wake the dispatcher (unread=0), got %d", got)
	}
}

// TestDoMessage_OwnerRoutesThroughBuildPayload guards the same routing-key
// strip that broke "dispatcher": buildPayload used to drop payload.owner, so
// the "owner" target (the lead-machine deal-stale re-nudge) resolved to nobody.
// A deal-stale firing must reach the owner-of-record.
func TestDoMessage_OwnerRoutesThroughBuildPayload(t *testing.T) {
	h := testHandlers(t)
	n := NewNotifier(h.db, h.registry, h.events)
	mustRegister(t, h.db, "p1", "deal-owner", nil)

	rule := models.NotificationRule{
		Project: "p1", Name: "Stale deal → owner", Event: EvDealStale,
		Match: "{}", Action: "message", Target: "owner",
	}
	sem := map[string]any{"agent": "donna", "owner": "deal-owner", "line": "stale deal: acme"}
	_, rec := n.fireRule(rule, EvDealStale, "p1", sem, false)
	if rec.Outcome != "ok" {
		t.Fatalf("owner target must resolve, got outcome=%q err=%q", rec.Outcome, rec.Error)
	}
	msgs, err := h.db.GetInbox("p1", "deal-owner", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("owner must receive the re-nudge, got %d messages", len(msgs))
	}
}

// TestEmitTaskEvent_CarriesDispatchedBy locks the MCP-path parity fix: the
// semantic payload must include dispatched_by so dispatcher/manager rules can
// resolve their target.
func TestEmitTaskEvent_CarriesDispatchedBy(t *testing.T) {
	bus := NewEventBus()
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	task := &models.Task{ID: "t1", Title: "x", DispatchedBy: "lead"}
	emitTaskEvent(bus, EvTaskBlocked, "block", "p1", task)

	evt := <-ch
	if evt.Semantic == nil {
		t.Fatal("no semantic payload")
	}
	if evt.Semantic["dispatched_by"] != "lead" {
		t.Fatalf("dispatched_by missing from semantic payload: %v", evt.Semantic)
	}
}

// --- helpers ---

func newEscalationDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.NewTestDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func mustRegister(t *testing.T, database *db.DB, project, name string, reportsTo *string) {
	t.Helper()
	if _, _, err := database.RegisterAgent(project, name, "", "", reportsTo, nil, false, nil, "", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}
