package cli

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// hookEvents maps a Claude Code hook event to the script that services it. This
// is the canonical wiring — the same set install.sh writes into settings.json.
var hookEvents = []struct{ event, script string }{
	{"PreToolUse", "ingest-pre-tool.sh"},
	{"PostToolUse", "ingest-post-tool.sh"},
	{"Stop", "ingest-stop.sh"},
	{"SubagentStart", "ingest-subagent-start.sh"},
	{"SubagentStop", "ingest-subagent-stop.sh"},
	{"SessionStart", "session-start.sh"},
}

// RunHooks installs or inspects the relay's activity/identity hooks. It is the
// one reliable, self-contained path to wire a Claude Code session into the relay
// (last_seen, activity stream, session identity, per-turn tokens) — replacing the
// fragile "scripts on disk but not wired in settings.json" state.
//
//	agent-relay hooks install   write scripts → ~/.claude/hooks + merge settings.json
//	agent-relay hooks status    show what's installed / wired / missing
func RunHooks(scripts embed.FS, args []string) {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find home dir: %v\n", err)
		os.Exit(1)
	}
	hooksDir := filepath.Join(home, ".claude", "hooks")
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	switch sub {
	case "install":
		hooksInstall(scripts, hooksDir, settingsPath)
	case "status":
		hooksStatus(hooksDir, settingsPath)
	default:
		fmt.Println("usage: agent-relay hooks [install|status]")
		os.Exit(1)
	}
}

func hooksInstall(scripts embed.FS, hooksDir, settingsPath string) {
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", hooksDir, err)
		os.Exit(1)
	}

	// 1. Write every embedded hook script (executable).
	entries, _ := fs.ReadDir(scripts, "skill/hooks")
	wrote := 0
	for _, e := range entries {
		data, err := scripts.ReadFile("skill/hooks/" + e.Name())
		if err != nil {
			continue
		}
		dst := filepath.Join(hooksDir, e.Name())
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", dst, err)
			continue
		}
		_ = os.Chmod(dst, 0o755)
		wrote++
	}
	fmt.Printf("✓ wrote %d hook scripts → %s\n", wrote, hooksDir)

	// 2. Merge the hook wiring into settings.json (backup first, idempotent).
	settings := map[string]any{}
	if raw, err := os.ReadFile(settingsPath); err == nil {
		_ = os.WriteFile(settingsPath+".bak", raw, 0o644) // best-effort backup
		_ = json.Unmarshal(raw, &settings)                // tolerate empty/garbage → {}
	} else {
		_ = os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	wired := 0
	for _, he := range hookEvents {
		cmd := filepath.Join(hooksDir, he.script)
		if _, err := os.Stat(cmd); err != nil {
			continue // script not on disk (e.g. not in this build) → don't wire a dead command
		}
		arr, _ := hooks[he.event].([]any)
		if hookAlreadyWired(arr, cmd) {
			continue
		}
		arr = append(arr, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": 5}},
		})
		hooks[he.event] = arr
		wired++
	}
	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "    ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode settings: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", settingsPath, err)
		os.Exit(1)
	}
	fmt.Printf("✓ wired %d new hook event(s) into %s\n", wired, settingsPath)
	fmt.Println("  events: PreToolUse, PostToolUse, Stop, SubagentStart, SubagentStop, SessionStart")
	fmt.Println("  hooks POST to ${RELAY_URL:-http://localhost:8090}; need `jq` + `curl` on PATH.")
	fmt.Println("  Restart the Claude Code session (or /clear) so it reloads settings.json.")
}

// hookAlreadyWired reports whether an event's hook array already runs cmd.
func hookAlreadyWired(arr []any, cmd string) bool {
	for _, h := range arr {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := hm["hooks"].([]any)
		if !ok {
			continue
		}
		for _, i := range inner {
			if im, ok := i.(map[string]any); ok {
				if c, _ := im["command"].(string); c == cmd {
					return true
				}
			}
		}
	}
	return false
}

func hooksStatus(hooksDir, settingsPath string) {
	fmt.Printf("hooks dir: %s\n", hooksDir)
	settings := map[string]any{}
	if raw, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(raw, &settings)
	}
	hooks, _ := settings["hooks"].(map[string]any)

	allOK := true
	for _, he := range hookEvents {
		cmd := filepath.Join(hooksDir, he.script)
		_, statErr := os.Stat(cmd)
		onDisk := statErr == nil
		wired := false
		if hooks != nil {
			if arr, ok := hooks[he.event].([]any); ok {
				wired = hookAlreadyWired(arr, cmd)
			}
		}
		mark := "✓"
		if !onDisk || !wired {
			mark = "✗"
			allOK = false
		}
		fmt.Printf("  %s %-14s script:%v wired:%v\n", mark, he.event, onDisk, wired)
	}
	if allOK {
		fmt.Println("all hooks installed + wired. Run from inside a session to verify last_seen/tokens flow.")
	} else {
		fmt.Println("incomplete — run `agent-relay hooks install` to fix.")
	}
}
