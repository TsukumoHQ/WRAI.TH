package relay

import (
	"strings"
	"testing"
)

// TestServerInstructionsCoverOverrides pins the O1 contract: the override
// semantics dropped from the per-tool `as`/`project` descriptions must live in
// the server-instructions blurb instead, so the client still learns them once.
func TestServerInstructionsCoverOverrides(t *testing.T) {
	for _, want := range []string{"as", "project", "override", "connection"} {
		if !strings.Contains(serverInstructions, want) {
			t.Errorf("serverInstructions must mention %q so the override semantics survive the param-description trim; got:\n%s", want, serverInstructions)
		}
	}
}
