# [wraith/relay] S6a: route REST create-project

## Team : wraith-backend (tsukumo)
## Branch : wraith/create-project-rest (from main)
## Relay task : fd991da3-2c0e-4930-9b80-c77f0774c458
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. POST /api/projects crée un projet via le handler existant
- [ ] 2. les refus existants (nom vide, 'default') remontent en erreur standard avec le bon status (test)
- [ ] 3. projet dupliqué -> erreur standard, pas de silent-ok (test)
- [ ] 4. test succès + 2 cas erreur
- [ ] 5. go test -tags fts5 ./internal/relay/... vert

## 2. Root cause & decisions

ROOT_CAUSE: create_project existed only as an MCP tool (handlers_projects.go HandleCreateProject:10) with no REST route (audit b983684b §1). The v2 console can only call REST, so no project-create UI could exist.

DECISION: add POST /api/projects to ServeAPI (same CORS -> RateLimit -> BodyLimit -> Auth chain as sibling routes). The wrapper reuses the existing name primitives — NormalizeProject (folds _ to -) and validProjectName (already rejects "", the reserved "default", path-shaped, >64 chars) — plus DB GetProject (duplicate pre-check) and EnsureProject (create). Zero new validation logic.

DELIBERATE DEVIATION FROM TICKET WORDING: the ticket said "reuse the existing handler". HandleCreateProject is unusable for a REST create: it returns the interactive onboarding MEGA-PROMPT as plain text (meant to drive a setup agent), and it treats a pre-existing project as a SUCCESS ("already_configured"), which directly violates the AC "duplicate -> standard error, not silent-ok". So the wrapper reuses the underlying primitives the handler itself calls (validProjectName + EnsureProject) rather than the prompt-returning handler, and adds the GetProject duplicate pre-check the AC requires. Flagged to wraith-cto.

REJECTED ALTERNATIVES:
- Calling HandleCreateProject and translating its result: rejected — it returns a text prompt, not a create result, and cannot signal a duplicate as an error (returns already_configured=success), so it cannot satisfy the duplicate-error AC.
- Relying on EnsureProject's INSERT OR IGNORE to no-op a duplicate: rejected — that returns 200 for a re-create, a silent-ok the AC explicitly forbids; GetProject first gives a clean 409.

No LEGACY_OPPORTUNITY items surfaced — additive REST wiring over existing primitives.

## review-wraith verdict: SHIP
Scope: internal/relay/api.go (POST /api/projects route + apiCreateProject wrapper), internal/relay/api_create_project_test.go (new: create, normalize, 5 refusal cases, duplicate).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt clean / test -tags fts5 OK (702 passed, 12 packages, full repo)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none. Route sits behind the same middleware chain as every /api sibling (no auth bypass); reuses existing validation + DB ops (no new writer, no schema change, RO GetProject pre-check); duplicate -> 409, refusals -> 400, both with the standard {"error":...} envelope.

## 3. Files changed

```
internal/relay/api.go                     | 45 ++++++++++++++++
 internal/relay/api_create_project_test.go | 90 +++++++++++++++++++++++++++++++
 2 files changed, 135 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `fd991da3-2c0e-4930-9b80-c77f0774c458`._
