# [relay/errors] resume_task on a task that is not blocked returns a non-retryable INVALID_ARGUMENT, not INTERNAL

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/resume-invalid (from main)
## Relay task : 45142210-5930-4161-af6c-d5d717d28df9
## Trace : trace=2fe2c8f0eedcbf348f09397ca01c1ed6
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 resume_task on an in-progress task returns code INVALID_ARGUMENT, category validation, retryable false, and the message names status=in-progress (test in handlers_tasks_test.go)
- [ ] 2. AC2 resume_task on a blocked task still moves it to in-progress (existing or new test)
- [ ] 3. AC3 classifyMessage of a message containing '(status=done)' returns INVALID_ARGUMENT / validation / false (table case in errors_test.go)
- [ ] 4. AC4 TestToolSchemaBudget byte total is unchanged vs origin/main

## 2. Root cause & decisions

# [relay/errors] resume_task on a task that is not blocked returns a non-retryable INVALID_ARGUMENT, not INTERNAL

Task: 45142210-5930-4161-af6c-d5d717d28df9

ROOT_CAUSE: HandleResumeTask (internal/relay/handlers_tasks.go:565) refused a task that was not blocked through toolResultError("task is not blocked (status=X)"). None of classifyMessage's keywords (errors.go) match that text, so it fell to the default INTERNAL / transient / retryable=true. The gate calls resume_task at qa-submit and retries INTERNAL, which likely produced the duplicate "submitted" rounds seen on d17673fc and 9c2d5b47.

DECISION:
- The resume site now returns validationError(CodeInvalidArgument, ...), following handlers_tasks.go:125. The message text is unchanged and still names the current status.
- classifyMessage adds "(status=" to the existing lost-race / state-conflict list ("conflict", "status changed", "changed from"). Other prose refusals worded the same way stop defaulting to INTERNAL.
- Blast radius, checked with git grep over internal/*.go: the only other "(status=" string is the success text of archive_tasks, which never goes through classifyMessage. No db error embeds it.
- Resuming a blocked task is unchanged.
- No tool-schema change: TestToolSchemaBudget passes with the same byte total, since no tools.go edit was made.
- The existing TestTaskRefusal_ResumeNotBlocked_LoggedAndAudited still passes. Its log line and audit row carry whatever code the caller received, which is now INVALID_ARGUMENT.

TESTS:
- AC1 and AC2: TestResumeTaskNotBlockedIsInvalidArgument (handlers_tasks_test.go). Resuming an in-progress task gives INVALID_ARGUMENT / validation / isRetryable=false with a message naming status=in-progress. After block_task, resume moves the task to in-progress.
- AC3: a TestClassifyMessageTaxonomy table case, "task is not blocked (status=done)", gives INVALID_ARGUMENT / validation / false.
- AC4: TestToolSchemaBudget passes and tools.go is untouched.

RED_EVIDENCE:
  cmd: go test -count=1 -tags fts5 -run 'TestResumeTaskNotBlockedIsInvalidArgument|TestClassifyMessageTaxonomy' ./internal/relay/   (on db2498b: tests only, code at origin/main b92d2ed)
  test_sha: db2498b
  output: |
         errors_test.go:69: classify("task is not blocked (status=done)") = (INTERNAL,transient,true), wan...
         handlers_tasks_test.go:366: resume of an in-progress task = map[code:INTERNAL errorCategory:trans...

## review-wraith verdict: SHIP
Scope: internal/relay/handlers_tasks.go (1 line), internal/relay/errors.go (1 keyword), plus the tests in errors_test.go and handlers_tasks_test.go.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 -race ./internal/db/... ./internal/relay/... OK (1221 passed)
Checklist:
- No schema, migration, DB write, lock, tool-schema or middleware change.
- The inbox and deliveries are untouched.
- The error envelope keeps the same shape; only the code, category and retryable fields of this one refusal change, and they now match its real semantics.

BLOCKERS: none
NITS:
- Any future transient DB error whose text happens to contain "(status=" would now classify as non-retryable. There is none today; typed constructors remain the rule for known failure modes.

## 3. Files changed

```
internal/relay/errors.go              |  2 +-
 internal/relay/errors_test.go         |  1 +
 internal/relay/handlers_tasks.go      |  2 +-
 internal/relay/handlers_tasks_test.go | 26 ++++++++++++++++++++++++++
 4 files changed, 29 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `45142210-5930-4161-af6c-d5d717d28df9`._
