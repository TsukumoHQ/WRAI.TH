package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// linearAPIURL is the Linear GraphQL endpoint. Overridable in tests via the
// connector's gql.url field (set by a test httptest server).
const linearAPIURL = "https://api.linear.app/graphql"

// graphqlClient is a tiny hand-written GraphQL client (no SDK). Personal API
// keys are sent in the Authorization header verbatim (no "Bearer" prefix).
type graphqlClient struct {
	apiKey string
	url    string
	http   *http.Client

	// delegateUnsupported latches true the first time the Issue.delegate field
	// is rejected by the schema, so the reconcile poll permanently falls back to
	// the delegate-less query. A missing/renamed field must NEVER 400 the whole
	// poll (that would be a fleet-wide dispatch outage).
	delegateUnsupported atomic.Bool
}

func newGraphQLClient(apiKey string) *graphqlClient {
	return &graphqlClient{
		apiKey: apiKey,
		url:    linearAPIURL,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

// gqlResponse is the standard GraphQL envelope.
type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// do executes a GraphQL query/mutation and unmarshals data into out.
func (c *graphqlClient) do(ctx context.Context, query string, vars map[string]any, out any) error {
	reqBody, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("marshal gql request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build gql request: %w", err)
	}
	req.Header.Set("Authorization", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gql request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gql http status %d", resp.StatusCode)
	}

	var env gqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode gql response: %w", err)
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("gql errors: %s", strings.Join(msgs, "; "))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("unmarshal gql data: %w", err)
	}
	return nil
}

// --- viewer (anti-loop): the API key's own user id ---

const viewerQuery = `query { viewer { id } }`

func (c *graphqlClient) viewerID(ctx context.Context) (string, error) {
	var out struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := c.do(ctx, viewerQuery, nil, &out); err != nil {
		return "", err
	}
	return out.Viewer.ID, nil
}

// --- workflow states (resolve the In Review state id) ---

const teamStatesQuery = `query States($key: String!) {
  teams(filter: { key: { eq: $key } }) {
    nodes { id states { nodes { id name type } } }
  }
}`

type stateInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (c *graphqlClient) teamStates(ctx context.Context, teamKey string) ([]stateInfo, error) {
	var out struct {
		Teams struct {
			Nodes []struct {
				ID     string `json:"id"`
				States struct {
					Nodes []stateInfo `json:"nodes"`
				} `json:"states"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := c.do(ctx, teamStatesQuery, map[string]any{"key": teamKey}, &out); err != nil {
		return nil, err
	}
	if len(out.Teams.Nodes) == 0 {
		return nil, fmt.Errorf("team %q not found", teamKey)
	}
	return out.Teams.Nodes[0].States.Nodes, nil
}

// --- open team issues (reconcile) ---

// gqlIssue is the shape returned by the reconcile query (and reused for mapping).
type gqlIssue struct {
	ID          string     `json:"id"`
	Identifier  string     `json:"identifier"`
	Number      float64    `json:"number"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Priority    float64    `json:"priority"`
	Estimate    *float64   `json:"estimate"`
	URL         string     `json:"url"`
	State       *stateInfo `json:"state"`
	Assignee    *struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"assignee"`
	// Delegate is Linear's agent-delegation field: when a user assigns an issue
	// to an agent, the human stays the assignee (primary owner) and the agent is
	// set as the delegate. Routing prefers this over assignee. Only populated by
	// the reconcile GraphQL query (and only when the schema supports the field);
	// Issue webhooks don't carry it, so the webhook path falls back to assignee.
	Delegate *struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"delegate"`
	Parent *struct {
		ID string `json:"id"`
	} `json:"parent"`
	ParentID string `json:"parentId"` // webhook flat fallback when parent is not expanded
	Project  *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	ProjectID string `json:"projectId"` // webhook flat fallback
	Cycle     *struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		StartsAt string `json:"startsAt"`
		EndsAt   string `json:"endsAt"`
	} `json:"cycle"`
	Labels labelList `json:"labels"`
}

// parentLinearID returns the parent issue's Linear id from either the expanded
// parent object (GraphQL + most webhooks) or the flat parentId (some webhooks).
func (i gqlIssue) parentLinearID() string {
	if i.Parent != nil && i.Parent.ID != "" {
		return i.Parent.ID
	}
	return i.ParentID
}

// teamOpenIssuesQuery pulls all NON-closed issues for a team (any cycle or none),
// so auto-dispatch covers issues that aren't in the active cycle. Flat issues
// query (not nested through team→cycle) stays under the complexity budget even
// with per-issue cycle/project expanded.
//
// Two variants share one node-field block: the default fetches Issue.delegate
// (Linear's agent-delegation field) for delegate-based routing; the fallback
// omits it. openTeamIssues uses the delegate variant and latches to the fallback
// only if the schema rejects the field — so a missing field can never break the
// poll. Keep the two `nodes {…}` blocks identical apart from the delegate line.
const teamOpenIssuesQuery = `query TeamOpenIssues($key: String!, $after: String) {
  issues(
    filter: { team: { key: { eq: $key } }, state: { type: { nin: ["completed", "canceled"] } } }
    first: 50, after: $after
  ) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id identifier number title description priority estimate url
      state { id name type }
      assignee { id name displayName }
      project { id name }
      parent { id }
      cycle { id name startsAt endsAt }
      labels { nodes { name } }
    }
  }
}`

// teamOpenIssuesQueryDelegate is teamOpenIssuesQuery + the delegate field.
const teamOpenIssuesQueryDelegate = `query TeamOpenIssues($key: String!, $after: String) {
  issues(
    filter: { team: { key: { eq: $key } }, state: { type: { nin: ["completed", "canceled"] } } }
    first: 50, after: $after
  ) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id identifier number title description priority estimate url
      state { id name type }
      assignee { id name displayName }
      delegate { id name displayName }
      project { id name }
      parent { id }
      cycle { id name startsAt endsAt }
      labels { nodes { name } }
    }
  }
}`

// isFieldError reports whether a GraphQL error looks like a schema/validation
// rejection of a field (an HTTP 400 or a "cannot query field" message) — as
// opposed to a transient network/5xx/rate-limit error. Only a field error
// latches the delegate fallback; transient errors propagate and the poll retries
// next interval (unchanged behavior), so a network blip can't permanently
// disable delegate routing.
func isFieldError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "status 400") ||
		strings.Contains(s, "cannot query field") ||
		strings.Contains(s, "delegate")
}

// openTeamIssues returns every open (not completed/canceled) issue in the team,
// paginated. Used by the reconcile poll so dispatch isn't limited to the active
// cycle. Bounded to 40 pages (2000 issues) as a backstop.
func (c *graphqlClient) openTeamIssues(ctx context.Context, teamKey string) ([]gqlIssue, error) {
	var issues []gqlIssue
	var after *string
	for page := 0; page < 40; page++ {
		var out struct {
			Issues struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []gqlIssue `json:"nodes"`
			} `json:"issues"`
		}
		vars := map[string]any{"key": teamKey}
		if after != nil {
			vars["after"] = *after
		}
		query := teamOpenIssuesQueryDelegate
		if c.delegateUnsupported.Load() {
			query = teamOpenIssuesQuery
		}
		if err := c.do(ctx, query, vars, &out); err != nil {
			// Guard: a missing/renamed Issue.delegate field must never break the
			// poll. On a field-shaped error from the delegate query, latch the
			// fallback and retry this page with the delegate-less query. Any other
			// (transient) error propagates unchanged.
			if query == teamOpenIssuesQueryDelegate && isFieldError(err) {
				c.delegateUnsupported.Store(true)
				log.Printf("[linear] Issue.delegate unsupported (%v) — falling back to assignee-only routing", err)
				if err := c.do(ctx, teamOpenIssuesQuery, vars, &out); err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
		}
		issues = append(issues, out.Issues.Nodes...)
		if !out.Issues.PageInfo.HasNextPage {
			break
		}
		cur := out.Issues.PageInfo.EndCursor
		after = &cur
	}
	return issues, nil
}

// --- issues by id (Done-dropout sync, TSU-159) ---

const issuesByIDsQuery = `query IssuesByIDs($ids: [ID!]!) {
  issues(filter: { id: { in: $ids } }, first: 250) {
    nodes { id state { id name type } }
  }
}`

// issuesByIDs fetches the current state of specific issues by id — used by the
// reconcile poll to resolve issues that dropped out of the OPEN set (so it can
// tell a Done/canceled issue from a transient miss). Chunked to stay under the
// query-complexity budget; partial results from a failed chunk are not returned
// (the caller treats a fetch error as "leave the mirror alone").
func (c *graphqlClient) issuesByIDs(ctx context.Context, ids []string) ([]gqlIssue, error) {
	const chunk = 100
	var out []gqlIssue
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		var resp struct {
			Issues struct {
				Nodes []gqlIssue `json:"nodes"`
			} `json:"issues"`
		}
		if err := c.do(ctx, issuesByIDsQuery, map[string]any{"ids": ids[start:end]}, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Issues.Nodes...)
	}
	return out, nil
}

// --- team meta (id + projects) for backfill routing ---

const teamMetaQuery = `query TeamMeta($key: String!) {
  teams(filter: { key: { eq: $key } }) {
    nodes { id projects(first: 100) { nodes { id name } } }
  }
}`

// teamMeta returns the team's id and a name→id map of its projects.
func (c *graphqlClient) teamMeta(ctx context.Context, teamKey string) (teamID string, projects map[string]string, err error) {
	var out struct {
		Teams struct {
			Nodes []struct {
				ID       string `json:"id"`
				Projects struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"projects"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err = c.do(ctx, teamMetaQuery, map[string]any{"key": teamKey}, &out); err != nil {
		return "", nil, err
	}
	if len(out.Teams.Nodes) == 0 {
		return "", nil, fmt.Errorf("team %q not found", teamKey)
	}
	projects = map[string]string{}
	for _, p := range out.Teams.Nodes[0].Projects.Nodes {
		projects[strings.ToLower(strings.TrimSpace(p.Name))] = p.ID
	}
	return out.Teams.Nodes[0].ID, projects, nil
}

// --- issue create (relay→Linear backfill) ---

const issueCreateMutation = `mutation IssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) { success issue { id identifier } }
}`

// createIssue creates a Linear issue and returns its id + identifier. teamID is
// required; projectID/stateID/description are optional (empty omitted).
func (c *graphqlClient) createIssue(ctx context.Context, teamID, title, description, projectID, stateID string) (id, identifier string, err error) {
	input := map[string]any{"teamId": teamID, "title": title}
	if description != "" {
		input["description"] = description
	}
	if projectID != "" {
		input["projectId"] = projectID
	}
	if stateID != "" {
		input["stateId"] = stateID
	}
	var out struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
			} `json:"issue"`
		} `json:"issueCreate"`
	}
	if err = c.do(ctx, issueCreateMutation, map[string]any{"input": input}, &out); err != nil {
		return "", "", err
	}
	if !out.IssueCreate.Success || out.IssueCreate.Issue.ID == "" {
		return "", "", fmt.Errorf("issueCreate returned success=false")
	}
	return out.IssueCreate.Issue.ID, out.IssueCreate.Issue.Identifier, nil
}

// --- write-back mutations ---

const issueUpdateMutation = `mutation IssueUpdate($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) { success }
}`

func (c *graphqlClient) issueUpdateState(ctx context.Context, issueID, stateID string) error {
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.do(ctx, issueUpdateMutation, map[string]any{"id": issueID, "stateId": stateID}, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return fmt.Errorf("issueUpdate returned success=false")
	}
	return nil
}

const commentCreateMutation = `mutation CommentCreate($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) { success }
}`

func (c *graphqlClient) commentCreate(ctx context.Context, issueID, body string) error {
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	if err := c.do(ctx, commentCreateMutation, map[string]any{"issueId": issueID, "body": body}, &out); err != nil {
		return err
	}
	if !out.CommentCreate.Success {
		return fmt.Errorf("commentCreate returned success=false")
	}
	return nil
}
