package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
)

// Hook-POST ingestion. Claude Code hooks POST here instead of dropping files in
// ~/.pixel-office/events (which only worked when the relay shared a filesystem
// with the agent). Activity is ephemeral → in-memory detector only, zero DB.
// Identity binds on cwd (stable across /clear, which rotates session_id).

type ingestActivityReq struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"` // tool_start | tool_end | stop | subagent_start | subagent_stop | exit
	Tool      string `json:"tool,omitempty"`
	File      string `json:"file,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
	TS        string `json:"ts,omitempty"`
}

type ingestTokensReq struct {
	SessionID     string `json:"session_id"`
	Input         int    `json:"input"`
	Output        int    `json:"output"`
	CacheRead     int    `json:"cache_read"`
	CacheCreation int    `json:"cache_creation"`
	Model         string `json:"model,omitempty"` // model tier the turn ran on (for $ cost)
	TS            string `json:"ts,omitempty"`
}

type ingestSessionStartReq struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	Source         string `json:"source,omitempty"` // startup | resume | clear | compact
	TranscriptPath string `json:"transcript_path,omitempty"`
}

// handleIngestActivity records a hook activity event into the in-memory detector.
// Fire-and-forget: returns 204 without touching the DB.
func (r *Relay) handleIngestActivity(w http.ResponseWriter, req *http.Request) {
	var body ingestActivityReq
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid json", err)
		return
	}
	if body.SessionID == "" || body.Type == "" {
		apiError(w, http.StatusBadRequest, "session_id and type required", nil)
		return
	}
	if r.Ingester != nil {
		r.Ingester.RecordHookEvent(hookEventToAgentEvent(body))
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIngestTokens records real per-turn token usage read from the transcript
// by the Stop hook, attributed to the agent bound to the session. Fire-and-forget.
func (r *Relay) handleIngestTokens(w http.ResponseWriter, req *http.Request) {
	var body ingestTokensReq
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid json", err)
		return
	}
	if body.SessionID == "" {
		apiError(w, http.StatusBadRequest, "session_id required", nil)
		return
	}
	if body.Input == 0 && body.Output == 0 && body.CacheRead == 0 && body.CacheCreation == 0 {
		w.WriteHeader(http.StatusNoContent) // nothing to record
		return
	}
	project, agent, found, err := r.DB.GetAgentBySessionID(body.SessionID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "attribution failed", err)
		return
	}
	if !found {
		// Unbound session (agent never registered with a cwd) — drop quietly.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Handlers != nil {
		r.Handlers.RecordTokens(db.TokenRecord{
			Project:       project,
			Agent:         agent,
			Tool:          "",
			Input:         body.Input,
			Output:        body.Output,
			CacheRead:     body.CacheRead,
			CacheCreation: body.CacheCreation,
			Model:         body.Model,
			CreatedAt:     body.TS,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIngestSessionStart re-binds the (rotated) session_id to the agent that
// owns the worktree cwd, and returns additionalContext so a freshly /clear'd
// agent re-learns who it is. Identity is scoped to the bound agent only.
func (r *Relay) handleIngestSessionStart(w http.ResponseWriter, req *http.Request) {
	var body ingestSessionStartReq
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid json", err)
		return
	}
	if body.SessionID == "" || body.Cwd == "" {
		apiError(w, http.StatusBadRequest, "session_id and cwd required", nil)
		return
	}

	project, name, found, err := r.DB.RebindSessionByCwd(body.Cwd, body.SessionID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "rebind failed", err)
		return
	}

	// Mark the session active in the detector so the board reflects it immediately.
	if r.Ingester != nil {
		r.Ingester.RecordHookEvent(ingest.AgentEvent{
			Type:      ingest.EventAgentSpawn,
			SessionID: body.SessionID,
			Activity:  ingest.ActivityReading,
			Timestamp: time.Now().UTC(),
		})
	}

	resp := map[string]any{"bound": found}
	if found {
		resp["agent"] = name
		resp["project"] = project
		resp["additionalContext"] = fmt.Sprintf(
			"You are the agent-relay agent %q in project %q (session re-bound after %s). "+
				"Your identity persists across /clear via this worktree. "+
				"Call get_inbox to see messages addressed to you, and set ?agent=%s on the relay MCP URL.",
			name, project, sourceOrStartup(body.Source), name,
		)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func sourceOrStartup(s string) string {
	if s == "" {
		return "startup"
	}
	return s
}

// hookEventToAgentEvent mirrors the mapping the old file-drop watcher did
// (internal/ingest/hooks.go) so behaviour is identical, just over HTTP.
func hookEventToAgentEvent(b ingestActivityReq) ingest.AgentEvent {
	ts, err := time.Parse(time.RFC3339, b.TS)
	if err != nil {
		ts = time.Now().UTC()
	}
	evtType := ingest.EventType(b.Type)
	activity := ingest.MapToolToActivity(b.Tool)
	switch evtType {
	case ingest.EventAgentSpawn:
		activity = ingest.ActivityReading
	case ingest.EventAgentExit:
		activity = ingest.ActivityIdle
	case ingest.EventStop:
		activity = ingest.ActivityWaiting
	}
	return ingest.AgentEvent{
		Type:      evtType,
		SessionID: b.SessionID,
		ParentID:  b.ParentID,
		Tool:      b.Tool,
		File:      b.File,
		Activity:  activity,
		Timestamp: ts,
	}
}
