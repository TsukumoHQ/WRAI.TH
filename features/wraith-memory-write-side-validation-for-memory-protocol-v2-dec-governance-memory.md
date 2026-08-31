# [wraith/memory] Write-side validation for Memory Protocol v2 (DEC-governance-memory-protocol-2)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/memory-protocol-v2-writeguard (from main)
## Relay task : 4e02d199-99bc-4f77-bbe2-278a47bc595e
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. set_memory rejects (clear error + hint) keys matching checkpoint/resume patterns (*checkpoint*, *resume*, *-2026*-dated keys) — checkpoints belong in resume files/trovex
- [ ] 2. set_memory with layer=context requires valid_until (≤7 days ahead); missing/too-far = rejected with hint
- [ ] 3. set_memory requires ≥1 tag; empty tags rejected with hint listing the lane+topic convention
- [ ] 4. value length: warn-annotate above 600 chars (accept but flag), so rule-first ledes stay the norm
- [ ] 5. remember() rejects area='general' with hint to use a real area
- [ ] 6. existing memories untouched (validation is write-path only); all existing tests still green -tags fts5

## 2. Root cause & decisions

ROOT_CAUSE: relay memory quality depended entirely on agent discipline — a founder-mission audit (2026-08-22) found ~17 never-expiring agent checkpoints stored as durable memories, fixed gotchas left live, duplicates, empty tags, and narrative ledes killed by the 300-char search truncation. Group CTO ran a manual janitor pass (16 archived) and graved Memory Protocol v2 (DEC-governance-memory-protocol-2), but nothing at the relay actually enforced the protocol's rules on write — so the same class of junk could reaccumulate immediately after any cleanup.

DECISION: enforce Memory Protocol v2 deterministically at the relay control plane (same philosophy as DEC-relay-comms-discipline-1), opt-in + debrayable per project (DEC-governance-enforcement-1) rather than imposed in-code universally — the relay serves many free-form projects that never agreed to this discipline. New `require_memory_discipline` column on `projects` (additive, `ensureColumns`, default 0), seeded ON for niwa+tsukumo via the same one-shot marker pattern as `require_typed_ticket` (`internal/db/db.go`) so an operator's later opt-out survives a restart. Pure, independently-testable validation functions in new `internal/db/memory_discipline.go` (`ValidateMemoryKey`, `ValidateMemoryTags`, `ValidateMemoryValidUntil`, `ValidateRememberArea`, `MemoryValueWarning`, and the `ValidateMemoryWrite` combinator), gated behind `ProjectRequiresMemoryDiscipline(project)` at both write surfaces: the MCP `set_memory`/`remember` handlers (`internal/relay/handlers_memory.go`) and the REST `/api/memories` POST handler (`internal/relay/api.go`) — mirroring how `InvalidTitleReason` is one shared rule checked at two call sites in the existing typed-ticket code.

Rules enforced (DEC-governance-memory-protocol-2 R1/R3/R4/R5/R6): a checkpoint/resume/dated-looking key is rejected (checkpoints belong in resume files or trovex); `layer=context` requires `valid_until` ≤7 days out; at least 1 tag is required; a value over 600 chars is accepted but flagged in the response (`warning` field), never blocked, so a legitimately longer write still lands; `remember()` rejects `area="general"`. Every rejection carries a hint pointing at the fix, never a bare refusal.

REJECTED ALTERNATIVE: changing `SetMemory`'s signature to thread `valid_until` all the way through the DB-layer write itself (the strictest "single choke" reading of the pattern). Rejected as disproportionate churn for a P2 governance ticket — 13 call sites across 5 test files would need mechanical edits for no behavioral gain, since the two real write surfaces (MCP handler, REST handler) already fully gate every rule before calling `SetMemory`/`RememberDecision`, and neither of `SetMemory`'s two other internal callers (`RememberDecision`, the REST handler) can independently reach an ungated write.

[LEGACY_OPPORTUNITY]: the REST `/api/memories` POST body has no `valid_until` field at all (only the MCP tool got that in the T5 work) — so on an opted-in project, a `layer=context` write via REST is always rejected today (missing valid_until has no way to be supplied). Not a regression (REST never supported layer=context validity before either), but a candidate follow-up if the REST surface ever needs `layer=context` write support: add `valid_until`/`valid_from` fields to the REST body struct.

## review-agent-runtime checklist (fleet-backbone gate) — result: no blockers
Scope: internal/db/db.go (new `require_memory_discipline` column via `ensureColumns`, one-shot boot seed for niwa+tsukumo mirroring `require_typed_ticket`), internal/db/projects.go (`ProjectRequiresMemoryDiscipline`/`SetProjectRequiresMemoryDiscipline` getter/setter, RO reads only), internal/db/memory_discipline.go (new, pure validation functions — no DB access at all), internal/relay/handlers_memory.go (`HandleSetMemory`/`HandleRemember` gate on `ProjectRequiresMemoryDiscipline` before the write, valid_from/valid_until reads reordered earlier — no behavior change to the values themselves), internal/relay/api.go (REST `apiPostMemory` gated the same way), 3 new test files (10 new tests: 8 pure unit + 2 boot-seed regression).

1. Schema/scan lockstep: `agentColumns`/`scanAgent` untouched (this touches `projects`, not `agents`). New column added via `ensureColumns` (additive, idempotent) with its own dedicated getter — not widened into any existing shared column list. `ProjectRequiresMemoryDiscipline` returns `false` on a missing row/column (same defensive pattern as `ProjectRequiresTypedTicket`), so an older DB or an unknown project never errors.
2. Concurrency: no task/lease transition touched. `SetMemory`'s own write path (the `beginWriterTx` critical section) is completely unchanged — validation runs strictly BEFORE it's called, as a pure read+compute step, so no new race surface.
3. Single-writer/availability: no new writer opened. The only new DB call on the read path (`ProjectRequiresMemoryDiscipline`) goes through `d.ro()`, not the writer pool, and is a single indexed-by-name SELECT — cheap, no hot-path write added. Reads handle the error/missing-row case without panicking (return `false`).
4. Boundary: not touched.
5. MCP/API surface: no new tool registered — `set_memory`/`remember`'s existing schemas (`toolset.go`/`tools.go`) are unchanged; validation reuses the already-resolved `project`/`agent` from `resolveProject`/`resolveAgent`, no bypass. REST handler behavior changes only for opted-in projects (niwa/tsukumo today) — additive for everyone else.
6. Process lifecycle: not touched.

BLOCKERS: none.

NITS:
- See [LEGACY_OPPORTUNITY] above: REST `/api/memories` has no `valid_until` field, so `layer=context` writes are unconditionally rejected there on an opted-in project. Pre-existing gap in REST feature parity with the MCP tool, not introduced by this change; flagged as a follow-up candidate, not blocking.
- `checkpointKeyPattern`'s date-fragment match (`20\d{2}-\d{2}(-\d{2})?`) is deliberately broad — it will also flag a legitimate key that happens to contain a year-month fragment for an unrelated reason (e.g. a version string). Acceptable false-positive rate for a governance nudge with a clear hint in the error, and only active on two opted-in projects.

## review-wraith verdict: SHIP
Scope: internal/db/db.go, internal/db/projects.go, internal/db/memory_discipline.go (new), internal/relay/handlers_memory.go, internal/relay/api.go, internal/db/memory_discipline_test.go (new), internal/db/memory_discipline_seed_test.go (new), internal/relay/handlers_memory_discipline_test.go (new).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (625 passed, up from 613 baseline — 12 new tests: 6 pure unit + 2 boot-seed + 4 handler-integration)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- REST /api/memories has no valid_until field — see [LEGACY_OPPORTUNITY] above.
- checkpointKeyPattern's date-fragment match is broad by design (governance nudge, opt-in only) — see NITS above.

## 3. Files changed

```
internal/db/db.go                                 |  18 +++
 internal/db/memory_discipline.go                  | 128 ++++++++++++++++++
 internal/db/memory_discipline_seed_test.go        |  58 ++++++++
 internal/db/memory_discipline_test.go             | 154 ++++++++++++++++++++++
 internal/db/projects.go                           |  28 ++++
 internal/relay/api.go                             |  12 ++
 internal/relay/handlers_memory.go                 |  26 +++-
 internal/relay/handlers_memory_discipline_test.go | 122 +++++++++++++++++
 8 files changed, 544 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `4e02d199-99bc-4f77-bbe2-278a47bc595e`._
