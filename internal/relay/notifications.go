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
	go n.runDigestScheduler(done)
}

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

// handleEvent matches a single event against enabled rules. Only semantic
// events (those carrying a Semantic payload) are considered; visual events are
// ignored.
func (n *Notifier) handleEvent(evt MCPEvent) {
	if evt.Semantic == nil {
		return
	}
	rules, err := n.db.ListEnabledNotificationRulesForEvent(evt.Type)
	if err != nil {
		log.Printf("notifier: list rules for %s: %v", evt.Type, err)
		return
	}
	for _, rule := range rules {
		rule := rule
		// Scope by project: a rule fires for its own project, or for any
		// project if it has no specific project ("default" acts as global only
		// when the event is also default).
		if rule.Project != evt.Project && rule.Project != "default" {
			continue
		}
		if !matchRule(rule, evt.Semantic) {
			continue
		}
		go n.fireRule(rule, evt.Type, evt.Project, evt.Semantic, false)
	}
}

// matchRule evaluates a rule's match conditions against the event payload.
// Supported conditions:
//   - assignee_is_agent: bool — payload.assignee_is_agent must equal it
//   - agent: string         — payload.agent must equal it
//   - any arbitrary key      — exact equality against the payload value
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
		if !valuesEqual(want, got) {
			return false
		}
	}
	return true
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
	content := subject
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
		msg, err := n.db.InsertMessage(project, "notifier", to, "notification", subject, content, meta, priority, ttl, nil, nil)
		if err != nil {
			rec.Error = err.Error()
			continue
		}
		_ = n.db.CreateDeliveries(msg.ID, project, []string{to})
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
	}
	// Role match (cto / cxx / lead / etc.): any agent whose role equals target.
	if role := matchByRole(n.db, project, target); len(role) > 0 {
		return role
	}
	// Fall back to a literal agent name.
	return []string{target}
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
		if !last.IsZero() && now.Sub(last) < intervalDur {
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
