package relay

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	linearconn "agent-relay/internal/connector/linear"
	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
	"agent-relay/internal/models"
)

// apiError logs the full error server-side and returns a safe message to the client.
func apiError(w http.ResponseWriter, status int, msg string, err error) {
	log.Printf("API error: %s: %v", msg, err)
	payload := map[string]string{"error": msg}
	if err != nil {
		payload["detail"] = err.Error() // surfaced to the same-origin dashboard
	}
	b, _ := json.Marshal(payload)
	http.Error(w, string(b), status)
}

// ServeAPI handles REST API requests for the web UI.
func (r *Relay) ServeAPI(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := strings.TrimPrefix(req.URL.Path, "/api")

	switch {
	case path == "/health" && req.Method == http.MethodGet:
		r.apiHealth(w)
	case path == "/projects" && req.Method == http.MethodGet:
		r.apiGetProjects(w)
	case strings.HasPrefix(path, "/projects/") && req.Method == http.MethodDelete:
		r.apiDeleteProject(w, strings.TrimPrefix(path, "/projects/"))
	case strings.HasPrefix(path, "/projects/") && req.Method == http.MethodPatch:
		r.apiPatchProject(w, req, strings.TrimPrefix(path, "/projects/"))
	case strings.HasPrefix(path, "/projects/") && req.Method == http.MethodGet:
		r.apiGetProject(w, strings.TrimPrefix(path, "/projects/"))
	case path == "/settings" && req.Method == http.MethodGet:
		r.apiGetSettings(w)
	case path == "/settings" && req.Method == http.MethodPut:
		r.apiPutSetting(w, req)
	case path == "/agents" && req.Method == http.MethodGet:
		r.apiGetAgents(w, req)
	case path == "/org" && req.Method == http.MethodGet:
		r.apiGetOrgTree(w, req)
	case path == "/agents/all" && req.Method == http.MethodGet:
		r.apiGetAllAgents(w)
	case path == "/conversations/all" && req.Method == http.MethodGet:
		r.apiGetAllConversations(w)
	case path == "/conversations" && req.Method == http.MethodGet:
		r.apiGetConversations(w, req)
	case strings.HasPrefix(path, "/conversations/") && strings.HasSuffix(path, "/messages") && req.Method == http.MethodGet:
		r.apiGetConversationMessages(w, path)
	case path == "/messages" && req.Method == http.MethodGet:
		r.apiGetAllMessages(w, req)
	case path == "/messages/all-projects" && req.Method == http.MethodGet:
		r.apiGetAllMessagesAllProjects(w)
	case path == "/messages/latest-all" && req.Method == http.MethodGet:
		r.apiGetLatestMessagesAllProjects(w, req)
	case path == "/messages/all" && req.Method == http.MethodGet:
		r.apiGetAllMessages(w, req)
	case path == "/messages/latest" && req.Method == http.MethodGet:
		r.apiGetLatestMessages(w, req)
	case path == "/user-response" && req.Method == http.MethodPost:
		r.apiPostUserResponse(w, req)
	// Memory endpoints
	case path == "/memories" && req.Method == http.MethodGet:
		r.apiGetMemories(w, req)
	case path == "/memories/search" && req.Method == http.MethodGet:
		r.apiSearchMemories(w, req)
	case path == "/memories" && req.Method == http.MethodPost:
		r.apiPostMemory(w, req)
	case strings.HasPrefix(path, "/memories/") && strings.HasSuffix(path, "/resolve") && req.Method == http.MethodPost:
		r.apiResolveMemoryConflict(w, req, path)
	case strings.HasPrefix(path, "/memories/") && req.Method == http.MethodDelete:
		r.apiDeleteMemory(w, path)
	case path == "/activity" && req.Method == http.MethodGet:
		r.apiGetActivity(w)
	case path == "/activity/stream" && req.Method == http.MethodGet:
		r.apiStreamActivity(w, req)
	case path == "/events/stream" && req.Method == http.MethodGet:
		r.apiStreamEvents(w, req)
	case path == "/events/recent" && req.Method == http.MethodGet:
		r.apiGetRecentEvents(w, req)
	// File locks
	case path == "/file-locks" && req.Method == http.MethodGet:
		r.apiGetFileLocks(w, req)
	// Task endpoints
	case path == "/tasks/human" && req.Method == http.MethodGet:
		r.apiGetHumanTasks(w, req)
	case path == "/tasks/board" && req.Method == http.MethodGet:
		r.apiGetBoardTasks(w, req)
	case path == "/cycles" && req.Method == http.MethodGet:
		r.apiGetCycles(w, req)
	case path == "/tasks/all" && req.Method == http.MethodGet:
		r.apiGetAllTasks(w)
	case path == "/tasks" && req.Method == http.MethodGet:
		r.apiGetTasks(w, req)
	case path == "/tasks/latest" && req.Method == http.MethodGet:
		r.apiGetLatestTasks(w, req)
	case path == "/tasks" && req.Method == http.MethodPost:
		r.apiDispatchTask(w, req)
	case strings.HasPrefix(path, "/tasks/") && strings.HasSuffix(path, "/transition") && req.Method == http.MethodPost:
		r.apiTransitionTask(w, req, path)
	case strings.HasPrefix(path, "/tasks/") && strings.HasSuffix(path, "/reassign") && req.Method == http.MethodPost:
		r.apiReassignTask(w, req, path)
	case strings.HasPrefix(path, "/tasks/") && strings.HasSuffix(path, "/comment") && req.Method == http.MethodPost:
		r.apiTaskComment(w, req, path)
	case strings.HasPrefix(path, "/tasks/") && strings.HasSuffix(path, "/progress") && req.Method == http.MethodGet:
		r.apiGetTaskProgress(w, req, path)
	case path == "/audit" && req.Method == http.MethodGet:
		r.apiGetAudit(w, req)
	case strings.HasPrefix(path, "/tasks/") && req.Method == http.MethodPut:
		r.apiUpdateTask(w, req, path)
	case strings.HasPrefix(path, "/tasks/") && req.Method == http.MethodDelete:
		r.apiDeleteTask(w, req, path)
	case strings.HasPrefix(path, "/tasks/") && req.Method == http.MethodGet:
		r.apiGetTask(w, req, path)
	// Profile endpoints (read-only; profiles are slimmed to identity)
	case path == "/profiles" && req.Method == http.MethodGet:
		r.apiGetProfiles(w, req)
	case strings.HasPrefix(path, "/profiles/") && req.Method == http.MethodGet:
		r.apiGetProfile(w, req, path)
	// Org + Team endpoints
	case path == "/orgs" && req.Method == http.MethodGet:
		r.apiGetOrgs(w)
	case path == "/teams/all" && req.Method == http.MethodGet:
		r.apiGetAllTeams(w)
	case path == "/teams" && req.Method == http.MethodGet:
		r.apiGetTeams(w, req)
	case strings.HasPrefix(path, "/teams/") && strings.HasSuffix(path, "/members") && req.Method == http.MethodGet:
		r.apiGetTeamMembers(w, req, path)
	// Board endpoints
	case path == "/boards" && req.Method == http.MethodGet:
		r.apiGetBoards(w, req)
	case path == "/boards/all" && req.Method == http.MethodGet:
		r.apiGetAllBoards(w)
	// Token usage
	case path == "/token-usage" && req.Method == http.MethodGet:
		r.apiGetTokenUsage(w, req)
	case path == "/token-usage/project" && req.Method == http.MethodGet:
		r.apiGetTokenUsageByProject(w, req)
	case path == "/token-usage/agent" && req.Method == http.MethodGet:
		r.apiGetTokenUsageByAgent(w, req)
	case path == "/token-usage/timeseries" && req.Method == http.MethodGet:
		r.apiGetTokenTimeSeries(w, req)
	// Agentic analytics (stats panel)
	case path == "/stats" && req.Method == http.MethodGet:
		r.apiGetAgentStats(w, req)
	// Notification rules (configurable event→action→target rules engine)
	case path == "/notification-rules" && req.Method == http.MethodGet:
		r.apiGetNotificationRules(w, req)
	case path == "/notification-rules" && req.Method == http.MethodPost:
		r.apiCreateNotificationRule(w, req)
	case strings.HasPrefix(path, "/notification-rules/") && strings.HasSuffix(path, "/test-fire") && req.Method == http.MethodPost:
		r.apiTestFireNotificationRule(w, req, path)
	case strings.HasPrefix(path, "/notification-rules/") && req.Method == http.MethodPatch:
		r.apiPatchNotificationRule(w, req, path)
	case strings.HasPrefix(path, "/notification-rules/") && req.Method == http.MethodDelete:
		r.apiDeleteNotificationRule(w, path)
	case path == "/notification-deliveries" && req.Method == http.MethodGet:
		r.apiGetNotificationDeliveries(w, req)
	case path == "/notification-events" && req.Method == http.MethodPost:
		r.apiEmitNotificationEvent(w, req)
	// Linear connector inbound webhook (404s unless the connector is active).
	case path == "/connectors/linear/webhook" && req.Method == http.MethodPost:
		r.apiLinearWebhook(w, req)
	case path == "/linear/teams" && req.Method == http.MethodGet:
		r.apiLinearTeams(w, req)
	case path == "/agents/avatar" && req.Method == http.MethodPut:
		r.apiSetAgentAvatar(w, req)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// projectFromRequest extracts the ?project= query parameter, defaulting to "default".
func projectFromRequest(req *http.Request) string {
	p := req.URL.Query().Get("project")
	if p == "" {
		return "default"
	}
	return p
}

func (r *Relay) apiGetProjects(w http.ResponseWriter) {
	projects, err := r.DB.ListProjectsWithInfo()
	if err != nil {
		http.Error(w, `{"error":"failed to list projects"}`, http.StatusInternalServerError)
		return
	}
	if projects == nil {
		projects = []models.ProjectInfo{}
	}
	writeJSON(w, projects)
}

func (r *Relay) apiHealth(w http.ResponseWriter) {
	stats := r.DB.GetHealthStats()
	v := r.Version
	if v == "" {
		v = "dev"
	}
	health := map[string]any{
		"status":      "ok",
		"version":     v,
		"uptime":      time.Since(r.StartedAt).String(),
		"started":     r.StartedAt.Format(time.RFC3339),
		"db":          stats,
		"linear_mode": r.Config.LinearMode,
		"mode":        modeString(r.Config.LinearMode),
	}
	// When the Linear connector is active, surface its live status
	// (last_webhook_at, last_reconcile_at, writer failure count, cache state).
	if lc := r.LinearConnector(); lc != nil {
		health["linear_connector"] = lc.Status()
	}
	writeJSON(w, health)
}

// modeString maps the linear_mode flag to a human-readable mode label the web
// UI uses to switch behavior (writable native board vs. read-replica mirror).
func modeString(linear bool) string {
	if linear {
		return "linear"
	}
	return "native"
}

func (r *Relay) apiGetProject(w http.ResponseWriter, name string) {
	proj, err := r.DB.GetProject(name)
	if err != nil {
		http.Error(w, `{"error":"failed to get project"}`, http.StatusInternalServerError)
		return
	}
	if proj == nil {
		http.Error(w, `{"error":"project not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, proj)
}

func (r *Relay) apiPatchProject(w http.ResponseWriter, req *http.Request, name string) {
	var body struct {
		PlanetType string `json:"planet_type"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.PlanetType == "" {
		http.Error(w, `{"error":"planet_type required"}`, http.StatusBadRequest)
		return
	}
	if err := r.DB.UpdateProjectPlanetType(name, body.PlanetType); err != nil {
		http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

func (r *Relay) apiDeleteProject(w http.ResponseWriter, name string) {
	if name == "" {
		http.Error(w, `{"error":"missing project name"}`, http.StatusBadRequest)
		return
	}
	if err := r.DB.DeleteProject(name); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to delete project", err)
		return
	}
	writeJSON(w, map[string]any{"deleted": true, "project": name})
}

func (r *Relay) apiGetSettings(w http.ResponseWriter) {
	sunType := r.DB.GetSetting("sun_type")
	if sunType == "" {
		sunType = "1"
	}
	apiKey, teamKey, enabled, interval, source := r.effectiveLinearConfig()
	masked := ""
	if apiKey != "" {
		masked = "set"
		if len(apiKey) > 8 {
			masked = "…" + apiKey[len(apiKey)-4:]
		}
	}
	writeJSON(w, map[string]any{
		"sun_type":    sunType,
		"linear_mode": enabled,
		"mode":        modeString(enabled),
		"linear": map[string]any{
			"enabled":        enabled,
			"team_key":       teamKey,
			"project":        r.linearProjectName(teamKey, enabled),
			"api_key_masked": masked,
			"interval":       interval.String(),
			"source":         source,
		},
	})
}

// writableSettings is the allowlist of keys the settings API may write. Without
// it, an (unauthenticated by default) caller could set arbitrary key→value pairs
// — including swapping linear_api_key or repointing the connector.
var writableSettings = map[string]bool{
	"sun_type":        true,
	setLinearEnabled:  true,
	setLinearAPIKey:   true,
	setLinearTeamKey:  true,
	setLinearProject:  true,
	setLinearInterval: true,
}

func (r *Relay) apiPutSetting(w http.ResponseWriter, req *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	// Reject the whole request if any key is not writable (fail before applying).
	for k := range body {
		if !writableSettings[k] {
			http.Error(w, fmt.Sprintf(`{"error":"setting %q is not writable"}`, k), http.StatusForbidden)
			return
		}
	}
	linearChanged := false
	for k, v := range body {
		r.DB.SetSetting(k, v)
		if strings.HasPrefix(k, "linear_") {
			linearChanged = true
		}
	}
	if linearChanged {
		// Hot-reload the connector — no restart needed.
		r.ReconfigureLinear()
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

// apiLinearTeams lists the Linear workspace teams using the effective API key
// (or one passed as ?key= for pre-save validation in the settings UI).
func (r *Relay) apiLinearTeams(w http.ResponseWriter, req *http.Request) {
	apiKey, _, _, _, _ := r.effectiveLinearConfig()
	if k := strings.TrimSpace(req.URL.Query().Get("key")); k != "" {
		apiKey = k
	}
	if apiKey == "" {
		http.Error(w, `{"error":"no linear api key configured"}`, http.StatusBadRequest)
		return
	}
	teams, err := linearconn.ListTeams(req.Context(), apiKey)
	if err != nil {
		apiError(w, http.StatusBadGateway, "linear teams fetch failed", err)
		return
	}
	writeJSON(w, teams)
}

type apiTeamRef struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Type string `json:"type"`
	Role string `json:"role"`
}

type apiAgent struct {
	Name         string       `json:"name"`
	Role         string       `json:"role"`
	Description  string       `json:"description"`
	LastSeen     string       `json:"last_seen"`
	RegisteredAt string       `json:"registered_at"`
	Online       bool         `json:"online"`
	ReportsTo    *string      `json:"reports_to,omitempty"`
	Project      string       `json:"project"`
	ProfileSlug  *string      `json:"profile_slug,omitempty"`
	Status       string       `json:"status"`
	IsExecutive  bool         `json:"is_executive"`
	SessionID    *string      `json:"session_id,omitempty"`
	AvatarURL    *string      `json:"avatar_url,omitempty"`
	Activity     string       `json:"activity,omitempty"`
	ActivityTool string       `json:"activity_tool,omitempty"`
	Teams        []apiTeamRef `json:"teams,omitempty"`
}

func (r *Relay) apiGetAgents(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)

	agents, err := r.DB.ListAgents(project)
	if err != nil {
		http.Error(w, `{"error":"failed to list agents"}`, http.StatusInternalServerError)
		return
	}

	// Bulk-fetch team memberships
	memberships, _ := r.DB.GetAllTeamMemberships()
	teamsByAgent := make(map[string][]apiTeamRef)
	for _, m := range memberships {
		key := m.Project + ":" + m.AgentName
		teamsByAgent[key] = append(teamsByAgent[key], apiTeamRef{
			Slug: m.TeamSlug,
			Name: m.TeamName,
			Type: m.TeamType,
			Role: m.Role,
		})
	}

	actMap := r.activityBySessionID()
	now := time.Now().UTC()
	result := make([]apiAgent, 0, len(agents))
	for _, a := range agents {
		key := project + ":" + a.Name
		aa := apiAgent{
			Name:         a.Name,
			Role:         a.Role,
			Description:  a.Description,
			LastSeen:     a.LastSeen,
			RegisteredAt: a.RegisteredAt,
			ReportsTo:    a.ReportsTo,
			Project:      project,
			ProfileSlug:  a.ProfileSlug,
			Status:       a.Status,
			IsExecutive:  a.IsExecutive,
			SessionID:    a.SessionID,
			AvatarURL:    a.AvatarURL,
			Teams:        teamsByAgent[key],
		}
		online := false
		if t, err := time.Parse(time.RFC3339, a.LastSeen); err == nil {
			online = now.Sub(t) < 5*time.Minute
		}
		aa.Online = online
		if a.SessionID != nil {
			if s, ok := actMap[*a.SessionID]; ok {
				aa.Activity = string(s.Activity)
				aa.ActivityTool = s.Tool
			}
		}
		result = append(result, aa)
	}

	writeJSON(w, result)
}

func (r *Relay) activityBySessionID() map[string]ingest.SessionState {
	m := make(map[string]ingest.SessionState)
	if r.Ingester != nil {
		for _, s := range r.Ingester.GetSessions() {
			m[s.SessionID] = s
		}
	}
	return m
}

func (r *Relay) apiGetAllAgents(w http.ResponseWriter) {
	agents, err := r.DB.ListAllAgents()
	if err != nil {
		http.Error(w, `{"error":"failed to list agents"}`, http.StatusInternalServerError)
		return
	}

	// Bulk-fetch team memberships
	memberships, _ := r.DB.GetAllTeamMemberships()
	teamsByAgent := make(map[string][]apiTeamRef)
	for _, m := range memberships {
		key := m.Project + ":" + m.AgentName
		teamsByAgent[key] = append(teamsByAgent[key], apiTeamRef{
			Slug: m.TeamSlug,
			Name: m.TeamName,
			Type: m.TeamType,
			Role: m.Role,
		})
	}

	actMap := r.activityBySessionID()
	now := time.Now().UTC()
	result := make([]apiAgent, 0, len(agents))
	for _, a := range agents {
		online := false
		if t, err := time.Parse(time.RFC3339, a.LastSeen); err == nil {
			online = now.Sub(t) < 5*time.Minute
		}
		aa := apiAgent{
			Name:         a.Name,
			Role:         a.Role,
			Description:  a.Description,
			LastSeen:     a.LastSeen,
			RegisteredAt: a.RegisteredAt,
			Online:       online,
			ReportsTo:    a.ReportsTo,
			Project:      a.Project,
			ProfileSlug:  a.ProfileSlug,
			Status:       a.Status,
			IsExecutive:  a.IsExecutive,
			SessionID:    a.SessionID,
			AvatarURL:    a.AvatarURL,
			Teams:        teamsByAgent[a.Project+":"+a.Name],
		}
		if a.SessionID != nil {
			if s, ok := actMap[*a.SessionID]; ok {
				aa.Activity = string(s.Activity)
				aa.ActivityTool = s.Tool
			}
		}
		result = append(result, aa)
	}

	writeJSON(w, result)
}

func (r *Relay) apiGetAllConversations(w http.ResponseWriter) {
	convs, err := r.DB.ListAllConversationsAcrossProjects()
	if err != nil {
		http.Error(w, `{"error":"failed to list conversations"}`, http.StatusInternalServerError)
		return
	}

	if convs == nil {
		convs = make([]models.ConversationWithMembers, 0)
	}

	writeJSON(w, convs)
}

func (r *Relay) apiGetAllMessagesAllProjects(w http.ResponseWriter) {
	msgs, err := r.DB.GetAllRecentMessagesAllProjects(500)
	if err != nil {
		http.Error(w, `{"error":"failed to get messages"}`, http.StatusInternalServerError)
		return
	}

	if msgs == nil {
		msgs = make([]models.Message, 0)
	}

	writeJSON(w, msgs)
}

func (r *Relay) apiGetLatestMessagesAllProjects(w http.ResponseWriter, req *http.Request) {
	since := req.URL.Query().Get("since")
	if since == "" {
		since = time.Now().UTC().Add(-30 * time.Second).Format("2006-01-02T15:04:05.000000Z")
	}

	msgs, err := r.DB.GetMessagesSinceAllProjects(since, 100)
	if err != nil {
		http.Error(w, `{"error":"failed to get messages"}`, http.StatusInternalServerError)
		return
	}

	if msgs == nil {
		msgs = make([]models.Message, 0)
	}

	writeJSON(w, msgs)
}

func (r *Relay) apiGetConversations(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)

	convs, err := r.DB.ListAllConversations(project)
	if err != nil {
		http.Error(w, `{"error":"failed to list conversations"}`, http.StatusInternalServerError)
		return
	}

	if convs == nil {
		convs = make([]models.ConversationWithMembers, 0)
	}

	writeJSON(w, convs)
}

func (r *Relay) apiGetConversationMessages(w http.ResponseWriter, path string) {
	// path: /conversations/{id}/messages
	trimmed := strings.TrimPrefix(path, "/conversations/")
	convID, _, _ := strings.Cut(trimmed, "/")
	if convID == "" {
		http.Error(w, `{"error":"missing conversation id"}`, http.StatusBadRequest)
		return
	}

	msgs, err := r.DB.GetConversationMessages(convID, 200)
	if err != nil {
		http.Error(w, `{"error":"failed to get messages"}`, http.StatusInternalServerError)
		return
	}

	if msgs == nil {
		msgs = make([]models.Message, 0)
	}

	writeJSON(w, msgs)
}

func (r *Relay) apiGetAllMessages(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)

	msgs, err := r.DB.GetAllRecentMessages(project, 500)
	if err != nil {
		http.Error(w, `{"error":"failed to get messages"}`, http.StatusInternalServerError)
		return
	}

	if msgs == nil {
		msgs = make([]models.Message, 0)
	}

	writeJSON(w, msgs)
}

func (r *Relay) apiGetLatestMessages(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	since := req.URL.Query().Get("since")
	if since == "" {
		since = time.Now().UTC().Add(-30 * time.Second).Format("2006-01-02T15:04:05.000000Z")
	}

	msgs, err := r.DB.GetMessagesSince(project, since, 100)
	if err != nil {
		http.Error(w, `{"error":"failed to get messages"}`, http.StatusInternalServerError)
		return
	}

	if msgs == nil {
		msgs = make([]models.Message, 0)
	}

	writeJSON(w, msgs)
}

// apiGetOrgTree returns the agent hierarchy as a nested tree structure.
func (r *Relay) apiGetOrgTree(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)

	agents, err := r.DB.GetOrgTree(project)
	if err != nil {
		http.Error(w, `{"error":"failed to get org tree"}`, http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()

	type orgNode struct {
		Name    string     `json:"name"`
		Role    string     `json:"role"`
		Online  bool       `json:"online"`
		Reports []*orgNode `json:"reports"`
	}

	// Build a map of nodes and track children
	nodeMap := make(map[string]*orgNode, len(agents))
	for _, a := range agents {
		online := false
		if t, err := time.Parse(time.RFC3339, a.LastSeen); err == nil {
			online = now.Sub(t) < 5*time.Minute
		}
		nodeMap[a.Name] = &orgNode{
			Name:    a.Name,
			Role:    a.Role,
			Online:  online,
			Reports: []*orgNode{},
		}
	}

	// Build tree
	var roots []*orgNode
	for _, a := range agents {
		node := nodeMap[a.Name]
		if a.ReportsTo != nil {
			if parent, ok := nodeMap[*a.ReportsTo]; ok {
				parent.Reports = append(parent.Reports, node)
				continue
			}
		}
		roots = append(roots, node)
	}

	if roots == nil {
		roots = []*orgNode{}
	}

	writeJSON(w, roots)
}

// apiPostUserResponse handles user responses from the web UI to agent questions.
func (r *Relay) apiPostUserResponse(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Project string `json:"project"`
		To      string `json:"to"`
		Content string `json:"content"`
		ReplyTo string `json:"reply_to"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.To == "" || body.Content == "" {
		http.Error(w, `{"error":"to and content are required"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}

	replyTo := optionalString(body.ReplyTo)

	msg, err := r.DB.InsertMessageWithDeliveries(body.Project, "user", body.To, "response", "User response", body.Content, "{}", "P1", 3600, replyTo, nil, []string{body.To})
	if err != nil {
		http.Error(w, `{"error":"failed to send response"}`, http.StatusInternalServerError)
		return
	}

	// Push notification to the target agent
	r.Registry.Notify(body.Project, body.To, "user", "User response", msg.ID)

	writeJSON(w, map[string]any{"ok": true, "message_id": msg.ID})
}

// --- Memory API endpoints ---

func (r *Relay) apiGetMemories(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	scope := req.URL.Query().Get("scope")
	agent := req.URL.Query().Get("agent")
	tag := req.URL.Query().Get("tag")

	var tags []string
	if tag != "" {
		tags = strings.Split(tag, ",")
	}

	var (
		memories []models.Memory
		err      error
	)

	if project == "" && scope == "" && agent == "" && len(tags) == 0 {
		memories, err = r.DB.ListAllMemories(200)
	} else {
		memories, err = r.DB.ListMemories(project, scope, agent, tags, 200)
	}

	if err != nil {
		http.Error(w, `{"error":"failed to list memories"}`, http.StatusInternalServerError)
		return
	}
	if memories == nil {
		memories = []models.Memory{}
	}
	writeJSON(w, memories)
}

func (r *Relay) apiSearchMemories(w http.ResponseWriter, req *http.Request) {
	query := req.URL.Query().Get("q")
	if query == "" {
		http.Error(w, `{"error":"q parameter required"}`, http.StatusBadRequest)
		return
	}

	memories, err := r.DB.SearchAllMemories(query, 50)
	if err != nil {
		http.Error(w, `{"error":"search failed"}`, http.StatusInternalServerError)
		return
	}
	if memories == nil {
		memories = []models.Memory{}
	}
	writeJSON(w, memories)
}

func (r *Relay) apiPostMemory(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Project    string   `json:"project"`
		AgentName  string   `json:"agent_name"`
		Key        string   `json:"key"`
		Value      string   `json:"value"`
		Tags       []string `json:"tags"`
		Scope      string   `json:"scope"`
		Confidence string   `json:"confidence"`
		Layer      string   `json:"layer"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Key == "" || body.Value == "" {
		http.Error(w, `{"error":"key and value are required"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}
	if body.AgentName == "" {
		body.AgentName = "user"
	}
	if body.Scope == "" {
		body.Scope = "project"
	}

	tagsJSON := db.TagsToJSON(body.Tags)
	mem, err := r.DB.SetMemory(body.Project, body.AgentName, body.Key, body.Value, tagsJSON, body.Scope, body.Confidence, body.Layer)
	if err != nil {
		http.Error(w, `{"error":"failed to set memory"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, mem)
}

func (r *Relay) apiDeleteMemory(w http.ResponseWriter, path string) {
	// path: /memories/{id}
	id := strings.TrimPrefix(path, "/memories/")
	if id == "" {
		http.Error(w, `{"error":"missing memory id"}`, http.StatusBadRequest)
		return
	}

	if err := r.DB.DeleteMemoryByID(id, "user"); err != nil {
		http.Error(w, `{"error":"failed to delete memory"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": true, "id": id})
}

func (r *Relay) apiResolveMemoryConflict(w http.ResponseWriter, req *http.Request, path string) {
	// path: /memories/{key}/resolve
	trimmed := strings.TrimPrefix(path, "/memories/")
	key, _, _ := strings.Cut(trimmed, "/")
	if key == "" {
		http.Error(w, `{"error":"missing key"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		Project     string `json:"project"`
		ChosenValue string `json:"chosen_value"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.ChosenValue == "" {
		http.Error(w, `{"error":"chosen_value is required"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}
	if body.Scope == "" {
		body.Scope = "project"
	}

	mem, err := r.DB.ResolveConflict(body.Project, "user", key, body.ChosenValue, body.Scope)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to resolve conflict", err)
		return
	}
	writeJSON(w, map[string]any{"resolved": true, "memory": mem})
}

func (r *Relay) apiGetActivity(w http.ResponseWriter) {
	if r.Ingester == nil {
		writeJSON(w, []any{})
		return
	}
	sessions := r.Ingester.GetSessions()
	if sessions == nil {
		sessions = make([]ingest.SessionState, 0)
	}
	writeJSON(w, sessions)
}

// apiGetRecentEvents returns recent MCP events from the in-memory ring buffer.
// Complements /api/activity (which tracks Claude Code session states) with an
// event log of MCP actions (send_message, dispatch_task, etc.) regardless of
// whether the caller went through a Claude Code session.
//
// Query params:
//   - project: filter by project (optional)
//   - limit: max entries (default 100, max 500)
func (r *Relay) apiGetRecentEvents(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	limit := 100
	if v := req.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	events := r.Events.Recent(project, limit)
	if events == nil {
		events = []MCPEvent{}
	}
	writeJSON(w, events)
}

type ssePayload struct {
	Sessions []ingest.SessionState `json:"sessions"`
	Agents   []sseAgent            `json:"agents"`
}

type sseAgent struct {
	Name         string  `json:"name"`
	Project      string  `json:"project"`
	Role         string  `json:"role"`
	Status       string  `json:"status"`        // busy, active, sleeping, inactive, deleted
	Activity     string  `json:"activity"`      // typing, reading, terminal, browsing, thinking, waiting, idle
	ActivityTool string  `json:"activity_tool"` // tool name when busy
	SessionID    *string `json:"session_id,omitempty"`
}

func (r *Relay) buildSSEPayload(sessions []ingest.SessionState) ssePayload {
	// Build session lookup
	sessMap := make(map[string]ingest.SessionState)
	for _, s := range sessions {
		sessMap[s.SessionID] = s
	}

	agents, _ := r.DB.ListAllAgents()
	now := time.Now().UTC()
	sseAgents := make([]sseAgent, 0, len(agents))

	for _, a := range agents {
		sa := sseAgent{
			Name:      a.Name,
			Project:   a.Project,
			Role:      a.Role,
			Status:    a.Status, // DB status: active, sleeping, inactive, deleted
			SessionID: a.SessionID,
		}

		// Enrich with SSE activity if session linked
		if a.SessionID != nil {
			if s, ok := sessMap[*a.SessionID]; ok {
				sa.Activity = string(s.Activity)
				if s.Activity != ingest.ActivityIdle && s.Activity != ingest.ActivityWaiting && s.Activity != ingest.ActivityThinking {
					sa.ActivityTool = s.Tool
				}

				// Derive status from activity
				switch {
				case a.Status == "sleeping":
					// sleeping stays sleeping
				case a.Status == "deleted":
					// deleted stays deleted
				case s.Activity != ingest.ActivityIdle && s.State != "idle" && s.State != "exited":
					sa.Status = "busy"
				case now.Sub(s.LastEvent) < 5*time.Minute:
					sa.Status = "active"
				default:
					sa.Status = "inactive"
				}
			}
		}

		sseAgents = append(sseAgents, sa)
	}

	return ssePayload{Sessions: sessions, Agents: sseAgents}
}

func (r *Relay) apiStreamActivity(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if r.Ingester == nil {
		return
	}

	ch := r.Ingester.SubscribeActivity()
	defer r.Ingester.UnsubscribeActivity(ch)

	// Send initial state
	sessions := r.Ingester.GetSessions()
	if sessions == nil {
		sessions = make([]ingest.SessionState, 0)
	}
	payload := r.buildSSEPayload(sessions)
	data, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	for {
		select {
		case <-req.Context().Done():
			return
		case <-r.shutdownCtx.Done():
			return
		case snap, ok := <-ch:
			if !ok {
				return
			}
			payload := r.buildSSEPayload(snap)
			data, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (r *Relay) apiStreamEvents(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy (nginx) buffering of the stream

	ch := r.Events.Subscribe()
	defer r.Events.Unsubscribe(ch)

	// Flush headers + an opening comment immediately so the client's EventSource
	// fires onopen right away. Without this the headers aren't sent until the
	// first event, so a quiet project leaves the UI stuck on "connecting".
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Heartbeat keeps idle connections (and proxies) alive and lets the client
	// detect a dead stream.
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case <-r.shutdownCtx.Done():
			return
		case <-ping.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case evt, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(evt)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
	}
}

// --- Task API endpoints ---

func (r *Relay) apiGetTasks(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	status := req.URL.Query().Get("status")
	profile := req.URL.Query().Get("profile")
	priority := req.URL.Query().Get("priority")

	boardID := req.URL.Query().Get("board_id")
	tasks, err := r.DB.ListTasks(project, status, profile, priority, "", boardID, 100, false)
	if err != nil {
		http.Error(w, `{"error":"failed to list tasks"}`, http.StatusInternalServerError)
		return
	}
	if tasks == nil {
		tasks = []models.Task{}
	}
	writeJSON(w, tasks)
}

func (r *Relay) apiGetHumanTasks(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	status := req.URL.Query().Get("status")
	if status == "" {
		status = "" // all statuses
	}
	tasks, err := r.DB.ListTasks(project, status, "human", "", "", "", 100, false)
	if err != nil {
		http.Error(w, `{"error":"failed to list human tasks"}`, http.StatusInternalServerError)
		return
	}
	if tasks == nil {
		tasks = []models.Task{}
	}
	writeJSON(w, tasks)
}

// apiGetBoardTasks serves the kanban board: every non-archived, non-cancelled
// task for a project (or a single cycle), flat, ordered priority → points →
// dispatched_at. The board nests by parent_task_id and maps linear_state →
// columns client-side. Single call, zero Linear round-trips (reads the mirror).
//
// Params: ?project= (default "default"), ?cycle= (cycle_id | "all" | "active" | "").
// "active" resolves to the cycle spanning today; empty/"all" returns everything.
func (r *Relay) apiGetBoardTasks(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	cycle := req.URL.Query().Get("cycle")

	if cycle == "active" {
		cycle = "" // default; resolve below if an active cycle exists
		if cycles, err := r.DB.ListCycles(project); err == nil {
			for _, c := range cycles {
				if c.Active {
					cycle = c.ID
					break
				}
			}
		}
	}

	tasks, err := r.DB.ListBoardTasks(project, cycle, 1000)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to list board tasks", err)
		return
	}
	if tasks == nil {
		tasks = []models.Task{}
	}
	writeJSON(w, tasks)
}

// apiGetCycles serves the kanban cycle filter: the distinct Linear cycles in the
// mirror for a project, with the active one (spanning today) flagged. Empty in
// native mode.
func (r *Relay) apiGetCycles(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	cycles, err := r.DB.ListCycles(project)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to list cycles", err)
		return
	}
	if cycles == nil {
		cycles = []db.Cycle{}
	}
	writeJSON(w, cycles)
}

func (r *Relay) apiGetAllTasks(w http.ResponseWriter) {
	tasks, err := r.DB.ListAllTasks(200)
	if err != nil {
		http.Error(w, `{"error":"failed to list tasks"}`, http.StatusInternalServerError)
		return
	}
	if tasks == nil {
		tasks = []models.Task{}
	}
	writeJSON(w, tasks)
}

func (r *Relay) apiGetLatestTasks(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	since := req.URL.Query().Get("since")
	if since == "" {
		// Default window is 1h — 30s was too narrow for human polling cycles,
		// so the endpoint appeared empty even when tasks existed.
		since = time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02T15:04:05.000000Z")
	}

	tasks, err := r.DB.GetTasksSince(project, since, 100)
	if err != nil {
		http.Error(w, `{"error":"failed to get tasks"}`, http.StatusInternalServerError)
		return
	}
	if tasks == nil {
		tasks = []models.Task{}
	}
	writeJSON(w, tasks)
}

func (r *Relay) apiGetTask(w http.ResponseWriter, req *http.Request, path string) {
	project := projectFromRequest(req)
	taskID := strings.TrimPrefix(path, "/tasks/")
	if taskID == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}

	task, err := r.DB.GetTask(taskID, project)
	if err != nil {
		http.Error(w, `{"error":"failed to get task"}`, http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, task)
}

func (r *Relay) apiGetTaskProgress(w http.ResponseWriter, req *http.Request, path string) {
	project := projectFromRequest(req)
	trimmed := strings.TrimPrefix(path, "/tasks/")
	taskID, _, _ := strings.Cut(trimmed, "/")
	if taskID == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}
	notes, err := r.DB.GetProgressNotes(taskID, project)
	if err != nil {
		http.Error(w, `{"error":"failed to get progress notes"}`, http.StatusInternalServerError)
		return
	}
	if notes == nil {
		notes = []db.ProgressNote{}
	}
	writeJSON(w, notes)
}

func (r *Relay) apiDispatchTask(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Project      string  `json:"project"`
		Profile      string  `json:"profile"`
		Title        string  `json:"title"`
		Description  string  `json:"description"`
		Priority     string  `json:"priority"`
		ParentTaskID *string `json:"parent_task_id,omitempty"`
		BoardID      *string `json:"board_id,omitempty"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Profile == "" || body.Title == "" {
		http.Error(w, `{"error":"profile and title are required"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}

	task, err := r.DB.DispatchTask(body.Project, body.Profile, "user", body.Title, body.Description, body.Priority, body.ParentTaskID, body.BoardID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to dispatch task", err)
		return
	}
	writeJSON(w, task)
}

func (r *Relay) apiTransitionTask(w http.ResponseWriter, req *http.Request, path string) {
	// path: /tasks/{id}/transition
	trimmed := strings.TrimPrefix(path, "/tasks/")
	taskID, _, _ := strings.Cut(trimmed, "/")
	if taskID == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		Project string  `json:"project"`
		Status  string  `json:"status"`
		Agent   string  `json:"agent"`
		Result  *string `json:"result,omitempty"`
		Reason  *string `json:"reason,omitempty"`
		Force   bool    `json:"force,omitempty"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Status == "" {
		http.Error(w, `{"error":"status is required"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}
	if body.Agent == "" {
		body.Agent = "user"
	}
	// A forced move bypasses the state machine + dependency gate — the DB grants
	// that only to the "user" actor.
	if body.Force {
		body.Agent = "user"
	}

	// Capture the prior status for the audit trail before the move. A forced
	// override on a Linear-mirrored task is refused — Linear is the SSOT there.
	var fromStatus string
	if prev, perr := r.DB.GetTask(taskID, body.Project); perr == nil && prev != nil {
		fromStatus = prev.Status
		if body.Force && prev.Source == "linear" {
			http.Error(w, `{"error":"task is mirrored from Linear (read-only here — Linear is the source of truth)"}`, http.StatusBadRequest)
			return
		}
	}

	var task *models.Task
	var err error
	switch body.Status {
	case "pending":
		task, err = r.DB.ResetTask(taskID, body.Agent, body.Project)
	case "accepted":
		task, err = r.DB.ClaimTask(taskID, body.Agent, body.Project)
	case "in-progress":
		task, err = r.DB.StartTask(taskID, body.Agent, body.Project)
	case "in-review":
		task, err = r.DB.ReviewTask(taskID, body.Agent, body.Project)
	case "done":
		task, err = r.DB.CompleteTask(taskID, body.Agent, body.Project, body.Result)
	case "blocked":
		task, err = r.DB.BlockTask(taskID, body.Agent, body.Project, body.Reason)
	case "cancelled":
		task, err = r.DB.CancelTask(taskID, body.Agent, body.Project, body.Reason)
	default:
		http.Error(w, `{"error":"invalid status"}`, http.StatusBadRequest)
		return
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, "task transition failed", err)
		return
	}

	// Audit the move (best-effort). A forced move is flagged distinctly.
	action := "transition"
	if body.Force {
		action = "force_transition"
	}
	reason := ""
	if body.Reason != nil {
		reason = *body.Reason
	}
	_ = r.DB.RecordAudit(models.AuditEntry{
		Project:      body.Project,
		Actor:        body.Agent,
		Action:       action,
		ResourceType: "task",
		ResourceID:   taskID,
		Summary:      fmt.Sprintf("%s → %s", orDash(fromStatus), body.Status),
		Reason:       reason,
	})

	// Emit the matching semantic event so notification rules fire for web-UI
	// driven transitions too (the MCP handlers emit their own).
	if evType, line := semanticForStatus(body.Status, task.Title); evType != "" {
		payload := taskSemantic(task, line)
		if body.Status == "blocked" && body.Reason != nil {
			payload["reason"] = *body.Reason
		}
		r.Events.EmitSemantic(evType, body.Project, body.Agent, payload)
	}

	// Write-back (Linear mode): mirror the status change to the Linear issue
	// (state move + optional comment), fire-and-forget. No-op in native mode.
	note := body.Result
	if note == nil {
		note = body.Reason
	}
	pushStatusAsync(r.TaskConn(), task, body.Status, note)

	writeJSON(w, task)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// apiReassignTask hands a task to a different agent. Path: /tasks/{id}/reassign
func (r *Relay) apiReassignTask(w http.ResponseWriter, req *http.Request, path string) {
	trimmed := strings.TrimPrefix(path, "/tasks/")
	taskID, _, _ := strings.Cut(trimmed, "/")
	if taskID == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}
	var body struct {
		Project string `json:"project"`
		Agent   string `json:"agent"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}
	prev := ""
	if cur, perr := r.DB.GetTask(taskID, body.Project); perr == nil && cur != nil && cur.AssignedTo != nil {
		prev = *cur.AssignedTo
	}
	task, err := r.DB.ReassignTask(taskID, body.Project, body.Agent)
	if err != nil {
		apiError(w, http.StatusBadRequest, "failed to reassign task", err)
		return
	}
	_ = r.DB.RecordAudit(models.AuditEntry{
		Project:      body.Project,
		Actor:        "user",
		Action:       "reassign",
		ResourceType: "task",
		ResourceID:   taskID,
		Summary:      fmt.Sprintf("%s → %s", orDash(prev), body.Agent),
	})
	writeJSON(w, task)
}

// apiTaskComment posts a comment on a task. On a Linear-mirrored task it goes to
// the Linear issue; otherwise it's saved as a local progress note. Path:
// /tasks/{id}/comment
func (r *Relay) apiTaskComment(w http.ResponseWriter, req *http.Request, path string) {
	trimmed := strings.TrimPrefix(path, "/tasks/")
	taskID, _, _ := strings.Cut(trimmed, "/")
	if taskID == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}
	var body struct {
		Project string `json:"project"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}
	body.Body = strings.TrimSpace(body.Body)
	if body.Body == "" {
		http.Error(w, `{"error":"body is required"}`, http.StatusBadRequest)
		return
	}
	task, err := r.DB.GetTask(taskID, body.Project)
	if err != nil || task == nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}
	conn := r.TaskConn()
	if task.Source == "linear" && conn.Active() && task.LinearIssueID != nil && *task.LinearIssueID != "" {
		if err := conn.Comment(*task.LinearIssueID, body.Body); err != nil {
			apiError(w, http.StatusBadGateway, "failed to post comment to Linear", err)
			return
		}
		writeJSON(w, map[string]any{"posted": "linear"})
		return
	}
	if err := r.DB.AddProgressNote(taskID, body.Project, "user", body.Body); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to add note", err)
		return
	}
	writeJSON(w, map[string]any{"posted": "note"})
}

// apiGetAudit returns the audit trail for a project, optionally scoped to one task.
func (r *Relay) apiGetAudit(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	resource := req.URL.Query().Get("resource")
	limit := 0
	if l := req.URL.Query().Get("limit"); l != "" {
		limit, _ = strconv.Atoi(l)
	}
	entries, err := r.DB.ListAudit(project, resource, limit)
	if err != nil {
		http.Error(w, `{"error":"failed to list audit"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, entries)
}

// semanticForStatus maps a kanban status to its semantic event type + line.
func semanticForStatus(status, title string) (string, string) {
	switch status {
	case "accepted":
		return EvTaskClaimed, "Claimed: " + title
	case "in-progress":
		return EvTaskInProgress, "In progress: " + title
	case "in-review":
		return EvTaskInReview, "In review: " + title
	case "done":
		return EvTaskDone, "Done: " + title
	case "blocked":
		return EvTaskBlocked, "Blocked: " + title
	}
	return "", ""
}

func (r *Relay) apiUpdateTask(w http.ResponseWriter, req *http.Request, path string) {
	trimmed := strings.TrimPrefix(path, "/tasks/")
	taskID := trimmed
	if taskID == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		Project     string  `json:"project"`
		Title       *string `json:"title,omitempty"`
		Description *string `json:"description,omitempty"`
		Priority    *string `json:"priority,omitempty"`
		BoardID     *string `json:"board_id,omitempty"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}

	task, err := r.DB.UpdateTaskFields(taskID, body.Project, body.Title, body.Description, body.Priority, body.BoardID)
	if err != nil {
		apiError(w, http.StatusBadRequest, "failed to update task", err)
		return
	}
	writeJSON(w, task)
}

func (r *Relay) apiDeleteTask(w http.ResponseWriter, req *http.Request, path string) {
	trimmed := strings.TrimPrefix(path, "/tasks/")
	taskID := trimmed
	if taskID == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}
	project := req.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}

	if err := r.DB.DeleteTask(taskID, project); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to delete task", err)
		return
	}
	writeJSON(w, map[string]any{"deleted": true, "id": taskID})
}

// --- Profile API endpoints ---

func (r *Relay) apiGetProfiles(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)

	profiles, err := r.DB.ListProfiles(project)
	if err != nil {
		http.Error(w, `{"error":"failed to list profiles"}`, http.StatusInternalServerError)
		return
	}
	if profiles == nil {
		profiles = []models.Profile{}
	}
	writeJSON(w, profiles)
}

func (r *Relay) apiGetOrgs(w http.ResponseWriter) {
	orgs, err := r.DB.ListOrgs()
	if err != nil {
		http.Error(w, `{"error":"failed to list orgs"}`, http.StatusInternalServerError)
		return
	}
	if orgs == nil {
		orgs = []models.Org{}
	}
	writeJSON(w, orgs)
}

func (r *Relay) apiGetAllTeams(w http.ResponseWriter) {
	teams, err := r.DB.ListAllTeams()
	if err != nil {
		http.Error(w, `{"error":"failed to list teams"}`, http.StatusInternalServerError)
		return
	}
	if teams == nil {
		teams = []models.Team{}
	}

	// Enrich with member counts
	type teamWithMembers struct {
		models.Team
		MemberCount int      `json:"member_count"`
		Members     []string `json:"members"`
	}
	result := make([]teamWithMembers, 0, len(teams))
	for _, t := range teams {
		members, _ := r.DB.GetTeamMemberNames(t.ID)
		if members == nil {
			members = []string{}
		}
		result = append(result, teamWithMembers{
			Team:        t,
			MemberCount: len(members),
			Members:     members,
		})
	}
	writeJSON(w, result)
}

func (r *Relay) apiGetTeams(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	teams, err := r.DB.ListTeams(project)
	if err != nil {
		http.Error(w, `{"error":"failed to list teams"}`, http.StatusInternalServerError)
		return
	}
	if teams == nil {
		teams = []models.Team{}
	}
	writeJSON(w, teams)
}

func (r *Relay) apiGetTeamMembers(w http.ResponseWriter, req *http.Request, path string) {
	project := projectFromRequest(req)
	trimmed := strings.TrimPrefix(path, "/teams/")
	slug, _, _ := strings.Cut(trimmed, "/")
	if slug == "" {
		http.Error(w, `{"error":"missing team slug"}`, http.StatusBadRequest)
		return
	}

	team, err := r.DB.GetTeam(project, slug)
	if err != nil || team == nil {
		http.Error(w, `{"error":"team not found"}`, http.StatusNotFound)
		return
	}

	members, err := r.DB.GetTeamMembers(team.ID)
	if err != nil {
		http.Error(w, `{"error":"failed to get members"}`, http.StatusInternalServerError)
		return
	}
	if members == nil {
		members = []models.TeamMember{}
	}
	writeJSON(w, map[string]any{"team": team, "members": members})
}

func (r *Relay) apiGetProfile(w http.ResponseWriter, req *http.Request, path string) {
	project := projectFromRequest(req)
	slug := strings.TrimPrefix(path, "/profiles/")
	if slug == "" {
		http.Error(w, `{"error":"missing profile slug"}`, http.StatusBadRequest)
		return
	}

	profile, err := r.DB.GetProfile(project, slug)
	if err != nil {
		http.Error(w, `{"error":"failed to get profile"}`, http.StatusInternalServerError)
		return
	}
	if profile == nil {
		http.Error(w, `{"error":"profile not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, profile)
}

func (r *Relay) apiGetBoards(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	boards, err := r.DB.ListBoards(project)
	if err != nil {
		http.Error(w, `{"error":"failed to list boards"}`, http.StatusInternalServerError)
		return
	}
	if boards == nil {
		boards = []models.Board{}
	}
	writeJSON(w, boards)
}

func (r *Relay) apiGetAllBoards(w http.ResponseWriter) {
	boards, err := r.DB.ListAllBoards()
	if err != nil {
		http.Error(w, `{"error":"failed to list boards"}`, http.StatusInternalServerError)
		return
	}
	if boards == nil {
		boards = []models.Board{}
	}
	writeJSON(w, boards)
}

func (r *Relay) apiGetFileLocks(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	locks, err := r.DB.ListFileLocks(project)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to list file locks", err)
		return
	}
	if locks == nil {
		locks = []models.FileLock{}
	}
	writeJSON(w, locks)
}

// --- Token Usage API ---

func (r *Relay) apiGetTokenUsage(w http.ResponseWriter, req *http.Request) {
	period := req.URL.Query().Get("period")
	since := db.PeriodToSince(period)
	data, err := r.DB.GetTokenUsageByProject(since)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to get token usage", err)
		return
	}
	if data == nil {
		data = []db.TokenUsageSummary{}
	}
	writeJSON(w, data)
}

func (r *Relay) apiGetTokenUsageByProject(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	period := req.URL.Query().Get("period")
	since := db.PeriodToSince(period)
	data, err := r.DB.GetTokenUsageByAgent(project, since)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to get token usage by agent", err)
		return
	}
	if data == nil {
		data = []db.TokenUsageSummary{}
	}
	writeJSON(w, data)
}

func (r *Relay) apiGetTokenUsageByAgent(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	agent := req.URL.Query().Get("agent")
	period := req.URL.Query().Get("period")
	since := db.PeriodToSince(period)
	data, err := r.DB.GetTokenUsageByTool(project, agent, since)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to get token usage by tool", err)
		return
	}
	if data == nil {
		data = []db.TokenUsageSummary{}
	}
	writeJSON(w, data)
}

func (r *Relay) apiGetTokenTimeSeries(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	agent := req.URL.Query().Get("agent")
	period := req.URL.Query().Get("period")
	since := db.PeriodToSince(period)
	bucket := db.PeriodToBucket(period)
	data, err := r.DB.GetTokenTimeSeries(project, agent, since, bucket)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to get token time series", err)
		return
	}
	if data == nil {
		data = []db.TokenTimeBucket{}
	}
	writeJSON(w, data)
}

// apiSetAgentAvatar sets or clears an agent's avatar image URL (photo/gif).
func (r *Relay) apiSetAgentAvatar(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Project string `json:"project"`
		Name    string `json:"name"`
		URL     string `json:"url"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}
	u := strings.TrimSpace(body.URL)
	if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "data:image/") {
		http.Error(w, `{"error":"url must be http(s) or data:image"}`, http.StatusBadRequest)
		return
	}
	if err := r.DB.SetAgentAvatar(body.Project, body.Name, u); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to set avatar", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "agent": body.Name, "avatar_url": u})
}
