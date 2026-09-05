# [wraith/relay][split] R2 field trim per DEC-wraith-r2-field-trim-1: summary field sets cut — full ≤6800 B / minimal ≤2700 B on the R1 fixture

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/r2-field-trim (from main)
## Relay task : a94df16b-d800-4164-99e2-ea101ccfb6af
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 message summaries carry NO delivery_id, NO timestamps, NO truncated flag, content_preview ≤160 chars — test asserts exact key set + preview length
- [ ] 2. AC2 assigned_to_me task summaries carry NO profile_slug/assigned_to/board_id/timestamps/truncated; dispatched_by_me summaries KEEP profile_slug + assigned_to; BOTH keep verify_cmd; desc_preview ≤200 — test asserts both key sets
- [ ] 3. AC3 R1 fixture (20 constraints, 60 decisions, 10 unread, 5 tasks): full ≤6800 B, minimal ≤2700 B — test asserts len(json), numbers cited in report
- [ ] 4. AC4 escape hatches unchanged: get_inbox(full_content), get_message, get_task, ack_delivery tests still green with full fields
- [ ] 5. AC5 go build ./..., go vet ./..., go test -tags fts5 ./... green (count cited); TestToolSchemaBudget green; ≤5 non-test source files

## 2. Root cause & decisions

# .niwa-decision — a94df16b R2 session_context field trim

ROOT_CAUSE: R1 (merged 6ba58f5) cut boot to full 7477 B / minimal 3365 B on the R1 fixture, but the residual floor was the two per-row summary field sets — measured 209 B/unread-message and 239 B/task, much of it fixed key tax (uuids, timestamps, structural keys) that the booting agent does not act on.

DECISION (per DEC-wraith-r2-field-trim-1, ruling on design doc trovex 452684788e494f349546fe54a17e8e9d — all recommendations accepted verbatim):
- MessageSummary: DROP created_at (positional recency — projectMessages sorts P0-first then created_at DESC), reply_to (thread via get_thread), delivery_id (ack_delivery resolves from message_id, handlers_messaging.go:500-503), content_truncated flag. Lower content_preview cap 300 -> 160.
- TaskSummary: DROP board_id (via get_task), dispatched_at (positional recency), desc_truncated flag. profile_slug + assigned_to ASYMMETRIC — cleared on assigned_to_me (summarizeTask selfList=true, == the booting agent), kept on dispatched_by_me. verify_cmd KEPT (hard contract DEC-niwa-goal-validate-1).
- Mechanics: projectTasks gains variadic selfList; handlers.go assigned_to_me passes true; message/task byte estimators updated (structural overhead 180->120 / 160->110). Zero schema change; escape hatches (get_inbox full_content, get_message, get_task, ack_delivery) untouched.

MEASURED BYTES (R1 fixture, marshaled JSON): full 7477 -> 6657 B (<=6800), minimal 3365 -> 2545 B (<=2700). go build/vet/test -tags fts5 ./... all green; TestToolSchemaBudget green (register_agent unchanged).

## review-wraith verdict: SHIP
Scope: internal/relay/project.go, handlers.go (+ project_test.go, handlers_test.go, verify_cmd_test.go)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK
BLOCKERS: none
NITS: P0 unread boot preview now capped at 160 B (was 300); full P0 body still delivered untruncated via get_inbox (session_context is a preview, not the first surfacing — invariant intact).

## 3. Files changed

```
internal/relay/handlers.go        |   4 +-
 internal/relay/handlers_test.go   |  73 +++++++++++++++++++++++
 internal/relay/project.go         | 119 ++++++++++++++++++++------------------
 internal/relay/project_test.go    | 116 ++++++++++++++++++++++++++++++++-----
 internal/relay/verify_cmd_test.go |  16 ++++-
 5 files changed, 255 insertions(+), 73 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `a94df16b-d800-4164-99e2-ea101ccfb6af`._
