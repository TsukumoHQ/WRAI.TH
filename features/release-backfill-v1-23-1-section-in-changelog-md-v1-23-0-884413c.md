# [release] backfill v1.23.1 section in CHANGELOG.md (v1.23.0..884413c)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/release-1.23.1 (from main)
## Relay task : 8bb924a2-bd79-4e59-a929-83849caecafc
## Trace : trace=6c9c4769685b42bae7554064d4ed1f75
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 CHANGELOG.md has exactly one '## [1.23.1] — 2026-09-27' heading, placed directly above '## [1.23.0]'
- [ ] 2. AC2 the section references b92d2ed, 59728c7 and 884413c with task:545a11d2, task:45142210 and task:5b16f32d
- [ ] 3. AC3 the section's upgrade notes name the compiled_guards.expiry_noticed_for column
- [ ] 4. AC4 git diff origin/main touches CHANGELOG.md only

## 2. Root cause & decisions

# [release] backfill v1.23.1 section in CHANGELOG.md (v1.23.0..884413c)

Task: 8bb924a2-bd79-4e59-a929-83849caecafc

ROOT_CAUSE: v1.23.1 was tagged on 884413c during the load hold, with its notes only in the tag message and the GitHub release (cto-tsukumo ok f9ce45d0). CHANGELOG.md stopped at 1.23.0.

DECISION:
- One `## [1.23.1] — 2026-09-27` section directly above `## [1.23.0]`, following 42eb2bb: a lead paragraph, Added, Fixed, Upgrade notes and a Full diff link.
- The text comes from `gh release view v1.23.1` with the Highlights list and the "follows in a later commit" line dropped, per the ticket.
- The three commits in v1.23.0..884413c are each referenced once:
  - (task:545a11d2, b92d2ed)
  - (task:5b16f32d, 884413c)
  - (task:45142210, 59728c7)
- Upgrade notes name `compiled_guards.expiry_noticed_for`.
- No other section is touched.

VERIFY:
- `grep -c '^## \[1.23.1\] — 2026-09-27' CHANGELOG.md` prints 1, on line 6, above [1.23.0] on line 23.
- `go build -tags fts5 ./...` passes.
- `git diff --stat origin/main` shows CHANGELOG.md only (+17).

## review-wraith verdict: SHIP
Scope: CHANGELOG.md only. No code, schema, tool-schema, migration, handler or updater change.
Gate: build -tags fts5 OK / vet N/A / gofmt N/A / test N/A (docs-only diff, no Go touched)

BLOCKERS: none
NITS: none

## 3. Files changed

```
CHANGELOG.md | 17 +++++++++++++++++
 1 file changed, 17 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `8bb924a2-bd79-4e59-a929-83849caecafc`._
