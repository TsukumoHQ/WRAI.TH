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
		// File exists — try to merge
		existing, err := os.ReadFile(mcpPath)
		if err == nil {
			var cfg mcpConfig
			if json.Unmarshal(existing, &cfg) == nil {
				if entry, exists := cfg.MCPServers["agent-relay"]; exists {
					fmt.Printf("agent-relay already configured in %s\n", mcpPath)
					fmt.Printf("  url: %s\n", entry.URL)
					return
				}
				// Add agent-relay to existing config
				cfg.MCPServers["agent-relay"] = mcpServerEntry{Type: "http", URL: url}
				writeConfig(mcpPath, cfg)
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

func writeConfig(path string, cfg mcpConfig) {
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
