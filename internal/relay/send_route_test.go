package relay

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Doer reach (task be29e23f): a doer can always message the agent that
// dispatched it an active task, answer every message an agent sent it, and
// reach anyone up its reports_to chain; an unrelated send is still refused
// with a FORBIDDEN body that names the refusing rule.

type routeFixture struct {
	h   *Handlers
	raw *sql.DB
}

// newRouteFixture registers lead as an executive (admin team, so the
// permission check is enforced) and each of plain as a non-admin agent.
func newRouteFixture(t *testing.T, lead string, plain ...map[string]any) *routeFixture {
	t.Helper()
	d, path := memDB(t)
	h := memHandlersAt(t, d)
	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": lead, "is_executive": true})); res.IsError {
		t.Fatalf("register %s: %s", lead, expectError(t, res))
	}
	for _, a := range plain {
		a["project"] = "p1"
		if res, _ := h.HandleRegisterAgent(ctx, call(a)); res.IsError {
			t.Fatalf("register %v: %s", a, expectError(t, res))
		}
	}
	return &routeFixture{h: h, raw: raw}
}

func (f *routeFixture) send(args map[string]any) *mcp.CallToolResult {
	args["project"] = "p1"
	if _, ok := args["content"]; !ok {
		args["content"] = "hi"
	}
	res, _ := f.h.HandleSendMessage(ctx, call(args))
	return res
}

// seedTask inserts a task dispatched by by and assigned to to, with no
// message between them (the dispatch notice purged by TTL, as in the field).
func (f *routeFixture) seedTask(t *testing.T, id, by, to, status string) {
	t.Helper()
	if _, err := f.raw.Exec(`INSERT INTO tasks (id, profile_slug, dispatched_by, assigned_to, title, status, project, dispatched_at, labels, blocked_periods, goal, acceptance_criteria, dod)
		VALUES (?, ?, ?, ?, 'title', ?, 'p1', ?, '[]', '[]', '', '[]', '')`, id, to, by, to, status, ago(time.Hour)); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// TestDoerCanMessageActiveTaskDispatcher (AC1): niwa-cto-2 dispatched an
// active task to gate-lead-2 and never messaged it; gate-lead-2 can message
// niwa-cto-2. A dispatcher of only finished tasks opens nothing.
func TestDoerCanMessageActiveTaskDispatcher(t *testing.T) {
	f := newRouteFixture(t, "niwa-cto-2", map[string]any{"name": "gate-lead-2"}, map[string]any{"name": "old-lead"})
	if res := f.send(map[string]any{"as": "gate-lead-2", "to": "niwa-cto-2"}); !res.IsError {
		t.Fatal("gate-lead-2 -> niwa-cto-2 allowed before any task or message")
	}
	f.seedTask(t, "t-active", "niwa-cto-2", "gate-lead-2", "in-progress")
	if res := f.send(map[string]any{"as": "gate-lead-2", "to": "niwa-cto-2"}); res.IsError {
		t.Fatalf("doer -> dispatcher of its active task refused: %s", expectError(t, res))
	}

	// old-lead dispatched gate-lead-2 a task that is done: no route back.
	f.seedTask(t, "t-done", "old-lead", "gate-lead-2", "done")
	if res := f.send(map[string]any{"as": "gate-lead-2", "to": "old-lead"}); !res.IsError {
		t.Fatal("doer -> dispatcher of a done task allowed")
	}
}

// TestReplyPathServesEveryMessage (AC2): cto-tsukumo messaged backend-lead
// twice; backend-lead answers both, each as a reply and as a fresh message.
func TestReplyPathServesEveryMessage(t *testing.T) {
	f := newRouteFixture(t, "cto-tsukumo", map[string]any{"name": "backend-lead"})
	var asks []string
	for i := 0; i < 2; i++ {
		res := f.send(map[string]any{"as": "cto-tsukumo", "to": "backend-lead"})
		if res.IsError {
			t.Fatalf("cto -> backend-lead: %s", expectError(t, res))
		}
		asks = append(asks, parseJSON(t, res)["id"].(string))
	}
	for _, ask := range asks {
		if res := f.send(map[string]any{"as": "backend-lead", "to": "cto-tsukumo", "reply_to": ask}); res.IsError {
			t.Fatalf("reply to %s refused: %s", ask, expectError(t, res))
		}
		if res := f.send(map[string]any{"as": "backend-lead", "to": "cto-tsukumo"}); res.IsError {
			t.Fatalf("fresh message after reply refused: %s", expectError(t, res))
		}
	}
}

// TestDoerCanMessageUpItsReportsToChain: a doer reaches every agent up its
// reports_to chain, not only its direct manager.
func TestDoerCanMessageUpItsReportsToChain(t *testing.T) {
	f := newRouteFixture(t, "founder-proxy",
		map[string]any{"name": "cto-a"},
		map[string]any{"name": "lead-a", "reports_to": "cto-a"},
		map[string]any{"name": "doer-a", "reports_to": "lead-a"})
	if res := f.send(map[string]any{"as": "doer-a", "to": "cto-a"}); res.IsError {
		t.Fatalf("doer -> its manager's manager refused: %s", expectError(t, res))
	}
}

// TestUnrelatedSendRefusedNamingRule (AC3): an agent with no route is still
// refused, FORBIDDEN, and the body names the rule that refused it.
func TestUnrelatedSendRefusedNamingRule(t *testing.T) {
	f := newRouteFixture(t, "niwa-cto-2", map[string]any{"name": "gate-lead-2"}, map[string]any{"name": "stranger"})
	f.seedTask(t, "t-active", "niwa-cto-2", "gate-lead-2", "in-progress")
	res := f.send(map[string]any{"as": "stranger", "to": "gate-lead-2"})
	if !res.IsError {
		t.Fatal("stranger -> gate-lead-2 allowed")
	}
	body := parseJSON(t, &mcp.CallToolResult{Content: res.Content})
	if body["code"] != "FORBIDDEN" {
		t.Fatalf("code = %v, want FORBIDDEN (body %v)", body["code"], body)
	}
	if body["rule"] != "send.no_route" || !strings.Contains(body["message"].(string), "send.no_route") {
		t.Fatalf("refusal does not name rule send.no_route: %v", body)
	}
}
