package linear

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/connector"
)

// dispatchCount counts task.dispatched events (the webhook path returns events
// rather than calling the sink).
func dispatchCount(evts []connector.TaskEvent) int {
	n := 0
	for _, e := range evts {
		if e.Type == "task.dispatched" {
			n++
		}
	}
	return n
}

// nonConformingPollServer serves a single agent-assigned, non-conforming issue on
// the TeamOpenIssues poll and records every CommentCreate so the anti-spam can be
// asserted across cycles. The issue sits in a started state so, absent refusal, it
// WOULD dispatch — proving the refusal (not a missing target) is what holds it.
func nonConformingPollServer(cr *commentRecorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := readQuery(r)
		switch {
		case strings.Contains(q, "CommentCreate"):
			cr.mu.Lock()
			cr.bodies = append(cr.bodies, q)
			cr.mu.Unlock()
			writeData(w, `{"commentCreate":{"success":true}}`)
		case strings.Contains(q, "TeamOpenIssues"):
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-nc","identifier":"SYN-9","number":9,"title":"No ticket","description":"just do the thing","priority":1,"url":"u","state":{"id":"s1","name":"In Progress","type":"started"},"assignee":{"id":"u1","name":"lead","displayName":"Lead"},"project":{"id":"proj-1","name":"p"},"labels":{"nodes":[]}}
			]}}`)
		default:
			writeData(w, `{}`)
		}
	}))
}

// AC2 + AC3: a non-conforming issue that reaches the relay ONLY via the poll (its
// webhook was missed) is refused loudly ONCE, and every later poll cycle stays
// silent (marker-deduped). The refused row never dispatches and never surfaces as
// a pending/active task — it holds as an explicit "refused" row.
func TestReconcile_TypedTicket_PollOnlyRefusesOnceAndNeverActive(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	database.EnsureProject(c.project)
	if err := database.SetProjectRequiresTypedTicket(c.project, true); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	cr := &commentRecorder{}
	srv := nonConformingPollServer(cr)
	defer srv.Close()
	c.gql.url = srv.URL

	var dispatched int
	c.SetEventSink(func(e connector.TaskEvent) {
		if e.Type == "task.dispatched" {
			dispatched++
		}
	})

	// Three cycles of the same non-conforming issue.
	for cycle := 1; cycle <= 3; cycle++ {
		if _, err := c.ReconcileCycle(c.project); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}

	if cr.count() != 1 {
		t.Errorf("anti-spam: want exactly 1 comment across 3 cycles, got %d", cr.count())
	}
	if dispatched != 0 {
		t.Errorf("refused issue must never dispatch, got %d", dispatched)
	}

	task, _ := database.GetTaskByLinearIssueID(c.project, "i-nc")
	if task == nil {
		t.Fatal("poll refusal must persist a refused mirror row")
	}
	if task.Status != linearRefusedStatus {
		t.Errorf("row status = %q, want %q", task.Status, linearRefusedStatus)
	}
	if task.RefusalNotifiedAt == nil {
		t.Error("refused row must carry the anti-spam marker")
	}

	// AC3: never presented as an active/pending task.
	pending, err := database.ListTasks(c.project, "pending", "", "", "", "", 100, false)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, tk := range pending {
		if tk.LinearIssueID != nil && *tk.LinearIssueID == "i-nc" {
			t.Error("refused row must NOT appear in the pending/active task list")
		}
	}
	// It IS visible under its explicit "refused" status (the other AC3 branch).
	refused, err := database.ListTasks(c.project, linearRefusedStatus, "", "", "", "", 100, false)
	if err != nil {
		t.Fatalf("ListTasks(refused): %v", err)
	}
	found := false
	for _, tk := range refused {
		if tk.LinearIssueID != nil && *tk.LinearIssueID == "i-nc" {
			found = true
		}
	}
	if !found {
		t.Error("refused row should be listable under an explicit 'refused' status")
	}
}

// AC4 (full cycle): refuse → becomes conforming (mirrors + dispatches, marker
// reset) → returns to backlog → becomes non-conforming again → re-notifies EXACTLY
// once. Driven over the webhook path (which returns dispatch events).
func TestIngest_TypedTicket_FullCycle_RefuseTransitionReNotify(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	database.EnsureProject(c.project)
	if err := database.SetProjectRequiresTypedTicket(c.project, true); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	cr := &commentRecorder{}
	srv := cr.server(t)
	defer srv.Close()
	c.gql.url = srv.URL

	todo := map[string]any{"id": "st-todo", "name": "Todo", "type": "unstarted"}
	prog := map[string]any{"id": "st-prog", "name": "In Progress", "type": "started"}
	stateChange := map[string]any{"stateId": "old"} // triggers the dispatch dedupe

	dispatched := 0

	// 1. Non-conforming, in backlog (Todo) — refused, one loud comment, marker set.
	iss := baseIssue()
	iss["state"] = todo
	iss["description"] = "no ticket here"
	b := issueFixture("create", time.Now().UnixMilli(), "human-1", iss, nil)
	evts, err := c.Ingest(b, sign(testSecret, b))
	if err != nil {
		t.Fatalf("step1 Ingest: %v", err)
	}
	dispatched += dispatchCount(evts)
	step1, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if step1 == nil || step1.Status != linearRefusedStatus || step1.RefusalNotifiedAt == nil {
		t.Fatalf("step1: want refused row + marker, got %+v", step1)
	}
	if cr.count() != 1 {
		t.Fatalf("step1: want 1 comment, got %d", cr.count())
	}

	// 2. Becomes conforming AND moves to In Progress — transitions to a normal
	// mirror, marker resets, and it dispatches. No new comment.
	iss["state"] = prog
	iss["description"] = conformingDesc()
	b = issueFixture("update", time.Now().UnixMilli(), "human-1", iss, stateChange)
	evts, err = c.Ingest(b, sign(testSecret, b))
	if err != nil {
		t.Fatalf("step2 Ingest: %v", err)
	}
	dispatched += dispatchCount(evts)
	step2, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if step2.Status == linearRefusedStatus {
		t.Error("step2: conforming issue must leave the refused state")
	}
	if step2.RefusalNotifiedAt != nil {
		t.Error("step2: marker must reset when the issue becomes conforming")
	}
	if dispatched != 1 {
		t.Errorf("step2: want exactly 1 dispatch, got %d", dispatched)
	}
	if cr.count() != 1 {
		t.Errorf("step2: no new comment expected, got %d total", cr.count())
	}

	// 3. Moves back to the backlog (still conforming) — becomes refusable again.
	iss["state"] = todo
	b = issueFixture("update", time.Now().UnixMilli(), "human-1", iss, stateChange)
	if _, err = c.Ingest(b, sign(testSecret, b)); err != nil {
		t.Fatalf("step3 Ingest: %v", err)
	}

	// 4. Loses its ticket again — re-refused, exactly one MORE comment, no extra dispatch.
	iss["description"] = "dropped the ticket"
	b = issueFixture("update", time.Now().UnixMilli(), "human-1", iss, nil)
	evts, err = c.Ingest(b, sign(testSecret, b))
	if err != nil {
		t.Fatalf("step4 Ingest: %v", err)
	}
	dispatched += dispatchCount(evts)
	step4, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if step4.Status != linearRefusedStatus || step4.RefusalNotifiedAt == nil {
		t.Errorf("step4: want refused row + marker, got %+v", step4)
	}
	if cr.count() != 2 {
		t.Errorf("step4: re-non-conformity must re-notify once (2 total), got %d", cr.count())
	}
	if dispatched != 1 {
		t.Errorf("step4: refusal must not dispatch, total dispatch = %d", dispatched)
	}
}
