package relay

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// W8 D2 (ruling wraith-deploying-ruling): deploy_task is the niwa pipeline's
// one fenced MCP call that moves a merged in-review task to deploying. Like
// review_task it is fenced to the lease holder or an override actor, and the
// caller never takes the doer's lease (N3).

const deploySHA = "c2c86b52de76"

// deployFixture: dev-a claims, starts and reviews a task dispatched by cto.
func deployFixture(t *testing.T) (*Handlers, string) {
	t.Helper()
	h, id := n3Fixture(t)
	n3Call(t, h, id, "dev-a", "review_task")
	return h, id
}

func deployCall(h *Handlers, id, caller, sha string) map[string]any {
	return map[string]any{"project": "p1", "as": caller, "task_id": id, "merge_sha": sha}
}

// AC1: deploy_task(task_id, merge_sha) moves in-review -> deploying and
// stores the sha.
func TestDeployTask_MovesToDeployingAndStoresSHA(t *testing.T) {
	h, id := deployFixture(t)
	res, _ := h.HandleDeployTask(ctx, call(deployCall(h, id, "niwa", deploySHA)))
	if res.IsError {
		t.Fatalf("deploy_task by niwa: %s", expectError(t, res))
	}
	body := parseJSON(t, res)
	if body["status"] != "deploying" {
		t.Fatalf("result status = %v, want deploying", body["status"])
	}
	if body["merge_sha"] != deploySHA {
		t.Fatalf("result merge_sha = %v, want %s", body["merge_sha"], deploySHA)
	}
	if got := h.db.DeployMergeSHA("p1", id); got != deploySHA {
		t.Fatalf("stored merge sha = %q, want %s", got, deploySHA)
	}
	task := n3Task(t, h, id)
	if task.Status != "deploying" {
		t.Fatalf("stored status = %q, want deploying", task.Status)
	}
	assertDoerKeeps(t, task, "deploy_task by niwa")

	res, _ = h.HandleDeployTask(ctx, call(deployCall(h, id, "niwa", "")))
	if !res.IsError {
		t.Fatal("deploy_task without merge_sha accepted, want refused")
	}
}

// AC2: a non-holder without authority is refused; the delegating service and
// a tokenless, unlisted 'niwa' never take the lease.
func TestDeployTask_FencedAndNeverTakesLease(t *testing.T) {
	h, id := deployFixture(t)
	res, _ := h.HandleDeployTask(ctx, call(deployCall(h, id, "peer", deploySHA)))
	if !res.IsError {
		t.Fatal("deploy_task by a non-holder without authority accepted, want refused")
	}
	if task := n3Task(t, h, id); task.Status != "in-review" {
		t.Fatalf("refused deploy changed status to %q", task.Status)
	}

	t.Run("unlisted niwa", func(t *testing.T) {
		t.Setenv("RELAY_OVERRIDE_ACTORS", "someone-else")
		h, id := deployFixture(t)
		_, _ = h.HandleDeployTask(ctx, call(deployCall(h, id, "niwa", deploySHA)))
		assertDoerKeeps(t, n3Task(t, h, id), "deploy_task by unlisted niwa")
	})
	t.Run("listed niwa then doer", func(t *testing.T) {
		h, id := deployFixture(t)
		if res, _ := h.HandleDeployTask(ctx, call(deployCall(h, id, "niwa", deploySHA))); res.IsError {
			t.Fatalf("deploy by niwa: %s", expectError(t, res))
		}
		n3Call(t, h, id, "niwa", "complete_task")
	})
}

// AC3: list_tasks status='deploying' returns only deploying tasks.
func TestListTasks_StatusDeploying(t *testing.T) {
	h, id := deployFixture(t)
	if res, _ := h.HandleDeployTask(ctx, call(deployCall(h, id, "niwa", deploySHA))); res.IsError {
		t.Fatalf("deploy: %s", expectError(t, res))
	}
	_, _ = h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "profile": "dev", "title": "other"}))

	res, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "p1", "as": "cto", "status": "deploying", "format": "json"}))
	if res.IsError {
		t.Fatalf("list_tasks: %s", expectError(t, res))
	}
	tasks := listedTasks(t, res)
	if len(tasks) != 1 || tasks[0]["id"] != id || tasks[0]["status"] != "deploying" {
		t.Fatalf("list_tasks status=deploying = %v, want exactly %s", tasks, id)
	}
}

// listedTasks decodes a list_tasks json result's tasks array.
func listedTasks(t *testing.T, res *mcp.CallToolResult) []map[string]any {
	t.Helper()
	raw, _ := parseJSON(t, res)["tasks"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
