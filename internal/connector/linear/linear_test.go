package linear

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/config"
	"agent-relay/internal/connector"
	"agent-relay/internal/db"
)

const testSecret = "whsec_test"

func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	database, err := db.NewTestDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func newTestConn(t *testing.T, database *db.DB) *Connector {
	t.Helper()
	c := New(database, config.Config{
		LinearMode:          true,
		LinearAPIKey:        "lin_api_test",
		LinearWebhookSecret: testSecret,
		LinearTeamKey:       "SYN",
	})
	// Pre-seed the viewer id so anti-loop checks don't hit the network.
	c.viewerID = "viewer-self"
	return c
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// issueFixture builds a Linear webhook payload for an Issue event.
func issueFixture(action string, ts int64, actorID string, issue, updatedFrom map[string]any) []byte {
	env := map[string]any{
		"action":           action,
		"type":             "Issue",
		"data":             issue,
		"webhookTimestamp": ts,
		"actor":            map[string]any{"id": actorID, "name": "Human", "type": "user"},
	}
	if updatedFrom != nil {
		env["updatedFrom"] = updatedFrom
	}
	b, _ := json.Marshal(env)
	return b
}

func baseIssue() map[string]any {
	return map[string]any{
		"id":          "issue-uuid-1",
		"identifier":  "SYN-123",
		"number":      123,
		"title":       "Wire the connector",
		"description": "Do the thing",
		"priority":    2, // high -> P1
		"estimate":    5,
		"url":         "https://linear.app/syn/issue/SYN-123",
		"state":       map[string]any{"id": "st-prog", "name": "In Progress", "type": "started"},
		"assignee":    map[string]any{"id": "u1", "name": "lead", "displayName": "Lead"},
		"labels":      []map[string]any{{"name": "backend"}, {"name": "infra"}},
		"cycle":       map[string]any{"id": "cyc-1", "name": "Cycle 7", "startsAt": "2026-06-01T00:00:00Z", "endsAt": "2026-06-14T00:00:00Z"},
	}
}

// --- HMAC verification ---

func TestVerifySignature(t *testing.T) {
	c := newTestConn(t, newTestDB(t))
	now := time.Now().UnixMilli()
	body := issueFixture("update", now, "human-1", baseIssue(), map[string]any{"stateId": "old"})

	t.Run("valid", func(t *testing.T) {
		if err := c.VerifySignature(body, sign(testSecret, body)); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		if err := c.VerifySignature(body, sign("wrong-secret", body)); err == nil {
			t.Fatal("expected signature mismatch error")
		}
	})
	t.Run("stale", func(t *testing.T) {
		old := time.Now().Add(-5 * time.Minute).UnixMilli()
		staleBody := issueFixture("update", old, "human-1", baseIssue(), nil)
		if err := c.VerifySignature(staleBody, sign(testSecret, staleBody)); err == nil {
			t.Fatal("expected stale webhook error")
		}
	})
	t.Run("empty-sig", func(t *testing.T) {
		if err := c.VerifySignature(body, ""); err == nil {
			t.Fatal("expected error on empty signature")
		}
	})
}

// --- payload -> upsert mapping ---

func TestIngestMapping(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	now := time.Now().UnixMilli()
	body := issueFixture("create", now, "human-1", baseIssue(), nil)

	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	task, err := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if err != nil || task == nil {
		t.Fatalf("mirror row not found: %v", err)
	}
	if task.Title != "Wire the connector" {
		t.Errorf("title = %q", task.Title)
	}
	if task.Source != "linear" {
		t.Errorf("source = %q, want linear", task.Source)
	}
	if task.LinearKey == nil || *task.LinearKey != "SYN-123" {
		t.Errorf("linear_key = %v", task.LinearKey)
	}
	if task.Priority != "P1" {
		t.Errorf("priority = %q, want P1", task.Priority)
	}
	if task.Points == nil || *task.Points != 5 {
		t.Errorf("points = %v, want 5", task.Points)
	}
	if task.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", task.Status)
	}
	if task.LinearState == nil || *task.LinearState != "In Progress" {
		t.Errorf("linear_state = %v", task.LinearState)
	}
	if task.Assignee == nil || *task.Assignee != "lead" {
		t.Errorf("assignee = %v, want lead", task.Assignee)
	}
	if task.CycleID == nil || *task.CycleID != "cyc-1" {
		t.Errorf("cycle_id = %v", task.CycleID)
	}
	if task.ExternalURL == nil || *task.ExternalURL != "https://linear.app/syn/issue/SYN-123" {
		t.Errorf("external_url = %v", task.ExternalURL)
	}
	if !strings.Contains(task.Labels, "backend") || !strings.Contains(task.Labels, "infra") {
		t.Errorf("labels = %q", task.Labels)
	}
}

// Update must preserve the relay task id (overlay survival).
func TestIngestUpdatePreservesID(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	now := time.Now().UnixMilli()

	create := issueFixture("create", now, "human-1", baseIssue(), nil)
	if _, err := c.Ingest(create, sign(testSecret, create)); err != nil {
		t.Fatal(err)
	}
	first, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")

	iss := baseIssue()
	iss["title"] = "Renamed"
	upd := issueFixture("update", time.Now().UnixMilli(), "human-1", iss, map[string]any{"title": "Wire the connector"})
	if _, err := c.Ingest(upd, sign(testSecret, upd)); err != nil {
		t.Fatal(err)
	}
	second, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if first.ID != second.ID {
		t.Errorf("task id changed on update: %s -> %s", first.ID, second.ID)
	}
	if second.Title != "Renamed" {
		t.Errorf("title not updated: %q", second.Title)
	}
}

// Done echo stamps the overlay done_at.
func TestIngestDoneEcho(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	iss := baseIssue()
	iss["state"] = map[string]any{"id": "st-done", "name": "Done", "type": "completed"}
	body := issueFixture("update", time.Now().UnixMilli(), "human-1", iss, map[string]any{"stateId": "st-prog"})
	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatal(err)
	}
	task, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if task.DoneAt == nil || *task.DoneAt == "" {
		t.Errorf("done_at not stamped on completed echo")
	}
	if task.Status != "done" {
		t.Errorf("status = %q, want done", task.Status)
	}
}

// --- state-type mapping ---

func TestMapStateType(t *testing.T) {
	c := newTestConn(t, newTestDB(t))
	cases := map[string]string{
		"backlog":   "pending",
		"unstarted": "pending",
		"started":   "in-progress",
		"completed": "done",
		"canceled":  "cancelled",
		"weird":     "pending",
	}
	for in, want := range cases {
		if got := c.MapState(in); got != want {
			t.Errorf("MapState(%q) = %q, want %q", in, got, want)
		}
	}
	// In Review (started + review name) maps to the in-review column.
	if got := mapStatus(&stateInfo{Type: "started", Name: "In Review"}); got != "in-review" {
		t.Errorf("mapStatus(In Review) = %q, want in-review", got)
	}
}

func TestMapPriority(t *testing.T) {
	cases := map[int]string{0: "P2", 1: "P0", 2: "P1", 3: "P2", 4: "P3"}
	for in, want := range cases {
		if got := mapPriority(float64(in)); got != want {
			t.Errorf("mapPriority(%d) = %q, want %q", in, got, want)
		}
	}
}

// --- dispatch dedupe (FR-3) ---

func TestDispatchDedupe(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	// (1) update into started with agent assignee + state change -> 1 event.
	body := issueFixture("update", time.Now().UnixMilli(), "human-1", baseIssue(), map[string]any{"stateId": "st-old"})
	evts, err := c.Ingest(body, sign(testSecret, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 1 || evts[0].Type != "task.dispatched" {
		t.Fatalf("expected 1 task.dispatched event, got %#v", evts)
	}
	if evts[0].Payload["assignee_is_agent"] != true {
		t.Errorf("assignee_is_agent should be true")
	}

	// (2) same started state but no updatedFrom state change -> no event (dedupe).
	body2 := issueFixture("update", time.Now().UnixMilli(), "human-1", baseIssue(), map[string]any{"title": "x"})
	evts2, _ := c.Ingest(body2, sign(testSecret, body2))
	if len(evts2) != 0 {
		t.Errorf("expected no event without state change, got %d", len(evts2))
	}

	// (3) In Review (started + review) -> no dispatch.
	iss := baseIssue()
	iss["state"] = map[string]any{"id": "st-rev", "name": "In Review", "type": "started"}
	body3 := issueFixture("update", time.Now().UnixMilli(), "human-1", iss, map[string]any{"stateId": "st-prog"})
	evts3, _ := c.Ingest(body3, sign(testSecret, body3))
	if len(evts3) != 0 {
		t.Errorf("expected no dispatch for In Review, got %d", len(evts3))
	}

	// (4) started but no assignee -> no dispatch.
	iss4 := baseIssue()
	delete(iss4, "assignee")
	body4 := issueFixture("update", time.Now().UnixMilli(), "human-1", iss4, map[string]any{"stateId": "st-old"})
	evts4, _ := c.Ingest(body4, sign(testSecret, body4))
	if len(evts4) != 0 {
		t.Errorf("expected no dispatch without assignee, got %d", len(evts4))
	}
}

// --- anti-loop drop (FR-7) ---

func TestAntiLoopDrop(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database) // viewerID = "viewer-self"

	body := issueFixture("update", time.Now().UnixMilli(), "viewer-self", baseIssue(), map[string]any{"stateId": "st-old"})
	evts, err := c.Ingest(body, sign(testSecret, body))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(evts) != 0 {
		t.Errorf("expected self-authored event dropped, got %d events", len(evts))
	}
	// And the mirror must NOT have been written from our own echo.
	if task, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1"); task != nil {
		t.Errorf("self-authored webhook should not upsert the mirror")
	}
}

// --- reconcile upsert path (stubbed GraphQL) ---

func TestReconcileCycle(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	// Issues must be in a ROUTED project to be mirrored (scope guard).
	database.SetSetting("linear_routing", `{"proj-1":"lead"}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		switch {
		case strings.Contains(query, "TeamOpenIssues"):
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-parent","identifier":"SYN-1","number":1,"title":"Parent","priority":2,"estimate":3,"url":"u1","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"lead","displayName":"Lead"},"project":{"id":"proj-1","name":"wrai.th"},"labels":{"nodes":[{"name":"x"}]}},
				{"id":"i-child","identifier":"SYN-2","number":2,"title":"Child","priority":3,"url":"u2","state":{"id":"s2","name":"Todo","type":"unstarted"},"parent":{"id":"i-parent"},"project":{"id":"proj-1","name":"wrai.th"},"labels":{"nodes":[]}}
			]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	n, err := c.ReconcileCycle(c.project)
	if err != nil {
		t.Fatalf("ReconcileCycle: %v", err)
	}
	if n != 2 {
		t.Fatalf("upserted = %d, want 2", n)
	}
	parent, _ := database.GetTaskByLinearIssueID(c.project, "i-parent")
	child, _ := database.GetTaskByLinearIssueID(c.project, "i-child")
	if parent == nil || child == nil {
		t.Fatal("expected both issues mirrored")
	}
	// Hierarchy: child.parent_task_id resolves to the parent's relay id (pass 2).
	if child.ParentTaskID == nil || *child.ParentTaskID != parent.ID {
		t.Errorf("child parent_task_id = %v, want %s", child.ParentTaskID, parent.ID)
	}
	if c.lastReconcileAt.Load() == 0 {
		t.Errorf("lastReconcileAt not stamped")
	}
}

// Delegate-based routing: an issue in an UNROUTED project but directly assigned
// to an agent must still be mirrored AND dispatched on the poll path (the only
// path on a webhook-less localhost). A project-less, unassigned onboarding issue
// must still be skipped as noise. Guards the scope-gate/dispatch-gate symmetry.
func TestReconcileDispatchUnroutedAgentAssignee(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	// No linear_routing entry for the growth project — routing is per-issue
	// assignee (delegate), not a fixed project→agent map.
	var dispatched []string
	c.SetEventSink(func(e connector.TaskEvent) {
		if e.Type == "task.dispatched" {
			dispatched = append(dispatched, e.Agent)
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		switch {
		case strings.Contains(query, "TeamOpenIssues"):
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-growth","identifier":"TSU-97","number":97,"title":"GEO readout","priority":1,"url":"u1","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"analytics-lead","displayName":"analytics-lead"},"project":{"id":"proj-growth","name":"growth"},"labels":{"nodes":[]}},
				{"id":"i-onboard","identifier":"TSU-1","number":1,"title":"Onboarding","priority":3,"url":"u2","state":{"id":"s2","name":"In Progress","type":"started"},"labels":{"nodes":[]}}
			]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	n, err := c.ReconcileCycle(c.project)
	if err != nil {
		t.Fatalf("ReconcileCycle: %v", err)
	}
	// Only the agent-assigned issue is in scope; the project-less unassigned
	// onboarding issue is skipped.
	if n != 1 {
		t.Fatalf("upserted = %d, want 1 (agent-assigned only)", n)
	}
	if task, _ := database.GetTaskByLinearIssueID(c.project, "i-growth"); task == nil {
		t.Fatal("agent-assigned unrouted issue should be mirrored")
	}
	if task, _ := database.GetTaskByLinearIssueID(c.project, "i-onboard"); task != nil {
		t.Error("project-less unassigned issue should be skipped as noise")
	}
	// Dispatch fired to the per-issue delegate, not a fixed project route.
	if len(dispatched) != 1 || dispatched[0] != "analytics-lead" {
		t.Errorf("dispatched = %v, want [analytics-lead]", dispatched)
	}
}

// Delegate-preferred routing: Linear assigns agents via Issue.delegate while the
// human stays the assignee. An issue with a HUMAN assignee + an AGENT delegate
// must dispatch to the DELEGATE, not the human.
func TestReconcileDispatchDelegatePreferred(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	var dispatched []string
	c.SetEventSink(func(e connector.TaskEvent) {
		if e.Type == "task.dispatched" {
			dispatched = append(dispatched, e.Agent)
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		if strings.Contains(query, "TeamOpenIssues") {
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-deleg","identifier":"TSU-99","number":99,"title":"Delegated","priority":1,"url":"u1","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u-h","name":"loicmancino.work","displayName":"loicmancino.work"},"delegate":{"id":"u-a","name":"content-lead","displayName":"content-lead"},"project":{"id":"proj-growth","name":"growth"},"labels":{"nodes":[]}}
			]}}`)
			return
		}
		writeData(w, `{}`)
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("ReconcileCycle: %v", err)
	}
	if len(dispatched) != 1 || dispatched[0] != "content-lead" {
		t.Errorf("dispatched = %v, want [content-lead] (delegate beats human assignee)", dispatched)
	}
}

// Guard: if Linear's schema rejects the delegate field, the poll must latch a
// fallback to the delegate-less query and keep working (dispatch on assignee) —
// a missing field can NEVER 400 the whole poll (no fleet-wide dispatch outage).
func TestReconcileDelegateFieldFallback(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	var dispatched []string
	c.SetEventSink(func(e connector.TaskEvent) {
		if e.Type == "task.dispatched" {
			dispatched = append(dispatched, e.Agent)
		}
	})

	var delegateAttempts, fallbackAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		switch {
		case strings.Contains(query, "delegate"):
			// Schema without the field — GraphQL field error.
			delegateAttempts++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errors":[{"message":"Cannot query field \"delegate\" on type \"Issue\"."}]}`))
		case strings.Contains(query, "TeamOpenIssues"):
			// Fallback (delegate-less) query — agent assignee, dispatches fine.
			fallbackAttempts++
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-fb","identifier":"TSU-100","number":100,"title":"Fallback","priority":1,"url":"u1","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"analytics-lead","displayName":"analytics-lead"},"project":{"id":"proj-growth","name":"growth"},"labels":{"nodes":[]}}
			]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	n, err := c.ReconcileCycle(c.project)
	if err != nil {
		t.Fatalf("ReconcileCycle must survive a bad delegate field, got: %v", err)
	}
	if n != 1 {
		t.Fatalf("upserted = %d, want 1 (fallback query succeeded)", n)
	}
	if delegateAttempts != 1 || fallbackAttempts != 1 {
		t.Errorf("attempts: delegate=%d fallback=%d, want 1/1", delegateAttempts, fallbackAttempts)
	}
	if !c.gql.delegateUnsupported.Load() {
		t.Error("delegateUnsupported should latch true after a field error")
	}
	if len(dispatched) != 1 || dispatched[0] != "analytics-lead" {
		t.Errorf("dispatched = %v, want [analytics-lead] (assignee fallback)", dispatched)
	}
}

// --- writer retry/backoff (stubbed GraphQL) ---

func TestPushInReviewRetry(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	c.reviewState = "state-review" // skip the states lookup

	var updateAttempts, commentCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		switch {
		case strings.Contains(query, "IssueUpdate"):
			updateAttempts++
			if updateAttempts < 2 {
				// First attempt fails (server error) -> exercise retry.
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			writeData(w, `{"issueUpdate":{"success":true}}`)
		case strings.Contains(query, "CommentCreate"):
			commentCalls++
			writeData(w, `{"commentCreate":{"success":true}}`)
		default:
			writeData(w, `{}`)
		}
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	if err := c.PushInReview("issue-uuid-1", "PR up: https://github.com/x/y/pull/1"); err != nil {
		t.Fatalf("PushInReview: %v", err)
	}
	if updateAttempts < 2 {
		t.Errorf("expected retry (>=2 attempts), got %d", updateAttempts)
	}
	if commentCalls != 1 {
		t.Errorf("expected 1 comment, got %d", commentCalls)
	}
	if c.writerFailures.Load() != 0 {
		t.Errorf("writerFailures = %d, want 0 (eventual success)", c.writerFailures.Load())
	}

	// Verify the audit log captured the outcomes.
	entries, err := database.RecentLinearSync(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Errorf("expected sync log entries")
	}
}

func TestPushInReviewExhausted(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	c.reviewState = "state-review"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "always down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	if err := c.PushInReview("issue-uuid-1", "x"); err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if c.writerFailures.Load() == 0 {
		t.Errorf("writerFailures should be incremented on exhaustion")
	}
}

// --- helpers ---

func readQuery(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Query
}

func writeData(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":` + data + `}`))
}

func TestIsTerminalOrActive(t *testing.T) {
	for _, s := range []string{"in-progress", "done", "cancelled"} {
		if !isTerminalOrActive(s) {
			t.Errorf("isTerminalOrActive(%q) = false, want true (must not re-dispatch)", s)
		}
	}
	for _, s := range []string{"pending", "accepted", "blocked", ""} {
		if isTerminalOrActive(s) {
			t.Errorf("isTerminalOrActive(%q) = true, want false (still dispatchable)", s)
		}
	}
}

// TestReconcileNoResurrectTerminal guards the phantom-stale fix: once a mirror
// task is completed in the relay, a reconcile poll that still sees the Linear
// issue in a started state (its PR wasn't auto-closed) must NOT re-dispatch it.
func TestReconcileNoResurrectTerminal(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)

	var dispatched []string
	c.SetEventSink(func(e connector.TaskEvent) {
		if e.Type == "task.dispatched" {
			dispatched = append(dispatched, e.Agent)
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(readQuery(r), "TeamOpenIssues") {
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-term","identifier":"TSU-98","number":98,"title":"merged work","priority":1,"url":"u1","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"analytics-lead","displayName":"analytics-lead"},"project":{"id":"proj-growth","name":"growth"},"labels":{"nodes":[]}}
			]}}`)
			return
		}
		writeData(w, `{}`)
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	// First poll: dispatches once, mirror is created in-progress.
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("ReconcileCycle #1: %v", err)
	}
	if len(dispatched) != 1 {
		t.Fatalf("first poll dispatched = %v, want 1", dispatched)
	}

	// Agent finishes the work → relay task done (the Linear issue still lags in
	// the started state).
	task, _ := database.GetTaskByLinearIssueID(c.project, "i-term")
	if task == nil {
		t.Fatal("mirror task not created")
	}
	if _, err := database.CompleteTask(task.ID, "analytics-lead", c.project, nil); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	// Second poll: the issue is STILL started, but the mirror is terminal — it
	// must NOT be resurrected (this was the phantom claim+start re-fire).
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("ReconcileCycle #2: %v", err)
	}
	if len(dispatched) != 1 {
		t.Errorf("after completion, dispatched = %v, want still 1 (no resurrection)", dispatched)
	}
}

// TestReconcileDropoutSync covers TSU-159: an issue that moves to Done in Linear
// drops out of the OPEN poll, so its mirror must be closed via the by-id state
// fetch — not left active forever firing phantom stale-escalations.
func TestReconcileDropoutSync(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	c.SetEventSink(func(e connector.TaskEvent) {})

	// open=true → the issue is in the open poll (started, agent-assigned).
	// done=true → open poll is empty; IssuesByIDs reports it completed.
	var done bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := readQuery(r)
		switch {
		case strings.Contains(q, "TeamOpenIssues"):
			if done {
				writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}`)
				return
			}
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-drop","identifier":"TSU-99","number":99,"title":"finish me","priority":1,"url":"u1","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"analytics-lead","displayName":"analytics-lead"},"project":{"id":"proj-growth","name":"growth"},"labels":{"nodes":[]}}
			]}}`)
		case strings.Contains(q, "IssuesByIDs"):
			// The dropped issue is now completed in Linear.
			writeData(w, `{"issues":{"nodes":[{"id":"i-drop","state":{"id":"s9","name":"Done","type":"completed"}}]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	// Poll 1: issue open → mirror created, active.
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("ReconcileCycle #1: %v", err)
	}
	task, _ := database.GetTaskByLinearIssueID(c.project, "i-drop")
	if task == nil {
		t.Fatal("mirror not created on first poll")
	}
	if task.Status == "done" || task.Status == "cancelled" {
		t.Fatalf("mirror should be active after poll 1, got %q", task.Status)
	}

	// Issue moved to Done in Linear → drops out of the open poll.
	done = true
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("ReconcileCycle #2: %v", err)
	}
	task, _ = database.GetTaskByLinearIssueID(c.project, "i-drop")
	if task == nil {
		t.Fatal("mirror disappeared after dropout sync")
	}
	if task.Status != "done" {
		t.Fatalf("dropped-out Done issue: mirror status = %q, want done", task.Status)
	}
}

// TestIngestFullyTypedIssueRendersTyped is the positive control for the
// Linear→task metadata carry (TSU linear-mirror ticket): a FULLY-TYPED issue —
// real title, a Goal, ≥1 Acceptance Criterion, a DoD, and an agent lead — must
// mirror as a typed, routable task. Asserting the happy path proves a later
// "0 untitled / 0 unrouted" result comes from the carry actually working, not
// from the guard never running against real data.
func TestIngestFullyTypedIssueRendersTyped(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	now := time.Now().UnixMilli()

	iss := baseIssue()
	iss["id"] = "issue-typed-1"
	iss["identifier"] = "SYN-777"
	iss["number"] = 777
	iss["title"] = "Carry Linear metadata into the mirror"
	iss["description"] = "## Goal\nMirror keeps the ticket typed.\n\n" +
		"## Acceptance Criteria\n- title survives\n- goal survives\n- dod survives\n\n" +
		"## DoD\nAll three sections round-trip to the task."
	// assignee "lead" is an agent -> the resolved routing lane.
	body := issueFixture("create", now, "human-1", iss, nil)

	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	task, err := database.GetTaskByLinearIssueID(c.project, "issue-typed-1")
	if err != nil || task == nil {
		t.Fatalf("mirror row not found: %v", err)
	}

	// Title = the Linear title — never "Untitled task {id}".
	if task.Title != "Carry Linear metadata into the mirror" {
		t.Errorf("title = %q, want the Linear title", task.Title)
	}
	if strings.Contains(strings.ToLower(task.Title), "untitled") {
		t.Errorf("title fell back to a placeholder: %q", task.Title)
	}
	// linear_key populated (traceable back to Linear).
	if task.LinearKey == nil || *task.LinearKey != "SYN-777" {
		t.Errorf("linear_key = %v, want SYN-777", task.LinearKey)
	}
	// Typed: goal + acceptance_criteria + DoD all carried.
	if task.Goal != "Mirror keeps the ticket typed." {
		t.Errorf("goal = %q", task.Goal)
	}
	for _, want := range []string{"title survives", "goal survives", "dod survives"} {
		if !strings.Contains(task.AcceptanceCriteria, want) {
			t.Errorf("acceptance_criteria missing %q: %s", want, task.AcceptanceCriteria)
		}
	}
	if task.AcceptanceCriteria == "[]" || strings.TrimSpace(task.AcceptanceCriteria) == "" {
		t.Errorf("acceptance_criteria rendered empty: %q", task.AcceptanceCriteria)
	}
	if task.Dod != "All three sections round-trip to the task." {
		t.Errorf("dod = %q", task.Dod)
	}
	// Routing lane populated from the resolved lead (assignee "lead").
	if task.ProfileSlug != "lead" {
		t.Errorf("profile_slug = %q, want lead (the routing lane)", task.ProfileSlug)
	}
}

// TestIngestProfileSlugFromProjectRoute checks the routing lane is taken from
// the owner-configured project route when present (it wins over the assignee),
// matching dispatchTarget's precedence.
func TestIngestProfileSlugFromProjectRoute(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	// Route the issue's Linear project to a specific lead.
	database.SetSetting("linear_routing", `{"proj-9":"wraith-backend"}`)
	now := time.Now().UnixMilli()

	iss := baseIssue()
	iss["id"] = "issue-routed-1"
	iss["project"] = map[string]any{"id": "proj-9", "name": "wrai.th"}
	body := issueFixture("create", now, "human-1", iss, nil)

	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	task, err := database.GetTaskByLinearIssueID(c.project, "issue-routed-1")
	if err != nil || task == nil {
		t.Fatalf("mirror row not found: %v", err)
	}
	if task.ProfileSlug != "wraith-backend" {
		t.Errorf("profile_slug = %q, want wraith-backend (project route wins)", task.ProfileSlug)
	}
}

// TestUpsertLinearMirrorProfileSlugNonDestructive guards that a content re-sync
// never blanks a routing lane that a relay reassignment already set: an update
// carrying an empty ProfileSlug must leave the stored slug intact.
func TestUpsertLinearMirrorProfileSlugNonDestructive(t *testing.T) {
	database := newTestDB(t)

	// First sync sets the lane.
	if _, _, err := database.UpsertLinearMirror(db.LinearMirrorSeed{
		Project: "default", LinearIssueID: "iss-keep", Title: "T",
		ProfileSlug: "wraith-backend", Status: "pending",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A later content re-sync with no target must not blank it.
	if _, _, err := database.UpsertLinearMirror(db.LinearMirrorSeed{
		Project: "default", LinearIssueID: "iss-keep", Title: "T2",
		ProfileSlug: "", Status: "in-progress",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	task, err := database.GetTaskByLinearIssueID("default", "iss-keep")
	if err != nil || task == nil {
		t.Fatalf("row: %v", err)
	}
	if task.ProfileSlug != "wraith-backend" {
		t.Errorf("profile_slug = %q, want it preserved as wraith-backend", task.ProfileSlug)
	}
}

// TestIngestMultiProjectRouting is the core of the multi-project mirror: two
// relay projects share one Linear team. linear_project_map routes each Linear
// project's issues into its own relay project's mirror; an unmapped project
// falls back to the connector's default project. Before this, every mirror row
// landed under c.project regardless of the Linear project — the other relay
// project's queue never saw its tasks.
func TestIngestMultiProjectRouting(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	// Both Linear projects are routed to an agent (scope guard) AND mapped to
	// their own relay project.
	database.SetSetting("linear_routing", `{"lp-a":"lead-a","lp-b":"lead-b"}`)
	database.SetSetting("linear_project_map", `{"lp-a":"relay-a","lp-b":"relay-b"}`)
	now := time.Now().UnixMilli()

	ingest := func(id, projectID string) {
		iss := baseIssue()
		iss["id"] = id
		iss["project"] = map[string]any{"id": projectID, "name": projectID}
		body := issueFixture("create", now, "human-1", iss, nil)
		if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
			t.Fatalf("Ingest %s: %v", id, err)
		}
	}
	ingest("iss-a", "lp-a")
	ingest("iss-b", "lp-b")

	// iss-a mirrors into relay-a only.
	if task, _ := database.GetTaskByLinearIssueID("relay-a", "iss-a"); task == nil {
		t.Error("iss-a not mirrored into relay-a")
	}
	if task, _ := database.GetTaskByLinearIssueID("relay-b", "iss-a"); task != nil {
		t.Error("iss-a leaked into relay-b")
	}
	if task, _ := database.GetTaskByLinearIssueID(c.project, "iss-a"); task != nil {
		t.Error("iss-a leaked into the default project")
	}
	// iss-b mirrors into relay-b only.
	if task, _ := database.GetTaskByLinearIssueID("relay-b", "iss-b"); task == nil {
		t.Error("iss-b not mirrored into relay-b")
	}
}

// TestIngestUnmappedProjectFallsBackToDefault checks an issue whose Linear
// project has no linear_project_map entry lands in the connector's default
// project (backward-compatible with the single-project mirror).
func TestIngestUnmappedProjectFallsBackToDefault(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	database.SetSetting("linear_routing", `{"lp-x":"lead-x"}`)
	database.SetSetting("linear_project_map", `{"lp-a":"relay-a"}`) // no entry for lp-x
	now := time.Now().UnixMilli()

	iss := baseIssue()
	iss["id"] = "iss-x"
	iss["project"] = map[string]any{"id": "lp-x", "name": "lp-x"}
	body := issueFixture("create", now, "human-1", iss, nil)
	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if task, _ := database.GetTaskByLinearIssueID(c.project, "iss-x"); task == nil {
		t.Errorf("unmapped project issue did not fall back to default project %q", c.project)
	}
	if task, _ := database.GetTaskByLinearIssueID("relay-a", "iss-x"); task != nil {
		t.Error("unmapped issue leaked into a mapped project")
	}
}

// TestReconcileMultiProjectDropoutSweep proves the reconcile poll heals AND
// closes mirrors across every mapped relay project, not just the default one.
// A mirror living in a mapped project that drops out of the open set (issue
// moved to Done in Linear) must be closed by the dropout sweep — before the
// multi-project sweep it was invisible and stayed active forever.
func TestReconcileMultiProjectDropoutSweep(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	database.SetSetting("linear_routing", `{"lp-a":"lead-a"}`)
	database.SetSetting("linear_project_map", `{"lp-a":"relay-a"}`)

	open := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		switch {
		case strings.Contains(query, "TeamOpenIssues"):
			if open {
				writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
					{"id":"i-a","identifier":"SYN-9","number":9,"title":"A","priority":2,"url":"u","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"lead-a","displayName":"Lead A"},"project":{"id":"lp-a","name":"relay-a"},"labels":{"nodes":[]}}
				]}}`)
			} else {
				writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}`)
			}
		case strings.Contains(query, "IssuesByIDs"):
			// The dropped issue is now Done in Linear.
			writeData(w, `{"issues":{"nodes":[{"id":"i-a","state":{"id":"s9","name":"Done","type":"completed"}}]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	// Poll 1: i-a is open and mirrors into relay-a (the mapped project).
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	task, _ := database.GetTaskByLinearIssueID("relay-a", "i-a")
	if task == nil {
		t.Fatal("i-a not mirrored into mapped project relay-a")
	}

	// Poll 2: i-a dropped out of the open set (moved to Done). The sweep must
	// find its mirror in relay-a and close it.
	open = false
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	closed, _ := database.GetTaskByLinearIssueID("relay-a", "i-a")
	if closed == nil {
		t.Fatal("mirror vanished")
	}
	if closed.Status != "done" {
		t.Errorf("mapped-project mirror status = %q, want done (dropout sweep must cover mapped projects)", closed.Status)
	}
}

// TestReconcileFanOutDedupOnRepoll proves the poll path (ReconcileCycle) fans
// an open issue out to every mapped relay project AND stays idempotent per
// (linear_issue_id, relay_project) across repeated polls of the same still-open
// issue — no duplicate task rows, same identity every cycle.
func TestReconcileFanOutDedupOnRepoll(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	database.SetSetting("linear_routing", `{"lp-fanout":"lead-a"}`)
	database.SetSetting("linear_project_map", `{"lp-fanout": ["relay-a", "relay-b"]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		switch {
		case strings.Contains(query, "TeamOpenIssues"):
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-fanout","identifier":"SYN-11","number":11,"title":"Fan out","priority":2,"url":"u","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"lead-a","displayName":"Lead A"},"project":{"id":"lp-fanout","name":"lp-fanout"},"labels":{"nodes":[]}}
			]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
	defer srv.Close()
	c.gql.url = srv.URL

	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	firstA, _ := database.GetTaskByLinearIssueID("relay-a", "i-fanout")
	firstB, _ := database.GetTaskByLinearIssueID("relay-b", "i-fanout")
	if firstA == nil || firstB == nil {
		t.Fatalf("poll 1 must mirror into both relay-a and relay-b (got a=%v b=%v)", firstA, firstB)
	}
	if firstA.LinearSecondary {
		t.Error("relay-a (index 0) must be the primary mirror")
	}
	if !firstB.LinearSecondary {
		t.Error("relay-b (index 1) must be a secondary mirror")
	}

	// Poll again: the issue is still open and unchanged — must NOT create a
	// second row per project.
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("reconcile 2 (repoll): %v", err)
	}
	secondA, _ := database.GetTaskByLinearIssueID("relay-a", "i-fanout")
	secondB, _ := database.GetTaskByLinearIssueID("relay-b", "i-fanout")
	if secondA == nil || secondA.ID != firstA.ID {
		t.Errorf("relay-a mirror identity changed on repoll (got %v, want %s)", secondA, firstA.ID)
	}
	if secondB == nil || secondB.ID != firstB.ID {
		t.Errorf("relay-b mirror identity changed on repoll (got %v, want %s)", secondB, firstB.ID)
	}
}

// --- linear_project_map fan-out (one Linear project -> N relay projects) ---

// TestParseProjectMapStringAndArray covers ParseProjectMap's back-compat
// (plain string = one target) and new (array = several targets) value forms,
// side by side in the same map, plus malformed-entry tolerance.
func TestParseProjectMapStringAndArray(t *testing.T) {
	raw := `{
		"lp-single": "relay-a",
		"lp-fanout": ["relay-a", "RELAY-B", " relay-c "],
		"lp-dup":    ["relay-x", "relay-x", ""],
		"lp-bad":    123,
		"lp-empty":  []
	}`
	m := ParseProjectMap(raw)

	if got := m["lp-single"]; len(got) != 1 || got[0] != "relay-a" {
		t.Errorf("lp-single = %v, want [relay-a]", got)
	}
	if got := m["lp-fanout"]; len(got) != 3 || got[0] != "relay-a" || got[1] != "relay-b" || got[2] != "relay-c" {
		t.Errorf("lp-fanout = %v, want [relay-a relay-b relay-c] (lowercased+trimmed, order preserved)", got)
	}
	if got := m["lp-dup"]; len(got) != 1 || got[0] != "relay-x" {
		t.Errorf("lp-dup = %v, want [relay-x] (de-duplicated, blank dropped)", got)
	}
	if _, ok := m["lp-bad"]; ok {
		t.Error("lp-bad (non-string/array JSON value) should be dropped, not present")
	}
	if _, ok := m["lp-empty"]; ok {
		t.Error("lp-empty (array with no usable entries) should be dropped, not present")
	}

	if ParseProjectMap("") != nil {
		t.Error("empty setting should parse to nil")
	}
	if ParseProjectMap("not json") != nil {
		t.Error("malformed top-level JSON should parse to nil, not error/panic")
	}
}

// TestIngestFanOutTwoProjects is the core of the fan-out feature: a single
// Linear project mapped to an ARRAY mirrors its issue into every listed relay
// project, index 0 is the primary (write-back-driving) mirror.
func TestIngestFanOutTwoProjects(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	database.SetSetting("linear_routing", `{"lp-fanout":"lead-a"}`)
	database.SetSetting("linear_project_map", `{"lp-fanout": ["relay-a", "relay-b"]}`)
	now := time.Now().UnixMilli()

	iss := baseIssue()
	iss["id"] = "iss-fanout"
	iss["project"] = map[string]any{"id": "lp-fanout", "name": "lp-fanout"}
	body := issueFixture("create", now, "human-1", iss, nil)
	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	taskA, err := database.GetTaskByLinearIssueID("relay-a", "iss-fanout")
	if err != nil || taskA == nil {
		t.Fatalf("issue not mirrored into relay-a: %v", err)
	}
	taskB, err := database.GetTaskByLinearIssueID("relay-b", "iss-fanout")
	if err != nil || taskB == nil {
		t.Fatalf("issue not mirrored into relay-b: %v", err)
	}
	if taskA.ID == taskB.ID {
		t.Error("relay-a and relay-b mirrors must be distinct task rows")
	}
	if taskA.LinearSecondary {
		t.Error("relay-a (index 0 of the fan-out list) must be the PRIMARY mirror (LinearSecondary=false)")
	}
	if !taskB.LinearSecondary {
		t.Error("relay-b (index 1) must be a SECONDARY mirror (LinearSecondary=true) — it must not drive Linear write-back")
	}
	// Never leaks into the connector's default project.
	if leaked, _ := database.GetTaskByLinearIssueID(c.project, "iss-fanout"); leaked != nil {
		t.Error("fanned-out issue leaked into the connector's default project")
	}
}

// TestIngestFanOutDedupOnReplay proves a re-ingest of the same issue (a
// duplicate webhook delivery, or the reconcile poll re-seeing an already-open
// issue) never creates a second task row per (linear_issue_id, relay_project)
// — UpsertLinearMirror's existing per-project lookup is the dedup key, and the
// fan-out loop must preserve it for every target, not just the first.
func TestIngestFanOutDedupOnReplay(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	database.SetSetting("linear_routing", `{"lp-fanout":"lead-a"}`)
	database.SetSetting("linear_project_map", `{"lp-fanout": ["relay-a", "relay-b"]}`)
	now := time.Now().UnixMilli()

	iss := baseIssue()
	iss["id"] = "iss-replay"
	iss["project"] = map[string]any{"id": "lp-fanout", "name": "lp-fanout"}
	body := issueFixture("create", now, "human-1", iss, nil)

	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest 1: %v", err)
	}
	firstA, _ := database.GetTaskByLinearIssueID("relay-a", "iss-replay")
	firstB, _ := database.GetTaskByLinearIssueID("relay-b", "iss-replay")

	// Replay the identical webhook (same content).
	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest 2 (replay): %v", err)
	}
	secondA, err := database.GetTaskByLinearIssueID("relay-a", "iss-replay")
	if err != nil || secondA == nil || secondA.ID != firstA.ID {
		t.Errorf("relay-a mirror row identity changed on replay (got %v, want %s) — dedup broke", secondA, firstA.ID)
	}
	secondB, err := database.GetTaskByLinearIssueID("relay-b", "iss-replay")
	if err != nil || secondB == nil || secondB.ID != firstB.ID {
		t.Errorf("relay-b mirror row identity changed on replay (got %v, want %s) — dedup broke", secondB, firstB.ID)
	}
}
