// Package linear implements the Linear TaskConnector: a one-way SSOT→mirror sync
// (verified webhook + reconcile poll) plus a single owned write-back (→ In Review
// + comment). Linear is the source of truth; the relay mirror is a read-replica.
package linear

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-relay/internal/config"
	"agent-relay/internal/connector"
	"agent-relay/internal/db"
)

// Connector is the Linear-mode TaskConnector. It is constructed only when the
// config is active (RELAY_LINEAR_MODE=1 + LINEAR_API_KEY); otherwise the relay
// uses connector.Noop and this code path is dormant.
type Connector struct {
	db      *db.DB
	gql     *graphqlClient
	project string // relay project the mirror rows live under
	teamKey string
	secret  string // webhook signing secret

	mu          sync.RWMutex
	viewerID    string               // the API key's own user id (anti-loop)
	reviewState string               // cached "In Review" state id
	states      map[string]stateInfo // team workflow states by id (cache)

	lastWebhookAt   atomic.Int64 // unix ms
	lastReconcileAt atomic.Int64 // unix ms
	writerFailures  atomic.Int64

	// onEvent receives semantic events found OUTSIDE the webhook path (the
	// reconcile poll detecting a → In Progress transition). The webhook path
	// returns events from Ingest instead; this sink exists because reconcile
	// runs on its own goroutine with no caller to hand events back to. Set
	// once at wiring time (relay.New), before StartReconcile.
	onEvent func(connector.TaskEvent)
}

// SetEventSink installs the bus callback used by the reconcile poll. Must be
// called before StartReconcile; nil-safe (events are dropped when unset).
func (c *Connector) SetEventSink(fn func(connector.TaskEvent)) { c.onEvent = fn }

// Verify the interface is satisfied at compile time.
var _ connector.TaskConnector = (*Connector)(nil)

// New builds a Linear connector from config. The project the mirror is stored
// under defaults to the lowercased team key, falling back to "default".
func New(database *db.DB, cfg config.Config) *Connector {
	return NewWithParams(database, cfg.LinearAPIKey, cfg.LinearTeamKey, cfg.LinearWebhookSecret, "")
}

// NewWithParams builds a connector from explicit credentials — used by the
// settings-driven runtime (re)configuration where values come from the DB
// rather than the environment. project overrides the relay project the mirror
// lives under; empty = lowercased team key.
func NewWithParams(database *db.DB, apiKey, teamKey, secret, project string) *Connector {
	project = strings.ToLower(strings.TrimSpace(project))
	if project == "" {
		project = strings.ToLower(strings.TrimSpace(teamKey))
	}
	if project == "" {
		project = "default"
	}
	return &Connector{
		db:      database,
		gql:     newGraphQLClient(apiKey),
		project: project,
		teamKey: strings.TrimSpace(teamKey),
		secret:  secret,
		states:  map[string]stateInfo{},
	}
}

// TeamInfo is a lightweight team descriptor for the settings UI picker.
type TeamInfo struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	ActiveCycle string `json:"active_cycle,omitempty"`
}

// ListTeams fetches the workspace's teams with a bare API key. Used by the
// settings UI to offer a team picker before any connector exists.
func ListTeams(ctx context.Context, apiKey string) ([]TeamInfo, error) {
	gql := newGraphQLClient(apiKey)
	var out struct {
		Teams struct {
			Nodes []struct {
				ID          string `json:"id"`
				Key         string `json:"key"`
				Name        string `json:"name"`
				ActiveCycle *struct {
					Name string `json:"name"`
				} `json:"activeCycle"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	q := `{ teams { nodes { id key name activeCycle { name } } } }`
	if err := gql.do(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	teams := make([]TeamInfo, 0, len(out.Teams.Nodes))
	for _, t := range out.Teams.Nodes {
		ti := TeamInfo{ID: t.ID, Key: t.Key, Name: t.Name}
		if t.ActiveCycle != nil {
			ti.ActiveCycle = t.ActiveCycle.Name
		}
		teams = append(teams, ti)
	}
	return teams, nil
}

// Active reports that this connector performs external I/O.
func (c *Connector) Active() bool { return true }

// Project is the relay project the mirror is stored under (used by the reconcile
// loop wiring in main.go).
func (c *Connector) Project() string { return c.project }

// MapState maps a Linear state TYPE to the relay's coarse status.
func (c *Connector) MapState(providerState string) string {
	return connector.MapStateType(providerState)
}

// mapStatus is the richer mapping used for upserts: state type drives the coarse
// status, but a "started" state whose name reads like review lands the mirror in
// the in-review column so the board column is correct.
func mapStatus(st *stateInfo) string {
	if st == nil {
		return "pending"
	}
	coarse := connector.MapStateType(st.Type)
	if st.Type == "started" && looksLikeReview(st.Name) {
		return "in-review"
	}
	return coarse
}

func looksLikeReview(name string) bool {
	return strings.Contains(strings.ToLower(name), "review")
}

// Warmup fetches the viewer id (anti-loop) and the team's workflow states. It is
// best-effort: failures are logged and retried lazily on first use. Safe to call
// from a startup goroutine.
func (c *Connector) Warmup(ctx context.Context) {
	if _, err := c.ensureViewerID(ctx); err != nil {
		log.Printf("[linear] warmup viewer id: %v", err)
	}
	if _, err := c.ensureReviewState(ctx); err != nil {
		log.Printf("[linear] warmup review state: %v", err)
	}
}

// ensureViewerID returns the API key's user id, fetching+caching on first call.
func (c *Connector) ensureViewerID(ctx context.Context) (string, error) {
	c.mu.RLock()
	id := c.viewerID
	c.mu.RUnlock()
	if id != "" {
		return id, nil
	}
	fetched, err := c.gql.viewerID(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.viewerID = fetched
	c.mu.Unlock()
	return fetched, nil
}

// ensureReviewState resolves and caches the team's In Review state id. Preference:
// a "started"-type state whose name reads like review; else any state named
// "in review"; else the first "started" state.
func (c *Connector) ensureReviewState(ctx context.Context) (string, error) {
	c.mu.RLock()
	id := c.reviewState
	c.mu.RUnlock()
	if id != "" {
		return id, nil
	}
	states, err := c.gql.teamStates(ctx, c.teamKey)
	if err != nil {
		return "", err
	}
	byID := make(map[string]stateInfo, len(states))
	var reviewStarted, namedReview, firstStarted string
	for _, s := range states {
		byID[s.ID] = s
		if s.Type == "started" && looksLikeReview(s.Name) && reviewStarted == "" {
			reviewStarted = s.ID
		}
		if looksLikeReview(s.Name) && namedReview == "" {
			namedReview = s.ID
		}
		if s.Type == "started" && firstStarted == "" {
			firstStarted = s.ID
		}
	}
	chosen := reviewStarted
	if chosen == "" {
		chosen = namedReview
	}
	if chosen == "" {
		chosen = firstStarted
	}
	c.mu.Lock()
	c.states = byID
	if chosen != "" {
		c.reviewState = chosen
	}
	c.mu.Unlock()
	if chosen == "" {
		return "", errNoReviewState
	}
	return chosen, nil
}

// Status returns a small health snapshot for /api/health.
func (c *Connector) Status() map[string]any {
	out := map[string]any{
		"active":          true,
		"team_key":        c.teamKey,
		"project":         c.project,
		"writer_failures": c.writerFailures.Load(),
	}
	if ms := c.lastWebhookAt.Load(); ms > 0 {
		out["last_webhook_at"] = time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	if ms := c.lastReconcileAt.Load(); ms > 0 {
		out["last_reconcile_at"] = time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	c.mu.RLock()
	out["viewer_resolved"] = c.viewerID != ""
	out["review_state_resolved"] = c.reviewState != ""
	c.mu.RUnlock()
	return out
}

// errNoReviewState is returned when the team exposes no usable In Review state.
var errNoReviewState = errConst("no In Review (started) state found for team")

type errConst string

func (e errConst) Error() string { return string(e) }
