package linear

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"agent-relay/internal/models"
)

// agentProjectName maps a relay agent/profile to a Linear project NAME (owner's
// inverse-routing rule). Growth lanes, cto, and unknowns default to the
// "tsukumo" studio bucket (re-filed later by cmo) — never dropped.
func agentProjectName(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "fullstack-trovex":
		return "trovex"
	case "wraith-dev":
		return "wrai.th"
	case "yoru-dev":
		return "yoru"
	case "dokan-core":
		return "dokan"
	case "fullstack-lead":
		return "tsukumo"
	case "donna-dev":
		return "donna"
	default:
		return "tsukumo"
	}
}

// stateIDForType returns the first workflow state id of the given type
// ("unstarted"/"started"/…), or "" to let Linear pick the team default.
func (c *Connector) stateIDForType(typ string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, s := range c.stateList {
		if s.Type == typ {
			return s.ID
		}
	}
	return ""
}

// BackfillResult summarises a relay→Linear backfill run.
type BackfillResult struct {
	DryRun   bool     `json:"dry_run"`
	Eligible int      `json:"eligible"`
	Created  int      `json:"created"`
	Linked   int      `json:"linked"`
	Errors   []string `json:"errors,omitempty"`
}

var taskUUIDRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// Backfill mirrors active relay-native tasks INTO Linear as linked issues, so
// the whole active board shows up in the cockpit. For each eligible task
// (active, not archived, not already a Linear mirror, not yet linked):
//   - if an existing open Linear issue's body references this task id (cto's
//     hand-created TSU-5..12), LINK to it (no duplicate);
//   - otherwise CREATE an issue in the agent's mapped project, in a state mapped
//     from the task status (never "started" for a not-started task), and link it.
//
// Linking sets linear_issue_id so the next reconcile upsert treats it as a
// mirror — and because the prior status already matches, the dispatch trigger is
// skipped (no re-dispatch, the hard invariant). dryRun writes nothing.
func (c *Connector) Backfill(ctx context.Context, dryRun bool, limit int) (BackfillResult, error) {
	res := BackfillResult{DryRun: dryRun}

	teamID, projects, err := c.gql.teamMeta(ctx, c.teamKey)
	if err != nil {
		return res, fmt.Errorf("team meta: %w", err)
	}

	// Map relay task id → existing Linear issue (from open-issue descriptions).
	existing := map[string]gqlIssue{}
	if issues, err := c.gql.openTeamIssues(ctx, c.teamKey); err == nil {
		for _, iss := range issues {
			for _, id := range taskUUIDRe.FindAllString(iss.Description, -1) {
				if _, dup := existing[id]; !dup {
					existing[id] = iss
				}
			}
		}
	}

	// Backfill spans every relay project this mirror serves (default + mapped
	// lanes), so tasks living in a mapped project are linked/created too.
	var tasks []models.Task
	for _, proj := range c.mirrorProjects() {
		pt, err := c.db.ListBackfillTasks(proj)
		if err != nil {
			return res, err
		}
		tasks = append(tasks, pt...)
	}
	res.Eligible = len(tasks)

	stateFor := func(status string) string {
		switch status {
		case "in-progress", "claimed", "started", "in-review", "review", "blocked":
			return c.stateIDForType("started")
		default:
			return c.stateIDForType("unstarted")
		}
	}

	n := 0
	for _, t := range tasks {
		if limit > 0 && n >= limit {
			break
		}
		// Link to a pre-existing issue that references this task id.
		if iss, ok := existing[t.ID]; ok {
			if !dryRun {
				if err := c.db.LinkTaskLinear(t.ID, iss.ID, issueKey(iss, c.teamKey)); err != nil {
					res.Errors = append(res.Errors, t.ID+": link: "+err.Error())
					continue
				}
			}
			res.Linked++
			n++
			continue
		}
		// Otherwise create a new linked issue in the mapped project.
		agent := t.ProfileSlug
		if t.AssignedTo != nil && *t.AssignedTo != "" {
			agent = *t.AssignedTo
		}
		if dryRun {
			res.Created++
			n++
			continue
		}
		title := t.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled relay task)"
		}
		desc := strings.TrimSpace(t.Description + "\n\n— relay task " + t.ID)
		issID, key, err := c.gql.createIssue(ctx, teamID, title, desc, projects[agentProjectName(agent)], stateFor(t.Status))
		if err != nil {
			res.Errors = append(res.Errors, t.ID+": create: "+err.Error())
			continue
		}
		if err := c.db.LinkTaskLinear(t.ID, issID, key); err != nil {
			res.Errors = append(res.Errors, t.ID+": link-after-create: "+err.Error())
			continue
		}
		res.Created++
		n++
	}
	return res, nil
}
