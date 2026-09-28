# [S3b relay identity F-04/F-27] per-agent token binds as/from; trust-loopback off for niwa-managed installs only after niwa sends the token

## Team : wraith-engine (tsukumo)
## Branch : wraith-engine/05525713-agent-token (from main)
## Relay task : 05525713-4152-4310-9fb1-e3341266e178
## Trace : trace=462e8d03a4eab3fad9da40c05a039ae6
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. token minted on register, stored hashed, returned once; re-register under the same name mints a fresh token and invalidates the old one (test)
- [ ] 2. request with X-Agent-Token whose agent != as/from -> 403 AGENT_IDENTITY_MISMATCH (test)
- [ ] 3. tokenless loopback works when trust-loopback on, refused when off (test)
- [ ] 4. trust-loopback stays ON by default for every install in this ticket; the RELAY_MANAGED_BY=niwa OFF flip is NOT here (follow-up after niwa counterpart 19a1986c is live)
- [ ] 5. go test -tags fts5 ./... green

## 2. Root cause & decisions

# S3b 05525713 — per-agent relay token binds as/from (F-04/F-27, relay half)

ROOT_CAUSE: the relay had no per-agent secret: identity was whatever `as` / ?agent= / REST from|agent|as a caller named, and the only anti-theft check (session registry) is best-effort and fail-open. Any pane (or any loopback process under trust-loopback) could act as any agent. RELAY_TRUST_LOOPBACK=0 only applied when RELAY_API_KEY was set.

## Decision (niwa-cto rulings 791f3cc8 / c79b7e3d)
- agents.token_hash (sha256, additive, indexed, out of agentColumns). register_agent mints on a name's first register, returns agent_token once.
- X-Agent-Token binds every MCP tool (guard next to guardRoutingParamTypes) and the 4 REST actor sites; AGENT_IDENTITY_MISMATCH / AGENT_TOKEN_INVALID (REST 403 / 401).
- Rotation: only own current token + rotate_token=true, an override actor (RELAY_OVERRIDE_ACTORS, default niwa) proven by its own token, or the API key. Tokenless re-register keeps the token ("kept" + hint) — closes the tokenless-rotation residual in this ticket as ruled.
- authMiddleware: live agent token authenticates; RELAY_TRUST_LOOPBACK=0 refuses tokenless loopback even without a key. Default stays ON; RELAY_MANAGED_BY=niwa flip NOT here (after niwa 19a1986c).
- Stacked on S3 0b980988 (same middleware/relay.go); submit after S3 merges.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db (agent_token.go, db.go, task_lease.go), internal/relay (agent_token.go, context.go, handlers_agents.go, middleware.go, relay.go, toolset.go, tools.go, api*.go), docs/deployment.md, skill/tools-reference.md
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (8 packages)
BLOCKERS: none
NITS:
- Token binds the NAME, not (project, name): an agent registered in several projects has one token per project row, each proving its name everywhere. Deliberate (niwa spans projects).
- Guard adds one indexed RO read per tool call only when a token is sent; no write on any hot path (mint only on first register / rotation).
- With trust-loopback OFF the web UI and ingest hooks carry no token and would 401 — exactly why the OFF flip waits for the niwa counterpart; default unchanged.

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/ -run TestAgentTokenMintedOnceKept -count=1
  test_sha: 406f3af
  output: |
    (run on the base tree pre-rebase S3 tip ac9e16f (= main e18260a content) with the test's main-compatible core)
    --- FAIL: TestAgentTokenMintedOnceKept (0.06s)
        zz_red_test.go:16: first register: want a minted agent_token, got status=<nil> token=<nil>
    FAIL
    FAIL	agent-relay/internal/relay	0.665s

## 3. Files changed

```
docs/deployment.md                  |  15 ++-
 internal/db/agent_token.go          |  74 +++++++++++
 internal/db/agent_token_test.go     |  40 ++++++
 internal/db/db.go                   |   6 +
 internal/db/task_lease.go           |   9 +-
 internal/relay/agent_token.go       | 123 ++++++++++++++++++
 internal/relay/agent_token_test.go  | 240 ++++++++++++++++++++++++++++++++++++
 internal/relay/api.go               |   7 ++
 internal/relay/api_messages.go      |   3 +
 internal/relay/api_notifications.go |   3 +
 internal/relay/context.go           |  21 ++++
 internal/relay/handlers_agents.go   |  16 ++-
 internal/relay/middleware.go        |  30 +++--
 internal/relay/middleware_test.go   |   6 +-
 internal/relay/relay.go             |  11 +-
 internal/relay/tools.go             |   1 +
 internal/relay/toolset.go           |   6 +
 skill/tools-reference.md            |   2 +-
 18 files changed, 594 insertions(+), 19 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `05525713-4152-4310-9fb1-e3341266e178`._
