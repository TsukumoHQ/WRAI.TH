package relay

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// Founder decision (e2f5ad14 #5): register_agent refuses anonymous/unnamed and
// default-project registrations with a typed code, before any DB write.
func TestRegisterRefusesAnonymousAndDefault(t *testing.T) {
	h := testHandlers(t)
	ctx := context.Background()

	cases := []struct{ name, project, label string }{
		{"", "p1", "empty name"},
		{"anonymous", "p1", "reserved name"},
		{"bot", "default", "default project"},
	}
	for _, c := range cases {
		res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{"name": c.name, "project": c.project, "role": "dev"}))
		txt := expectError(t, res)
		if !strings.Contains(txt, "ANONYMOUS_REGISTRATION_REFUSED") {
			t.Errorf("%s: expected typed code ANONYMOUS_REGISTRATION_REFUSED, got: %s", c.label, txt)
		}
	}

	// A real name + explicit non-default project still registers.
	if res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{"name": "realbot", "project": "p1", "role": "dev"})); res.IsError {
		t.Errorf("valid registration should pass, got: %s", res.Content[0].(mcp.TextContent).Text)
	}
}
