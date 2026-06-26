package cli

import "testing"

// TestHookAlreadyWired guards the idempotency of the settings.json merge — a
// re-install must not duplicate an event's hook entry.
func TestHookAlreadyWired(t *testing.T) {
	cmd := "/home/u/.claude/hooks/session-start.sh"
	arr := []any{
		map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": cmd, "timeout": 5},
		}},
	}
	if !hookAlreadyWired(arr, cmd) {
		t.Fatal("should detect the already-wired command")
	}
	if hookAlreadyWired(arr, "/home/u/.claude/hooks/other.sh") {
		t.Fatal("must not match a different command")
	}
	if hookAlreadyWired(nil, cmd) {
		t.Fatal("empty array is not wired")
	}
	// Tolerates malformed entries without matching.
	if hookAlreadyWired([]any{"garbage", map[string]any{"nope": 1}}, cmd) {
		t.Fatal("malformed entries must not match")
	}
}
