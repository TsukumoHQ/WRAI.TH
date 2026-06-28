package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"agent-relay/internal/models"
)

func TestMatchRule(t *testing.T) {
	cases := []struct {
		name    string
		match   string
		payload map[string]any
		want    bool
	}{
		{"empty matches all", "", map[string]any{"agent": "x"}, true},
		{"empty object matches all", "{}", map[string]any{"agent": "x"}, true},
		{"bool true matches", `{"assignee_is_agent":true}`, map[string]any{"assignee_is_agent": true}, true},
		{"bool true rejects false", `{"assignee_is_agent":true}`, map[string]any{"assignee_is_agent": false}, false},
		{"missing key rejects", `{"assignee_is_agent":true}`, map[string]any{"agent": "x"}, false},
		{"string equality", `{"agent":"alice"}`, map[string]any{"agent": "alice"}, true},
		{"string mismatch", `{"agent":"alice"}`, map[string]any{"agent": "bob"}, false},
		{"number equality (float vs int)", `{"points":3}`, map[string]any{"points": 3}, true},
		{"malformed match is permissive", `{not json`, map[string]any{"agent": "x"}, true},
		{"multi-condition all must pass", `{"agent":"alice","assignee_is_agent":true}`,
			map[string]any{"agent": "alice", "assignee_is_agent": true}, true},
		{"multi-condition one fails", `{"agent":"alice","assignee_is_agent":true}`,
			map[string]any{"agent": "alice", "assignee_is_agent": false}, false},
		// set membership (array value = OR) — scopes the stale-rule to active tasks
		{"array matches member", `{"status":["in-progress","accepted"]}`,
			map[string]any{"status": "in-progress"}, true},
		{"array matches other member", `{"status":["in-progress","accepted"]}`,
			map[string]any{"status": "accepted"}, true},
		{"array rejects non-member (parked Todo)", `{"status":["in-progress","accepted"]}`,
			map[string]any{"status": "pending"}, false},
		{"array + other condition both pass", `{"status":["in-progress","accepted"],"escalate":true}`,
			map[string]any{"status": "in-progress", "escalate": true}, true},
		{"array passes but sibling fails", `{"status":["in-progress","accepted"],"escalate":true}`,
			map[string]any{"status": "in-progress", "escalate": false}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rule := models.NotificationRule{Match: c.match}
			if got := matchRule(rule, c.payload); got != c.want {
				t.Fatalf("matchRule(%q, %v) = %v, want %v", c.match, c.payload, got, c.want)
			}
		})
	}
}

func TestSignBody(t *testing.T) {
	t.Setenv(RelayWebhookSecretEnv, "topsecret")
	body := []byte(`{"agent":"alice","task_id":"t1"}`)
	got := signBody(body)

	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Fatalf("signBody = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(got))
	}
}

func TestSignBodyNoSecret(t *testing.T) {
	_ = os.Unsetenv(RelayWebhookSecretEnv)
	if got := signBody([]byte("x")); got != "" {
		t.Fatalf("expected empty signature with no secret, got %q", got)
	}
}

func TestBuildPayloadTemplate(t *testing.T) {
	n := &Notifier{}
	rule := models.NotificationRule{Event: EvTaskBlocked}
	sem := map[string]any{
		"agent":      "alice",
		"task_id":    "t-42",
		"linear_key": nil,
		"title":      "Fix the bug",
		"line":       "Blocked: Fix the bug",
	}
	opts := ruleOpts{Template: "{agent} is blocked on {title}"}
	got := n.buildPayload(rule, EvTaskBlocked, sem, opts)

	if got["line"] != "alice is blocked on Fix the bug" {
		t.Fatalf("template not applied: %v", got["line"])
	}
	if got["agent"] != "alice" || got["task_id"] != "t-42" {
		t.Fatalf("unexpected payload fields: %v", got)
	}
	if got["linear_key"] != nil {
		t.Fatalf("expected nil linear_key, got %v", got["linear_key"])
	}
}

// TestBuildPayloadEventPassthrough is the TSU-38 guarantee at the build layer:
// a custom event:* carries its full structured payload through, while a built-in
// event does not (it stays a tiny fixed-field payload).
func TestBuildPayloadEventPassthrough(t *testing.T) {
	n := &Notifier{}
	sem := map[string]any{"agent": "donna", "email": "x@y.com", "tier": "A", "score": 91}

	got := n.buildPayload(models.NotificationRule{}, "event:lead-ready", sem, ruleOpts{})
	p, ok := got["payload"].(map[string]any)
	if !ok {
		t.Fatalf("event:* should pass payload through, got %v", got["payload"])
	}
	if p["email"] != "x@y.com" || p["tier"] != "A" {
		t.Fatalf("payload fields not preserved: %v", p)
	}

	// Built-in events stay lean — no payload passthrough.
	builtin := n.buildPayload(models.NotificationRule{}, EvTaskDone, sem, ruleOpts{})
	if _, exists := builtin["payload"]; exists {
		t.Fatalf("built-in event must not carry a payload passthrough, got %v", builtin["payload"])
	}
}

// TestEventPayloadPassthroughToMessage is the end-to-end TSU-38 guarantee: a
// message-action rule firing on a custom event delivers the structured payload
// into the recipient's inbox message body, not an empty body.
func TestEventPayloadPassthroughToMessage(t *testing.T) {
	h := testHandlers(t)
	n := NewNotifier(h.db, h.registry, h.events)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "donna"}))

	rule := models.NotificationRule{
		Project: "p1", Name: "lead-ready→donna", Event: "event:lead-ready",
		Match: "{}", Action: "message", Target: "donna",
	}
	sem := map[string]any{
		"agent": "donna", "email": "lead@acme.com", "tier": "A",
		"personId": "p_123", "score": 91, "bookUrl": "https://cal/x",
	}
	n.fireRule(rule, "event:lead-ready", "p1", sem, false)

	msgs, err := h.db.GetInbox("p1", "donna", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 delivered message, got %d", len(msgs))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(msgs[0].Content), &body); err != nil {
		t.Fatalf("message body is not the JSON payload: %q (%v)", msgs[0].Content, err)
	}
	// The relay normalizes all message-content keys to snake_case
	// (normalize.JSONKeys), so consumers read person_id / book_url, not the
	// emitter's camelCase. Values pass through verbatim.
	if body["email"] != "lead@acme.com" || body["tier"] != "A" ||
		body["person_id"] != "p_123" || body["book_url"] != "https://cal/x" {
		t.Fatalf("structured fields missing from delivered body: %v", body)
	}
}

func TestBuildPayloadDefaultLine(t *testing.T) {
	n := &Notifier{}
	rule := models.NotificationRule{Event: EvTaskDone}
	sem := map[string]any{"agent": "bob", "task_id": "t9", "title": "Ship it"}
	got := n.buildPayload(rule, EvTaskDone, sem, ruleOpts{})
	// No line and no template → falls back to title.
	if got["line"] != "Ship it" {
		t.Fatalf("expected fallback to title, got %v", got["line"])
	}
}

func TestTaskSemantic(t *testing.T) {
	assignee := "agent-x"
	task := &models.Task{ID: "t1", Title: "Do thing", Priority: "P1", AssignedTo: &assignee}
	sem := taskSemantic(task, "In progress: Do thing")
	if sem["task_id"] != "t1" {
		t.Fatalf("task_id wrong: %v", sem["task_id"])
	}
	if sem["assignee_is_agent"] != true {
		t.Fatalf("expected assignee_is_agent true, got %v", sem["assignee_is_agent"])
	}
	if sem["linear_key"] != nil {
		t.Fatalf("expected nil linear_key")
	}

	// human assignee → not an agent
	human := "human"
	task2 := &models.Task{ID: "t2", Title: "X", AssignedTo: &human}
	sem2 := taskSemantic(task2, "line")
	if sem2["assignee_is_agent"] != false {
		t.Fatalf("expected assignee_is_agent false for human")
	}
}

func TestParseOpts(t *testing.T) {
	o := parseOpts(`{"ttl":3600,"priority":"P1","template":"x","interval_hours":8}`)
	if o.TTL != 3600 || o.Priority != "P1" || o.Template != "x" || o.IntervalHours != 8 {
		t.Fatalf("parseOpts wrong: %+v", o)
	}
	empty := parseOpts("")
	if empty.TTL != 0 || empty.Priority != "" {
		t.Fatalf("expected zero opts, got %+v", empty)
	}
}

func TestSyntheticPayload(t *testing.T) {
	d := syntheticPayload(EvCycleDigest)
	if d["line"] == "" {
		t.Fatal("expected non-empty digest sample line")
	}
	g := syntheticPayload(EvTaskInProgress)
	if g["assignee_is_agent"] != true {
		t.Fatal("expected sample task payload to assert assignee_is_agent")
	}
}
