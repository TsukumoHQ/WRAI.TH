package linear

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-relay/internal/connector"
	"agent-relay/internal/models"
)

// W8 D4 (ruling wraith-deploying-ruling Q2): a deploying mirror moves to the
// team's state named *deploy*; a team without one keeps the issue In Review
// and gets ONE comment, never In Progress. An issue in Deploying maps to
// deploying and never dispatches an agent.

type deployLinear struct {
	withDeploying bool
	issueState    string // JSON state object of the open issue
	mu            sync.Mutex
	updates       []string // stateId of each issueUpdate
	comments      []string
}

func (p *deployLinear) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(raw, &req)
		q := req.Query
		switch {
		case strings.Contains(q, "IssueUpdate"):
			p.mu.Lock()
			st, _ := req.Variables["stateId"].(string)
			p.updates = append(p.updates, st)
			p.mu.Unlock()
			writeData(w, `{"issueUpdate":{"success":true}}`)
		case strings.Contains(q, "CommentCreate"):
			p.mu.Lock()
			body, _ := req.Variables["body"].(string)
			p.comments = append(p.comments, body)
			p.mu.Unlock()
			writeData(w, `{"commentCreate":{"success":true}}`)
		case strings.Contains(q, "states"):
			deploying := ""
			if p.withDeploying {
				deploying = `{"id":"dep","name":"Deploying","type":"started"},`
			}
			writeData(w, `{"teams":{"nodes":[{"states":{"nodes":[
				{"id":"td","name":"Todo","type":"unstarted"},
				{"id":"ip","name":"In Progress","type":"started"},
				{"id":"rev","name":"In Review","type":"started"},`+deploying+`
				{"id":"dn","name":"Done","type":"completed"}]}}]}}`)
		case strings.Contains(q, "TeamOpenIssues"):
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-dep","identifier":"SYN-8","number":8,"title":"deploying","priority":2,"url":"u","state":`+p.issueState+`,"assignee":{"id":"u1","name":"analytics-lead","displayName":"analytics-lead"},"labels":{"nodes":[]}}
			]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
}

func (p *deployLinear) counts() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.updates...), append([]string(nil), p.comments...)
}

const (
	stateInReview  = `{"id":"rev","name":"In Review","type":"started"}`
	stateDeploying = `{"id":"dep","name":"Deploying","type":"started"}`
)

// AC1: a deploying mirror on a team with a state named *deploy* -> one
// issueUpdate to that state, no comment.
func TestDeployingPush_TeamWithDeployingState(t *testing.T) {
	c := newTestConn(t, newTestDB(t))
	pl := &deployLinear{withDeploying: true, issueState: stateInReview}
	srv := pl.server()
	defer srv.Close()
	c.gql.url = srv.URL
	if err := c.PushStatus("i-dep", "deploying", ""); err != nil {
		t.Fatal(err)
	}
	updates, comments := pl.counts()
	if len(updates) != 1 || updates[0] != "dep" {
		t.Fatalf("issueUpdate calls = %v, want exactly one to dep (Deploying)", updates)
	}
	if len(comments) != 0 {
		t.Fatalf("comments = %v, want none", comments)
	}
	if got := resolveStateID("in-progress", []stateInfo{{ID: "dep", Name: "Deploying", Type: "started"}, {ID: "ip", Name: "In Progress", Type: "started"}}); got != "ip" {
		t.Fatalf("in-progress resolves to %q, want ip (never the Deploying state)", got)
	}
}

// AC2: a team without one -> 0 issueUpdate, exactly one comment; the next
// reconcile tick posts nothing and leaves the mirror deploying (never pulled
// back to in-review by the In Review issue).
func TestDeployingPush_TeamWithoutDeployingState(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	dispatched := 0
	c.SetEventSink(func(e connector.TaskEvent) {
		if e.Type == "task.dispatched" {
			dispatched++
		}
	})
	pl := &deployLinear{issueState: stateInReview}
	srv := pl.server()
	defer srv.Close()
	c.gql.url = srv.URL

	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatal(err)
	}
	m, _ := database.GetTaskByLinearIssueID(c.project, "i-dep")
	if m == nil || m.Status != "in-review" {
		t.Fatalf("precondition: in-review mirror, got %+v", m)
	}
	if _, err := database.DeployTask(m.ID, "user", c.project, "c2c86b52de76"); err != nil {
		t.Fatalf("deploy mirror: %v", err)
	}
	if err := c.PushStatus("i-dep", "deploying", ""); err != nil {
		t.Fatal(err)
	}
	updates, comments := pl.counts()
	if len(updates) != 0 {
		t.Fatalf("issueUpdate calls = %v, want 0 (issue stays In Review, never In Progress)", updates)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "deploying") {
		t.Fatalf("comments = %v, want exactly one deploying note", comments)
	}

	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatal(err)
	}
	updates, comments = pl.counts()
	if len(updates) != 0 || len(comments) != 1 {
		t.Fatalf("after the next tick: updates %v comments %v, want 0 / still 1", updates, comments)
	}
	if m, _ := database.GetTaskByLinearIssueID(c.project, "i-dep"); statusOf(m) != "deploying" {
		t.Fatalf("mirror after the next tick = %s, want deploying", statusOf(m))
	}
	if dispatched != 0 {
		t.Fatalf("dispatched %d, want 0", dispatched)
	}
}

// AC3: an inbound Deploying issue maps to deploying and emits 0
// task.dispatched, via the poll and via the webhook.
func TestDeployingInbound_NeverDispatches(t *testing.T) {
	if got := mapStatus(&stateInfo{ID: "dep", Name: "Deploying", Type: "started"}); got != "deploying" {
		t.Fatalf("mapStatus(Deploying) = %q, want deploying", got)
	}
	database := newTestDB(t)
	c := newTestConn(t, database)
	dispatched := 0
	c.SetEventSink(func(e connector.TaskEvent) {
		if e.Type == "task.dispatched" {
			dispatched++
		}
	})
	pl := &deployLinear{withDeploying: true, issueState: stateDeploying}
	srv := pl.server()
	defer srv.Close()
	c.gql.url = srv.URL
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatal(err)
	}
	if dispatched != 0 {
		t.Fatalf("poll: issue in Deploying dispatched %d time(s), want 0", dispatched)
	}
	if m, _ := database.GetTaskByLinearIssueID(c.project, "i-dep"); statusOf(m) != "deploying" {
		t.Fatalf("mirror status = %s, want deploying", statusOf(m))
	}

	iss := baseIssue()
	iss["assignee"] = map[string]any{"id": "u1", "name": "analytics-lead", "displayName": "analytics-lead"}
	iss["state"] = map[string]any{"id": "dep", "name": "Deploying", "type": "started"}
	body := issueFixture("update", time.Now().UnixMilli(), "human-1", iss, map[string]any{"stateId": "rev"})
	evts, err := c.Ingest(body, sign(testSecret, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evts {
		if e.Type == "task.dispatched" {
			t.Fatalf("webhook into Deploying emitted %v, want no task.dispatched", evts)
		}
	}
}

func statusOf(m *models.Task) string {
	if m == nil {
		return "<missing>"
	}
	return m.Status
}
