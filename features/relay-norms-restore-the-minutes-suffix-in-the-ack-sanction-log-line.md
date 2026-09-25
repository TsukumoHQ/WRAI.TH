# [relay/norms] restore the minutes suffix in the ACK sanction log line

## Team : wraith-engine (tsukumo)
## Branch : wraith/ack-sanction-minutes (from main)
## Relay task : 904e024d-3cbb-4e92-8334-8809d68affd5
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. The 'ACK <norm>: task ... -> target' log line emitted by the sweeper ends with the task age as '<N>min', identical in format to the pre-obligations line.
- [ ] 2. A test captures the log line for one notify and one escalate sanction and asserts the '<N>min' suffix.
- [ ] 3. No other behaviour changes: TestACKEquivalence and the chain tests stay green; go vet clean; go test -tags fts5 -race ./internal/relay/... green.

## 2. Root cause & decisions

ROOT_CAUSE: 79f48b9e extracted sendAckSanction from evaluateObligations but the helper never received the task age, so its log line lost the legacy '— %dmin' suffix (oracle: legacyCheckUnackedTasks in obligations_equivalence_test.go).
DECISION: pass the minutes already computed for the sanction text into sendAckSanction and append ' — %dmin' to 'ACK <norm>: task <id> (<title>) -> <target>'. Both call sites (sweeper, obligation_decline) pass their minutes. Regression test TestACKSanctionLogCarriesMinutes (notify + escalate).
REJECTED: storing minutes on ackRungSanction — widens a routing struct for a log-only field; recomputing age inside sendAckSanction — duplicates the DispatchedAt parse and could drift from the text's value.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/relay/cleanup.go (sendAckSanction signature + log line), internal/relay/handlers_obligations.go (decline call site), internal/relay/obligations_equivalence_test.go (new test). Log-only; no DB/schema/transition/delivery change.
Gate: build -tags fts5 OK / vet OK / gofmt OK (touched files) / test -tags fts5 -race ./internal/relay/... OK (586 passed; new test fails on main)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/referential_integrity_test.go — gofmt drift already on main, untouched here; separate chore.

## 3. Files changed

```
internal/relay/cleanup.go                      |  9 +++++----
 internal/relay/handlers_obligations.go         |  2 +-
 internal/relay/obligations_equivalence_test.go | 18 ++++++++++++++++++
 3 files changed, 24 insertions(+), 5 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `904e024d-3cbb-4e92-8334-8809d68affd5`._
