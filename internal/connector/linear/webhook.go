package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-relay/internal/connector"
	"agent-relay/internal/db"
)

// webhookFreshness bounds how old a webhook's self-reported timestamp may be.
const webhookFreshness = 60 * time.Second

// MaxWebhookBody caps the inbound webhook size (defense against oversized bodies).
const MaxWebhookBody = 1 << 20 // 1 MiB

// labelList decodes Linear labels from either the webhook array form
// ([{ "name": ... }]) or the GraphQL connection form ({ "nodes": [{ "name": ... }] }).
type labelList []string

func (l *labelList) UnmarshalJSON(data []byte) error {
	data = trimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*l = nil
		return nil
	}
	// Array form: [{name}, ...]
	if data[0] == '[' {
		var arr []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		out := make(labelList, 0, len(arr))
		for _, a := range arr {
			if a.Name != "" {
				out = append(out, a.Name)
			}
		}
		*l = out
		return nil
	}
	// Connection form: {nodes:[{name}]}
	var conn struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(data, &conn); err != nil {
		return err
	}
	out := make(labelList, 0, len(conn.Nodes))
	for _, n := range conn.Nodes {
		if n.Name != "" {
			out = append(out, n.Name)
		}
	}
	*l = out
	return nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 {
		last := b[len(b)-1]
		if last == ' ' || last == '\t' || last == '\n' || last == '\r' {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}

// webhookEnvelope is the Linear webhook payload shell.
type webhookEnvelope struct {
	Action           string                     `json:"action"` // create | update | remove
	Type             string                     `json:"type"`   // Issue | Comment | ...
	Data             gqlIssue                   `json:"data"`
	UpdatedFrom      map[string]json.RawMessage `json:"updatedFrom"`
	WebhookTimestamp int64                      `json:"webhookTimestamp"` // unix ms
	Actor            struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"actor"`
}

// VerifySignature checks the HMAC-SHA256 of the raw body against the
// Linear-Signature header and the webhook timestamp freshness. It is the cheap
// synchronous gate the HTTP handler runs before returning 200 + async-processing.
func (c *Connector) VerifySignature(payload []byte, sig string) error {
	if c.secret == "" {
		return fmt.Errorf("webhook secret not configured")
	}
	if len(payload) == 0 {
		return fmt.Errorf("empty payload")
	}
	if len(payload) > MaxWebhookBody {
		return fmt.Errorf("payload too large")
	}
	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.TrimSpace(sig))) {
		return fmt.Errorf("signature mismatch")
	}
	// Freshness: webhookTimestamp must be within the freshness window.
	var head struct {
		WebhookTimestamp int64 `json:"webhookTimestamp"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return fmt.Errorf("decode timestamp: %w", err)
	}
	if head.WebhookTimestamp > 0 {
		age := time.Since(time.UnixMilli(head.WebhookTimestamp))
		if age < 0 {
			age = -age
		}
		if age > webhookFreshness {
			return fmt.Errorf("stale webhook (age %s)", age)
		}
	}
	return nil
}

// Ingest verifies, parses, and applies a Linear webhook. It upserts the mirror
// (Linear zone only) and returns the semantic events the relay should emit.
// Self-authored echoes (anti-loop FR-7) are dropped with no events and no error.
func (c *Connector) Ingest(payload []byte, sig string) ([]connector.TaskEvent, error) {
	if err := c.VerifySignature(payload, sig); err != nil {
		return nil, err
	}
	var env webhookEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("decode webhook: %w", err)
	}
	// Only Issue events are mirrored today (Comment etc. ignored).
	if !strings.EqualFold(env.Type, "Issue") {
		return nil, nil
	}
	// Anti-loop: drop events authored by our own API-key user (our In Review
	// write echoes back as a webhook — never re-process it).
	if env.Actor.ID != "" {
		if viewer, _ := c.ensureViewerID(context.Background()); viewer != "" && env.Actor.ID == viewer {
			c.lastWebhookAt.Store(time.Now().UnixMilli())
			return nil, nil
		}
	}
	c.lastWebhookAt.Store(time.Now().UnixMilli())

	iss := env.Data
	if iss.ID == "" {
		return nil, fmt.Errorf("webhook issue missing id")
	}

	seed := c.seedFromIssue(iss)
	if strings.EqualFold(env.Action, "remove") {
		// Issue deleted/archived in Linear — mark the mirror cancelled, keep history.
		seed.Status = "cancelled"
	}

	taskID, _, err := c.db.UpsertLinearMirror(seed)
	if err != nil {
		return nil, err
	}

	// Done echo (the one inbound exception that touches the overlay): when the
	// issue lands in a completed-type state, stamp done_at/completed_at.
	if iss.State != nil && iss.State.Type == "completed" {
		_ = c.db.MarkLinearDone(taskID)
	}

	// Dispatch emit (FR-3): a → In Progress (started) transition with an agent
	// assignee fires exactly one task.in_progress. Dedupe on updatedFrom: only
	// emit when the state actually changed in this update.
	var events []connector.TaskEvent
	if c.shouldDispatch(env, iss) {
		events = append(events, c.dispatchEvent(taskID, iss.Title, seed))
	}
	return events, nil
}

// dispatchEvent builds the semantic task.in_progress launch event. Shared by
// the webhook path (Ingest) and the reconcile poll (transition detection).
func (c *Connector) dispatchEvent(taskID, title string, seed db.LinearMirrorSeed) connector.TaskEvent {
	agent := c.routedAgent(seed)
	return connector.TaskEvent{
		Type:    "task.in_progress",
		Project: c.project,
		Agent:   agent,
		Payload: map[string]any{
			"agent":             agent,
			"task_id":           taskID,
			"linear_key":        seedLinearKey(seed),
			"title":             title,
			"line":              "In progress: " + title,
			"priority":          seed.Priority,
			"assignee_is_agent": isAgent(agent),
		},
	}
}

// shouldDispatch reports whether this webhook is a genuine → In Progress
// transition with an agent assignee that has not already been signaled.
func (c *Connector) shouldDispatch(env webhookEnvelope, iss gqlIssue) bool {
	if !strings.EqualFold(env.Action, "update") {
		return false
	}
	if iss.State == nil || iss.State.Type != "started" {
		return false
	}
	// In Review is also a "started" type — don't treat it as a launch.
	if looksLikeReview(iss.State.Name) {
		return false
	}
	// Dedupe: only fire when the state changed in this very update.
	if !stateChanged(env.UpdatedFrom) {
		return false
	}
	// Dispatch when the issue's project has a configured route (owner-chosen
	// agent per project) OR it's directly assigned to an agent.
	return c.hasRoute(iss) || isAgent(issueAssignee(iss))
}

// stateChanged reports whether updatedFrom carries a prior state (the transition
// touched the workflow state in this update).
func stateChanged(updatedFrom map[string]json.RawMessage) bool {
	if updatedFrom == nil {
		return false
	}
	if _, ok := updatedFrom["stateId"]; ok {
		return true
	}
	if _, ok := updatedFrom["state"]; ok {
		return true
	}
	return false
}

// seedFromIssue maps a Linear issue (webhook or GraphQL) to a mirror seed. It
// resolves the parent's relay task id by the parent's Linear issue id when the
// parent has already been mirrored.
func (c *Connector) seedFromIssue(iss gqlIssue) db.LinearMirrorSeed {
	seed := db.LinearMirrorSeed{
		Project:       c.project,
		LinearIssueID: iss.ID,
		Title:         iss.Title,
		Description:   iss.Description,
		Priority:      mapPriority(iss.Priority),
		Status:        mapStatus(iss.State),
		Labels:        marshalLabels(iss.Labels),
	}
	if key := issueKey(iss, c.teamKey); key != "" {
		seed.LinearKey = strptr(key)
	}
	if iss.URL != "" {
		seed.ExternalURL = strptr(iss.URL)
	}
	if iss.Estimate != nil {
		pts := int(*iss.Estimate)
		seed.Points = &pts
	}
	if iss.State != nil && iss.State.Name != "" {
		seed.LinearState = strptr(iss.State.Name)
	}
	if a := issueAssignee(iss); a != "" {
		seed.Assignee = strptr(a)
	}
	if pid := issueProjectID(iss); pid != "" {
		seed.LinearProjectID = strptr(pid)
	}
	if iss.Cycle != nil && iss.Cycle.ID != "" {
		seed.CycleID = strptr(iss.Cycle.ID)
		if iss.Cycle.Name != "" {
			seed.CycleName = strptr(iss.Cycle.Name)
		}
		if iss.Cycle.StartsAt != "" {
			seed.CycleStart = strptr(iss.Cycle.StartsAt)
		}
		if iss.Cycle.EndsAt != "" {
			seed.CycleEnd = strptr(iss.Cycle.EndsAt)
		}
	}
	if pid := iss.parentLinearID(); pid != "" {
		if parent, err := c.db.GetTaskByLinearIssueID(c.project, pid); err == nil && parent != nil {
			seed.ParentTaskID = strptr(parent.ID)
		}
	}
	return seed
}

// --- small mapping helpers ---

// mapPriority maps Linear's 0..4 priority to the relay's P0..P3.
// Linear: 0 none, 1 urgent, 2 high, 3 normal, 4 low.
func mapPriority(p float64) string {
	switch int(p) {
	case 1:
		return "P0"
	case 2:
		return "P1"
	case 3:
		return "P2"
	case 4:
		return "P3"
	default:
		return "P2"
	}
}

func issueAssignee(iss gqlIssue) string {
	if iss.Assignee == nil {
		return ""
	}
	name := iss.Assignee.DisplayName
	if name == "" {
		name = iss.Assignee.Name
	}
	return strings.ToLower(strings.TrimSpace(name))
}

func issueProjectID(iss gqlIssue) string {
	if iss.Project != nil && iss.Project.ID != "" {
		return iss.Project.ID
	}
	return strings.TrimSpace(iss.ProjectID)
}

// linearRouting reads the owner-configured project→agent map (setting
// "linear_routing", JSON {linearProjectId: agentName}). Empty when unset.
func (c *Connector) linearRouting() map[string]string {
	raw := strings.TrimSpace(c.db.GetSetting("linear_routing"))
	if raw == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// routedAgent resolves the dispatch target: the agent configured for the issue's
// Linear project (owner-chosen, one per project), falling back to the issue's
// own assignee when that project has no routing entry.
func (c *Connector) routedAgent(seed db.LinearMirrorSeed) string {
	if seed.LinearProjectID != nil {
		if a := c.linearRouting()[*seed.LinearProjectID]; a != "" {
			return strings.ToLower(strings.TrimSpace(a))
		}
	}
	return seedAssignee(seed)
}

// hasRoute reports whether the issue's project has a configured routing target.
func (c *Connector) hasRoute(iss gqlIssue) bool {
	pid := issueProjectID(iss)
	return pid != "" && c.linearRouting()[pid] != ""
}

func issueKey(iss gqlIssue, teamKey string) string {
	if iss.Identifier != "" {
		return iss.Identifier
	}
	if teamKey != "" && iss.Number > 0 {
		return fmt.Sprintf("%s-%d", teamKey, int(iss.Number))
	}
	return ""
}

func marshalLabels(l labelList) string {
	if len(l) == 0 {
		return "[]"
	}
	b, err := json.Marshal([]string(l))
	if err != nil {
		return "[]"
	}
	return string(b)
}

// isAgent reports whether an assignee name denotes an agent (not a human seat).
func isAgent(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n != "" && n != "human" && n != "user"
}

func seedAssignee(s db.LinearMirrorSeed) string {
	if s.Assignee != nil {
		return *s.Assignee
	}
	return ""
}

func seedLinearKey(s db.LinearMirrorSeed) any {
	if s.LinearKey != nil {
		return *s.LinearKey
	}
	return nil
}

func strptr(s string) *string { return &s }
