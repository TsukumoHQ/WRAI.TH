package linear

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-relay/internal/connector"
	"agent-relay/internal/db"
)

// Park P4 / W5 (165dc08e, ruling wraith-park-ruling Q3): Linear shows In
// Progress only when work really started, and a parked mirror stays parked
// whatever Linear does — one deduped "parked on the relay" comment.

type parkLinear struct {
	started  atomic.Bool
	mu       sync.Mutex
	updates  int
	comments []string
}

func (p *parkLinear) server() *httptest.Server {
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
			p.updates++
			p.mu.Unlock()
			writeData(w, `{"issueUpdate":{"success":true}}`)
		case strings.Contains(q, "CommentCreate"):
			p.mu.Lock()
			body, _ := req.Variables["body"].(string)
			p.comments = append(p.comments, body)
			p.mu.Unlock()
			writeData(w, `{"commentCreate":{"success":true}}`)
		case strings.Contains(q, "states"):
			writeData(w, `{"teams":{"nodes":[{"states":{"nodes":[
				{"id":"td","name":"Todo","type":"unstarted"},
				{"id":"ip","name":"In Progress","type":"started"},
				{"id":"dn","name":"Done","type":"completed"}]}}]}}`)
		case strings.Contains(q, "TeamOpenIssues"):
			st := `{"id":"td","name":"Todo","type":"unstarted"}`
			if p.started.Load() {
				st = `{"id":"ip","name":"In Progress","type":"started"}`
			}
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-park","identifier":"SYN-5","number":5,"title":"parked","priority":2,"url":"u","state":`+st+`,"assignee":{"id":"u1","name":"analytics-lead","displayName":"analytics-lead"},"labels":{"nodes":[]}}
			]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
}

func (p *parkLinear) counts() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.updates, append([]string(nil), p.comments...)
}

func TestClaimMirror_NoLinearWrite(t *testing.T) {
	c := newTestConn(t, newTestDB(t))
	pl := &parkLinear{}
	srv := pl.server()
	defer srv.Close()
	c.gql.url = srv.URL
	if err := c.PushStatus("i-park", "accepted", ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := pl.counts(); n != 0 {
		t.Errorf("claim of a pending mirror: %d issueUpdate call(s), want 0", n)
	}
}

func parkedMirror(t *testing.T) (*Connector, *parkLinear, *httptest.Server, string, *int) {
	t.Helper()
	database := newTestDB(t)
	c := newTestConn(t, database)
	dispatched := 0
	c.SetEventSink(func(e connector.TaskEvent) {
		if e.Type == "task.dispatched" {
			dispatched++
		}
	})
	pl := &parkLinear{}
	srv := pl.server()
	c.gql.url = srv.URL
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatal(err)
	}
	m, _ := database.GetTaskByLinearIssueID(c.project, "i-park")
	if m == nil || m.Status != "pending" {
		t.Fatalf("precondition: pending mirror, got %+v", m)
	}
	if _, err := database.ParkTask(c.project, m.ID, "lead", "waiting on the founder", db.ParkUntilFounder, ""); err != nil {
		t.Fatal(err)
	}
	return c, pl, srv, m.ID, &dispatched
}

func TestParkedMirror_PollHoldsAndCommentsOnce(t *testing.T) {
	c, pl, srv, id, dispatched := parkedMirror(t)
	defer srv.Close()
	pl.started.Store(true)
	for i := 0; i < 2; i++ {
		if _, err := c.ReconcileCycle(c.project); err != nil {
			t.Fatal(err)
		}
	}
	if *dispatched != 0 {
		t.Errorf("parked mirror moved to started: %d task.dispatched, want 0", *dispatched)
	}
	_, comments := pl.counts()
	if len(comments) != 1 || !strings.Contains(comments[0], "parked on the relay: waiting on the founder") {
		t.Errorf("want exactly one 'parked on the relay: <reason>' comment over two ticks, got %d: %v", len(comments), comments)
	}
	m, _ := c.db.GetTask(id, c.project)
	if m.Status != "pending" || !c.db.TaskParked(c.project, id) {
		t.Errorf("parked mirror: status %q parked=%v, want still pending and parked", m.Status, c.db.TaskParked(c.project, id))
	}

	// A re-park is a new park: it may comment once more.
	if _, err := c.db.UnparkTask(c.project, id, "lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ParkTask(c.project, id, "lead", "second wait", db.ParkUntilFounder, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatal(err)
	}
	if _, comments = pl.counts(); len(comments) != 2 {
		t.Errorf("after a re-park: %d comments, want 2", len(comments))
	}
}

func TestParkedMirror_WebhookHoldsAndCommentsOnce(t *testing.T) {
	c, pl, srv, _, _ := parkedMirror(t)
	defer srv.Close()
	iss := baseIssue()
	iss["id"] = "i-park"
	iss["state"] = map[string]any{"id": "ip", "name": "In Progress", "type": "started"}
	events := 0
	for i := 0; i < 2; i++ {
		b := issueFixture("update", time.Now().UnixMilli(), "human-1", iss, map[string]any{"stateId": "td"})
		evts, err := c.Ingest(b, sign(testSecret, b))
		if err != nil {
			t.Fatal(err)
		}
		events += len(evts)
	}
	if events != 0 {
		t.Errorf("parked mirror moved to started via webhook: %d event(s), want 0", events)
	}
	if _, comments := pl.counts(); len(comments) != 1 {
		t.Errorf("want exactly one parked comment over two webhooks, got %d", len(comments))
	}
}
