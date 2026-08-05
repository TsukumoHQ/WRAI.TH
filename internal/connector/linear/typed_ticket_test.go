package linear

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTicket(t *testing.T) {
	t.Run("conforming", func(t *testing.T) {
		desc := "## Goal\nShip the thing\n\n## Acceptance Criteria\n- builds green\n- refuses without goal\n\n## DoD\ntests pass\n"
		tk := parseTicket(desc)
		if len(tk.missing) != 0 {
			t.Fatalf("missing = %v, want none", tk.missing)
		}
		if tk.goal != "Ship the thing" {
			t.Errorf("goal = %q", tk.goal)
		}
		if tk.dod != "tests pass" {
			t.Errorf("dod = %q", tk.dod)
		}
		if len(tk.acceptance) != 2 || tk.acceptance[0] != "builds green" || tk.acceptance[1] != "refuses without goal" {
			t.Errorf("acceptance = %v", tk.acceptance)
		}
	})

	t.Run("tolerant headers and markers", func(t *testing.T) {
		// case-insensitive, "### " depth, "Definition of Done" synonym, mixed bullets.
		desc := "### goal\ng\n\n### Acceptance criteria\n* one\n1. two\n+ three\n\n### Definition of Done\nd"
		tk := parseTicket(desc)
		if len(tk.missing) != 0 {
			t.Fatalf("missing = %v, want none", tk.missing)
		}
		if len(tk.acceptance) != 3 {
			t.Errorf("acceptance = %v, want 3 items", tk.acceptance)
		}
		if tk.dod != "d" {
			t.Errorf("dod = %q", tk.dod)
		}
	})

	t.Run("names each missing section", func(t *testing.T) {
		// Only DoD present; goal blank body and no acceptance section.
		tk := parseTicket("## Goal\n\n## DoD\ndone")
		if strings.Join(tk.missing, ",") != "goal,acceptance_criteria" {
			t.Errorf("missing = %v, want [goal acceptance_criteria]", tk.missing)
		}
	})

	t.Run("acceptance section with no bullets is missing", func(t *testing.T) {
		tk := parseTicket("## Goal\ng\n\n## Acceptance Criteria\njust prose, no list\n\n## DoD\nd")
		if len(tk.missing) != 1 || tk.missing[0] != "acceptance_criteria" {
			t.Errorf("missing = %v, want [acceptance_criteria]", tk.missing)
		}
	})

	t.Run("empty description misses all three", func(t *testing.T) {
		tk := parseTicket("")
		if strings.Join(tk.missing, ",") != "goal,acceptance_criteria,dod" {
			t.Errorf("missing = %v", tk.missing)
		}
	})
}

// commentRecorder is a fake Linear GraphQL endpoint that records commentCreate
// calls so a refusal's loud feedback can be asserted.
type commentRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (cr *commentRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := readQuery(r)
		if strings.Contains(q, "CommentCreate") {
			cr.mu.Lock()
			cr.bodies = append(cr.bodies, q) // query carries the body var; presence is enough
			cr.mu.Unlock()
			writeData(w, `{"commentCreate":{"success":true}}`)
			return
		}
		writeData(w, `{}`)
	}))
}

func (cr *commentRecorder) count() int {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return len(cr.bodies)
}

func conformingDesc() string {
	return "## Goal\nShip it\n\n## Acceptance Criteria\n- a\n- b\n\n## DoD\ngreen"
}

// AC2: a non-conforming issue on a typed-ticket project is refused at birth —
// a comment fires on the issue, and NO mirror row is created.
func TestIngest_TypedTicket_RefusesNonConforming(t *testing.T) {
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

	iss := baseIssue() // description "Do the thing" — no sections
	body := issueFixture("create", time.Now().UnixMilli(), "human-1", iss, nil)
	events, err := c.Ingest(body, sign(testSecret, body))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("refused issue must not dispatch, got %d events", len(events))
	}
	if cr.count() != 1 {
		t.Errorf("expected exactly one refusal comment on the issue, got %d", cr.count())
	}
	if task, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1"); task != nil {
		t.Error("refused issue must not create a mirror row")
	}
}

// AC1 + AC4: a conforming issue mirrors AND carries the parsed typed ticket on
// the mirror row; no refusal comment fires.
func TestIngest_TypedTicket_ConformingMirrorsAndPopulates(t *testing.T) {
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

	iss := baseIssue()
	iss["description"] = conformingDesc()
	body := issueFixture("create", time.Now().UnixMilli(), "human-1", iss, nil)
	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if cr.count() != 0 {
		t.Errorf("conforming issue must not be commented, got %d", cr.count())
	}
	task, err := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if err != nil || task == nil {
		t.Fatalf("conforming issue must mirror: %v", err)
	}
	if task.Goal != "Ship it" {
		t.Errorf("goal not populated: %q", task.Goal)
	}
	if task.AcceptanceCriteria != `["a","b"]` {
		t.Errorf("acceptance_criteria not populated: %q", task.AcceptanceCriteria)
	}
	if task.Dod != "green" {
		t.Errorf("dod not populated: %q", task.Dod)
	}
}

// The UPDATE path of UpsertLinearMirror must persist the typed ticket too (a
// second webhook for the same issue), not just the INSERT — guards column /
// arg alignment on the update statement.
func TestIngest_TypedTicket_UpdatePersists(t *testing.T) {
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

	iss := baseIssue()
	iss["description"] = conformingDesc()
	b1 := issueFixture("create", time.Now().UnixMilli(), "human-1", iss, nil)
	if _, err := c.Ingest(b1, sign(testSecret, b1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Second webhook: acceptance list changed.
	iss["description"] = "## Goal\nShip it v2\n\n## Acceptance Criteria\n- c\n- d\n- e\n\n## DoD\nstill green"
	b2 := issueFixture("update", time.Now().UnixMilli(), "human-1", iss, nil)
	if _, err := c.Ingest(b2, sign(testSecret, b2)); err != nil {
		t.Fatalf("update: %v", err)
	}
	task, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if task == nil {
		t.Fatal("mirror gone after update")
	}
	if task.Goal != "Ship it v2" || task.AcceptanceCriteria != `["c","d","e"]` || task.Dod != "still green" {
		t.Errorf("update did not persist ticket: goal=%q ac=%q dod=%q", task.Goal, task.AcceptanceCriteria, task.Dod)
	}
}

// AC3 (regression): with the flag OFF the Linear sync is unchanged — a
// non-conforming issue mirrors as before, with no comment.
func TestIngest_TypedTicket_FlagOff_Unchanged(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database) // c.project "syn" is not flagged (default off)
	cr := &commentRecorder{}
	srv := cr.server(t)
	defer srv.Close()
	c.gql.url = srv.URL

	iss := baseIssue() // non-conforming
	body := issueFixture("create", time.Now().UnixMilli(), "human-1", iss, nil)
	if _, err := c.Ingest(body, sign(testSecret, body)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if cr.count() != 0 {
		t.Errorf("flag off must not comment, got %d", cr.count())
	}
	task, _ := database.GetTaskByLinearIssueID(c.project, "issue-uuid-1")
	if task == nil {
		t.Fatal("flag off: non-conforming issue must still mirror (unchanged behavior)")
	}
	if task.AcceptanceCriteria != "[]" {
		t.Errorf("no-ticket mirror should default acceptance_criteria to '[]', got %q", task.AcceptanceCriteria)
	}
}
