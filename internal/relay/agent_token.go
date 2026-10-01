package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Per-agent token binding (S3b 05525713, F-04/F-27). A request that carries
// X-Agent-Token acts as the agent the token was minted for, and only as that
// agent: `as` / ?agent= / REST from|agent|as naming anyone else is refused.
// A request with no token is unchanged (trust-loopback governs it).

const (
	// CodeAgentIdentityMismatch — the request's token belongs to a different
	// agent than the identity it acts as.
	CodeAgentIdentityMismatch = "AGENT_IDENTITY_MISMATCH"
	// CodeAgentTokenInvalid — the X-Agent-Token matches no agent (never minted,
	// or rotated away by a re-register).
	CodeAgentTokenInvalid = "AGENT_TOKEN_INVALID"
)

// tokenBinding resolves a request token to the agent name it proves.
// present=false: no token. ok=false: a token that matches no agent.
func (h *Handlers) tokenBinding(token string) (name string, present, ok bool) {
	if token == "" {
		return "", false, false
	}
	_, name, ok = h.db.AgentByToken(token)
	return name, true, ok
}

// guardAgentToken wraps every tool: with a token, the acting identity must be
// the token's (project, name) principal. register_agent acts as its `name`
// argument. A delegating service (db.IsDelegatingService, operator-listed in
// RELAY_OVERRIDE_ACTORS, default "niwa") may act as any agent in any project,
// audited as identity.delegated. A tokenless call is governed by
// RELAY_IDENTITY_MODE (guardTokenless, identity_mode.go).
func (h *Handlers) guardAgentToken(toolName string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := AgentTokenFromContext(ctx)
		if token == "" {
			return h.guardTokenless(ctx, req, toolName, next)
		}
		boundProject, bound, ok := h.db.AgentByToken(token)
		if !ok {
			return toolError(CodeAgentTokenInvalid, CategoryPermission, false,
				"X-Agent-Token matches no agent (never minted, or rotated by a re-register) — use the token register_agent last returned", nil), nil
		}
		claimed := strings.ToLower(resolveAgent(ctx, req))
		if toolName == "register_agent" {
			claimed = strings.ToLower(req.GetString("name", ""))
		}
		project := h.resolveProject(ctx, req)
		if db.IsDelegatingService(bound) {
			if claimed != "" && claimed != bound {
				// An unresolved project audits under the service's own,
				// never the retired 'default' fallback of RecordAudit.
				auditProject := project
				if auditProject == "" {
					auditProject = boundProject
				}
				h.ident.noteAudit("identity.delegated", auditProject, bound, claimed, toolName)
			}
			return next(ctx, req)
		}
		if claimed != "" && claimed != bound {
			return toolError(CodeAgentIdentityMismatch, CategoryPermission, false,
				fmt.Sprintf("this request's X-Agent-Token belongs to %q and cannot act as %q", bound, claimed),
				map[string]any{"token_agent": bound, "claimed": claimed}), nil
		}
		if res := h.tokenProjectMismatch(toolName, project, boundProject, bound); res != nil {
			return res, nil
		}
		return next(ctx, req)
	}
}

// registerToken decides register_agent's token outcome for (project, name):
//   - no token yet → mint ("minted");
//   - the caller proves this name's current token → keep ("unchanged"), or
//     rotate when rotate_token=true ("rotated");
//   - a proven override actor (its own valid token) or the API key → rotate
//     ("rotated") — the daemon re-registering a respawned pane;
//   - anyone else (tokenless) → keep, no token returned ("kept"), so a
//     tokenless caller can never rotate a tokened agent out of its identity.
//
// token is the clear token only when minted/rotated.
func (h *Handlers) registerToken(ctx context.Context, req mcp.CallToolRequest, project, name string) (status, token string, err error) {
	if !h.db.AgentHasToken(project, name) {
		token, err = h.db.MintAgentToken(project, name)
		return "minted", token, err
	}
	reqToken := AgentTokenFromContext(ctx)
	bound, present, ok := h.tokenBinding(reqToken)
	owner := present && ok && bound == name && h.db.AgentTokenMatches(project, name, reqToken)
	switch {
	case owner && !req.GetBool("rotate_token", false):
		return "unchanged", "", nil
	case owner, (present && ok && db.IsOverrideActorName(bound)), APIKeyAuthed(ctx):
		token, err = h.db.MintAgentToken(project, name)
		return "rotated", token, err
	default:
		return "kept", "", nil
	}
}

// apiIdentityRefused applies the MCP identity rule (guardAgentToken +
// guardTokenless, identity_mode.go) to a REST write whose actor is `claimed`
// in `project` (W1 S4): a token acts only as its (project, name) principal or,
// for a delegating service, on behalf of anyone (audited); a tokenless write
// follows RELAY_IDENTITY_MODE, the console human/user exempt on a loopback TCP
// peer only (audited). It writes the refusal and returns true when refused.
func (r *Relay) apiIdentityRefused(w http.ResponseWriter, req *http.Request, claimed, project string) bool {
	claimed = strings.ToLower(strings.TrimSpace(claimed))
	project = NormalizeProject(project)
	if project == "default" {
		project = ""
	}
	tool := "rest:" + req.Method + " " + req.URL.Path
	var ident *identityLedger
	if r.Handlers != nil {
		ident = &r.Handlers.ident
	}
	tok := strings.TrimSpace(req.Header.Get(AgentTokenHeader))
	if tok == "" {
		mode := IdentityMode()
		switch {
		case mode == IdentityModeOff:
			return false
		case isOperator(claimed) && isLoopbackRemote(req.RemoteAddr):
			ident.noteAudit("identity.exempt", project, claimed, claimed, tool)
			return false
		case mode == IdentityModeEnforce:
			return apiIdentityRefusal(w, http.StatusUnauthorized, CodeAgentTokenRequired,
				fmt.Sprintf("RELAY_IDENTITY_MODE=enforce: this write needs an X-Agent-Token header (the token register_agent returned for %q)", claimed))
		}
		ident.noteUnverified(project, claimed, "tokenless")
		return false
	}
	boundProject, bound, ok := r.DB.AgentByToken(tok)
	switch {
	case !ok:
		return apiIdentityRefusal(w, http.StatusUnauthorized, CodeAgentTokenInvalid, "X-Agent-Token matches no agent")
	case db.IsDelegatingService(bound):
		if claimed != bound {
			if project == "" {
				project = boundProject
			}
			ident.noteAudit("identity.delegated", project, bound, claimed, tool)
		}
		return false
	case claimed != bound:
		return apiIdentityRefusal(w, http.StatusForbidden, CodeAgentIdentityMismatch,
			fmt.Sprintf("this request's X-Agent-Token belongs to %q and cannot act as %q", bound, claimed))
	case project != "" && project != boundProject:
		switch IdentityMode() {
		case IdentityModeEnforce:
			return apiIdentityRefusal(w, http.StatusForbidden, CodeAgentIdentityMismatch,
				fmt.Sprintf("this request's X-Agent-Token belongs to %q in project %q and cannot act in project %q", bound, boundProject, project))
		case IdentityModeWarn:
			ident.noteUnverified(project, bound, "project_mismatch")
		}
	}
	return false
}

func apiIdentityRefusal(w http.ResponseWriter, status int, code, msg string) bool {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]string{"error": msg, "code": code})
	_, _ = w.Write(b)
	return true
}
