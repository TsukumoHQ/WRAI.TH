package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type mcpConfig struct {
	MCPServers map[string]mcpServerEntry `json:"mcpServers"`
}

type mcpServerEntry struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

func runInit(args []string) {
	// Parse flags
	port := "8090"
	host := "localhost"
	project := ""
	dir := "."
	global := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				port = args[i+1]
				i++
			}
		case "--host":
			if i+1 < len(args) {
				host = args[i+1]
				i++
			}
		case "-p", "--project":
			if i+1 < len(args) {
				project = args[i+1]
				i++
			}
		case "--global":
			global = true
		default:
			// First positional arg is the project name
			if project == "" && args[i][0] != '-' {
				project = args[i]
			}
		}
	}

	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot determine home directory: %v\n", err)
			os.Exit(1)
		}
		dir = filepath.Join(home, ".claude")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot create %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	mcpPath := filepath.Join(dir, ".mcp.json")

	// Default URL = discovery mode (lean ~1.5k-token tools/list). The relay keeps
	// the onboarding core (create_project, register_agent, whoami,
	// get_session_context) visible even in discovery, so setup works directly
	// without paying the ~11k-token full list. Power users can append
	// ?tools=full to list every tool.
	url := fmt.Sprintf("http://%s:%s/mcp", host, port)
	if project != "" {
		url += "?project=" + project
	}

	// Check if file already exists
	if _, err := os.Stat(mcpPath); err == nil {
		// File exists — merge losslessly. Existing entries are kept as raw JSON so
		// stdio-style servers (command/args/env) and any other keys survive the
		// round-trip; typing them into the narrow http-only struct would drop
		// those fields and corrupt the user's config (issue #20).
		existing, err := os.ReadFile(mcpPath)
		if err == nil {
			if root, already, existingURL, ok := mergeRelayServer(existing, url); ok {
				if already {
					fmt.Printf("agent-relay already configured in %s\n", mcpPath)
					fmt.Printf("  url: %s\n", existingURL)
					return
				}
				writeConfig(mcpPath, root)
				fmt.Printf("added agent-relay to existing %s\n", mcpPath)
				fmt.Printf("  url: %s\n", url)
				return
			}
		}
	}

	// Create new config
	cfg := mcpConfig{
		MCPServers: map[string]mcpServerEntry{
			"agent-relay": {Type: "http", URL: url},
		},
	}
	writeConfig(mcpPath, cfg)

	absPath, _ := filepath.Abs(mcpPath)
	fmt.Printf("created %s\n", absPath)
	fmt.Printf("  url: %s\n", url)
	if project != "" {
		fmt.Printf("  project: %s (set as default via URL param)\n", project)
	}

	// Land the public end-user skill so a fresh Claude Code session knows how to
	// drive relay setup + usage. Best-effort — never fail `init` over it.
	if home, err := os.UserHomeDir(); err == nil {
		if err := InstallPublicSkill(home); err == nil {
			fmt.Printf("  skill: ~/.claude/skills/%s/SKILL.md\n", PublicSkillName)
		}
	}

	fmt.Println("\nnext steps:")
	fmt.Println("  1. Run /mcp in Claude Code to reload MCP connections")
	fmt.Println("  2. Call whoami() with a unique salt to identify your session")
	fmt.Println("  3. Call register_agent() to announce your presence")
}

// mergeRelayServer adds the agent-relay entry to an existing .mcp.json without
// disturbing anything else. Existing servers are kept as raw JSON so stdio-style
// entries (command/args/env) and any unknown top-level keys survive the
// round-trip — the http-only struct would silently drop those fields (issue #20).
// ok=false means the input wasn't parseable JSON (caller falls back to a fresh
// config). already=true means agent-relay was present; existingURL is its url.
func mergeRelayServer(existing []byte, url string) (root map[string]json.RawMessage, already bool, existingURL string, ok bool) {
	if json.Unmarshal(existing, &root) != nil {
		return nil, false, "", false
	}
	var servers map[string]json.RawMessage
	if raw, has := root["mcpServers"]; has {
		_ = json.Unmarshal(raw, &servers)
	}
	if servers == nil {
		servers = map[string]json.RawMessage{}
	}
	if raw, exists := servers["agent-relay"]; exists {
		var entry mcpServerEntry
		_ = json.Unmarshal(raw, &entry)
		return root, true, entry.URL, true
	}
	entryJSON, _ := json.Marshal(mcpServerEntry{Type: "http", URL: url})
	servers["agent-relay"] = entryJSON
	serversJSON, _ := json.Marshal(servers)
	root["mcpServers"] = serversJSON
	return root, false, "", true
}

func writeConfig(path string, cfg any) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", path, err)
		os.Exit(1)
	}
}
