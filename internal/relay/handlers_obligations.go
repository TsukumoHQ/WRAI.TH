package relay

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// Obligation tools (DEC-wraith-obligations-1 slice 2b): an agent sees what it
// owes and discharges or declines it itself. The relay re-checks every
// discharge against the obligation's predicate; a decline is an honest early
// breach that escalates the rung's sanction now, with its reason class.

// callerProfile is the caller's profile slug ("" when unregistered or unset).
func (h *Handlers) callerProfile(project, agent string) string {
	if a, err := h.db.GetAgent(project, agent); err == nil && a != nil && a.ProfileSlug != nil {
		return *a.ProfileSlug
	}
	return ""
}

// HandleObligationsMine lists the caller's active obligations: its profile
// pool, or tasks assigned to it. Read-only.
func (h *Handlers) HandleObligationsMine(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	obs, err := h.db.MyTaskObligations(project, agent, h.callerProfile(project, agent))
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to list obligations: %v", err)), nil
	}
	answers, err := h.db.MyAnswerObligations(project, agent)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to list obligations: %v", err)), nil
	}
	obs = append(obs, answers...)
	return h.resultJSONTracked(project, agent, "obligations_mine", map[string]any{"obligations": obs, "count": len(obs)})
}

// HandleObligationDischarge fulfils an obligation only if its predicate holds
// now; otherwise it refuses and nothing changes.
func (h *Handlers) HandleObligationDischarge(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	id := req.GetString("id", "")
	if id == "" {
		return validationError(CodeInvalidArgument, "id is required"), nil
	}
	ok, reason, err := h.db.DischargeTaskObligation(project, id, agent, req.GetString("evidence", ""), h.db.Now())
	if err == nil && !ok && reason == "unknown obligation" {
		// Not a task obligation: an answer obligation re-checks the bearer's reply.
		ok, reason, err = h.db.DischargeAnswerObligation(project, id, agent, req.GetString("evidence", ""), h.db.Now())
	}
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to discharge: %v", err)), nil
	}
	if !ok {
		code := CodeInvalidArgument
		if reason == "unknown obligation" {
			code = CodeNotFound
		}
		return validationError(code, "not discharged: "+reason), nil
	}
	return h.resultJSONTracked(project, agent, "obligation_discharge", map[string]any{"id": id, "state": db.ObligationFulfilled})
}

// HandleObligationDecline records the bearer's decline with a closed reason
// class and follows the norm's decline path: the rung breaches now and its
// sanction goes out immediately, naming the reason.
func (h *Handlers) HandleObligationDecline(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	id := req.GetString("id", "")
	reasonClass := req.GetString("reason_class", "")
	if id == "" {
		return validationError(CodeInvalidArgument, "id is required"), nil
	}
	if !db.DeclineReasons[reasonClass] {
		return validationError(CodeInvalidArgument, "reason_class must be one of not_mine|cannot|blocked_by|duplicate"), nil
	}
	o, err := h.db.TaskObligationByID(project, id)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to read obligation: %v", err)), nil
	}
	if o == nil {
		return h.declineAnswerObligation(project, agent, id, reasonClass)
	}
	if !o.IsBearer(agent, h.callerProfile(project, agent)) {
		return permissionError(CodeForbidden, "only the obligation's bearer can decline it"), nil
	}
	if o.State != db.ObligationActive {
		return validationError(CodeInvalidArgument, "obligation is "+o.State+", not active"), nil
	}

	now := h.db.Now()
	minutes := 0
	if at, err := time.Parse("2006-01-02T15:04:05Z", o.DispatchedAt); err == nil {
		minutes = int(now.Sub(at).Minutes())
	}
	sn := ackSanction(h.db, *o, minutes)
	ok, err := h.db.DeclineTaskAck(o.ID, o.NormID, o.TaskID, reasonClass, now, sn.alsoClose...)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to decline: %v", err)), nil
	}
	if !ok {
		return validationError(CodeInvalidArgument, "not declined: the obligation or its task moved concurrently"), nil
	}
	detail := reasonClass
	if reason := strings.TrimSpace(req.GetString("reason", "")); reason != "" {
		detail += ": " + reason
	}
	sn.text += fmt.Sprintf(" Declined by %s (%s).", agent, detail)
	sendAckSanction(h.db, h.registry, *o, sn, minutes)
	return h.resultJSONTracked(project, agent, "obligation_decline", map[string]any{
		"id": id, "state": db.ObligationUnfulfilled, "reason_class": reasonClass, "escalated_to": sn.target,
	})
}

// declineAnswerObligation declines an answer obligation (task 044a4876): only
// its bearer may, and it closes unfulfilled with the reason class. The role
// escalation that follows a breach is the sweeper's (slice B).
func (h *Handlers) declineAnswerObligation(project, agent, id, reasonClass string) (*mcp.CallToolResult, error) {
	o, err := h.db.AnswerObligationByID(project, id)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to read obligation: %v", err)), nil
	}
	if o == nil {
		return validationError(CodeNotFound, "unknown obligation"), nil
	}
	if !strings.EqualFold(o.Bearer, agent) {
		return permissionError(CodeForbidden, "only the obligation's bearer can decline it"), nil
	}
	if o.State != db.ObligationActive {
		return validationError(CodeInvalidArgument, "obligation is "+o.State+", not active"), nil
	}
	ok, err := h.db.DeclineAnswerObligation(project, id, reasonClass, h.db.Now())
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to decline: %v", err)), nil
	}
	if !ok {
		return validationError(CodeInvalidArgument, "not declined: the obligation moved concurrently"), nil
	}
	return h.resultJSONTracked(project, agent, "obligation_decline", map[string]any{
		"id": id, "state": db.ObligationUnfulfilled, "reason_class": reasonClass,
	})
}
