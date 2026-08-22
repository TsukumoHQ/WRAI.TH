# [relay] Dispatch guard: require a REAL human title (reject empty/placeholder/UUID-like) — extend typed-ticket enforce

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/dispatch-title-guard (from main)
## Relay task : dbdf5f32-16eb-44cd-8bc0-3c9cb6d7d6ae
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. dispatchCore/typed-ticket guard rejects a task where title is empty/whitespace or matches a UUID/placeholder pattern, with a clear error naming the fix
- [ ] 2. a normal human title passes unchanged; existing goal/AC/dod checks untouched
- [ ] 3. unit test: bare-UUID title rejected, real title accepted; build + go test -tags fts5 green

## 2. Root cause & decisions

ROOT_CAUSE: DEC-trovex-scribe-titles-1 — the founder-visible trovex browser showed a grid of unnavigable UUID/"IN-FLIGHT" cards. Root: dispatch_task never validated the human title, so an empty title, a bare UUID pasted as the title, or a filler placeholder ("untitled"/"TBD"/"n/a") sailed through db.DispatchTask unchecked. Every downstream consumer (scribe H1, board cards, trovex index) then falls back to the UUID or first prose line, producing the unfindable cards the founder flagged. The per-project typed-ticket enforce (b092a6be) already proved the single-choke pattern for this class of dispatch-time guard; title had no equivalent.

DECISION: Added InvalidTitleReason() (internal/db/tasks.go) as the single source of truth for what makes a title unusable — empty/whitespace, a bare UUID (regexp match), or a small case-insensitive placeholder set (untitled/todo/tbd/n-a/na/placeholder/test) — returning InvalidTitleError when it fires. Checked in DispatchTask immediately after ticket.Missing(), gated by the same ProjectRequiresTypedTicket(project) check the ticket guard uses, so free-form (non-enforced) projects keep today's permissive behavior and nothing regresses there. Wired the errors.As branch into both call paths that reach DispatchTask: dispatchCore (hoisted, so MCP dispatch_task rejects before any profile/board auto-create side effect) and apiDispatchTask (REST /tasks dispatch handler in internal/relay/api.go). Tests added in internal/db/title_guard_test.go: a table test over InvalidTitleReason's cases, plus TestDispatchTaskRejectsPlaceholderTitleOnEnforcedProject covering the enforced-project rejection and the free-form-project retrocompat path (placeholder titles still accepted where typed tickets aren't enforced).

REJECTED_ALTERNATIVES:
- Validating title only in the scribe/H1-rendering path (cosmetic fix at render time): rejected — per DEC-trovex-scribe-titles-1's mandate ("everything niwa can control+impose, it imposes"), the fix belongs at the dispatch choke so a bad title is never accepted onto the board in the first place, not patched over downstream.
- A broader NLP/heuristic "looks like a real title" check: rejected as over-engineering — the observed failure modes are exactly empty/UUID/placeholder-string, so a small explicit set covers the real cases without false-positiving on legitimate short titles.
- Enforcing the title guard on ALL projects unconditionally: rejected — would break existing free-form (non-typed-ticket) projects that never had a title convention; scoping it to ProjectRequiresTypedTicket(project) keeps it paired with the existing typed-ticket enforce boundary instead of introducing a second inconsistent enforcement surface.

## review-wraith verdict: SHIP
Scope: internal/db/tasks.go (InvalidTitleReason + InvalidTitleError, DispatchTask choke), internal/db/title_guard_test.go (new), internal/relay/handlers_tasks.go (dispatchCore hoist + errors.As branch), internal/relay/api.go (apiDispatchTask errors.As branch). db+relay package only, no schema/ALTER, no new HTTP/MCP surface (existing dispatch endpoints only), no messaging/auth/ingest/SSE touched.
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (599 passed, 12 packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
...human-title-reject-empty-placeholder-uuid-li.md | 51 ++++++++++++++
 internal/db/tasks.go                               | 59 ++++++++++++++++
 internal/db/title_guard_test.go                    | 78 ++++++++++++++++++++++
 internal/relay/api.go                              |  5 ++
 internal/relay/handlers_tasks.go                   |  7 ++
 5 files changed, 200 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `dbdf5f32-16eb-44cd-8bc0-3c9cb6d7d6ae`._
