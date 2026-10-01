package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-relay/internal/models"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Identity mode (W1 S1 63fbe94b, design docs/design/identity-routing.md §1).
// The authenticated principal of a call is the (project, name) owning its
// X-Agent-Token; loopback and RELAY_API_KEY authenticate the transport only.
// RELAY_IDENTITY_MODE decides what happens to a call that has no principal
// (tokenless) or acts outside its token's project:
//
//	off     — pre-S1 behaviour, nothing recorded;
//	warn    — (default) the call proceeds and is counted; one
//	          `identity.unverified` log line per (project, as, reason) per
//	          flush window carries the count (no synchronous row);
//	enforce — the call is refused AGENT_TOKEN_REQUIRED / AGENT_IDENTITY_MISMATCH,
//	          except the bootstrap tools (identityBootstrapTools, register_agent
//	          first mint) and human/user on a loopback peer (audited).
const (
	IdentityModeOff     = "off"
	IdentityModeWarn    = "warn"
	IdentityModeEnforce = "enforce"
)

// CodeAgentTokenRequired — enforce mode: the call carries no X-Agent-Token.
const CodeAgentTokenRequired = "AGENT_TOKEN_REQUIRED"

// identityFlushInterval is the aggregation window of the identity ledger.
const identityFlushInterval = 5 * time.Minute

// IdentityMode returns the configured RELAY_IDENTITY_MODE; anything other than
// off/enforce (unset included) is warn.
func IdentityMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_IDENTITY_MODE"))) {
	case IdentityModeOff:
		return IdentityModeOff
	case IdentityModeEnforce:
		return IdentityModeEnforce
	default:
		return IdentityModeWarn
	}
}

// identityBootstrapTools stay open to a tokenless caller in enforce: an agent
// must be able to create its project and orient before it holds a token.
// register_agent is bootstrap only for a first mint (see guardTokenless);
// discover_tools/call_tool are not wrapped (call_tool's inner dispatch is).
var identityBootstrapTools = map[string]bool{
	"create_project": true,
	"whoami":         true,
}

// guardTokenless applies the identity mode to a call with no X-Agent-Token.
func (h *Handlers) guardTokenless(ctx context.Context, req mcp.CallToolRequest, toolName string, next server.ToolHandlerFunc) (*mcp.CallToolResult, error) {
	mode := IdentityMode()
	if mode == IdentityModeOff {
		return next(ctx, req)
	}
	project := h.resolveProject(ctx, req)
	claimed := strings.ToLower(resolveAgent(ctx, req))
	if toolName == "register_agent" {
		claimed = strings.ToLower(req.GetString("name", ""))
	}
	switch {
	case isOperator(claimed) && PeerIsLoopback(ctx):
		h.ident.noteAudit("identity.exempt", project, claimed, claimed, toolName)
		return next(ctx, req)
	case identityBootstrapTools[toolName]:
		return next(ctx, req)
	case toolName == "register_agent" && project != "" && claimed != "" && !h.db.AgentHasToken(project, claimed):
		return next(ctx, req) // first mint
	}
	if mode == IdentityModeEnforce {
		return toolError(CodeAgentTokenRequired, CategoryPermission, false,
			fmt.Sprintf("RELAY_IDENTITY_MODE=enforce: %s needs an X-Agent-Token header (the token register_agent returned for %q)", toolName, claimed),
			map[string]any{"claimed": claimed, "project": project}), nil
	}
	h.ident.noteUnverified(project, claimed, "tokenless")
	return next(ctx, req)
}

// identityLedger aggregates identity observations between flushes so the hot
// path never writes a row: a map increment under a mutex, nothing else.
type identityLedger struct {
	mu         sync.Mutex
	unverified map[unverifiedKey]int
	audits     map[identityAuditKey]int
}

type unverifiedKey struct{ project, as, reason string }

type identityAuditKey struct{ action, project, actor, onBehalfOf, tool string }

func (l *identityLedger) noteUnverified(project, as, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.unverified == nil {
		l.unverified = map[unverifiedKey]int{}
	}
	l.unverified[unverifiedKey{project, as, reason}]++
}

func (l *identityLedger) noteAudit(action, project, actor, onBehalfOf, tool string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.audits == nil {
		l.audits = map[identityAuditKey]int{}
	}
	l.audits[identityAuditKey{action, project, actor, onBehalfOf, tool}]++
}

func (l *identityLedger) drain() (map[unverifiedKey]int, map[identityAuditKey]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u, a := l.unverified, l.audits
	l.unverified, l.audits = nil, nil
	return u, a
}

// logField keeps a caller-supplied value one ASCII token in the log line:
// whitespace, control characters and '=' become '_', so a crafted `as` cannot
// forge or split an identity.unverified line the S5 grep counts.
func logField(s string) string {
	if s == "" {
		return "-"
	}
	return strings.Map(func(r rune) rune {
		if r <= ' ' || r == '=' || r == 0x7f || r > 0x7e {
			return '_'
		}
		return r
	}, s)
}

// flushIdentity emits one `identity.unverified` line per (project, as, reason)
// with its count, and one audit row per (action, project, actor, on_behalf_of,
// tool) with its count, for everything noted since the last flush.
func (h *Handlers) flushIdentity() {
	unverified, audits := h.ident.drain()
	ukeys := make([]unverifiedKey, 0, len(unverified))
	for k := range unverified {
		ukeys = append(ukeys, k)
	}
	sort.Slice(ukeys, func(i, j int) bool {
		a, b := ukeys[i], ukeys[j]
		if a.project != b.project {
			return a.project < b.project
		}
		if a.as != b.as {
			return a.as < b.as
		}
		return a.reason < b.reason
	})
	for _, k := range ukeys {
		log.Printf("identity.unverified project=%s as=%s reason=%s count=%d",
			logField(k.project), logField(k.as), k.reason, unverified[k])
	}
	for k, n := range audits {
		details, _ := json.Marshal(map[string]any{
			"actor": k.actor, "on_behalf_of": k.onBehalfOf, "tool": k.tool, "count": n,
		})
		if err := h.db.RecordAudit(models.AuditEntry{
			Project:      k.project,
			Actor:        k.actor,
			Action:       k.action,
			ResourceType: "agent",
			ResourceID:   k.onBehalfOf,
			Summary:      fmt.Sprintf("%s acted as %s via %s ×%d", k.actor, k.onBehalfOf, k.tool, n),
			Details:      string(details),
		}); err != nil {
			log.Printf("identity: audit %s: %v", k.action, err)
		}
	}
}

func (h *Handlers) runIdentityFlusher() {
	defer close(h.identDone)
	ticker := time.NewTicker(identityFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.flushIdentity()
		case <-h.stopCh:
			h.flushIdentity()
			return
		}
	}
}

// tokenProjectMismatch applies the mode to a token of project bp used in
// another resolved project. Returns a refusal in enforce, nil otherwise.
func (h *Handlers) tokenProjectMismatch(toolName, project, bp, bound string) *mcp.CallToolResult {
	if project == "" || project == bp {
		return nil
	}
	// A pane joining a second project under its own name mints there first.
	if toolName == "register_agent" && !h.db.AgentHasToken(project, bound) {
		return nil
	}
	switch IdentityMode() {
	case IdentityModeEnforce:
		return toolError(CodeAgentIdentityMismatch, CategoryPermission, false,
			fmt.Sprintf("this request's X-Agent-Token belongs to %q in project %q and cannot act in project %q", bound, bp, project),
			map[string]any{"token_agent": bound, "token_project": bp, "claimed_project": project})
	case IdentityModeWarn:
		h.ident.noteUnverified(project, bound, "project_mismatch")
	}
	return nil
}
