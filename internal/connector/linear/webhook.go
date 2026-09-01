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

	// Fan-out: mirror the issue into every relay project linear_project_map
	// routes it to (usually one; several when the setting maps this Linear
	// project to an array). Each target gets its own seed/upsert/dispatch;
	// only the primary (index 0) may drive Linear write-back — see
	// seedFromIssue's primary/Secondary threading and handleTypedTicket below.
	var events []connector.TaskEvent
	for i, project := range c.projectsFor(iss) {
		primary := i == 0
		seed := c.seedFromIssue(iss, project, primary)
		if strings.EqualFold(env.Action, "remove") {
			// Issue deleted/archived in Linear — mark the mirror cancelled, keep history.
			seed.Status = "cancelled"
		}

		// Typed-ticket enforcement (V-lifecycle), unified with the reconcile poll on
		// a single refused mirror row + refusal_notified_at marker (see
		// handleTypedTicket). A non-conforming issue on a typed-ticket project is
		// refused with a loud comment back on the issue (never a silent relay log, or
		// the executive thinks it dispatched and the work dies in the void). A "remove"
		// is never refused; a non-agent issue was never dispatchable so it is not
		// refused either (kept symmetric with the poll, which skips non-agent issues
		// before enforcement). Work already in flight is never retro-refused. Only the
		// primary mirror ever posts the loud refusal comment (handleTypedTicket's
		// primary param) — a secondary mirror still gets a refused, non-dispatching
		// row, silently.
		if !strings.EqualFold(env.Action, "remove") &&
			c.db.ProjectRequiresTypedTicket(seed.Project) && isAgent(c.dispatchTarget(iss)) {
			existing, err := c.db.GetTaskByLinearIssueID(seed.Project, iss.ID)
			if err != nil {
				return nil, err
			}
			decision, err := c.handleTypedTicket(iss, seed, existing, primary)
			if err != nil {
				return nil, err
			}
			if decision == refusedHold {
				continue
			}
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
		// assignee fires exactly one task.in_progress PER mirror project — each
		// target project needs its own registered agent to see/claim its own copy
		// of the task. Dedupe on updatedFrom: only emit when the state actually
		// changed in this update.
		if c.shouldDispatch(env, iss) {
			events = append(events, c.dispatchEvent(taskID, iss.Title, c.dispatchTarget(iss), seed))
		}
	}
	return events, nil
}

// dispatchEvent builds the semantic task.in_progress launch event. Shared by
// the webhook path (Ingest) and the reconcile poll (transition detection). The
// dispatch target agent is resolved by the caller (c.dispatchTarget) — it has
// the full issue, including the delegate field the seed doesn't carry.
func (c *Connector) dispatchEvent(taskID, title, agent string, seed db.LinearMirrorSeed) connector.TaskEvent {
	// Emit "task.dispatched", NOT "task.in_progress". Routing a Linear issue to an
	// agent is semantically a DISPATCH ("here is assigned work, go claim it") —
	// the same signal relay-native dispatch_task emits — so it must fire the same
	// agent-notification rule (auto-claim: task.dispatched + assignee_is_agent →
	// message → assignee). Emitting "task.in_progress" only matched the (disabled)
	// external-launcher webhook, so the routed agent was never notified and Linear
	// dispatch silently never fired. "task.in_progress" is for an agent that has
	// already started work (start_task), a different lifecycle moment.
	return connector.TaskEvent{
		Type:    "task.dispatched",
		Project: seed.Project,
		Agent:   agent,
		Payload: map[string]any{
			"agent":             agent,
			"task_id":           taskID,
			"linear_key":        seedLinearKey(seed),
			"title":             title,
			"line":              "Dispatched: " + title,
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
	// Dispatch when the resolved target is an agent: a configured project route,
	// the issue's delegate (Linear's agent-delegation field), or a direct agent
	// assignee. dispatchTarget folds all three in priority order.
	return isAgent(c.dispatchTarget(iss))
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

// seedFromIssue maps a Linear issue (webhook or GraphQL) to a mirror seed for
// ONE target relay project (a caller fanning an issue out to several projects
// calls this once per target — see projectsFor). primary marks whether project
// is index 0 of the fan-out list — the one mirror allowed to drive Linear
// write-back (db.LinearMirrorSeed.Secondary = !primary). It resolves the
// parent's relay task id by the parent's Linear issue id when the parent has
// already been mirrored into this same project.
func (c *Connector) seedFromIssue(iss gqlIssue, project string, primary bool) db.LinearMirrorSeed {
	seed := db.LinearMirrorSeed{
		Project:       project,
		Secondary:     !primary,
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
	// Routing lane: the resolved dispatch target (project route → delegate →
	// assignee) IS the lead this issue routes to, so it becomes the mirror task's
	// profile_slug — the field GetOldestPendingTaskForProfile pulls a lane's queue
	// by. Without it every Linear-born task inserted blank and was unroutable.
	// Only an agent target sets it; a human/empty target leaves profile_slug unset.
	if target := c.dispatchTarget(iss); isAgent(target) {
		seed.ProfileSlug = target
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
		// Parent resolves within the child's own relay project. A sub-issue in a
		// different Linear project than its parent is not linked (cross-project
		// parenting is out of scope); same-project parenting is the common case.
		if parent, err := c.db.GetTaskByLinearIssueID(seed.Project, pid); err == nil && parent != nil {
			seed.ParentTaskID = strptr(parent.ID)
		}
	}
	// Typed ticket (V-lifecycle): parse the issue description onto the mirror so
	// EVERY path that builds a seed (webhook + reconcile) carries goal/AC/dod
	// consistently. Enforcement (refuse/dispatch-gate) lives at the call sites.
	tk := parseTicket(seed.Description)
	seed.Goal = tk.goal
	seed.Dod = tk.dod
	if acJSON, err := json.Marshal(tk.acceptance); err == nil && len(tk.acceptance) > 0 {
		seed.AcceptanceCriteria = string(acJSON)
	} else {
		seed.AcceptanceCriteria = "[]"
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

// issueDelegate returns the lowercased name of the issue's delegate (Linear's
// agent-delegation field), or "" when none. Only the reconcile GraphQL query
// populates this; Issue webhooks don't carry it.
func issueDelegate(iss gqlIssue) string {
	if iss.Delegate == nil {
		return ""
	}
	name := iss.Delegate.DisplayName
	if name == "" {
		name = iss.Delegate.Name
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
	return c.settingMap("linear_routing")
}

// ParseProjectMap decodes the linear_project_map setting value into
// {linearProjectId: []relayProject}. Each entry's value may be a plain JSON
// string (one target — the original single-project form, kept byte-identical)
// or a JSON array of strings (fan-out: the issue mirrors into every listed
// relay project, index 0 is the PRIMARY mirror — the only one that drives
// Linear write-back, see db.LinearMirrorSeed.Secondary). Malformed top-level
// JSON, an unparseable entry, or an entry that normalizes to nothing are all
// dropped rather than erroring — callers treat "no entry" as "falls back to
// the default project", never a fatal config error. Entries are lowercased,
// trimmed, and de-duplicated within themselves.
func ParseProjectMap(raw string) map[string][]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for pid, rawVal := range m {
		var list []string
		trimmed := trimSpace(rawVal)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			if err := json.Unmarshal(rawVal, &list); err != nil {
				continue
			}
		} else {
			var s string
			if err := json.Unmarshal(rawVal, &s); err != nil {
				continue
			}
			list = []string{s}
		}
		seen := make(map[string]bool, len(list))
		norm := make([]string, 0, len(list))
		for _, p := range list {
			p = strings.ToLower(strings.TrimSpace(p))
			if p != "" && !seen[p] {
				seen[p] = true
				norm = append(norm, p)
			}
		}
		if len(norm) > 0 {
			out[pid] = norm
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// projectTargets reads the owner-configured Linear-project→relay-project(s)
// map (setting "linear_project_map"). It lets several relay projects share one
// Linear team, and — since ParseProjectMap accepts an array value — lets ONE
// Linear project fan out into SEVERAL relay projects at once. Empty when unset.
func (c *Connector) projectTargets() map[string][]string {
	return ParseProjectMap(c.db.GetSetting("linear_project_map"))
}

// settingMap decodes a JSON string→string settings value; nil on empty/invalid.
func (c *Connector) settingMap(key string) map[string]string {
	raw := strings.TrimSpace(c.db.GetSetting(key))
	if raw == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// projectsFor resolves every relay project an issue's mirror fans out to: the
// linear_project_map entry for the issue's Linear project (a plain string is
// one target, an array is several), or [c.project] when unmapped — the
// original single-project fallback, unchanged. Index 0 is always the PRIMARY
// mirror. This is the ONE decision that makes the mirror multi-project: every
// write/lookup for an issue must scope to one of projectsFor(iss)'s entries,
// never a hardcoded c.project.
func (c *Connector) projectsFor(iss gqlIssue) []string {
	if pid := issueProjectID(iss); pid != "" {
		if targets := c.projectTargets()[pid]; len(targets) > 0 {
			return targets
		}
	}
	return []string{c.project}
}

// mirrorProjects returns every relay project this connector's mirror may write
// to: the default project plus every distinct target in linear_project_map
// (flattened across every mapped Linear project's target list, single-string or
// array). Used by cross-project sweeps (the dropout-sync, backfill) that must
// cover all lanes, not just the default one. The default is always first and
// never duplicated.
func (c *Connector) mirrorProjects() []string {
	out := []string{c.project}
	seen := map[string]bool{c.project: true}
	for _, targets := range c.projectTargets() {
		for _, p := range targets {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// dispatchTarget resolves the agent to dispatch an issue to, in priority order:
//  1. the agent configured for the issue's Linear project (owner-chosen route,
//     one fixed agent per project — wins for single-lane dev projects);
//  2. the issue's delegate (Linear's agent-delegation field — the human stays
//     assignee, the agent is the delegate; this is how multi-lead projects route
//     each issue to its own lead without a fixed project route);
//  3. the issue's assignee (when it's an agent directly).
func (c *Connector) dispatchTarget(iss gqlIssue) string {
	if pid := issueProjectID(iss); pid != "" {
		if a := c.linearRouting()[pid]; a != "" {
			return strings.ToLower(strings.TrimSpace(a))
		}
	}
	if d := issueDelegate(iss); d != "" {
		return d
	}
	return issueAssignee(iss)
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

func seedLinearKey(s db.LinearMirrorSeed) any {
	if s.LinearKey != nil {
		return *s.LinearKey
	}
	return nil
}

func strptr(s string) *string { return &s }
