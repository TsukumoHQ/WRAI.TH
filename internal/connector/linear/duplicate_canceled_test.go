package linear

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/connector"
)

// W7 (3bdcb725): work closed as Duplicate / Canceled in Linear never appears
// claimable on the relay. Linear's Duplicate is handled both as its own state
// type ("duplicate") and as a canceled-type state named Duplicate.

var closedStates = []struct{ name, stName, stType string }{
	{"duplicate typed duplicate", "Duplicate", "duplicate"},
	{"duplicate typed canceled", "Duplicate", "canceled"},
	{"canceled", "Canceled", "canceled"},
}

func issueJSON(id, stName, stType string) string {
	return `{"id":"` + id + `","identifier":"SYN-7","number":7,"title":"` + id + `","priority":2,"url":"u","state":{"id":"s","name":"` + stName + `","type":"` + stType + `"},"assignee":{"id":"u1","name":"analytics-lead","displayName":"analytics-lead"},"labels":{"nodes":[]}}`
}

// closedLinear serves issue id in the given state to both the open poll and the
// by-id dropout fetch, as Linear does for a duplicate-typed state that passes
// the open-state filter.
func closedLinear(id, stName, stType string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := readQuery(r)
		switch {
		case strings.Contains(q, "TeamOpenIssues"):
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[`+issueJSON(id, stName, stType)+`]}}`)
		case strings.Contains(q, "IssuesByIDs"):
			writeData(w, `{"issues":{"nodes":[`+issueJSON(id, stName, stType)+`]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
}

func mirrorStatus(t *testing.T, c *Connector, id string) string {
	t.Helper()
	m, err := c.db.GetTaskByLinearIssueID(c.project, id)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		return "<none>"
	}
	return m.Status
}

func TestClosedIssue_FirstSeenNoMirrorNoDispatch(t *testing.T) {
	for _, st := range closedStates {
		t.Run(st.name+"/poll", func(t *testing.T) {
			c := newTestConn(t, newTestDB(t))
			events := 0
			c.SetEventSink(func(connector.TaskEvent) { events++ })
			srv := closedLinear("i-new", st.stName, st.stType)
			defer srv.Close()
			c.gql.url = srv.URL
			if _, err := c.ReconcileCycle(c.project); err != nil {
				t.Fatal(err)
			}
			if s := mirrorStatus(t, c, "i-new"); s == "pending" || s == "in-progress" {
				t.Errorf("first-seen %s issue: live mirror %q, want none", st.name, s)
			}
			if events != 0 {
				t.Errorf("first-seen %s issue: %d dispatch signal(s), want 0", st.name, events)
			}
		})
		t.Run(st.name+"/webhook", func(t *testing.T) {
			c := newTestConn(t, newTestDB(t))
			iss := baseIssue()
			iss["id"] = "i-new"
			iss["state"] = map[string]any{"id": "s", "name": st.stName, "type": st.stType}
			b := issueFixture("create", time.Now().UnixMilli(), "human-1", iss, nil)
			evts, err := c.Ingest(b, sign(testSecret, b))
			if err != nil {
				t.Fatal(err)
			}
			if s := mirrorStatus(t, c, "i-new"); s == "pending" || s == "in-progress" {
				t.Errorf("first-seen %s issue: live mirror %q, want none", st.name, s)
			}
			if len(evts) != 0 {
				t.Errorf("first-seen %s issue: %d event(s), want 0", st.name, len(evts))
			}
		})
	}
}

func TestClosedIssue_LiveMirrorCancelled(t *testing.T) {
	for _, st := range closedStates {
		t.Run(st.name+"/poll", func(t *testing.T) {
			c := newTestConn(t, newTestDB(t))
			seed := c.seedFromIssue(gqlIssue{ID: "i-live", Title: "live", State: &stateInfo{Name: "Todo", Type: "unstarted"}}, c.project, true)
			if _, _, err := c.db.UpsertLinearMirror(seed); err != nil {
				t.Fatal(err)
			}
			srv := closedLinear("i-live", st.stName, st.stType)
			defer srv.Close()
			c.gql.url = srv.URL
			if _, err := c.ReconcileCycle(c.project); err != nil {
				t.Fatal(err)
			}
			if s := mirrorStatus(t, c, "i-live"); s != "cancelled" {
				t.Errorf("live mirror moved to %s: status %q, want cancelled", st.name, s)
			}
		})
		t.Run(st.name+"/webhook", func(t *testing.T) {
			c := newTestConn(t, newTestDB(t))
			iss := baseIssue()
			b := issueFixture("create", time.Now().UnixMilli(), "human-1", iss, nil)
			if _, err := c.Ingest(b, sign(testSecret, b)); err != nil {
				t.Fatal(err)
			}
			iss["state"] = map[string]any{"id": "s", "name": st.stName, "type": st.stType}
			b = issueFixture("update", time.Now().UnixMilli(), "human-1", iss, map[string]any{"stateId": "s-ip"})
			if _, err := c.Ingest(b, sign(testSecret, b)); err != nil {
				t.Fatal(err)
			}
			if s := mirrorStatus(t, c, iss["id"].(string)); s != "cancelled" {
				t.Errorf("live mirror moved to %s: status %q, want cancelled", st.name, s)
			}
		})
	}
}
