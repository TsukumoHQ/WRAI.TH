package relay

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-relay/internal/models"

	"github.com/mark3labs/mcp-go/mcp"
)

// Cross-project send arms (W9 S3 0bbbf8dd, design identity-routing.md §3).
// A cross-project DM never opens non-executive -> non-executive traffic, a
// cross-project team, or a notify channel; it is allowed through exactly one
// of these arms, and every send is audited as xproject.send {arm}.
const (
	xprojectArmExec     = "exec"     // sender and target are executives
	xprojectArmEscalate = "escalate" // sender.reports_to = to@dstProject, target executive
	xprojectArmReply    = "reply"    // reply on a cross-project thread to its author
)

// splitQualifiedAgent parses a qualified "name@project" (project normalized).
// ok=false for a plain name or an empty side.
func splitQualifiedAgent(s string) (name, project string, ok bool) {
	i := strings.LastIndex(s, "@")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], NormalizeProject(s[i+1:]), true
}

// xprojectArm returns the arm that allows sender@srcProject -> target@dstProject,
// or "" when none does.
func (h *Handlers) xprojectArm(sender, target *models.Agent, srcProject, dstProject string, replyTo *string) string {
	if sender.IsExecutive && target.IsExecutive {
		return xprojectArmExec
	}
	if target.IsExecutive && sender.ReportsTo != nil {
		if name, project, ok := splitQualifiedAgent(*sender.ReportsTo); ok && name == target.Name && project == dstProject {
			return xprojectArmEscalate
		}
	}
	if h.isXProjectReplyDown(sender.Name, srcProject, target.Name, dstProject, replyTo) {
		return xprojectArmReply
	}
	return ""
}

// isXProjectReplyDown: replyTo is a cross-project message that `to`@dstProject
// sent to `from`, stored in srcProject. The grant is that thread only.
func (h *Handlers) isXProjectReplyDown(from, srcProject, to, dstProject string, replyTo *string) bool {
	if replyTo == nil || *replyTo == "" {
		return false
	}
	parent, err := h.db.GetMessage(*replyTo)
	if err != nil || parent == nil || parent.Project != srcProject || parent.To != from || parent.From != to {
		return false
	}
	var meta struct {
		CrossProject  bool   `json:"cross_project"`
		SourceProject string `json:"source_project"`
		SourceAgent   string `json:"source_agent"`
	}
	if json.Unmarshal([]byte(parent.Metadata), &meta) != nil {
		return false
	}
	return meta.CrossProject && meta.SourceAgent == to && meta.SourceProject == dstProject
}

// xprojectForbidden names every sanctioned path, starting with the one the
// sender can set up itself.
func xprojectForbidden(from, srcProject, to, dstProject string, targetIsExec bool) *mcp.CallToolResult {
	hint := fmt.Sprintf("set reports_to=%s@%s on %s (register_agent), or relay via an executive of %s", to, dstProject, from, srcProject)
	if !targetIsExec {
		hint = fmt.Sprintf("%s@%s is not an executive: escalate to an executive of %s, or relay via an executive of %s", to, dstProject, dstProject, srcProject)
	}
	return toolError(CodeForbidden, CategoryPermission, false,
		fmt.Sprintf("cross-project message %s@%s -> %s@%s is not allowed: needs two executives, a reports_to=%s@%s chain to an executive, or a reply on their cross-project thread",
			from, srcProject, to, dstProject, to, dstProject),
		map[string]any{"hint": hint})
}

// auditXProjectSend records one xproject.send row in the sender's project.
// Cross-project DMs are rare (not a hot path); best-effort, never fails a send.
func (h *Handlers) auditXProjectSend(arm, srcProject, from, dstProject, to, msgID string) {
	details, _ := json.Marshal(map[string]any{"arm": arm, "to": to, "to_project": dstProject, "message_id": msgID})
	if err := h.db.RecordAudit(models.AuditEntry{
		Project:      srcProject,
		Actor:        from,
		Action:       "xproject.send",
		ResourceType: "message",
		ResourceID:   msgID,
		Summary:      fmt.Sprintf("%s -> %s@%s (%s)", from, to, dstProject, arm),
		Details:      string(details),
	}); err != nil {
		log.Printf("xproject: audit: %v", err)
	}
}
