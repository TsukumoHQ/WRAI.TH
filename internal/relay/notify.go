package relay

import (
	"log"
	"sync"

	"github.com/mark3labs/mcp-go/server"
)

// SessionRegistry tracks connected agent sessions for push notifications.
type SessionRegistry struct {
	mu sync.RWMutex
	// sessions is nested project → agentName → []sessionID so a project-scoped
	// lookup (AgentForSession on every guarded write, NotifyBroadcast) touches
	// only the sessions IN that project — O(agents-in-project) — instead of a
	// HasPrefix scan of every session across every project (relay-perf R5).
	sessions map[string]map[string][]string
	mcpSrv   *server.MCPServer
}

func NewSessionRegistry(mcpSrv *server.MCPServer) *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]map[string][]string),
		mcpSrv:   mcpSrv,
	}
}

// Register associates a session ID with an agent in a project.
func (r *SessionRegistry) Register(project, agentName, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	byAgent := r.sessions[project]
	if byAgent == nil {
		byAgent = make(map[string][]string)
		r.sessions[project] = byAgent
	}
	byAgent[agentName] = append(byAgent[agentName], sessionID)
}

// Unregister removes a session ID from an agent. The caller knows only the
// session id (an agent may hold sessions in multiple projects), so this scans —
// but it runs on disconnect, never on the hot guarded-write path.
func (r *SessionRegistry) Unregister(agentName, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for project, byAgent := range r.sessions {
		for agent, ids := range byAgent {
			for i, id := range ids {
				if id == sessionID {
					byAgent[agent] = append(ids[:i], ids[i+1:]...)
					if len(byAgent[agent]) == 0 {
						delete(byAgent, agent)
					}
					if len(byAgent) == 0 {
						delete(r.sessions, project)
					}
					return
				}
			}
		}
	}
}

// AgentForSession reverse-looks-up which agent a connection's session is bound
// to within a project, for the identity-binding check in guardIdentity. Returns
// (name, true) ONLY when the session is bound to exactly one agent in that
// project — the caller's provable identity. Returns ("", false) when the session
// is unknown (e.g. the in-memory registry was cleared by a relay restart and the
// agent has not re-registered yet) or bound to more than one agent, so the guard
// fails open and never drops a legitimate write on incomplete knowledge.
func (r *SessionRegistry) AgentForSession(project, sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	found := ""
	for agent, ids := range r.sessions[project] {
		for _, id := range ids {
			if id == sessionID {
				if found != "" && found != agent {
					return "", false // session bound to >1 agent — ambiguous, fail open
				}
				found = agent
				break
			}
		}
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// Notify sends a notification to all sessions of the given agent in a project.
func (r *SessionRegistry) Notify(project, agentName, from, subject, messageID string) {
	r.mu.RLock()
	ids := r.sessions[project][agentName]
	sessionIDs := make([]string, len(ids))
	copy(sessionIDs, ids)
	r.mu.RUnlock()

	params := map[string]any{
		"from":       from,
		"subject":    subject,
		"message_id": messageID,
	}

	for _, sid := range sessionIDs {
		if err := r.mcpSrv.SendNotificationToSpecificClient(sid, "notifications/message", params); err != nil {
			log.Printf("notify %s (session %s): %v", agentName, sid, err)
		}
	}
}

// NotifyBroadcast sends a notification to all connected agents in the same project, except the sender.
func (r *SessionRegistry) NotifyBroadcast(project, senderName, subject, messageID string) {
	r.mu.RLock()
	agents := make(map[string][]string)
	for agent, ids := range r.sessions[project] {
		if agent != senderName {
			agents[agent] = append([]string{}, ids...)
		}
	}
	r.mu.RUnlock()

	params := map[string]any{
		"from":       senderName,
		"subject":    subject,
		"message_id": messageID,
	}

	for _, sessionIDs := range agents {
		for _, sid := range sessionIDs {
			if err := r.mcpSrv.SendNotificationToSpecificClient(sid, "notifications/message", params); err != nil {
				log.Printf("broadcast notify (session %s): %v", sid, err)
			}
		}
	}
}

// NotifyProfile sends a notification to all connected agents running a specific profile in a project.
func (r *SessionRegistry) NotifyProfile(project, profileSlug, senderName, subject, taskID string) {
	// For now, we broadcast to all agents in the project since we don't track profile→session mapping.
	// This is acceptable because agents filter by their own profile when picking up tasks.
	r.NotifyBroadcast(project, senderName, subject, taskID)
}
