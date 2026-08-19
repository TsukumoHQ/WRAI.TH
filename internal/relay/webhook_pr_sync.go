package relay

import (
	"regexp"
	"strings"
)

// PR-link S2 — GitHub PR → relay task status-sync.
//
// A consumer of the pull_request webhooks the relay already receives (TSU-52
// slice-D). It resolves the linked task (by stored pr_number+repo, else the
// `relay-task: <id>` magic-word in the PR body) and syncs its state ONE-WAY,
// GitHub → relay (DEC-wraith-pr-linking-1):
//
//	opened / reopened / ready_for_review → in-review
//	closed + merged                      → done
//	closed + unmerged                    → blocked ("PR closed unmerged")
//	synchronize (new commits)            → no transition, pr_state stays open
//
// Guards: no-resurrect (a terminal task is never pulled back — enforced in
// ForcePRTransition), idempotent (already-in-target = no-op), and delivery-GUID
// dedup upstream (InsertEvent). Best-effort: a sync failure never fails the
// webhook (the reconcile poll, S3, is the backstop).

// relayTaskMagicWord matches `relay-task: <id>` in a PR body. The id is a full
// UUID (primary) or an 8+ hex-ish short id; short ids are only accepted when
// they resolve unambiguously within the webhook's project (resolveTaskID).
var relayTaskMagicWord = regexp.MustCompile(`(?i)relay-task:\s*([0-9a-fA-F][0-9a-fA-F-]{7,35})`)

// syncPullRequestToTask maps one pull_request webhook onto its linked task.
// project is the webhook's resolved project; raw is the decoded GitHub payload.
func (r *Relay) syncPullRequestToTask(project string, raw map[string]any) {
	pr, ok := raw["pull_request"].(map[string]any)
	if !ok {
		return
	}
	number := intVal(pr["number"])
	if number <= 0 {
		return
	}
	action := strings.ToLower(strVal(raw["action"]))
	htmlURL := strVal(pr["html_url"])
	merged, _ := pr["merged"].(bool)
	repo := ""
	if rp, ok := raw["repository"].(map[string]any); ok {
		repo = strVal(rp["full_name"])
	}
	prState := prStateFrom(action, merged)

	// Resolve the linked task: stored PR first (deterministic), else the
	// magic-word in the body (human-opened PR path).
	task, _ := r.DB.GetTaskByPR(project, number, repo)
	if task == nil {
		body := strVal(pr["body"])
		if m := relayTaskMagicWord.FindStringSubmatch(body); m != nil {
			if id, err := r.Handlers.resolveTaskID(m[1], project); err == nil {
				// First sighting of a human-declared link: stamp the PR zone (S1).
				if _, err := r.DB.SetTaskPR(id, project, ptr(htmlURL), &number, ptr(prState), ptr(repo)); err == nil {
					task, _ = r.DB.GetTask(id, project)
				}
			}
		}
	}
	if task == nil {
		return // no linked task — this PR isn't ours, ignore
	}

	// Always record the last-observed PR state (state-only, COALESCE keeps the
	// url/number/repo). Also (re)fill url/repo if the link came in bare.
	_, _ = r.DB.SetTaskPR(task.ID, project, ptr(htmlURL), &number, ptr(prState), ptr(repo))

	target, reason := prTargetState(action, merged)
	if target == "" {
		// synchronize / edited / other: no state change (activity already bumped
		// by the transition path elsewhere); nothing to do.
		return
	}
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	updated, changed, err := r.DB.ForcePRTransition(project, task.ID, target, reasonPtr)
	if err != nil || !changed || updated == nil {
		return
	}
	name := "task.pr_synced"
	if target == "done" && merged {
		name = "task.pr_merged"
	}
	emitTaskEvent(r.Events, name, "pr_sync", project, updated, map[string]any{
		"pr_number": number, "pr_state": prState, "pr_url": htmlURL, "action": action,
	})
}

// prStateFrom collapses a PR action+merged flag into the stored last-observed
// state (open | merged | closed).
func prStateFrom(action string, merged bool) string {
	if action == "closed" {
		if merged {
			return "merged"
		}
		return "closed"
	}
	return "open"
}

// prTargetState maps a PR action to the task state it should drive, and an
// optional block reason. Empty target = no transition.
func prTargetState(action string, merged bool) (target, reason string) {
	switch action {
	case "opened", "reopened", "ready_for_review":
		return "in-review", ""
	case "closed":
		if merged {
			return "done", ""
		}
		return "blocked", "PR closed unmerged"
	default:
		return "", "" // synchronize / edited / labeled / …
	}
}

// prTargetFromState maps a last-observed PR STATE (open|merged|closed, the form
// a poller reads off gh — PR-link S3 reconcile) onto the task state it should
// drive, and an optional block reason. It is the state-keyed twin of
// prTargetState (which keys on a webhook action): open collapses "opened /
// reopened / ready_for_review" → in-review, closed is closed-unmerged (a merged
// PR reads state=merged, not closed). Empty target = unknown state, no
// transition. The reconcile poll (which cannot see the webhook action) uses this
// so the two convergence paths — webhook and poll — apply the SAME map.
func prTargetFromState(prState string) (target, reason string) {
	switch prState {
	case "open":
		return "in-review", ""
	case "merged":
		return "done", ""
	case "closed":
		return "blocked", "PR closed unmerged"
	default:
		return "", ""
	}
}

// intVal reads a JSON number (float64) or numeric string into an int; 0 on miss.
func intVal(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

func ptr(s string) *string { return &s }
