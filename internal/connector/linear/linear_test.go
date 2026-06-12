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
	if len(evts) != 1 || evts[0].Type != "task.in_progress" {
		t.Fatalf("expected 1 task.in_progress event, got %#v", evts)
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		switch {
		case strings.Contains(query, "ActiveCycle"):
			writeData(w, `{"teams":{"nodes":[{"activeCycle":{"id":"cyc-1","name":"Cycle 7","startsAt":"2026-06-01T00:00:00Z","endsAt":"2026-06-14T00:00:00Z"}}]}}`)
		case strings.Contains(query, "CycleIssues"):
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-parent","identifier":"SYN-1","number":1,"title":"Parent","priority":2,"estimate":3,"url":"u1","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"lead","displayName":"Lead"},"labels":{"nodes":[{"name":"x"}]}},
				{"id":"i-child","identifier":"SYN-2","number":2,"title":"Child","priority":3,"url":"u2","state":{"id":"s2","name":"Todo","type":"unstarted"},"parent":{"id":"i-parent"},"labels":{"nodes":[]}}
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
