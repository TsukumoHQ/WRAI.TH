# [wraith/relay][split] Bound delivery_status rows: limit arg + clamp (default 50)

## Team : wraith-backend (tsukumo)
## Branch : wraith/deliverystatus-limit-clamp (from main)
## Relay task : e42ddc0f-ca43-4c31-a916-663e510481bd
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. DeliveryStatus takes a limit; SQL carries LIMIT bound to it; deliveries.go:255 unbounded query gone
- [ ] 2. handler clamps: absent -> 50 (clampLimit), >max -> max, <=0 -> refused or floored (stated in code + test)
- [ ] 3. delivery_status tool schema exposes limit with default documented
- [ ] 4. NAMED tests for BOTH paths: default-clamp (no limit given, >50 rows seeded, exactly 50 returned) and explicit-limit (limit=N, exactly N returned)
- [ ] 5. go test -tags fts5 ./internal/db/... ./internal/relay/... green in worktree

## 2. Root cause & decisions

ROOT_CAUSE: DB.DeliveryStatus (internal/db/deliveries.go:255) ran its message_id/agent-filtered SELECT with no LIMIT clause, so a busy message or a high-traffic recipient agent returned every matching delivery row unbounded — the same unbounded-read shape as the deadletter/list_tasks endpoints had before they were clamped.

DECISION: mirror the existing Deadletter(project, agent, limit) pattern exactly — add a `limit int` param to DeliveryStatus, floor `<=0` to 50 inside the DB function, append `LIMIT ?` to the query. In the handler, parse the caller's `limit` via the existing `clampLimit(req.GetInt("limit", 50))` helper (already used by ack_delivery/deadletter/list_tasks/etc — no new helper introduced). Exposed `limit` on the `delivery_status` MCP tool schema with the same "default 50" description text as `deadletter`.

REJECTED ALTERNATIVES:
- A DB-side-only default with no handler clamp: rejected because every other bounded list endpoint in this codebase clamps at the handler layer (upper bound via clampLimit/maxToolLimit=200) in addition to the DB-layer floor; skipping that would make delivery_status the only inconsistent read path and would let a caller request an effectively unbounded limit (e.g. limit=999999) that clampLimit exists specifically to prevent.
- A new dedicated clamp helper for this one call site: rejected as needless duplication — clampLimit already exists and is the established pattern for exactly this shape (see handlers_messaging.go:361,553,753; handlers_tasks.go:1311; handlers_memory.go:182,260; handlers_context.go:40; handlers_conversations.go:77).

No LEGACY_OPPORTUNITY items surfaced — this is a narrow, isolated bounded-read fix following an existing in-repo pattern, not a refactor opportunity.

## review-wraith verdict: SHIP
Scope: internal/db/deliveries.go (DeliveryStatus limit param + LIMIT clause), internal/db/deliveries_t4_test.go (existing calls updated + 2 new named tests), internal/relay/handlers_messaging.go (HandleDeliveryStatus clampLimit wiring), internal/relay/tools.go (delivery_status tool schema limit param).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt clean / test -tags fts5 OK (695 passed, 12 packages, full repo)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none — read-only path, no schema/writer/TOCTOU/inbox-invariant surface touched; change is an exact mirror of the existing Deadletter(project, agent, limit) + clampLimit pattern already used by 7 other handlers.

## 3. Files changed

```
internal/db/deliveries.go            | 11 ++++++--
 internal/db/deliveries_t4_test.go    | 54 +++++++++++++++++++++++++++++++++---
 internal/relay/handlers_messaging.go |  3 +-
 internal/relay/tools.go              |  1 +
 4 files changed, 61 insertions(+), 8 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `e42ddc0f-ca43-4c31-a916-663e510481bd`._
