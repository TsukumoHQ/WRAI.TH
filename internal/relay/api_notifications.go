package relay

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"agent-relay/internal/models"
)

// GET /api/notification-rules?project=
func (r *Relay) apiGetNotificationRules(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	rules, err := r.DB.ListNotificationRules(project)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to list notification rules", err)
		return
	}
	if rules == nil {
		rules = []models.NotificationRule{}
	}
	writeJSON(w, rules)
}

// notificationRuleBody is the request payload for create/patch. Pointers let
// PATCH distinguish "absent" from "set to zero".
type notificationRuleBody struct {
	Project string          `json:"project"`
	Name    *string         `json:"name"`
	Enabled *bool           `json:"enabled"`
	Event   *string         `json:"event"`
	Match   json.RawMessage `json:"match"`
	Action  *string         `json:"action"`
	Target  *string         `json:"target"`
	Opts    json.RawMessage `json:"opts"`
}

// POST /api/notification-rules
func (r *Relay) apiCreateNotificationRule(w http.ResponseWriter, req *http.Request) {
	var body notificationRuleBody
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Name == nil || *body.Name == "" || body.Event == nil || *body.Event == "" || body.Action == nil || *body.Action == "" {
		http.Error(w, `{"error":"name, event and action are required"}`, http.StatusBadRequest)
		return
	}
	rule := &models.NotificationRule{
		Project: body.Project,
		Name:    *body.Name,
		Enabled: body.Enabled == nil || *body.Enabled, // default enabled
		Event:   *body.Event,
		Match:   rawOrDefault(body.Match, "{}"),
		Action:  *body.Action,
		Opts:    rawOrDefault(body.Opts, "{}"),
	}
	if body.Target != nil {
		rule.Target = *body.Target
	}
	created, err := r.DB.CreateNotificationRule(rule)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to create notification rule", err)
		return
	}
	writeJSON(w, created)
}

// PATCH /api/notification-rules/{id}
func (r *Relay) apiPatchNotificationRule(w http.ResponseWriter, req *http.Request, path string) {
	id := strings.TrimPrefix(path, "/notification-rules/")
	if id == "" {
		http.Error(w, `{"error":"missing rule id"}`, http.StatusBadRequest)
		return
	}
	var body notificationRuleBody
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	var match, opts *string
	if body.Match != nil {
		s := rawOrDefault(body.Match, "{}")
		match = &s
	}
	if body.Opts != nil {
		s := rawOrDefault(body.Opts, "{}")
		opts = &s
	}
	updated, err := r.DB.PatchNotificationRule(id, body.Name, body.Event, match, body.Action, body.Target, opts, body.Enabled)
	if err != nil {
		apiError(w, http.StatusBadRequest, "failed to update notification rule", err)
		return
	}
	writeJSON(w, updated)
}

// DELETE /api/notification-rules/{id}
func (r *Relay) apiDeleteNotificationRule(w http.ResponseWriter, path string) {
	id := strings.TrimPrefix(path, "/notification-rules/")
	if id == "" {
		http.Error(w, `{"error":"missing rule id"}`, http.StatusBadRequest)
		return
	}
	if err := r.DB.DeleteNotificationRule(id); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to delete notification rule", err)
		return
	}
	writeJSON(w, map[string]any{"deleted": true, "id": id})
}

// POST /api/notification-rules/{id}/test-fire[?send=true]
// Dry-runs a rule and returns the would-be payload. With ?send=true it actually
// sends (useful for verifying webhook wiring).
func (r *Relay) apiTestFireNotificationRule(w http.ResponseWriter, req *http.Request, path string) {
	trimmed := strings.TrimPrefix(path, "/notification-rules/")
	id, _, _ := strings.Cut(trimmed, "/")
	if id == "" {
		http.Error(w, `{"error":"missing rule id"}`, http.StatusBadRequest)
		return
	}
	rule, err := r.DB.GetNotificationRule(id)
	if err != nil || rule == nil {
		http.Error(w, `{"error":"rule not found"}`, http.StatusNotFound)
		return
	}
	if r.Notifier == nil {
		http.Error(w, `{"error":"notifier unavailable"}`, http.StatusInternalServerError)
		return
	}
	send, _ := strconv.ParseBool(req.URL.Query().Get("send"))
	payload, rec := r.Notifier.TestFire(*rule, send)
	writeJSON(w, map[string]any{
		"sent":     send,
		"payload":  payload,
		"outcome":  rec.Outcome,
		"delivery": rec,
	})
}

// GET /api/notification-deliveries?limit=N
func (r *Relay) apiGetNotificationDeliveries(w http.ResponseWriter, req *http.Request) {
	limit := 100
	if v := req.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	deliveries, err := r.DB.ListNotificationDeliveries(limit)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to list notification deliveries", err)
		return
	}
	if deliveries == nil {
		deliveries = []models.NotificationDelivery{}
	}
	writeJSON(w, deliveries)
}

// POST /api/notification-events
// Emits an agent-authored custom event into the rules engine. The event is
// matched by rules as "event:<name>". Body: {project, agent, name, payload}.
func (r *Relay) apiEmitNotificationEvent(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Project string         `json:"project"`
		Agent   string         `json:"agent"`
		Name    string         `json:"name"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}
	if r.Notifier == nil {
		http.Error(w, `{"error":"notifier unavailable"}`, http.StatusInternalServerError)
		return
	}
	r.Notifier.EmitCustomEvent(body.Project, body.Agent, body.Name, body.Payload)
	writeJSON(w, map[string]any{"emitted": true, "event": "event:" + body.Name})
}

// rawOrDefault returns the compact JSON string of a raw message, or def when
// empty/invalid.
func rawOrDefault(raw json.RawMessage, def string) string {
	if len(raw) == 0 {
		return def
	}
	// Validate it's a JSON object/value; if not, fall back to default.
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return string(raw)
}
