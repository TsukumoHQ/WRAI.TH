package relay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"agent-relay/internal/db"
	"agent-relay/internal/models"
)

// RelayWebhookSecretEnv is the env var holding the HMAC secret used to sign
// outbound webhook payloads (X-Relay-Signature header).
const RelayWebhookSecretEnv = "RELAY_WEBHOOK_SECRET"

const (
	webhookMaxRetries  = 3
	webhookRetryBase   = 200 * time.Millisecond
	webhookTimeout     = 10 * time.Second
	digestTickInterval = time.Minute
	defaultDigestHours = 8
)

// Notifier is the notifications subsystem: a rules evaluator that subscribes to
// the event bus and fires fire-and-forget actions (message / webhook / slack),
// plus a coalesced digest scheduler. It never blocks the bus and never mutates
// task state.
type Notifier struct {
	db       *db.DB
	registry *SessionRegistry
	events   *EventBus
	http     *http.Client
	done     <-chan struct{}

	// digestLastFired tracks the last cycle.digest emission per project to
	// coalesce intervals into a single event.
	digestLastFired map[string]time.Time
}

// NewNotifier builds the subsystem. Seeds default rules on first run.
func NewNotifier(database *db.DB, registry *SessionRegistry, events *EventBus) *Notifier {
	n := &Notifier{
		db:              database,
		registry:        registry,
		events:          events,
		http:            &http.Client{Timeout: webhookTimeout},
		digestLastFired: make(map[string]time.Time),
	}
	n.seedDefaults()
	return n
}

// Start launches the evaluator loop and the digest scheduler. Stops when done
// is closed.
func (n *Notifier) Start(done <-chan struct{}) {
	n.done = done
	go n.runEvaluator(done)
	go n.runSweeper(done)
	go n.runDigestScheduler(done)
}

// Outbox sweeper tuning.
const (
	sweepInterval    = 1500 * time.Millisecond // delivery latency ceiling per tick
	sweepBatch       = 100                     // undelivered events processed per tick
	maxEventAttempts = 5                       // dead-letter past this many failed tries
	sweepPruneEvery  = 40                      // prune the replay log every Nth tick (~1min)
	eventLogKeep     = 5000                    // delivered rows retained for replay
)

// --- Evaluator ---

// runEvaluator subscribes to the bus and dispatches matching rules. Each rule
// fires in its own goroutine so a slow webhook never blocks the bus.
func (n *Notifier) runEvaluator(done <-chan struct{}) {
	ch := n.events.Subscribe()
	defer n.events.Unsubscribe(ch)
	for {
		select {
		case <-done:
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			n.handleEvent(evt)
		}
	}
}

// handleEvent persists a semantic event to the durable outbox (TSU-52). Delivery
// is no longer synchronous here — the sweeper (runSweeper) drains the outbox and
// fires rules, so a relay restart or a transient action failure can't silently
// drop a notification. Only semantic events (those carrying a Semantic payload)
// are persisted; visual events are ignored.
func (n *Notifier) handleEvent(evt MCPEvent) {
	if evt.Semantic == nil {
		return
	}
	// delivery_id is empty here (internal event → fresh UUID); external sources
	// (a GitHub webhook) pass a stable key so a retry dedupes via INSERT OR IGNORE.
	payload, err := json.Marshal(evt.Semantic)
	if err != nil {
		log.Printf("notifier: marshal event %s: %v", evt.Type, err)
		return
	}
	if _, _, err := n.db.InsertEvent("", evt.Project, evt.Type, strVal(evt.Semantic["agent"]), string(payload)); err != nil {
		log.Printf("notifier: persist event %s: %v", evt.Type, err)
	}
}

// deliverEvent fires the rules matching one outbox event, then marks it
// delivered. It only retries (toward the DLQ) when EVERY matched rule failed —
// that path has no successful side effect (e.g. an already-sent inbox message)
// to duplicate on the next attempt, so retries stay safe.
func (n *Notifier) deliverEvent(e db.Event) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
		n.failEvent(e, "payload unmarshal: "+err.Error())
		return
	}
	rules, err := n.db.ListEnabledNotificationRulesForEvent(e.EventType)
	if err != nil {
		n.failEvent(e, "list rules: "+err.Error())
		return
	}
	fired, failures := 0, 0
	for _, rule := range rules {
		// Scope by project: a rule fires for its own project, or "default" acts
		// as a global fallback.
		if rule.Project != e.Project && rule.Project != "default" {
			continue
		}
		if !matchRule(rule, payload) {
			continue
		}
		fired++
		if _, rec := n.fireRule(rule, e.EventType, e.Project, payload, false); rec != nil && rec.Outcome != "ok" {
			failures++
		}
	}
	// Delivered when nothing matched, or at least one matched rule succeeded.
	if fired == 0 || failures < fired {
		_ = n.db.MarkEventDelivered(e.ID)
		return
	}
	n.failEvent(e, "all matched rules failed")
}

// failEvent retries an event or dead-letters it past the attempt cap.
func (n *Notifier) failEvent(e db.Event, reason string) {
	if e.Attempts+1 >= maxEventAttempts {
		log.Printf("notifier: event %s dead-lettered after %d attempts: %s", e.ID, e.Attempts+1, reason)
		_ = n.db.MarkEventDead(e.ID, "DLQ: "+reason)
		return
	}
	_ = n.db.IncrementEventAttempt(e.ID, reason)
}

// runSweeper drains the outbox: polls undelivered events, fires their rules, and
// marks them delivered (or dead-letters after maxEventAttempts). Replaces the
// old synchronous fire-and-forget so delivery is durable + replayable. Prunes
// the replay log periodically to bound growth.
func (n *Notifier) runSweeper(done <-chan struct{}) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			events, err := n.db.UndeliveredEvents(sweepBatch)
			if err != nil {
				log.Printf("notifier: sweep: %v", err)
				continue
			}
			for _, e := range events {
				n.deliverEvent(e)
			}
			if ticks++; ticks%sweepPruneEvery == 0 {
				n.db.PruneDeliveredEvents(eventLogKeep)
			}
		}
	}
}

// matchRule evaluates a rule's match conditions against the event payload.
// Supported conditions:
//   - assignee_is_agent: bool — payload.assignee_is_agent must equal it
//   - agent: string         — payload.agent must equal it
//   - any arbitrary key      — exact equality against the payload value
//   - any key with an ARRAY value — set membership: matches if the payload value
//     equals ANY element (OR), e.g. {"status":["in-progress","accepted"]} so a
//     stale-rule fires only on active tasks, not parked Todo/backlog.
func matchRule(rule models.NotificationRule, payload map[string]any) bool {
	if strings.TrimSpace(rule.Match) == "" || rule.Match == "{}" {
		return true
	}
	var cond map[string]any
	if err := json.Unmarshal([]byte(rule.Match), &cond); err != nil {
		return true // malformed match → don't silently drop; treat as match-all
	}
	for k, want := range cond {
		got, ok := payload[k]
		if !ok {
			return false
		}
		if !matchValue(want, got) {
			return false
		}
	}
	return true
}

// matchValue supports set membership: a JSON array matches if the payload value
// equals ANY element (OR semantics); otherwise it's exact equality.
func matchValue(want, got any) bool {
	if arr, ok := want.([]any); ok {
		for _, w := range arr {
			if valuesEqual(w, got) {
				return true
			}
		}
		return false
	}
	return valuesEqual(want, got)
}

func valuesEqual(want, got any) bool {
	// JSON numbers decode as float64; normalize before comparing.
	switch w := want.(type) {
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	case float64:
		switch g := got.(type) {
		case float64:
			return g == w
		case int:
			return float64(g) == w
		}
		return false
	case string:
		g, ok := got.(string)
		return ok && g == w
	}
	return fmt.Sprintf("%v", want) == fmt.Sprintf("%v", got)
}

// --- Actions ---

// fireRule executes a rule's action. dryRun builds and logs the payload without
// actually sending (test-fire). Returns the would-be/sent payload and outcome.
func (n *Notifier) fireRule(rule models.NotificationRule, event, project string, payload map[string]any, dryRun bool) (map[string]any, *models.NotificationDelivery) {
	opts := parseOpts(rule.Opts)
	rec := &models.NotificationDelivery{
		Project:  project,
		RuleID:   rule.ID,
		RuleName: rule.Name,
		Event:    event,
		Action:   rule.Action,
		Target:   rule.Target,
	}

	built := n.buildPayload(rule, event, payload, opts)
	pj, _ := json.Marshal(built)
	rec.Payload = string(pj)

	if dryRun {
		rec.Outcome = "dryrun"
		_ = n.db.LogNotificationDelivery(rec)
		return built, rec
	}

	switch rule.Action {
	case "webhook":
		n.doWebhook(rule, built, rec)
	case "message":
		n.doMessage(rule, project, built, opts, rec)
	case "slack":
		n.doSlack(rule, project, built, rec)
	default:
		rec.Outcome = "failed"
		rec.Error = "unknown action: " + rule.Action
	}
	_ = n.db.LogNotificationDelivery(rec)
	return built, rec
}

// buildPayload assembles the tiny outbound payload. A template in opts (with
// {agent}/{task_id}/{linear_key}/{title}/{line} placeholders) overrides the
// default "line".
func (n *Notifier) buildPayload(rule models.NotificationRule, event string, sem map[string]any, opts ruleOpts) map[string]any {
	out := map[string]any{
		"event":      event,
		"agent":      strVal(sem["agent"]),
		"task_id":    strVal(sem["task_id"]),
		"linear_key": sem["linear_key"], // may be nil
		"title":      strVal(sem["title"]),
	}
	line := strVal(sem["line"])
	if opts.Template != "" {
		line = renderTemplate(opts.Template, sem)
	}
	if line == "" {
		line = strVal(sem["title"])
	}
	out["line"] = line
	// Custom agent-authored events (event:*) carry an arbitrary structured
	// payload (e.g. lead-ready → {email,tier,personId,...}). The fixed fields
	// above would drop it, so a 'message' action delivered an empty body and the
	// consumer got nothing usable (TSU-38). Pass the full event payload through so
	// doMessage serializes it into the message content and webhook/slack
	// consumers receive the structured data, not just the one-line summary.
	if strings.HasPrefix(event, "event:") {
		out["payload"] = sem
	}
	return out
}

func renderTemplate(tpl string, sem map[string]any) string {
	for k, v := range sem {
		tpl = strings.ReplaceAll(tpl, "{"+k+"}", strVal(v))
	}
	return tpl
}

// doWebhook performs the signed outbound POST with bounded retry/backoff.
func (n *Notifier) doWebhook(rule models.NotificationRule, payload map[string]any, rec *models.NotificationDelivery) {
	url := strings.TrimSpace(rule.Target)
	if url == "" {
		rec.Outcome = "failed"
		rec.Error = "webhook target URL is empty (rule disabled until configured)"
		return
	}
	body, _ := json.Marshal(payload)
	sig := signBody(body)

	var lastErr error
	var lastStatus int
	for attempt := 0; attempt < webhookMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(webhookRetryBase * time.Duration(1<<uint(attempt-1)))
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break // bad URL — retrying won't help
		}
		req.Header.Set("Content-Type", "application/json")
		if sig != "" {
			req.Header.Set("X-Relay-Signature", sig)
		}
		resp, err := n.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		lastStatus = resp.StatusCode
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rec.Outcome = "ok"
			rec.StatusCode = resp.StatusCode
			return
		}
		lastErr = fmt.Errorf("status %d", resp.StatusCode)
	}
	rec.Outcome = "failed"
	rec.StatusCode = lastStatus
	if lastErr != nil {
		rec.Error = lastErr.Error()
	}
}

// signBody returns the hex HMAC-SHA256 of body using RELAY_WEBHOOK_SECRET.
// Empty string when no secret is configured (unsigned).
func signBody(body []byte) string {
	secret := os.Getenv(RelayWebhookSecretEnv)
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// doMessage sends a relay inbox message to the resolved target(s) honoring
// ttl/priority. Best-effort: loss is harmless (Linear SSOT / mirror).
func (n *Notifier) doMessage(rule models.NotificationRule, project string, payload map[string]any, opts ruleOpts, rec *models.NotificationDelivery) {
	recipients := n.resolveTargets(project, rule.Target, payload)
	if len(recipients) == 0 {
		rec.Outcome = "failed"
		rec.Error = "no recipients resolved for target: " + rule.Target
		return
	}
	subject := strVal(payload["line"])
	if subject == "" {
		subject = rule.Event
	}
	// Content intentionally empty for built-in events: the line IS the message
	// (tiny payloads by design); duplicating it as body just doubles inbox noise.
	// For custom event:* deliveries, serialize the structured payload into the
	// body so the recipient's consumer can read its fields (TSU-38) — the line
	// alone can't carry {email,tier,personId,...}.
	content := ""
	if p, ok := payload["payload"]; ok {
		if b, err := json.Marshal(p); err == nil {
			content = string(b)
		}
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = 14400
	}
	priority := opts.Priority
	if priority == "" {
		priority = "P2"
	}
	meta := fmt.Sprintf(`{"task_id":%q,"event":%q}`, strVal(payload["task_id"]), rule.Event)

	var sent int
	for _, to := range recipients {
		msg, err := n.db.InsertMessageWithDeliveries(project, "notifier", to, "notification", subject, content, meta, priority, ttl, nil, nil, []string{to})
		if err != nil {
			rec.Error = err.Error()
			continue
		}
		n.registry.Notify(project, to, "notifier", subject, msg.ID)
		sent++
	}
	if sent > 0 {
		rec.Outcome = "ok"
	} else {
		rec.Outcome = "failed"
	}
}

// doSlack posts to the human's configured notify channel(s). With no Slack
// integration wired, it routes through the same message path to the human as a
// best-effort fallback, recording the intent.
func (n *Notifier) doSlack(rule models.NotificationRule, project string, payload map[string]any, rec *models.NotificationDelivery) {
	// If the target looks like a URL, treat it as a Slack incoming webhook.
	if strings.HasPrefix(rule.Target, "http://") || strings.HasPrefix(rule.Target, "https://") {
		body, _ := json.Marshal(map[string]any{"text": strVal(payload["line"])})
		req, err := http.NewRequest(http.MethodPost, rule.Target, bytes.NewReader(body))
		if err != nil {
			rec.Outcome = "failed"
			rec.Error = err.Error()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := n.http.Do(req)
		if err != nil {
			rec.Outcome = "failed"
			rec.Error = err.Error()
			return
		}
		rec.StatusCode = resp.StatusCode
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rec.Outcome = "ok"
		} else {
			rec.Outcome = "failed"
			rec.Error = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return
	}
	// Otherwise fall back to an inbox message to the human.
	n.doMessage(rule, project, payload, ruleOpts{}, rec)
}

// resolveTargets maps a rule target to concrete agent recipient names.
//
//	"human"                 → the literal "human"/"user" inbox
//	"manager" / "reports_to"→ the manager of the event's agent (reports_to)
//	a role (cto/cxx/lead)   → all agents with that role in the project
//	an agent name           → that agent
func (n *Notifier) resolveTargets(project, target string, payload map[string]any) []string {
	target = strings.TrimSpace(target)
	switch strings.ToLower(target) {
	case "human", "user":
		return []string{"user"}
	case "manager", "reports_to":
		agentName := strVal(payload["agent"])
		if agentName == "" {
			return nil
		}
		a, err := n.db.GetAgent(project, agentName)
		if err != nil || a == nil || a.ReportsTo == nil || *a.ReportsTo == "" {
			return nil
		}
		return []string{*a.ReportsTo}
	case "assignee":
		// the task's own assignee. Prefer the explicit payload agent; otherwise
		// resolve from the task itself — its claimer, or (for Linear-sourced tasks
		// with an empty profile_slug) the agent routed to its Linear project. This
		// lets a stale-scanner emit task-stale with just a task_id and still target
		// the right lead.
		if a := strVal(payload["agent"]); a != "" {
			return []string{a}
		}
		if a := n.resolveTaskAssignee(project, strVal(payload["task_id"])); a != "" {
			return []string{a}
		}
		return nil
	case "dispatcher", "dispatched_by":
		// the agent that dispatched the task
		if d := strVal(payload["dispatched_by"]); d != "" {
			return []string{d}
		}
		return nil
	}
	// Role match (cto / cxx / lead / etc.): any agent whose role equals target.
	if role := matchByRole(n.db, project, target); len(role) > 0 {
		return role
	}
	// Fall back to a literal agent name.
	return []string{target}
}

// resolveTaskAssignee resolves the agent responsible for a task when the event
// payload didn't carry one. Order: the task's claimer (assigned_to), then the
// agent routed to the task's Linear project (linear_routing map) — the path for
// Linear-sourced tasks whose profile_slug is empty. Returns "" when unresolvable.
func (n *Notifier) resolveTaskAssignee(project, taskID string) string {
	if taskID == "" {
		return ""
	}
	task, err := n.db.GetTask(taskID, project)
	if err != nil || task == nil {
		return ""
	}
	if task.AssignedTo != nil && *task.AssignedTo != "" {
		return *task.AssignedTo
	}
	if task.LinearProjectID == nil || *task.LinearProjectID == "" {
		return ""
	}
	raw := strings.TrimSpace(n.db.GetSetting("linear_routing"))
	if raw == "" {
		return ""
	}
	var routing map[string]string
	if err := json.Unmarshal([]byte(raw), &routing); err != nil {
		return ""
	}
	return routing[*task.LinearProjectID]
}

func matchByRole(database *db.DB, project, role string) []string {
	agents, err := database.ListAgents(project)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range agents {
		if strings.EqualFold(a.Role, role) {
			out = append(out, a.Name)
		}
	}
	return out
}

// --- Opts ---

type ruleOpts struct {
	TTL           int
	Priority      string
	Template      string
	IntervalHours int
}

func parseOpts(raw string) ruleOpts {
	var o ruleOpts
	if strings.TrimSpace(raw) == "" {
		return o
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return o
	}
	if v, ok := m["ttl"]; ok {
		o.TTL = toInt(v)
	}
	if v, ok := m["priority"]; ok {
		o.Priority = strVal(v)
	}
	if v, ok := m["template"]; ok {
		o.Template = strVal(v)
	}
	if v, ok := m["interval_hours"]; ok {
		o.IntervalHours = toInt(v)
	}
	return o
}

// --- Digest scheduler ---

// runDigestScheduler ticks every minute and, for each digest rule, emits a
// coalesced cycle.digest event per project once its configured interval elapses.
func (n *Notifier) runDigestScheduler(done <-chan struct{}) {
	ticker := time.NewTicker(digestTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			n.maybeEmitDigests()
		}
	}
}

func (n *Notifier) maybeEmitDigests() {
	rules, err := n.db.ListEnabledNotificationRulesForEvent(EvCycleDigest)
	if err != nil || len(rules) == 0 {
		return
	}
	// Smallest configured interval across digest rules wins (coalesced).
	interval := defaultDigestHours
	for _, r := range rules {
		if h := parseOpts(r.Opts).IntervalHours; h > 0 && h < interval {
			interval = h
		}
	}
	intervalDur := time.Duration(interval) * time.Hour

	projects, err := n.db.ProjectsWithTasks()
	if err != nil {
		return
	}
	now := time.Now()
	for _, project := range projects {
		last := n.digestLastFired[project]
		if last.IsZero() {
			// Prime on first sight (process boot or new project): the first
			// digest fires one interval later, not immediately — otherwise
			// every restart floods one digest per project.
			n.digestLastFired[project] = now
			continue
		}
		if now.Sub(last) < intervalDur {
			continue
		}
		n.digestLastFired[project] = now
		stats, err := n.db.ComputeDigestStats(project)
		if err != nil {
			continue
		}
		line := fmt.Sprintf("Cycle %s: %d/%d done, %d blocked, %d in review",
			stats.CycleName, stats.Done, stats.Total, stats.Blocked, stats.InReview)
		n.events.EmitSemantic(EvCycleDigest, project, "notifier", map[string]any{
			"line":        line,
			"done":        stats.Done,
			"total":       stats.Total,
			"blocked":     stats.Blocked,
			"in_review":   stats.InReview,
			"in_progress": stats.InProgress,
			"cycle_name":  stats.CycleName,
		})
	}
}

// --- Custom events ---

// EmitCustomEvent publishes an agent-emitted custom event onto the bus under the
// "event:<name>" type so rules can target it.
func (n *Notifier) EmitCustomEvent(project, agent, name string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["agent"]; !ok {
		payload["agent"] = agent
	}
	n.events.EmitSemantic("event:"+name, project, agent, payload)
}

// --- Test-fire ---

// TestFire builds the would-be payload for a rule from the most recent matching
// event (or a synthetic sample) and optionally actually sends it.
func (n *Notifier) TestFire(rule models.NotificationRule, send bool) (map[string]any, *models.NotificationDelivery) {
	sample := syntheticPayload(rule.Event)
	return n.fireRule(rule, rule.Event, rule.Project, sample, !send)
}

// syntheticPayload returns a representative sample payload for an event type,
// used by test-fire when no real event is available.
func syntheticPayload(event string) map[string]any {
	switch event {
	case EvCycleDigest:
		return map[string]any{
			"agent": "notifier",
			"line":  "Cycle sample: 3/8 done, 1 blocked, 2 in review",
			"title": "cycle digest",
		}
	default:
		return map[string]any{
			"agent":             "sample-agent",
			"task_id":           "sample-task-id",
			"linear_key":        nil,
			"title":             "Sample task title",
			"line":              "Sample: " + event,
			"assignee_is_agent": true,
		}
	}
}

// --- Helpers ---

func strVal(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// taskSemantic builds the tiny semantic payload from a task. linear_key is nil
// in native mode (the field is reserved for the Linear mirror).
func taskSemantic(task *models.Task, line string) map[string]any {
	if task == nil {
		return map[string]any{"line": line}
	}
	assignee := ""
	if task.AssignedTo != nil {
		assignee = *task.AssignedTo
	}
	return map[string]any{
		"agent":             assignee,
		"dispatched_by":     task.DispatchedBy,
		"task_id":           task.ID,
		"linear_key":        nil,
		"title":             task.Title,
		"line":              line,
		"priority":          task.Priority,
		"assignee_is_agent": assignee != "" && assignee != "human" && assignee != "user",
	}
}

// --- Seed defaults ---

// seedDefaults installs the default rule set on first run (only when the table
// is empty). All rules are fully editable afterwards.
func (n *Notifier) seedDefaults() {
	count, err := n.db.CountNotificationRules()
	if err != nil || count > 0 {
		return
	}
	defaults := []models.NotificationRule{
		{
			Name:    "Dispatch → external launcher",
			Enabled: false, // disabled until the launcher URL is configured
			Event:   EvTaskInProgress,
			Match:   `{"assignee_is_agent":true}`,
			Action:  "webhook",
			Target:  "", // launcher URL — fill in via UI to enable
			Opts:    `{}`,
		},
		{
			Name:    "Blocked → manager (P1)",
			Enabled: true,
			Event:   EvTaskBlocked,
			Match:   `{}`,
			Action:  "message",
			Target:  "manager",
			Opts:    `{"priority":"P1","ttl":86400}`,
		},
		{
			Name:    "In review → reviewer / human",
			Enabled: false, // disabled placeholder until reviewer URL configured
			Event:   EvTaskInReview,
			Match:   `{}`,
			Action:  "webhook",
			Target:  "",
			Opts:    `{}`,
		},
		{
			Name:    "Cycle digest → human (every 8h)",
			Enabled: true,
			Event:   EvCycleDigest,
			Match:   `{}`,
			Action:  "message",
			Target:  "human",
			Opts:    `{"interval_hours":8,"priority":"P3"}`,
		},
		{
			// Technical signal → cto (owner rule: tech→cto). A real cost-emergency
			// is escalated to the owner by the cto, not fired at them directly.
			Name:    "Budget exceeded → cto (P1)",
			Enabled: true,
			Event:   "event:budget-exceeded",
			Match:   `{}`,
			Action:  "message",
			Target:  "cto",
			Opts:    `{"priority":"P1","template":"💸 {line}"}`,
		},
	}
	for i := range defaults {
		r := defaults[i]
		r.Project = "default"
		if _, err := n.db.CreateNotificationRule(&r); err != nil {
			log.Printf("notifier: seed default rule %q: %v", r.Name, err)
		}
	}
	log.Printf("notifier: seeded %d default notification rules", len(defaults))
}
