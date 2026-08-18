package relay

import (
	"testing"

	"agent-relay/internal/db"
)

func TestPrTargetState(t *testing.T) {
	cases := []struct {
		action string
		merged bool
		target string
		reason string
	}{
		{"opened", false, "in-review", ""},
		{"reopened", false, "in-review", ""},
		{"ready_for_review", false, "in-review", ""},
		{"closed", true, "done", ""},
		{"closed", false, "blocked", "PR closed unmerged"},
		{"synchronize", false, "", ""},
		{"edited", false, "", ""},
	}
	for _, c := range cases {
		target, reason := prTargetState(c.action, c.merged)
		if target != c.target || reason != c.reason {
			t.Fatalf("prTargetState(%q,%v)=(%q,%q) want (%q,%q)", c.action, c.merged, target, reason, c.target, c.reason)
		}
	}
}

func TestPrStateFrom(t *testing.T) {
	if prStateFrom("opened", false) != "open" {
		t.Fatal("opened -> open")
	}
	if prStateFrom("closed", true) != "merged" {
		t.Fatal("closed+merged -> merged")
	}
	if prStateFrom("closed", false) != "closed" {
		t.Fatal("closed+unmerged -> closed")
	}
}

func TestMagicWordRegex(t *testing.T) {
	body := "Fixes the thing.\n\nrelay-task: 2f8dae4a-8bbf-44e0-b04a-0906f206762c\n"
	m := relayTaskMagicWord.FindStringSubmatch(body)
	if m == nil || m[1] != "2f8dae4a-8bbf-44e0-b04a-0906f206762c" {
		t.Fatalf("magic-word not matched: %v", m)
	}
	if relayTaskMagicWord.FindStringSubmatch("no link here") != nil {
		t.Fatal("false match")
	}
}

// seedLinkedTask dispatches a task and links a PR to it, returning the id.
func seedLinkedTask(t *testing.T, r *Relay, number int, repo string) string {
	t.Helper()
	task, err := r.DB.DispatchTask("p1", "dev", "boss", "pr work", "", "P2", nil, nil, db.TypedTicket{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := r.DB.SetTaskPR(task.ID, "p1", ptr("https://github.com/"+repo+"/pull/1"), &number, ptr("open"), ptr(repo)); err != nil {
		t.Fatalf("link: %v", err)
	}
	return task.ID
}

func prPayload(action string, number int, repo string, merged bool, body string) map[string]any {
	return map[string]any{
		"action":     action,
		"repository": map[string]any{"full_name": repo},
		"pull_request": map[string]any{
			"number": float64(number), "html_url": "https://github.com/" + repo + "/pull/1",
			"merged": merged, "body": body,
		},
	}
}

func statusOf(t *testing.T, r *Relay, id string) string {
	t.Helper()
	task, err := r.DB.GetTask(id, "p1")
	if err != nil || task == nil {
		t.Fatalf("get task: %v", err)
	}
	return task.Status
}

func TestSyncOpenedToInReview(t *testing.T) {
	r := testRelay(t)
	id := seedLinkedTask(t, r, 42, "o/repo")
	r.syncPullRequestToTask("p1", prPayload("opened", 42, "o/repo", false, ""))
	if s := statusOf(t, r, id); s != "in-review" {
		t.Fatalf("opened should drive in-review, got %s", s)
	}
}

func TestSyncMergedToDone(t *testing.T) {
	r := testRelay(t)
	id := seedLinkedTask(t, r, 42, "o/repo")
	r.syncPullRequestToTask("p1", prPayload("closed", 42, "o/repo", true, ""))
	if s := statusOf(t, r, id); s != "done" {
		t.Fatalf("merged should drive done, got %s", s)
	}
	task, _ := r.DB.GetTask(id, "p1")
	if task.PRState == nil || *task.PRState != "merged" {
		t.Fatalf("pr_state should be merged: %v", task.PRState)
	}
}

func TestSyncClosedUnmergedToBlocked(t *testing.T) {
	r := testRelay(t)
	id := seedLinkedTask(t, r, 42, "o/repo")
	r.syncPullRequestToTask("p1", prPayload("closed", 42, "o/repo", false, ""))
	task, _ := r.DB.GetTask(id, "p1")
	if task.Status != "blocked" {
		t.Fatalf("closed-unmerged should block, got %s", task.Status)
	}
	if task.BlockedReason == nil || *task.BlockedReason != "PR closed unmerged" {
		t.Fatalf("block reason missing: %v", task.BlockedReason)
	}
}

// No-resurrect: a merged→done task must NOT be pulled back by a late 'opened'.
func TestSyncNoResurrectTerminal(t *testing.T) {
	r := testRelay(t)
	id := seedLinkedTask(t, r, 42, "o/repo")
	r.syncPullRequestToTask("p1", prPayload("closed", 42, "o/repo", true, "")) // → done
	r.syncPullRequestToTask("p1", prPayload("reopened", 42, "o/repo", false, ""))
	if s := statusOf(t, r, id); s != "done" {
		t.Fatalf("terminal task resurrected by late reopen: %s", s)
	}
}

// Human-opened PR with the magic-word auto-links + syncs even with no prior link.
func TestSyncMagicWordAutoLinks(t *testing.T) {
	r := testRelay(t)
	task, err := r.DB.DispatchTask("p1", "dev", "boss", "human pr", "", "P2", nil, nil, db.TypedTicket{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	body := "does the thing\n\nrelay-task: " + task.ID
	r.syncPullRequestToTask("p1", prPayload("opened", 99, "o/repo", false, body))
	got, _ := r.DB.GetTask(task.ID, "p1")
	if got.Status != "in-review" {
		t.Fatalf("magic-word PR should drive in-review, got %s", got.Status)
	}
	if got.PRNumber == nil || *got.PRNumber != 99 {
		t.Fatalf("magic-word PR should auto-link pr_number: %v", got.PRNumber)
	}
}

// An unlinked PR (no stored link, no magic-word) is ignored — no false moves.
func TestSyncUnlinkedIgnored(t *testing.T) {
	r := testRelay(t)
	task, _ := r.DB.DispatchTask("p1", "dev", "boss", "unrelated", "", "P2", nil, nil, db.TypedTicket{})
	r.syncPullRequestToTask("p1", prPayload("closed", 500, "o/repo", true, "no link"))
	if s := statusOf(t, r, task.ID); s != "pending" {
		t.Fatalf("unlinked PR moved an unrelated task: %s", s)
	}
}
