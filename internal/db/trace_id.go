package db

import (
	"crypto/rand"
	"encoding/hex"
)

// newTraceID mints a 32-hex-lowercase (128-bit) W3C trace-id via crypto/rand —
// the GROUPING key for one dispatch's whole causal chain (task -> messages ->
// events -> gate). Deliberately just the trace-id half of a W3C traceparent,
// not the full header: span-id/flags mutate per hop and don't fit an additive
// sqlite column, and the causal EDGES already exist via parent_task_id/
// reply_to — what's missing is this grouping key.
func newTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ValidTraceID reports whether s is a well-formed trace-id: exactly 32
// lowercase hex characters. Used to refuse a caller-supplied trace_id that
// isn't shaped right, rather than silently accepting garbage into the
// correlation key.
func ValidTraceID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
