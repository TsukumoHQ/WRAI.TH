package cli

import (
	"encoding/json"
	"testing"
)

// Issue #20: `ar init` must not corrupt existing stdio-style MCP servers
// (command/args/env form) when it adds the agent-relay entry.
func TestMergeRelayServerPreservesStdioEntries(t *testing.T) {
	existing := []byte(`{
	  "mcpServers": {
	    "filesystem": {
	      "command": "npx",
	      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
	      "env": {"FOO": "bar"}
	    }
	  },
	  "someOtherTopLevelKey": {"keep": true}
	}`)

	root, already, _, ok := mergeRelayServer(existing, "http://localhost:8090/mcp")
	if !ok {
		t.Fatal("expected parse ok")
	}
	if already {
		t.Fatal("agent-relay was not present; already must be false")
	}

	out, _ := json.Marshal(root)

	// Unknown top-level key survives.
	if root["someOtherTopLevelKey"] == nil {
		t.Error("top-level key dropped")
	}

	var parsed struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("re-parse: %v", err)
	}

	fs, hasFS := parsed.MCPServers["filesystem"]
	if !hasFS {
		t.Fatal("stdio server 'filesystem' dropped")
	}
	if fs.Command != "npx" || len(fs.Args) != 3 || fs.Env["FOO"] != "bar" {
		t.Errorf("stdio fields corrupted: %+v", fs)
	}
	if relay, ok := parsed.MCPServers["agent-relay"]; !ok || relay.URL == "" {
		t.Errorf("agent-relay entry not added: %+v", relay)
	}
}

// Idempotency: re-running detects the existing agent-relay entry and reports it.
func TestMergeRelayServerAlreadyConfigured(t *testing.T) {
	existing := []byte(`{"mcpServers":{"agent-relay":{"type":"http","url":"http://localhost:8090/mcp"}}}`)
	_, already, existingURL, ok := mergeRelayServer(existing, "http://localhost:9999/mcp")
	if !ok || !already {
		t.Fatalf("expected already-configured, got ok=%v already=%v", ok, already)
	}
	if existingURL != "http://localhost:8090/mcp" {
		t.Errorf("wrong existing url: %s", existingURL)
	}
}

// Non-JSON input signals the caller to fall back to a fresh config.
func TestMergeRelayServerBadJSON(t *testing.T) {
	if _, _, _, ok := mergeRelayServer([]byte("not json"), "http://x/mcp"); ok {
		t.Error("expected ok=false for unparseable input")
	}
}
