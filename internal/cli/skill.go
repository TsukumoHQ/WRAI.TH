package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// PublicSkillName is the directory under ~/.claude/skills/ that holds the
// shipped end-user skill.
const PublicSkillName = "agent-relay"

// PublicSkillMD is the PUBLIC "how to self-host + use wrai.th" Claude Code
// skill, written verbatim by `agent-relay skill install` (and `init`, and the
// auto-updater's refresh) to ~/.claude/skills/agent-relay/SKILL.md.
//
// This is the PUBLIC user-facing skill, NOT the private maintainer persona and
// NOT the /relay slash command. It teaches a fresh Claude Code session to drive
// relay setup + usage for the user. Hard rails baked into the copy (keep them
// true if you edit): the relay is a single self-hosted Go binary (sqlite, no
// required external services); no hosted/cloud/sign-up; no private paths or
// internal fleet/project references (public/AGPL split); Linear is optional.
const PublicSkillMD = `---
name: agent-relay
description: >-
  Set up and use wrai.th (agent-relay) — a self-hosted MCP relay that lets a
  fleet of Claude Code (and other) agents coordinate: shared inbox + messaging,
  a task board, scoped memory, and a live activity stream. Use when the user
  wants to install or configure the relay, register an agent, check their inbox,
  message or dispatch work to another agent, manage tasks, or stand up a
  multi-agent project. The relay is self-hosted only — a single binary on the
  user's own machine; there is no hosted service or sign-up.
---

# wrai.th (agent-relay) — self-hosted agent coordination

wrai.th is a single Go binary that runs an MCP server on localhost. Agents
register, message each other, share memory, and pick up tasks from a board —
all over MCP, backed by one SQLite file. It is **self-hosted only**: the user
runs the relay on their own box. There is no hosted relay, no cloud, no sign-up.

When this skill is active, drive the user through whichever part they need. Run
the real commands, read their output, and fix problems — don't just narrate.

## 1. Is the relay installed and running?

` + "```bash" + `
agent-relay --version
curl -fsS http://localhost:8090/api/health   # {"status":"ok",...} when up
` + "```" + `

If ` + "`agent-relay`" + ` is not found, install it (one command — builds from source
when Go + a C compiler are present, else downloads a checksum-verified prebuilt):

` + "```bash" + `
curl -fsSL https://raw.githubusercontent.com/TsukumoHQ/WRAI.TH/main/install.sh | bash
` + "```" + `

If the binary is installed but the health check fails, start it:

` + "```bash" + `
agent-relay serve     # foreground; or rely on the auto-start service the installer set up
` + "```" + `

If ` + "`agent-relay`" + ` isn't on PATH, the installer put it in ` + "`~/.local/bin`" + ` — add
that to PATH or call it by full path.

## 2. Wire this machine's hooks

The relay only sees what agents do once the hooks are installed. They feed it
activity, real token usage, and identity that survives ` + "`/clear`" + `. Idempotent —
the installer already ran this; re-run to be sure:

` + "```bash" + `
agent-relay hooks install     # writes hooks + merges ~/.claude/settings.json
agent-relay hooks status      # show what's installed / wired
` + "```" + `

Identity binds on the working directory (cwd), not the session id — so ` + "`/clear`" + `
keeps your identity, and one agent = one worktree.

## 3. Register the MCP server for this project

` + "```bash" + `
agent-relay init [project-name]   # writes/merges .mcp.json in the repo
` + "```" + `

Then run ` + "`/mcp`" + ` in Claude Code to load the relay tools. Existing ` + "`.mcp.json`" + `
files are merged, not overwritten (a ` + "`.bak`" + ` is kept).

Tools use progressive disclosure to stay cheap. If a tool seems missing (e.g.
` + "`tool 'register_agent' not found`" + `), you're in discovery mode: call
` + "`discover_tools(category)`" + ` then ` + "`call_tool(tool, args)`" + `, or append
` + "`?tools=full`" + ` to the relay URL in ` + "`.mcp.json`" + ` and re-run ` + "`/mcp`" + `.

## 4. Identify and register this agent

` + "```" + `
whoami({ salt: "<three-random-words>" })       // returns a stable session id
register_agent({ name: "<agent-name>", project: "<project>", cwd: "<$PWD>" })
` + "```" + `

Pass ` + "`cwd`" + ` — it binds your session so per-turn token usage attributes to you.
Add ` + "`role`" + `, ` + "`reports_to`" + `, or ` + "`is_executive: true`" + ` as needed.

## 5. Coordinate

- **Inbox:** ` + "`get_inbox({ as: \"<agent>\" })`" + ` — non-destructive; ` + "`mark_read`" + ` /
  ` + "`ack_delivery`" + ` clear it. Unread = not yet acknowledged.
- **Message:** ` + "`send_message({ as, to, type: \"notification\"|\"question\", subject, content })`" + `.
- **Tasks:** ` + "`dispatch_task`" + ` → ` + "`claim_task`" + ` → ` + "`start_task`" + ` → ` + "`review_task`" + `
  (PR-up) → ` + "`complete_task`" + `; ` + "`block_task`" + ` / ` + "`resume_task`" + ` / ` + "`cancel_task`" + `
  for the rest. ` + "`list_tasks`" + ` to see the board.
- **Memory:** ` + "`set_memory`" + ` / ` + "`search_memory`" + ` / ` + "`get_memory`" + ` — scoped
  (` + "`agent`" + ` / ` + "`project`" + `) shared knowledge that rides along in ` + "`get_session_context()`" + `.
- **Context:** ` + "`get_session_context()`" + ` returns your profile, memories, pending
  tasks, and unread messages in one call — read it first on boot.

## 6. Stand up a multi-agent project

` + "`create_project({ name, cwd })`" + ` returns an onboarding plan you execute: wire
hooks, learn the relay, analyze the codebase, store memories, create the org
(teams / profiles / a CTO agent), set up the board, and spawn worker agents.
Follow it step by step.

## Honesty rails

- Self-hosted only. Never tell the user to "sign up" or point at a hosted /
  managed service — there is none.
- One binary, one SQLite file, no required external services. The web UI is
  embedded; an optional Linear mirror exists but is off by default.
- The activity stream is ephemeral (in-memory, served over SSE); messages,
  tasks, and memory are the durable state.
`

// RunSkill installs or inspects the public end-user Claude Code skill.
//
//	agent-relay skill install   write the skill → ~/.claude/skills/agent-relay/SKILL.md
//	agent-relay skill status    show whether it's installed
func RunSkill(args []string) {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find home dir: %v\n", err)
		os.Exit(1)
	}
	skillPath := filepath.Join(home, ".claude", "skills", PublicSkillName, "SKILL.md")

	switch sub {
	case "install":
		if err := InstallPublicSkill(home); err != nil {
			fmt.Fprintf(os.Stderr, "install skill: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✓ installed /%s skill → %s\n", PublicSkillName, skillPath)
	case "status":
		if _, err := os.Stat(skillPath); err == nil {
			fmt.Printf("✓ skill installed → %s\n", skillPath)
		} else {
			fmt.Printf("✗ skill not installed (run `agent-relay skill install`)\n")
		}
	default:
		fmt.Println("usage: agent-relay skill [install|status]")
		os.Exit(1)
	}
}

// InstallPublicSkill writes the bundled public skill to
// ~/.claude/skills/agent-relay/SKILL.md (idempotent — the const is the single
// source of truth, so a re-run/refresh just rewrites the canonical copy).
// Exposed so `init` and the auto-updater can refresh the skill alongside hooks.
func InstallPublicSkill(home string) error {
	dir := filepath.Join(home, ".claude", "skills", PublicSkillName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(PublicSkillMD), 0o644)
}
