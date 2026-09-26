package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	knowledge, err := h.db.MyCoherenceObligations(project, agent)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to list obligations: %v", err)), nil
	}
	obs = append(obs, knowledge...)
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
	var coh db.CoherenceDischargeResult
	if err == nil && !ok && reason == "unknown obligation" {
		// A knowledge coherence obligation (design e731f3c9 §4.2, §5): verdict
		// prepared (default) | unaffected | conflict on a reassess, upheld |
		// overturned on a contest review. Flush buffered recalls first so a
		// get_memory made just before counts as having read the new version.
		h.flushRecalls()
		coh, err = h.db.DischargeCoherence(db.CoherenceDischarge{Project: project, ID: id, By: agent,
			Verdict: req.GetString("verdict", ""), Reason: req.GetString("reason", ""),
			Evidence: req.GetString("evidence", ""), Now: h.db.Now()})
		ok, reason = coh.OK, coh.Refusal
		if ok {
			if coh.StampTaskID != "" {
				// The work now runs on the version it just read (task_basis event=reassess).
				h.stampBasis(project, agent, db.BasisReassess, []string{coh.StampTaskID})
			}
			sendCoherenceNotices(h.db, h.registry, coh.Notices)
			out := map[string]any{"id": id, "state": coh.State}
			if v := req.GetString("verdict", ""); v != "" {
				out["verdict"] = v
			}
			if coh.ContestID != "" {
				out["contest_id"] = coh.ContestID
			}
			return h.resultJSONTracked(project, agent, "obligation_discharge", out)
		}
	}
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

// sendCoherenceNotices delivers the messages a coherence discharge produced
// (the contest reviewer, the human's round-2 decide with its reply schema, the
// raiser's round-2 reassess) and pushes each live.
func sendCoherenceNotices(database *db.DB, notifier ackNotifier, notices []db.CoherenceNotice) {
	for _, n := range notices {
		action, prio := "do", "P2"
		meta := map[string]any{"obligation_id": n.ObligationID}
		if n.Action != "" {
			action, prio = n.Action, "P1"
		}
		if n.Schema != nil {
			meta["schema"] = n.Schema
		}
		raw, _ := json.Marshal(meta)
		msg, _, err := database.InsertMessageWithDeliveries(n.Project, "relay", n.To, "notification", n.Subject, n.Body, string(raw),
			prio, -1, nil, nil, []string{n.To}, action)
		if err != nil {
			log.Printf("[coherence] notice %s: %v", n.ObligationID, err)
			continue
		}
		notifier.Notify(n.Project, n.To, "relay", n.Subject, msg.ID)
	}
}

// withStaleContext adds stale_context to a claim/start result when the claimer
// bears open reassess obligations (design e731f3c9 §6, advisory: the claim
// stands and the agent sees the delta before it starts). Best-effort.
func (h *Handlers) withStaleContext(project, agent string, result any) any {
	stale, err := h.db.StaleContextFor(project, agent)
	if err != nil || len(stale) == 0 {
		return result
	}
	if m, ok := result.(map[string]any); ok {
		m["stale_context"] = stale
	}
	return result
}
