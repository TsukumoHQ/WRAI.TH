package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// guardActionPoint derives the enforcement point from the action: each action
// of the closed enum is valid at exactly one point (design §4.1).
var guardActionPoint = map[string]string{
	db.GuardActionSuppress:        db.GuardPointOpen,
	db.GuardActionResolveSystemic: db.GuardPointLadder,
	db.GuardActionRoute:           db.GuardPointLadder,
}

// HandleGuard is the one multiplexed compiled-guard tool (design 4d2e57a3 §8
// slice 2, ruling 3ece19c0 OQ5): compile a resolved exception into a shadow
// guard (the response carries the 90-day replay), promote, renew, withdraw or
// get one. Who may do what is decided by the db layer, never here.
func (h *Handlers) HandleGuard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	op := req.GetString("op", "")
	id := req.GetString("id", "")
	if id == "" {
		return validationError(CodeInvalidArgument, "id is required"), nil
	}
	now := time.Now()
	expiresIn := time.Duration(req.GetFloat("days", 0) * float64(24*time.Hour))
	var g db.Guard
	var err error
	if op == "compile" {
		action := req.GetString("action", "")
		point, ok := guardActionPoint[action]
		if !ok {
			return validationError(db.GuardErrBadAction, fmt.Sprintf("action must be suppress|resolve_systemic|route (got %q)", action)), nil
		}
		in := db.GuardCompile{ExceptionID: id, Caller: agent, Point: point, Action: action, ExpiresIn: expiresIn, Now: now}
		if raw := req.GetString("params", ""); raw != "" {
			if err := json.Unmarshal([]byte(raw), &in.ActionParams); err != nil {
				return validationError(CodeInvalidArgument, fmt.Sprintf("params must be a JSON object of strings: %v", err)), nil
			}
		}
		if raw := req.GetString("scope", ""); raw != "" {
			if err := json.Unmarshal([]byte(raw), &in.Scope); err != nil {
				return validationError(CodeInvalidArgument, fmt.Sprintf("scope must be a JSON object: %v", err)), nil
			}
		}
		g, err = h.db.CompileResolution(in)
		if err != nil {
			return guardOpError(err), nil
		}
		return h.resultJSONTracked(project, agent, "guard", g)
	}
	// Every other op names a guard, which must belong to this project (or be global).
	if g, err = h.db.GetGuard(id); err != nil {
		return guardOpError(err), nil
	}
	if g.Project != project && g.Project != "*" {
		return validationError(db.GuardErrNotFound, fmt.Sprintf("guard %s not found in %s", id, project)), nil
	}
	switch op {
	case "get":
	case "promote":
		g, err = h.db.PromoteGuard(id, agent, now)
	case "renew":
		g, err = h.db.RenewGuard(id, agent, expiresIn, now)
	case "withdraw":
		g, err = h.db.WithdrawGuard(id, agent, now)
	default:
		return validationError(CodeInvalidArgument, fmt.Sprintf("op must be compile|promote|renew|withdraw|get (got %q)", op)), nil
	}
	if err != nil {
		return guardOpError(err), nil
	}
	return h.resultJSONTracked(project, agent, "guard", g)
}

// guardOpError maps a typed db.GuardError to the canonical envelope with its
// own code; anything else goes through the legacy classifier.
func guardOpError(err error) *mcp.CallToolResult {
	var ge *db.GuardError
	if !errors.As(err, &ge) {
		return toolResultError(err.Error())
	}
	if ge.Code == db.GuardErrForbidden {
		return permissionError(ge.Code, ge.Error())
	}
	return validationError(ge.Code, ge.Error())
}
