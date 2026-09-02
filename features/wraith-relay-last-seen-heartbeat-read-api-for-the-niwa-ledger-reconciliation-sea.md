# [wraith/relay] last_seen/heartbeat read API for the niwa ledger-reconciliation seam

## Team : wraith-backend (agent-relay)
## Branch : wraith-backend/agent-health-lookup (from main)
## Relay task : 6c1c5167-c16e-418a-bfdc-7c2b7bf379a5
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. API shape agreed with niwa-cto (consumer) — field semantics: what bumps last_seen, staleness contract
- [ ] 2. Read endpoint/tool returns per-agent last_seen cheaply (no write amplification on the hot path)
- [ ] 3. Covered by tests per wraith-dev bar (fts5/CGO build green)

## 2. Root cause & decisions

# Decision — single-agent liveness lookup for the niwa ledger seam (task 6c1c5167)

ROOT_CAUSE: not a bug fix — a cross-lane contract task. DEC-niwa-ledger-seam-1
(niwa-cto, 2026-08-24) deferred relay-side last_seen to cto-tsukumo/wraith as
a follow-up; the ledger seam's v1 liveness (pane-existence + owned-work) has
no relay-truth cross-check yet. This task closes that gap.

Process: per cto-tsukumo's explicit instruction ("Design the contract WITH
niwa-cto (consumer)"), no code was written until niwa-cto answered. Surveyed
what already exists first (design note, trovex 62ffb4bd) — `/api/agents/health`
and `/api/agents/stuck` already gave most of the shape — then sent 3 concrete
questions through cto-tsukumo (no direct message path to niwa-cto). Answer
came back (relay 014b8152):
1. `/api/agents/health` covers the seam; add a single-agent `?agent=<name>`
   lookup (agentd's IMPL-2 reads per-agent with no cache; the whole-project
   pull stays for heal/reap sweeps).
2. Return raw `last_seen` (already did) + `idle_seconds` (already did) + a
   NEW `as_of` field (server clock, ISO) so agentd computes staleness without
   clock skew. Keep the existing 30min `dead` label as advisory only (agentd
   never acts on it alone). `threshold_minutes` is nice-to-have, not
   required. NO confidence/staleness flag — agentd maps a read
   failure/timeout to its own tri-state `Unknown` itself.
3. Passive `TouchAgent`-on-any-relay-call `last_seen` is enough — no push
   heartbeat endpoint.

Decision (implements the answer verbatim, no scope beyond it):
- `internal/db/health.go`: added `AsOf string` to `AgentHealth` (RFC3339,
  stamped at computation time, same format `TouchAgent` already writes so no
  new parse path). Added `GetAgentHealthOne(project, agent)` — single-agent
  form using the existing `GetAgent` (nil,nil on not-found) plus a new
  `tokensForAgentSince` scalar query (WHERE agent=?, not the whole-project
  GROUP BY `tokensByAgentSince` uses) so a per-agent poll doesn't pay for a
  project-wide scan.
- `internal/relay/api_messages.go`: `GET /api/agents/health` gained an
  optional `?agent=` query param — present: single `AgentHealth` object,
  404 if the agent doesn't exist in the project (never a fabricated "dead"
  row for a typo'd name); absent: today's whole-project array, byte-for-byte
  unchanged behavior for every existing caller (board health view, sweeps).
- No push heartbeat endpoint — out of scope per the answer. No
  `threshold_minutes` on this path — out of scope per the answer (nice-to-
  have; `/api/agents/stuck` already has the equivalent if ever needed).

Alternatives considered:
- A new MCP tool instead of extending the REST endpoint. Rejected: agentd
  (Rust daemon) already polls other liveness surfaces (`/api/agents/stuck`)
  over REST per the existing "host daemon polls this" pattern in this
  codebase; REST is the native integration surface here, not MCP.
- Confidence/staleness field in the response. Rejected explicitly by
  niwa-cto — the ledger's `Answer<T>` Unknown-on-failure semantics already
  live client-side; adding a relay-side flag would just be a second,
  potentially-inconsistent source of the same signal.

Verification: `go build -tags fts5 ./...`, `go vet -tags fts5 ./...`,
`gofmt -l .` (clean), `go test -tags fts5 ./...` (668 passed, up from 666 —
2 new: `TestGetAgentHealthOne`, `TestAgentHealthSingleLookup`),
`go test -tags fts5 -race ./internal/db/... ./internal/relay/...` (594
passed).

Scope: internal/db/health.go (+AsOf field, +GetAgentHealthOne,
+tokensForAgentSince) + internal/db/health_test.go (2 new assertions/1 new
test) + internal/relay/api_messages.go (?agent= param on the existing
handler) + internal/relay/agent_health_lookup_test.go (new REST-level test).

## review-wraith verdict: SHIP
Scope: internal/db/health.go (AsOf field on AgentHealth; new GetAgentHealthOne + tokensForAgentSince, single-agent scoped query); internal/relay/api_messages.go (?agent= query param on GET /api/agents/health, 404 on unknown agent, whole-project behavior unchanged when omitted); internal/db/health_test.go + internal/relay/agent_health_lookup_test.go (new tests).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (668 passed, +2 new) / test -tags fts5 -race OK (internal/db + internal/relay, 594 passed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
...d-api-for-the-niwa-ledger-reconciliation-sea.md | 37 +++++++++++++
 internal/db/health.go                              | 64 +++++++++++++++++++++-
 internal/db/health_test.go                         | 53 ++++++++++++++++++
 internal/relay/agent_health_lookup_test.go         | 54 ++++++++++++++++++
 internal/relay/api_messages.go                     | 22 +++++++-
 5 files changed, 228 insertions(+), 2 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-6c1c5167-c16e-418a-bfdc-7c2b7bf379a5

## 5. Timeline

- round 1 → **reject** (review-6c1c5167-c16e-418a-bfdc-7c2b7bf379a5)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `6c1c5167-c16e-418a-bfdc-7c2b7bf379a5`._
