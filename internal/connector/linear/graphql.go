package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// --- active cycle issues (reconcile) ---

// The active-cycle pull is split in two queries to stay under Linear's GraphQL
// complexity budget (10k): one cheap cycle-meta lookup, then paginated issues.
// A single nested teams→activeCycle→issues{...} query scores ~20k on a 50-issue
// cycle and is rejected with "Query too complex".
const activeCycleQuery = `query ActiveCycle($key: String!) {
  teams(filter: { key: { eq: $key } }) {
    nodes {
      activeCycle { id name startsAt endsAt }
    }
  }
}`

const cycleIssuesQuery = `query CycleIssues($cycleId: ID!, $after: String) {
  issues(filter: { cycle: { id: { eq: $cycleId } } }, first: 25, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id identifier number title description priority estimate url
      state { id name type }
      assignee { id name displayName }
      project { id name }
      parent { id }
      labels { nodes { name } }
    }
  }
}`

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

func (c *graphqlClient) activeCycleIssues(ctx context.Context, teamKey string) ([]gqlIssue, error) {
	var meta struct {
		Teams struct {
			Nodes []struct {
				ActiveCycle *struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					StartsAt string `json:"startsAt"`
					EndsAt   string `json:"endsAt"`
				} `json:"activeCycle"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := c.do(ctx, activeCycleQuery, map[string]any{"key": teamKey}, &meta); err != nil {
		return nil, err
	}
	if len(meta.Teams.Nodes) == 0 {
		return nil, fmt.Errorf("team %q not found", teamKey)
	}
	ac := meta.Teams.Nodes[0].ActiveCycle
	if ac == nil {
		return nil, nil // no active cycle right now — nothing to heal
	}

	// Page through the cycle's issues (25/page keeps each request well under
	// the complexity budget; bounded to 40 pages = 1000 issues as a backstop).
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
		vars := map[string]any{"cycleId": ac.ID}
		if after != nil {
			vars["after"] = *after
		}
		if err := c.do(ctx, cycleIssuesQuery, vars, &out); err != nil {
			return nil, err
		}
		issues = append(issues, out.Issues.Nodes...)
		if !out.Issues.PageInfo.HasNextPage {
			break
		}
		cur := out.Issues.PageInfo.EndCursor
		after = &cur
	}

	// Backfill cycle fields onto each issue (the paginated query doesn't expand
	// the per-issue cycle to keep complexity down — they're all in this cycle).
	for i := range issues {
		if issues[i].Cycle == nil {
			issues[i].Cycle = &struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				StartsAt string `json:"startsAt"`
				EndsAt   string `json:"endsAt"`
			}{ID: ac.ID, Name: ac.Name, StartsAt: ac.StartsAt, EndsAt: ac.EndsAt}
		}
	}
	return issues, nil
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
