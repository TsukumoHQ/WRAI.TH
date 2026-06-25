package relay

import (
	"log"
	"strings"
	"time"

	"agent-relay/internal/connector"
	linearconn "agent-relay/internal/connector/linear"
)

// Settings-table keys for the runtime (UI-driven) Linear configuration.
// Environment variables, when set, always win over these (env = ops override).
const (
	setLinearEnabled  = "linear_enabled"  // "1" / "0"
	setLinearAPIKey   = "linear_api_key"  // lin_api_…
	setLinearTeamKey  = "linear_team_key" // e.g. SYN
	setLinearProject  = "linear_project"  // relay project hosting the mirror (default: lowercased team key)
	setLinearInterval = "linear_reconcile_interval"
	setLinearRouting  = "linear_routing" // JSON {linearProjectId: agentName} — project→agent auto-dispatch
)

// effectiveLinearConfig resolves the Linear connector configuration: env wins
// (RELAY_LINEAR_MODE + LINEAR_API_KEY), else the settings table (UI-driven).
// Settings mode defaults to a 1m reconcile interval (webhook-less localhost).
func (r *Relay) effectiveLinearConfig() (apiKey, teamKey string, enabled bool, interval time.Duration, source string) {
	if r.Config.LinearActive() {
		return r.Config.LinearAPIKey, r.Config.LinearTeamKey, true, r.Config.LinearReconcileIval, "env"
	}
	apiKey = strings.TrimSpace(r.DB.GetSetting(setLinearAPIKey))
	teamKey = strings.TrimSpace(r.DB.GetSetting(setLinearTeamKey))
	enabled = r.DB.GetSetting(setLinearEnabled) == "1" && apiKey != ""
	interval = time.Minute
	if v := r.DB.GetSetting(setLinearInterval); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	return apiKey, teamKey, enabled, interval, "settings"
}

// LinearConnector returns the live Linear connector, or nil when inactive.
func (r *Relay) LinearConnector() *linearconn.Connector {
	r.linearMu.RLock()
	defer r.linearMu.RUnlock()
	return r.linearConn
}

// TaskConn returns the current task connector (Linear when active, else Noop).
func (r *Relay) TaskConn() connector.TaskConnector {
	r.linearMu.RLock()
	defer r.linearMu.RUnlock()
	return r.taskConn
}

// ReconfigureLinear (re)builds the connector from the effective config. Called
// once at boot and again on every linear_* settings change — the previous
// reconcile loop is stopped, the connector swapped atomically, no restart
// needed. Safe to call concurrently.
func (r *Relay) ReconfigureLinear() {
	r.linearMu.Lock()
	defer r.linearMu.Unlock()

	if r.linearStop != nil {
		close(r.linearStop)
		r.linearStop = nil
	}

	apiKey, teamKey, enabled, interval, source := r.effectiveLinearConfig()
	if !enabled || apiKey == "" {
		if r.linearConn != nil {
			log.Printf("[linear] connector disabled")
		}
		r.linearConn = nil
		r.taskConn = connector.Noop{}
		r.Handlers.SetConnector(connector.Noop{})
		return
	}

	conn := linearconn.NewWithParams(r.DB, apiKey, teamKey, r.Config.LinearWebhookSecret, r.DB.GetSetting(setLinearProject))
	conn.SetEventSink(func(e connector.TaskEvent) {
		r.Events.EmitSemantic(e.Type, e.Project, e.Agent, e.Payload)
	})
	r.linearConn = conn
	r.taskConn = conn
	r.Handlers.SetConnector(conn)

	stop := make(chan struct{})
	r.linearStop = stop
	conn.StartReconcile(interval, stop)
	log.Printf("[linear] connector active (team=%s, interval=%s, source=%s)", teamKey, interval, source)
}

// linearProjectName is the relay project the Linear mirror lives under (the
// lowercased team key). Empty when the connector is disabled — the UI uses it
// to scope read-only mode to the mirror project instead of globally.
func (r *Relay) linearProjectName(teamKey string, enabled bool) string {
	if !enabled {
		return ""
	}
	p := strings.ToLower(strings.TrimSpace(r.DB.GetSetting(setLinearProject)))
	if p == "" {
		p = strings.ToLower(strings.TrimSpace(teamKey))
	}
	if p == "" {
		p = "default"
	}
	return p
}
