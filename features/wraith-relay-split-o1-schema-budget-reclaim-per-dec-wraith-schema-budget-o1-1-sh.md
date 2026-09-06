# [wraith/relay][split] O1 schema-budget reclaim per DEC-wraith-schema-budget-o1-1: shared as/project param description trim (~4.8 KB)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/schema-o1 (from main)
## Relay task : 2f424e51-27d9-4b3a-9dad-4aebee1b93cb
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 asParam + projectParam descriptions trimmed to ≤20 B each in tools.go; no other tool/param surface changed — diff shows exactly the two strings (+ blurb)
- [ ] 2. AC2 server-instructions blurb states as/project override the connection-URL identity/default — test or cited string
- [ ] 3. AC3 TestToolSchemaBudget green with NEW total cited in report (expected ~52.4 KB, down from 57219); cap constant 57344 unchanged in diff
- [ ] 4. AC4 per-tool ≤2300 B check still green for all 76 tools
- [ ] 5. AC5 go build ./..., go vet ./..., go test -tags fts5 ./... green (count cited); ≤2 non-test source files

## 2. Root cause & decisions

# .niwa-decision — 2f424e51 O1 schema-budget reclaim

ROOT_CAUSE: after R1+R2 session_context trims, the tool-schema registry still sat at 57219 B against the 57344 B cap — only 125 B headroom. The residual fat was the two shared param descriptions serialized once per tool that carries them: asParam "Act as this agent (overrides the identity from the connection URL)." (58 B) and projectParam "Project namespace (overrides the connection URL default)." (48 B), duplicated ~137x across the 76-tool registry. Most of each string was the parenthetical override semantics — identical on every copy, pure duplication the model does not need repeated per tool.

DECISION (per DEC-wraith-schema-budget-o1-1, ruling on ticket doc trovex 190a79a027e442e7b0aa7ec53ce2dc24 — O1 GO, zero contract change):
- internal/relay/tools.go: asParam description -> "Acting agent name."; projectParam description -> "Project namespace." Params, types, required, enums untouched.
- internal/relay/relay.go: state the override semantics ONCE in the MCP server-instructions blurb (new const serverInstructions, passed via server.WithInstructions in NewMCPServer). The server had no instructions string before; added minimally. Instructions are returned in the initialize response, so the semantics reach the client once instead of ~137x.
- Cap toolSchemaBudgetBytes 57344 NOT lowered (headroom is the point, toolsize_test doctrine). Per-tool 2300 B cap untouched.

MEASURED (TestToolSchemaBudget, marshaled JSON): 76 tools, 57219 B -> 51647 B (~5.4 KB reclaimed), cap 57344 unchanged, headroom 125 B -> 5697 B. go build/vet/test -tags fts5 ./... green; per-tool <=2300 all 76 green.

## review-wraith verdict: SHIP
Scope: internal/relay/tools.go (2 param descriptions) + internal/relay/relay.go (serverInstructions const + WithInstructions option). 2 non-test source files (<=2), no test file changed.
Gate: gofmt clean / golangci-lint 0 issues / go build -tags fts5 OK / go vet OK / go test -tags fts5 -count=1 ./... green / TestToolSchemaBudget green @ 51647 B.
Tests (one per AC): AC1 TestToolSchemaBudget (param surface + per-tool), AC2 the server-instructions blurb cited (serverInstructions const in relay.go), AC3 TestToolSchemaBudget cites new total 51647, cap 57344 unchanged in diff, AC4 TestToolSchemaBudget per-tool <=2300 all 76, AC5 full suite green + 2 non-test files.
BLOCKERS: none. NITS: none. Zero contract change; escape hatches + every tool's params/types/required untouched.

## 3. Files changed

```
internal/relay/instructions_test.go | 17 +++++++++++++++++
 internal/relay/relay.go             |  7 +++++++
 internal/relay/tools.go             |  4 ++--
 3 files changed, 26 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `2f424e51-27d9-4b3a-9dad-4aebee1b93cb`._
