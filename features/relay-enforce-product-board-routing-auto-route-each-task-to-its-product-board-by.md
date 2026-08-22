# [relay] Enforce product-board routing: auto-route each task to its product board by profile, + one-time backfill of mis-boarded tasks

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/product-board-routing (from main)
## Relay task : c254c55d-3e63-4ae0-96f8-c86d5061f0f9
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. dispatch auto-assigns board_id from the profile->product mapping when board_id is omitted: profile prefix yoru-* -> Yoru board, wraith-* -> Wraith board, trovex-* -> Trovex board, the niwa leads (backend-lead/backend-lead-2/gate-lead/frontend-lead/cli-lead/niwa-cto) -> Niwa board; unknown -> Backlog
- [ ] 2. an explicit board_id still wins (override)
- [ ] 3. mapping is config-driven or clearly centralized, not hardcoded scattered
- [ ] 4. one-time backfill: move all EXISTING active (non-done) tsukumo tasks currently on the wrong board to their correct product board using the same mapping; report counts moved per board
- [ ] 5. unit test: a yoru-frontend dispatch with no board_id lands on Yoru board; a niwa lead dispatch lands on Niwa; explicit board_id respected; build + go test -tags fts5 green

## 2. Root cause & decisions

ROOT_CAUSE: DispatchTask's board-resolution block (internal/db/tasks.go), since a0de115 (board-explicit-or-refuse), refuses any dispatch that omits board_id on a project with more than one board — correct for safety, but too blunt: on tsukumo, every wraith/yoru/niwa/trovex dispatch has to name its board explicitly or it's rejected, and there was no migration path for the ~dozen tasks already mis-filed onto the wrong board from before that fix landed.

DECISION: Added a single centralized mapping, ProductBoardSlugForProfile (internal/db/board_routing.go), consulted at the same DispatchTask choke right before the ambiguous-refuse branch fires: a profile matching a known product prefix (wraith-*/yoru-*/trovex-*/niwa-* or the fixed niwa-lead name set) auto-routes to that product's board by slug; anything else falls back to the "backlog" board slug if the project has one. An explicit board_id still always wins — the auto-router only fires when board_id is omitted AND the board set is ambiguous (>1 board), so the existing 0-board and 1-board paths are untouched. Paired with a boot-time, settings-marker-guarded one-shot backfill (runProductBoardRoutingBackfill / backfillProductBoardRouting) that moves existing ACTIVE (non-done/cancelled) tasks onto their mapped board across all projects, using the exact same mapping function so the live-routing rule and the historical cleanup can never drift apart, and logs per-board-slug counts moved.

REJECTED_ALTERNATIVES:
- A per-project JSON config file for the profile->board mapping: rejected as over-engineering for a 5-board, 4-product convention that's currently uniform across projects; the mapping function is already the single centralized definition the ticket asked for, and a config file would need its own load/validate/migrate path for no real flexibility gained today.
- Doing the backfill as an ad-hoc one-off script run once by hand against the production DB: rejected — no audit trail, not idempotent/safe on redeploy, and every other historical backfill in this codebase (backfillTaskProfileSlugs, migratePurgeDefaultProject) already uses the settings-marker boot-migration pattern, so following it keeps one convention instead of two.
- Silently defaulting an unmapped profile straight to BoardRequiredError (keep today's refusal for anything not in the explicit product list): rejected per the ticket's own acceptance criteria ("unknown -> Backlog"), and it's a strictly friendlier fallback than a hard refusal for the fleet's many small/experimental profiles that don't fit a product prefix.

[LEGACY_OPPORTUNITY]: internal/relay/dispatch_board_priority_test.go's existing multi-board refusal tests use profile "dev" against trovex/wraith-only board sets (no "backlog" board present) — they keep passing unchanged because "dev" maps to the backlog fallback, which doesn't exist in those fixtures. Worth a follow-up note to whoever touches that suite next: adding a "backlog" board to those fixtures would now make "dev" auto-route instead of refuse, which is correct new behavior but would look like a broken test if someone doesn't know why.

## review-wraith verdict: SHIP
Scope: internal/db/board_routing.go (new), internal/db/board_routing_test.go (new), internal/db/tasks.go (DispatchTask board-resolution block), internal/db/db.go (boot-time backfill wiring). db-package only, no schema/ALTER, no new HTTP/MCP surface, no messaging/auth/ingest/SSE touched.
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (601 passed, 12 packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/relay/dispatch_board_priority_test.go's "dev"-profile refusal fixtures have no "backlog" board, so they still exercise the refuse path post-change — see [LEGACY_OPPORTUNITY] above; no action needed now, flagged for whoever touches that file next.

## 3. Files changed

```
features/DEBT.md                                   |   2 +
 ...auto-route-each-task-to-its-product-board-by.md |  53 +++++++
 internal/db/board_routing.go                       | 125 +++++++++++++++++
 internal/db/board_routing_test.go                  | 156 +++++++++++++++++++++
 internal/db/db.go                                  |   5 +
 internal/db/tasks.go                               |  14 +-
 6 files changed, 354 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c254c55d-3e63-4ae0-96f8-c86d5061f0f9`._
