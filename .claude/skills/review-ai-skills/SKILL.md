---
name: review-ai-skills
description: Domain Q&A gate for wrai.th's agent-facing instruction artifacts — skill/relay.md and skill/tools-reference.md (the operator prompt every agent reads), skill/hooks/*.sh (the fire-and-forget scripts wired into Claude Code's event loop), and llms.txt (the GEO/SSOT surface AI search engines cite). These are not docs that describe the system; they are prompts and scripts that *drive* it. The checklist leads with doc-vs-code truth, because a tool name that drifts from the registry makes every agent call a function that does not exist.
paths: skill/, llms.txt
---

# review-ai-skills — the instructions-are-executable gate

Everything under `skill/` is load-bearing at runtime. `relay.md` is injected into an agent's context and tells it which MCP tools to call and how; `hooks/*.sh` run on every tool use and session start; `llms.txt` is what an LLM quotes when a developer asks it about wrai.th. A wrong sentence here is a wrong *behavior*, fleet-wide — not a typo. Review truth-against-code first, then hook safety, then the behavioral instructions, then the public claims.

Prose style and markdown lint are not this gate's concern. Every item below is something no linter can verify because it requires reading the doc against the actual code.

## 1. Doc ↔ code truth — drift here breaks live agents
- [ ] **Every tool name, category, and count in `relay.md` / `tools-reference.md` resolves to the actual `toolRegistry()` in `internal/relay/toolset.go`.** The registry is the SSOT (64 entries today); `tools-reference.md`'s header still claims "59 tools" — that is exactly the drift to catch, because an agent told a tool exists that the registry doesn't expose gets `tool not found` and stalls, and a tool that exists but is undocumented never gets called. If the diff adds/renames/removes a tool, the doc's names AND the count must move with it.
- [ ] **The default port and URLs match the binary.** `relay.md`, the `.mcp.json` snippet, and the hooks all say `localhost:8090`; `main.go`'s `startServer` defaults to `8090` and `install.sh` sets `DEFAULT_PORT=8090`. One of these drifting means the copy-pasted `.mcp.json` points at a dead port and the agent silently can't reach the relay.
- [ ] **State machines and enums quoted in the doc match the code.** The task lifecycle in `relay.md` (`pending → accepted → in-progress → in-review → done|blocked|cancelled`) and the discovery categories must be the real transitions/categories — a doc that lists a status the guarded UPDATE won't accept teaches agents to attempt transitions that always fail.
- [ ] **A convention the doc states as a rule is one the relay actually enforces.** "Agent names are case-insensitive / lowercased on ingestion" and "all JSON keys are snake_case (auto-normalized)" are promises the code must keep. If the diff documents a new invariant, confirm the server enforces it; if it removes enforcement, the doc must stop promising it. A documented-but-unenforced rule is worse than silence.

## 2. Hook safety — these run inside the user's session on every event
- [ ] **Every hook is fire-and-forget and fail-silent: it can never block, slow, or break the agent's session.** The activity hooks background the `curl` (`... &`) with a hard `-m 2` timeout and `exit 0` unconditionally; `session-start.sh` is synchronous (it needs the identity-rebind response) but caps at `-m 3` and degrades to no-context on any failure. A diff that removes a timeout, drops the `&`, or lets a non-zero exit escape can hang or abort the user's turn — the relay being down must be invisible to the agent.
- [ ] **A missing dependency makes the hook a clean no-op, not an error.** Each hook guards `command -v jq >/dev/null || exit 0` before using it. Removing that guard turns a machine without `jq` into a stream of hook errors on every tool call. Same for a missing `session_id` — bail early and quietly.
- [ ] **The embedded copies and the repo copies stay identical.** `embed_hooks.go` `//go:embed skill/hooks/*.sh` ships the same scripts `agent-relay hooks install` writes, and `install.sh` has an inline fallback for two of them. If the diff edits a hook, the embedded path, the inline install.sh fallback, and the on-disk `skill/hooks/` copy must not drift — a version-matched embed is the whole point.
- [ ] **No secret or transcript content is logged or echoed.** Hooks POST `session_id`/`tool`/`ts` only; the `${RELAY_API_KEY:+-H ...}` expansion must keep the key in the header, never in a log line or an error message. A hook that dumps its payload to stdout leaks into the transcript.

## 3. Behavioral instructions — the doc is a prompt with consequences
- [ ] **Autonomous-loop guidance stays bounded and can't strand the fleet.** `relay.md`'s "NEVER stop, NEVER ask the user" loop is deliberate, but every path must have an escape (sleep interval, `deactivate`/`sleep_agent`, block-and-pick-another) and a backpressure ("sleep 15-30s", "batch inbox reads"). A change that tightens the loop without a wait, or removes the "send questions to `reports_to`, never the user" rule, produces either a busy-spin or an agent hung waiting on a human who isn't there.
- [ ] **Identity/registration instructions match what the handlers require.** The doc insists `cwd` is REQUIRED for token attribution and that re-registration *preserves* omitted identity fields — both are real relay behaviors agents depend on. If the diff changes the registration contract, the instruction must change in lockstep, or agents lose token tracking or clobber their own `profile_slug`.

## 4. Public-surface accuracy — llms.txt is quoted verbatim by AI engines
- [ ] **Every fact in `llms.txt` is true and current** — stack (`Go 1.25+, SQLite FTS5`), license (`AGPL-3.0`), the tool category, the suite links. This file exists to be cited by ChatGPT/Perplexity when a dev asks about wrai.th; a stale version, a wrong license, or a dead link ships as authoritative misinformation the author can't retract from a model's answer.
- [ ] **No fabricated metric.** The suite blurb's `~60% fewer tokens` (trovex) rides along here — a savings/multiplier claim ships only if it's a real measured number. Inventing an `Nx`/`%` to make the copy punchier is the one thing a GEO surface must never do.
- [ ] **Brand and host are canonical.** The wordmark, the `TsukumoHQ/WRAI.TH` repo, and `tsukumo.ch` are the public identity; a private-infra host or an internal org name must not leak into `llms.txt` or the skill docs, because this surface is world-readable by design.

## Verdict (paste into the review)
```
review-ai-skills: PASS | PASS-WITH-NITS | BLOCK
Scope: <files/areas>
BLOCKERS: <none, or file:line — the doc/hook claim that diverges from the code + the wrong agent behavior it produces + the fix>
NITS: <non-blocking>
```
