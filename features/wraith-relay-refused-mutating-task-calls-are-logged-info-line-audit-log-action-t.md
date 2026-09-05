# [wraith/relay] refused mutating task calls are logged: INFO line + audit_log action='task.refused' (observability, zero schema change)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/task-refused-log (from main)
## Relay task : a1b07961-a6c9-4a7e-9190-38277aaa0335
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. httptest: complete_task with a wrong project -> NOT_FOUND response unchanged AND one INFO line 'task-call refused tool=complete_task as=<as> project=<p> task=<id> code=NOT_FOUND' AND one audit_log row action='task.refused' with actor/project/resource_id/reason
- [ ] 2. httptest: resume_task on an accepted (non-blocked) task -> refusal unchanged AND the same log line + audit row with the 'not blocked' code
- [ ] 3. A successful complete_task writes NO task.refused row (no false positives)
- [ ] 4. TestToolSchemaBudget unchanged; go test -tags fts5 ./... green; diff <=3 files; zero schema change

## 2. Root cause & decisions

ROOT_CAUSE: A refused mutating task call left NO trace anywhere on the relay. The motivating incident: a niwa daemon's block/resume/complete calls on two tasks were refused (NOT_FOUND because the caller sent the doer's project instead of the board's; "task is not blocked" on resume), yet relay-serve.log had zero tool-call lines, relay.analytics.db is token_usage only, and audit_log had no refusal rows — while the caller's own journal unreliably logged "applied" on a refusal. A refused mutation on a shared task is a coordination fault that MUST be checkable against the relay's own record, not the caller's word.

DECISION: Add the observability half only (the daemon-side fix that stops sending the wrong project is a separate niwa ticket). Hook the single existing choke — guardIdentity, which every mutating tool already flows through — to intercept next()'s result. When the tool is one of the 7 task-lifecycle verbs (claim/start/complete/block/resume/cancel/update) and the result IsError, emit one INFO line `task-call refused tool=%s as=%s project=%s task=%s code=%s` and write one best-effort audit_log row (action=task.refused, actor=<as>, project, resource_id=<task id>, reason=<code>+": "+<message>). The code/message are read back from the error envelope the caller actually received, so the log/audit never diverge from the response.

WHY THIS SEAM: guardIdentity is the one place all mutating task tools converge (stdio, HTTP /mcp, and discovery-mode call_tool all route through the wrapped registry), so one interception covers every path without scattering call-site edits across ~7 handlers. It runs AFTER next() returns, so it observes without touching refusal semantics or status codes.

ZERO SCHEMA CHANGE: reuses db.RecordAudit and the existing audit_log columns (same row shape as lease_transferred). No migration, no agentColumns/scanAgent touch, TestToolSchemaBudget untouched (no tool schema added).

REJECTED ALTERNATIVES:
- Log inside taskOpError (the shared DB-op error router): rejected — it is a pure function with no access to the DB handle, caller identity, project, or task id, and it does NOT cover the resume "not blocked" guard (a plain toolResultError, not a taskOpError path). The guardIdentity seam covers both.
- Give the resume "not blocked" refusal a proper typed code instead of its current INTERNAL classification: rejected as out of scope — the ticket forbids changing refusal semantics or status codes. This seam OBSERVES the miscoding rather than fixing it (see NON-BLOCKING NOTE); the value is precisely that the record now surfaces it.

SCOPING NOTE: guardIdentity's OWN pre-next refusals (unresolved project, archived project, unregistered/mismatched identity, sender-inactive) are a different, already-loud fault class and are intentionally NOT audited as task.refused — they return before next() and would carry an empty/ambiguous resource_id. The motivating incident (wrong project passed by a registered doer) passes identity and reaches next(), so it IS captured.

NON-BLOCKING NOTE ([LEGACY_OPPORTUNITY]): the resume "task is not blocked" refusal currently classifies to code=INTERNAL / isRetryable=true via classifyMessage — a latent miscoding (a deterministic not-blocked refusal is not transient/retryable). This observability change now makes that visible in the audit trail. Retyping that refusal to a non-retryable validation code is a separate follow-up (it changes the envelope a caller receives, which this ticket must not do).

## review-wraith verdict: SHIP
Scope: internal/relay/toolset.go (guardIdentity interception + taskRefusalAuditedTools + refusalCodeMessage + recordTaskRefusal), internal/relay/task_refused_audit_test.go (new). No db/schema/migration, no messaging/inbox, no updater/ingest/SSE touched.
Gate: build -tags fts5 OK / vet -tags fts5 OK (no issues) / gofmt OK (no drift) / test -tags fts5 ./... OK (777 passed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- The audit resource_id is the raw task_id the caller referenced; a prefix caller would store the prefix rather than the resolved full id. Acceptable — it records exactly what the caller sent.
- One audit write per refusal is on the error path (single-writer, best-effort, non-retryable codes = callers park), so no hot-path write amplification; audit_log retention (PurgeOldAuditLog) bounds growth.

## 3. Files changed

```
internal/relay/task_refused_audit_test.go | 168 ++++++++++++++++++++++++++++++
 internal/relay/toolset.go                 |  75 ++++++++++++-
 2 files changed, 242 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `a1b07961-a6c9-4a7e-9190-38277aaa0335`._
