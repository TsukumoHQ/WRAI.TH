package linear

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agent-relay/internal/db"
)

// W3 (552b1e22): after any run of failed GraphQL ticks, the next good tick
// leaves every open Linear issue with exactly one relay mirror.

// flakyLinear answers HTTP 400 (Linear's rate-limit / overload shape) while
// failing is set, else one open delegate-routed issue (the delegate field is
// served only to the delegate query, as on a schema that supports it).
func flakyLinear(failing *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(r)
		if failing.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`))
			return
		}
		if strings.Contains(query, "TeamOpenIssues") {
			deleg := ""
			if strings.Contains(query, "delegate") {
				deleg = `"delegate":{"id":"u-a","name":"content-lead","displayName":"content-lead"},`
			}
			writeData(w, `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
				{"id":"i-w3","identifier":"TSU-301","number":301,"title":"Missed webhook","priority":2,"url":"u","state":{"id":"s0","name":"Todo","type":"unstarted"},"assignee":{"id":"u-h","name":"loicmancino.work","displayName":"loicmancino.work"},`+deleg+`"labels":{"nodes":[]}}
			]}}`)
			return
		}
		writeData(w, `{}`)
	}))
}

// rawTestDB is newTestDB plus a raw handle on the same file, to count and
// delete mirror rows directly.
func rawTestDB(t *testing.T) (*db.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.NewTestDB(path)
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close(); _ = database.Close() })
	return database, raw
}

func mirrorCount(t *testing.T, raw *sql.DB) int {
	t.Helper()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM tasks WHERE linear_issue_id = 'i-w3'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReconcile_FirstGoodTickAfterErrorsCreatesMirror(t *testing.T) {
	database := newTestDB(t)
	c := newTestConn(t, database)
	var failing atomic.Bool
	failing.Store(true)
	srv := flakyLinear(&failing)
	defer srv.Close()
	c.gql.url = srv.URL

	for i := 0; i < 3; i++ {
		if _, err := c.ReconcileCycle(c.project); err == nil {
			t.Fatalf("tick %d: want an error while Linear answers 400", i)
		}
	}
	failing.Store(false)
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatalf("recovered tick: %v", err)
	}
	m, err := database.GetTaskByLinearIssueID(c.project, "i-w3")
	if err != nil || m == nil {
		t.Fatalf("mirror for TSU-301 not created on the first good tick: %v", err)
	}
	if m.ProfileSlug != "content-lead" {
		t.Errorf("profile_slug = %q, want content-lead: an errored tick latched the delegate-less fallback", m.ProfileSlug)
	}
}

func TestReconcile_MissingMirrorRecreatedNoDuplicate(t *testing.T) {
	database, raw := rawTestDB(t)
	c := newTestConn(t, database)
	var failing atomic.Bool
	srv := flakyLinear(&failing)
	defer srv.Close()
	c.gql.url = srv.URL

	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatal(err)
	}
	if n := mirrorCount(t, raw); n != 1 {
		t.Fatalf("two cycles over the same issue: %d mirrors, want 1", n)
	}
	if _, err := raw.Exec(`DELETE FROM tasks WHERE linear_issue_id = 'i-w3'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReconcileCycle(c.project); err != nil {
		t.Fatal(err)
	}
	if n := mirrorCount(t, raw); n != 1 {
		t.Errorf("missing mirror row: %d mirrors after the next tick, want 1 (recreated)", n)
	}
}
