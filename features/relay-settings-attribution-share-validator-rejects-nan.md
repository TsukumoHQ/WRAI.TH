# [relay/settings] attribution_share validator rejects NaN

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/settings-nan (from main)
## Relay task : 7aad5bad-2a40-4f95-b9c4-1c27686eb589
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 PUT /api/settings attribution_share=NaN, 0, -0.1, 1.5 and Inf are each refused with 400 naming the key; 0.6 and 1 are accepted. Test case 'NaN' added to the existing attribution_share table test.
- [ ] 2. AC2 no other validator changes; existing settings_spec tests stay green.

## 2. Root cause & decisions

ROOT_CAUSE: the attribution_share rule was written as `v <= 0 || v > 1`. Every comparison with NaN is false, so "NaN" passed validation, while the reader (db/class_budgets.go, `v > 0 && v <= 1`) silently falls back to the default. The validator and the reader disagreed on that edge.

DECISION: write the validator as the reader's accept test negated, `!(v > 0 && v <= 1)`, so the two can only disagree if the reader changes (TestSettingsSpecClampsMatchReaders still pins 0/1). Tests: NaN, -0.1 and Inf are refused with 400 naming the key; 0.6 and 1 are accepted. No other validator is touched.

## review-wraith verdict: SHIP
Scope: internal/relay/settings_spec.go, internal/relay/settings_spec_test.go
Gate: vet OK / gofmt OK / test -count=1 -tags fts5 -race ./internal/relay/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
internal/relay/settings_spec.go      | 4 +++-
 internal/relay/settings_spec_test.go | 9 ++++++++-
 2 files changed, 11 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `7aad5bad-2a40-4f95-b9c4-1c27686eb589`._
