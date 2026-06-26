package relay

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"agent-relay/internal/db"
)

// apiListQuotas returns the per-agent quotas for a project. Path: GET /api/quotas
func (r *Relay) apiListQuotas(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	data, err := r.DB.ListAgentQuotas(project)
	if err != nil {
		http.Error(w, `{"error":"failed to list quotas"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, data)
}

// apiSetQuota sets (upserts) an agent's quota. Path: POST /api/quotas
// {project, agent, max_tokens_per_day, max_messages_per_hour, max_tasks_per_hour, max_spawns_per_hour}
// 0 on a field = unlimited for that dimension. The per-day token quota drives
// both the hard block and the budget-exceeded alert (TSU-53 slice-C).
func (r *Relay) apiSetQuota(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Project          string `json:"project"`
		Agent            string `json:"agent"`
		MaxTokensPerDay  int64  `json:"max_tokens_per_day"`
		MaxMessagesPerHr int64  `json:"max_messages_per_hour"`
		MaxTasksPerHr    int64  `json:"max_tasks_per_hour"`
		MaxSpawnsPerHr   int64  `json:"max_spawns_per_hour"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Project == "" {
		body.Project = "default"
	}
	if strings.TrimSpace(body.Agent) == "" {
		http.Error(w, `{"error":"agent is required"}`, http.StatusBadRequest)
		return
	}
	if err := r.DB.SetAgentQuota(body.Project, body.Agent, body.MaxTokensPerDay, body.MaxMessagesPerHr, body.MaxTasksPerHr, body.MaxSpawnsPerHr); err != nil {
		http.Error(w, `{"error":"failed to set quota"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"set": true, "agent": body.Agent, "max_tokens_per_day": body.MaxTokensPerDay})
}

// apiGetAgentHealth returns the per-agent health snapshot (TSU-53 slice-B).
// Path: GET /api/agents/health
func (r *Relay) apiGetAgentHealth(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	data, err := r.DB.GetAgentHealth(project)
	if err != nil {
		http.Error(w, `{"error":"failed to get agent health"}`, http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = []db.AgentHealth{}
	}
	writeJSON(w, data)
}

// apiPostMessage is the plain-REST send endpoint (owner directive: dokan scripts
// notify the relay over REST, never the /mcp JSON-RPC call_tool dispatcher). It
// reuses the same delivery primitives as send_message for the cases a notifier
// needs — direct, team:slug, broadcast (*). Conversations and cross-project DMs
// stay MCP-only. Path: POST /api/messages.
func (r *Relay) apiPostMessage(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Project    string `json:"project"`
		From       string `json:"from"`
		To         string `json:"to"`
		Type       string `json:"type"`
		Subject    string `json:"subject"`
		Content    string `json:"content"`
		Priority   string `json:"priority"`
		Metadata   string `json:"metadata"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	project := strings.TrimSpace(body.Project)
	if project == "" {
		project = "default"
	}
	from := strings.ToLower(strings.TrimSpace(body.From))
	to := strings.ToLower(strings.TrimSpace(body.To))
	if from == "" || to == "" || strings.TrimSpace(body.Content) == "" {
		http.Error(w, `{"error":"from, to, and content are required"}`, http.StatusBadRequest)
		return
	}
	msgType := body.Type
	if msgType == "" {
		msgType = "notification"
	}
	priority := mapPriority(body.Priority) // "" → P2
	ttl := body.TTLSeconds
	if ttl <= 0 {
		ttl = 14400
	}
	metadata := body.Metadata
	if metadata == "" {
		metadata = "{}"
	}

	// Quota (same gate as send_message).
	if q := r.DB.CheckQuotaError(project, from, "messages"); q != "" {
		http.Error(w, `{"error":`+strconv.Quote(q)+`}`, http.StatusTooManyRequests)
		return
	}
	// Permission: when teams are configured, a direct send needs a path
	// (shared team / reports_to / notify channel). "user" + broadcast + team: handled below.
	if to != "*" && to != "user" && !strings.HasPrefix(to, "team:") {
		if hasTeams, _ := r.DB.HasTeams(project); hasTeams {
			if allowed, _ := r.DB.CanMessage(project, from, to); !allowed {
				http.Error(w, `{"error":"not authorized to message '`+to+`' (no shared team / reports_to / notify channel)"}`, http.StatusForbidden)
				return
			}
		}
	}
	_ = r.DB.TouchAgent(project, from)

	// team:slug → fan out to members + team inbox.
	if strings.HasPrefix(to, "team:") {
		team, err := r.DB.ResolveTeamSlug(project, strings.TrimPrefix(to, "team:"))
		if err != nil || team == nil {
			http.Error(w, `{"error":"team not found"}`, http.StatusBadRequest)
			return
		}
		members, _ := r.DB.GetTeamMemberNames(team.ID)
		var recipients []string
		for _, m := range members {
			if m != from {
				recipients = append(recipients, m)
			}
		}
		msg, err := r.DB.InsertMessageWithDeliveries(project, from, to, msgType, body.Subject, body.Content, metadata, priority, ttl, nil, nil, recipients)
		if err != nil {
			http.Error(w, `{"error":"failed to send"}`, http.StatusInternalServerError)
			return
		}
		_ = r.DB.AddToTeamInbox(team.ID, msg.ID)
		for _, m := range recipients {
			r.Registry.Notify(project, m, from, body.Subject, msg.ID)
		}
		r.Events.Emit(MCPEvent{Type: "message", Action: "team", Agent: from, Project: project, Label: to})
		writeJSON(w, msg)
		return
	}

	// Direct or broadcast.
	recipients, _ := r.DB.ResolveRecipients(project, to, from, nil)
	msg, err := r.DB.InsertMessageWithDeliveries(project, from, to, msgType, body.Subject, body.Content, metadata, priority, ttl, nil, nil, recipients)
	if err != nil {
		http.Error(w, `{"error":"failed to send"}`, http.StatusInternalServerError)
		return
	}
	action := "send"
	if to == "*" {
		r.Registry.NotifyBroadcast(project, from, body.Subject, msg.ID)
		action = "broadcast"
	} else {
		r.Registry.Notify(project, to, from, body.Subject, msg.ID)
	}
	r.Events.Emit(MCPEvent{Type: "message", Action: action, Agent: from, Project: project, Label: to})
	writeJSON(w, msg)
}
