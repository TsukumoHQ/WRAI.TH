package relay

import (
	"log"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/server"
)

// SessionRegistry tracks connected agent sessions for push notifications.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string][]string // "project:agentName" → []sessionID
	mcpSrv   *server.MCPServer
}

func NewSessionRegistry(mcpSrv *server.MCPServer) *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string][]string),
		mcpSrv:   mcpSrv,
	}
}

// registryKey returns the composite key for project-scoped session tracking.
func registryKey(project, agent string) string {
	return project + ":" + agent
}

// Register associates a session ID with an agent in a project.
func (r *SessionRegistry) Register(project, agentName, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := registryKey(project, agentName)
	r.sessions[key] = append(r.sessions[key], sessionID)
}

// Unregister removes a session ID from an agent.
func (r *SessionRegistry) Unregister(agentName, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Search all keys for this session (agent may be in multiple projects)
	for key, ids := range r.sessions {
		for i, id := range ids {
			if id == sessionID {
				r.sessions[key] = append(ids[:i], ids[i+1:]...)
				if len(r.sessions[key]) == 0 {
					delete(r.sessions, key)
				}
				return
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
	prefix := project + ":"
	r.mu.RLock()
	defer r.mu.RUnlock()
	found := ""
	for key, ids := range r.sessions {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		for _, id := range ids {
			if id == sessionID {
				if found != "" && found != key[len(prefix):] {
					return "", false // session bound to >1 agent — ambiguous, fail open
				}
				found = key[len(prefix):]
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
	key := registryKey(project, agentName)
	r.mu.RLock()
	sessionIDs := make([]string, len(r.sessions[key]))
	copy(sessionIDs, r.sessions[key])
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
	prefix := project + ":"
	r.mu.RLock()
	agents := make(map[string][]string)
	for key, ids := range r.sessions {
		if strings.HasPrefix(key, prefix) {
			// Extract agent name from key
			agent := key[len(prefix):]
			if agent != senderName {
				agents[key] = append([]string{}, ids...)
			}
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
