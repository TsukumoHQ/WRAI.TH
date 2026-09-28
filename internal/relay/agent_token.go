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
// the token's agent. register_agent acts as its `name` argument, except that
// an override actor (db.IsOverrideActorName, default "niwa") may register a
// pane on its behalf.
func (h *Handlers) guardAgentToken(toolName string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		bound, present, ok := h.tokenBinding(AgentTokenFromContext(ctx))
		if !present {
			return next(ctx, req)
		}
		if !ok {
			return toolError(CodeAgentTokenInvalid, CategoryPermission, false,
				"X-Agent-Token matches no agent (never minted, or rotated by a re-register) — use the token register_agent last returned", nil), nil
		}
		claimed := strings.ToLower(resolveAgent(ctx, req))
		if toolName == "register_agent" {
			if db.IsOverrideActorName(bound) {
				return next(ctx, req)
			}
			claimed = strings.ToLower(req.GetString("name", ""))
		}
		if claimed != "" && claimed != bound {
			return toolError(CodeAgentIdentityMismatch, CategoryPermission, false,
				fmt.Sprintf("this request's X-Agent-Token belongs to %q and cannot act as %q", bound, claimed),
				map[string]any{"token_agent": bound, "claimed": claimed}), nil
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

// apiIdentityRefused binds a REST actor field (from / agent / as) to the
// request's X-Agent-Token. It writes the refusal and returns true when the
// token is unknown (401) or belongs to another agent (403); no token → false.
func (r *Relay) apiIdentityRefused(w http.ResponseWriter, req *http.Request, claimed string) bool {
	tok := strings.TrimSpace(req.Header.Get(AgentTokenHeader))
	if tok == "" {
		return false
	}
	_, bound, ok := r.DB.AgentByToken(tok)
	status, code, msg := 0, "", ""
	switch {
	case !ok:
		status, code, msg = http.StatusUnauthorized, CodeAgentTokenInvalid, "X-Agent-Token matches no agent"
	case strings.ToLower(strings.TrimSpace(claimed)) != bound:
		status, code, msg = http.StatusForbidden, CodeAgentIdentityMismatch,
			fmt.Sprintf("this request's X-Agent-Token belongs to %q and cannot act as %q", bound, claimed)
	default:
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]string{"error": msg, "code": code})
	_, _ = w.Write(b)
	return true
}
