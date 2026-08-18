package relay

import (
	"encoding/json"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// CCAR-G1 — uniform typed tool errors.
//
// EVERY relay tool error returns the same JSON envelope so a caller can branch
// on machine fields instead of pattern-matching human prose:
//
//	{ "code": <stable code>, "errorCategory": <taxonomy>, "isRetryable": <bool>, "message": <prose> }
//
// errorCategory is one of a small fixed taxonomy; isRetryable tells the caller
// whether retrying the SAME call as-is can ever succeed (park vs hot-loop). The
// envelope is additive — success shapes are untouched, and specific typed codes
// (SENDER_INACTIVE, TASK_LEASE_HELD, …) are preserved, now carrying the category
// and retry fields too.
const (
	// CategoryTransient — a retry of the same call may succeed later: an internal
	// or DB hiccup, or a temporarily-held resource (a lease that will expire).
	CategoryTransient = "transient"
	// CategoryValidation — the caller sent bad or missing input, or referenced
	// something that does not exist; the same call will keep failing until the
	// caller changes it.
	CategoryValidation = "validation"
	// CategoryPermission — the caller is not allowed to do this; retrying as-is
	// will keep failing until identity/authorization changes.
	CategoryPermission = "permission"
)

// Generic codes for error paths that never had a specific typed code. Specific
// codes (SENDER_INACTIVE, TASK_STATE_CONFLICT, …) keep their own identity.
const (
	CodeInvalidArgument = "INVALID_ARGUMENT"
	CodeNotFound        = "NOT_FOUND"
	CodeForbidden       = "FORBIDDEN"
	CodeInternal        = "INTERNAL"
)

// toolError builds the canonical typed-error envelope. `extra` merges additional
// fields (e.g. SENDER_INACTIVE's reason/agent/project) without displacing the
// four canonical keys. If marshaling ever fails, it falls back to the raw
// message so an error is never swallowed.
func toolError(code, category string, retryable bool, message string, extra map[string]any) *mcp.CallToolResult {
	body := map[string]any{
		"code":          code,
		"errorCategory": category,
		"isRetryable":   retryable,
		"message":       message,
	}
	for k, v := range extra {
		if _, canonical := body[k]; !canonical {
			body[k] = v
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return mcp.NewToolResultError(message)
	}
	return mcp.NewToolResultError(string(b))
}

// Category shortcuts for call sites that know their own code + category.
func validationError(code, message string) *mcp.CallToolResult {
	return toolError(code, CategoryValidation, false, message, nil)
}

func permissionError(code, message string) *mcp.CallToolResult {
	return toolError(code, CategoryPermission, false, message, nil)
}

// transientError marks an error whose same-call retry may later succeed.
func transientError(code, message string) *mcp.CallToolResult {
	return toolError(code, CategoryTransient, true, message, nil)
}

// toolResultError is the drop-in replacement for the legacy
// mcp.NewToolResultError(msg): it wraps a plain message in the canonical
// envelope, inferring code/category/retryable from the message when the call
// site has no specific typed code. Every legacy error path routes through here
// so the whole tool surface speaks one error shape. Sites with a KNOWN failure
// mode should prefer validationError / permissionError / transientError (or a
// dedicated typed helper) for an exact code.
func toolResultError(message string) *mcp.CallToolResult {
	code, category, retryable := classifyMessage(message)
	return toolError(code, category, retryable, message, nil)
}

// classifyMessage infers the typed fields from a legacy prose message. The order
// is deliberate: permission first (its vocabulary is narrow), then validation of
// bad/missing input, then not-found; everything else defaults to a transient
// internal error, the safe default for an unclassified DB/internal failure (a
// retry may succeed). Heuristic by design — a call site that wants a guaranteed
// code should use a typed constructor instead of relying on this.
func classifyMessage(message string) (code, category string, retryable bool) {
	m := strings.ToLower(message)
	switch {
	case containsAny(m,
		"not allowed", "forbidden", "not permitted", "unauthorized",
		"permission", "not a member", "must be an admin", "admin-only",
		"exec-gated", "executive-only", "cannot broadcast", "not authorized"):
		return CodeForbidden, CategoryPermission, false
	case containsAny(m,
		"not found", "does not exist", "no such", "unresolved", "no agent named"):
		return CodeNotFound, CategoryValidation, false
	case containsAny(m,
		"is required", "required", "must be", "must provide", "must not",
		"invalid", "malformed", "cannot be empty", "is empty", "missing",
		"unknown ", "not valid", "out of range",
		// Lost-race / state-conflict wording: the same call as-is keeps failing
		// (the state moved or the row already exists) — never retryable.
		"conflict", "already claimed", "already exists", "already in",
		"status changed", "changed from"):
		return CodeInvalidArgument, CategoryValidation, false
	default:
		return CodeInternal, CategoryTransient, true
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
