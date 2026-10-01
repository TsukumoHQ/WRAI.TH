# Identity and routing: W1, W2, W9 (task a28c0e69)

Status: ruled by cto-tsukumo on 2026-10-01 (relay memory `wraith-identity-routing-ruling`); every recommendation below was approved. S1 is implemented in `internal/relay/identity_mode.go` and `agent_token.go`.

## 1. W1: who is calling

### Current resolution path

1. `context.go:35` `HTTPContextFunc`: takes `?agent=`, `?project=` and the `X-Agent-Token` header from the connection.
2. `handlers.go:473` `resolveAgent`: uses the `as` argument when present, otherwise `?agent=`. The value is self-asserted.
3. `toolset.go:279` wraps every tool in `agent_token.go:44` `guardAgentToken`, which works as follows:
   - **Token present:** the `as` name must equal the token owner's name. A mismatch returns `AGENT_IDENTITY_MISMATCH`. `register_agent` is exempt for override actors (`db.IsOverrideActorName`, `task_lease.go:107`, env `RELAY_OVERRIDE_ACTORS`, default `niwa`).
   - **Gap A:** only the name is compared. `AgentByToken` returns `(project, name)` and the guard drops the project. A token minted for `infra-lead@synergix-dev` therefore passes as `infra-lead` in `synergix`. W1 and W2 meet here.
   - **Gap B:** with no token, the call passes unchecked. The comment at `agent_token.go:19` says "trust-loopback governs it". This is the whole of W1.
4. `toolset.go:424` `guardIdentity` (mutating tools only) checks that the name is registered in the project. It also runs a session-binding check (`toolset.go:482`) that fails open when the session is unknown, for example after a restart.
5. REST has the same rule through `agent_token.go:102` `apiIdentityRefused`, which applies only when a token is present.

### Day-1 evidence

This pane (`wraith-backend`) runs with no `NIWA_AGENT_TOKEN`, and both `~/.claude/.mcp.json` and the repo `.mcp.json` carry no `X-Agent-Token` header. Every call this pane makes is tokenless. Enforcing on day 1 would lock it out, and probably most of the fleet with it. niwa 19a1986c (the per-pane header) does exist, but it has not reached this pane.

### Proposed rule

The authenticated principal is the `(project, name)` that owns a valid `X-Agent-Token`. Loopback and `RELAY_API_KEY` authenticate the transport only, never an identity.

| Caller | `as` / project claimed | Result |
|---|---|---|
| Token of `(P, N)` | `N` in `P` | allow |
| Token of `(P, N)` | another name, or project ≠ `P` | `AGENT_IDENTITY_MISMATCH` (name mismatch is already refused today; project mismatch is new) |
| Token of a delegating service `S` | any `(P, N)` | allow, plus audit `identity.delegated {actor:S, on_behalf_of:N@P, tool}` |
| No token | anything | depends on mode (below) |

- **Delegating service:** the token owner's name is in `RELAY_OVERRIDE_ACTORS`. This list is set by the operator in the relay env and agents cannot change it. Use a new helper `IsDelegatingService` that reads only the env list. It must not include the `human`/`user` literals that `IsOverrideActorName` accepts. The agent flag `is_service` is not a trust root, because any caller can set it in `register_agent` (`handlers_agents.go:110`). It stays what it is today, a liveness-gate exemption.
- **Mode flag `RELAY_IDENTITY_MODE`:**
  - `off`: today's behavior.
  - `warn` (new default): tokenless and project-mismatched calls go through but emit `identity.unverified {tool, as, project, peer}` to the log and audit, rate-limited per `(project, as)`. This writes no row on the hot path; the audit goes through the existing batched/async path.
  - `enforce`: every tool needs a principal, except bootstrap (`register_agent` first mint, `create_project`, `whoami`, `discover_tools`). Reads are covered too, because `get_inbox` changes delivery state and shows another agent's mail.
- **Migration:** ship `warn` → niwa provisions a header on every pane (19a1986c) and lands 910c077b → the warn log shows zero tokenless calls from niwa panes across one full fleet cycle → cto/founder set `RELAY_IDENTITY_MODE=enforce` on the prod relay. OSS self-hosters stay on `warn` until they opt in.
- **Interaction with 910c077b (niwa facade):** the facade should forward the pane's own `X-Agent-Token` upstream. The relay then verifies the identity end to end and does not have to trust the facade. The niwa service token (delegation) is reserved for the daemon's own calls and for register-on-behalf. Warn mode will show which tools niwa really calls on behalf of others (`watchdog.rs` calls `as:"niwa"`; the qa gate's review identities are unmeasured). We then narrow delegation to that list or keep it fleet-wide; that is a ruling for later.

### As built (S1)

- **Mode:** `RELAY_IDENTITY_MODE` is read on each call. An unset or unknown value means `warn`.
- **Name mismatch:** a token used under another name is refused in every mode, as before S1.
- **Project mismatch:** a token used in another project is refused in `enforce`, counted in `warn` (`reason=project_mismatch`), and ignored in `off`.
  - Exception: `register_agent` under the token's own name in a project where that name has no token yet. This is a first mint, so a pane can join a second project.
- **Delegation:**
  - Applies to every tool, in every project.
  - Delegated calls are aggregated into one `identity.delegated` audit row per (project, actor, on_behalf_of, tool) per window. The row's details carry `{actor, on_behalf_of, tool, count}`.
  - `register_agent` on behalf of a pane goes through the same path. Before S1, `IsOverrideActorName` allowed it, and that function also accepted a token owned by an agent named `human`/`user`.
- **Warn log:** nothing is written on the hot path. The in-memory ledger is flushed every 5 minutes, and on shutdown. Each flush logs one line per (project, as, reason): `identity.unverified project=P as=X reason=tokenless|project_mismatch count=N`.
  - **S5 go/no-go.** Run this over the log the relay actually writes. `log.Printf` goes to stderr.
    - Under launchd that is the plist's `StandardErrorPath`, read below, with `/tmp/agent-relay.err` as the default `install.sh` writes. `~/.agent-relay/serve.log` is stale.
    - Under the systemd user unit, use `journalctl --user -u agent-relay` as the source instead.
    - The command counts only lines after the last `listening on` boot line, so lines from before the upgrade never count.
    - Run it at least one flush window (5 min) after the restart.
    - Empty output means go. Otherwise it prints `<count> project=P as=X` per tokenless caller.

    ```
    LOG=$(/usr/libexec/PlistBuddy -c 'Print :StandardErrorPath' ~/Library/LaunchAgents/com.agent-relay.plist 2>/dev/null || echo /tmp/agent-relay.err)
    START=$(grep -n 'listening on ' "$LOG" | tail -1 | cut -d: -f1)
    tail -n +$(( ${START:-0} + 1 )) "$LOG" | grep -o 'identity.unverified project=[^ ]* as=[^ ]* reason=tokenless count=[0-9]*' | awk '{split($5,c,"=");s[$2" "$3]+=c[2]} END{for(k in s) print s[k],k}' | sort -rn
    ```
- **Human/user exemption:** applies to `as` = `human`/`user` from a loopback TCP peer, checked on the real `RemoteAddr`, never on `X-Forwarded-For`. It is aggregated into `identity.exempt` audit rows in `warn` and `enforce`.
- **stdio transport:** it carries no header, so it has no principal. Under `enforce`, only bootstrap tools work over stdio. That is acceptable because `enforce` is opt-in and S5 only flips the HTTP relay.
- **REST (S4):** `apiIdentityRefused(claimed, project)` applies the same rule to every REST write that names an actor:
  - routes covered: `/api/messages`, notifications emit, task transition, task archive, memory POST, and `/api/user-response` (acts as `human`);
  - the audit tool label is `rest:<METHOD> <path>`;
  - a tokenless write in `enforce` returns 401 `AGENT_TOKEN_REQUIRED`.

## 2. W2: homonyms across projects

Inboxes and deliveries are already scoped by project (`deliveries.go:101`, `WHERE d.project=? AND d.to_agent=?`). The leak comes from three routing bugs:

1. **Cross-project reply goes to the local homonym.**
   - `sendCrossProject` (`handlers.go:392`) stores the message in the destination project with `from_agent=cto` and `metadata.source_project=<src>`.
   - When the recipient replies with `to:"cto"` and no `target_project`, the name resolves in the recipient's project. If a `cto` exists there, that agent gets the reply and no error is raised.
2. **The reply-path grant is given to the homonym.**
   - `orgs.go:475` (`CanMessage`) and `orgs.go:515` (`CanReplyTo`) count deliveries in project P where `from_agent=X`.
   - A cross-project message from `X@Q` therefore opens a channel to the unrelated `X@P`.
3. **Token project is ignored.** This is gap A above.

### Rules

- **R1:** a name resolves in exactly one project, the caller's resolved project (`project_ns.go:32`: the param, then the connection, then a single registration). There is never a fallback across projects.
- **R2:** if `reply_to` points to a `cross_project` parent, `to` equals the parent's `source_agent`, and no `target_project` is given, refuse with `RECIPIENT_AMBIGUOUS`. Details: `{name, candidates:[{name, project}], hint:"pass target_project=<source_project>"}`. The refusal applies even when no local homonym exists; today that case returns an unknown-recipient error or queues the message, which hides the intent. An explicit `target_project` routes through the reply arm of W9 (below).
  - *Alternative for ruling:* route such replies to `source_project` automatically. It is friendlier, but it is implicit, and the AC asks for ambiguity to be an error.
- **R3:** the reply-path grant ignores deliveries of `cross_project` messages (filter `json_extract(m.metadata,'$.cross_project')`). A foreign message never opens a local channel.
- **R4:** if the project cannot be resolved and the caller's name is registered in two or more projects, return `PROJECT_AMBIGUOUS {candidates:[projects]}` on every tool. Today writes get a generic error string.
- **R5:** gap A is closed by the `(P, N)` token rule in W1.

## 3. W9: a sanctioned path for prod lead → cto escalation

Today, cross-project DMs must go executive to executive (`handlers.go:398` and `:413`). Same-project grants are team, `reports_to`, notify, or reply-path (`orgs.go:403`). A non-executive prod lead can only reach the dev `cto` through `cto-synergix`. That relay hop is the current sanctioned path, and it costs one hop per message.

Proposed: a **qualified cross-project `reports_to`**, using the same trust level as the existing same-project chain.

- `register_agent(reports_to:"cto@synergix-dev")` stores the qualified value. It is validated: the project must exist, and the format is `name@project` with the project normalized.
  - Because the qualified string never equals a local name, it opens no same-project grant to a local `cto`. This makes it safe against homonyms by construction.
  - It needs no schema change, and `agentColumns` is untouched.
- In `sendCrossProject`, a send is allowed if one of these holds:
  - (a) sender and target are both executives (unchanged);
  - (b) **escalate up:** the sender's `reports_to` is `to@dstProject` and the target is an executive in `dstProject`;
  - (c) **reply down:** `reply_to` is a `cross_project` message that the target sent to the sender. Its `source_agent` is the target and its `source_project` is `dstProject`. This grant is scoped to the thread.
- Every cross-project send writes an `xproject.send {arm: exec|escalate|reply}` audit entry.
- A refusal returns a typed `FORBIDDEN` with the hint `set reports_to=<to>@<dstProject> (target must be executive) or relay via <your exec>`.
- What this does not do: it does not open non-executive → non-executive channels, it does not create a cross-project team, and it does not make cross-project notify channels. An escalation always lands on an executive.
- niwa sets the qualified `reports_to` from team config. That is a separate niwa ticket.

### Rule (as built, S3; founder Q4 2026-10-01)

Synergix prod leads escalate **directly** to `cto@synergix-dev`. The `cto-synergix` relay hop is no longer required, and it keeps working (arm (a)).

```
register_agent(project:"synergix", name:"ai-lead", reports_to:"cto@synergix-dev")
send_message(project:"synergix", as:"ai-lead", target_project:"synergix-dev", to:"cto", ...)   # arm escalate
send_message(project:"synergix-dev", as:"cto", target_project:"synergix", to:"ai-lead",
             reply_to:<that message id>, ...)                                                   # arm reply
```

- **Validation:** `register_agent` checks a qualified `reports_to` before any write and stores it normalized (`CTO@Synergix_Dev` becomes `cto@synergix-dev`). It is refused when:
  - a side is empty;
  - the project is unknown;
  - the project is the agent's own (use the plain name there).
- **Refusal:** `FORBIDDEN` (permission, not retryable). `hint` names `reports_to=<to>@<project>`, or says the target is not an executive.
- **Audit:** `xproject.send {arm, to, to_project, message_id}` is recorded in the sender's project, once per send. Cross-project DMs are rare, so this is not a hot path.
- **Local reads of `reports_to`:**
  - A qualified value never matches a local name. `CanMessage`, the task chain walk, and the org tree all treat it as "no local manager".
  - The notifier's `manager` target falls back to the dispatcher instead of resolving it.

## 4. Slices

All slices are ordered by risk, and every risky change lands behind the mode flag. Each slice is at most 5 files and has one test per AC.

| # | Slice | Files | Test (AC) |
|---|---|---|---|
| S1 | W1 principal: `(P, N)` token binding, delegating service, `RELAY_IDENTITY_MODE` off/warn/enforce, typed `AGENT_TOKEN_REQUIRED` | `agent_token.go`, `context.go` (mode), `task_lease.go` (`IsDelegatingService`), `errors.go`, `agent_token_test.go` | AC1: token X + `as:Y` refused; niwa token + `as:Y` allowed and audited; tokenless call refused in enforce, allowed in warn |
| S2 | W2 routing: R2 `RECIPIENT_AMBIGUOUS`, R3 grant filter, R4 `PROJECT_AMBIGUOUS` | `handlers_messaging.go`, `orgs.go`, `project_ns.go`, `errors.go`, `homonym_routing_test.go` | AC2: `cto` in `dev` and `prod`; cross-project message then reply lands only where it was resolved; a bare reply gives `RECIPIENT_AMBIGUOUS`; no grant to the homonym |
| S3 | W9 arms (b) and (c), qualified `reports_to` validation, audit | `handlers.go`, `handlers_agents.go`, `xproject_escalation_test.go`, this doc to `docs/` rule section | AC3: prod lead with `reports_to=cto@dev` reaches exec `cto`; without it gets `FORBIDDEN` plus hint; `cto` reply-down to the lead works on the thread only |
| S4 | REST parity (`apiIdentityRefused` under mode) and the console exemption ruling | `agent_token.go`, `api_messages.go`, test | REST tokenless write in enforce → 401 `AGENT_TOKEN_REQUIRED` |
| S5 | Flip prod to `enforce` (env only, cto/founder) | none | warn log at zero tokenless niwa calls |

Every slice must pass `go build/vet/test -tags fts5 ./...`, be gofmt-clean, and carry a review-wraith verdict.

## Open questions for the ruling

1. Default mode for OSS: `warn` (recommended) or `off`?
2. R2: refuse (recommended, matches AC) or route to `source_project` automatically?
3. Delegation scope: fleet-wide for `RELAY_OVERRIDE_ACTORS` now, narrowed later from warn data (recommended), or a tool allowlist now?
4. W9 target: does synergix prod lead `reports_to` point at `cto@synergix-dev` or stay `cto-synergix`? This is a founder/org call, not a relay call.
5. Console/REST `human`/`user` under enforce: keep exempt on loopback only (recommended), or require an operator token?
