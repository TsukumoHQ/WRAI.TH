# [relay/settings] declare the obligations/answer/budget/knowledge settings in settingSpecs so the REST settings PUT accepts them

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/settings-spec (from main)
## Relay task : 00734b64-6a89-414f-ac98-75ee1c9d7e5e
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 PUT /api/settings accepts each new key with a valid value and persists it (read back equal); an out-of-range duration and class_budget_mode=bogus are refused with 400 naming the key. Test TestSettingsSpecNewNormKeys.
- [ ] 2. AC2 clamps in the spec equal the clamps the readers apply (e.g. ack_manager_age 1m..48h, answer_reply_age 1m..24h, answer_role_age 1m..48h); a test asserts each pair so they cannot drift. Test TestSettingsSpecClampsMatchReaders.
- [ ] 3. AC3 GET settings metadata lists the new keys in the operational group; existing settings_spec tests stay green.

## 2. Root cause & decisions

ROOT_CAUSE: writableKeys() derives from settingSpecs, and none of the settings added since v1.21 had a spec. So PUT /api/settings returned 403 for ack_manager_age, ack_human_age, answer_reply_age, answer_role_age, class_budget_mode, attribution_share and knowledge_min_compaction_lag, and the only way to tune them in prod was hand-edited SQL.

DECISION:
- One settingSpec per key, group operational, with bounds and defaults equal to what the readers apply:
  - ack_* : the evaluateObligations clamps and the ACKManagerAge/ACKHumanAge constants.
  - answer_* : the norms row the obligations engine reads (deadline_default_s/min_s/max_s).
  - class_budget_mode: the db enum.
- TestSettingsSpecClampsMatchReaders reads the norms rows and constants, so the spec cannot drift from them.
- attribution_share is kindString with a per-key (0,1] rule, the same test the reader applies. Adding a float kind would change the frozen GET contract (trovex 6f73f179) that the v2 panel renders.
- budget_epoch is declared READ-ONLY: listed in GET, PUT is refused with 403. It is stamped by migration, and moving it would re-count history toward class budgets.
- knowledge_min_compaction_lag (7d..365d, default 30d) matches compactKnowledgeLogs in knowledge S2 (1edb1377, in the gate). Its reader is not on main yet, so the drift test pins the literal values with a pointer to the reader.
- +1 file, internal/web/static/v2/settings.js (7 LABELS lines). TestConsoleV2ConfigPanel requires a label for every editable writable key, and the goal names the v2 panel. cto was informed.

REJECTED: a float kind (contract change); a writable budget_epoch.

## review-wraith verdict: SHIP
Scope: internal/relay/settings_spec.go, internal/relay/settings_spec_test.go, internal/web/static/v2/settings.js
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 -race OK (1121 passed, 12 packages; relay re-run green after rebase on 55e546c)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- knowledge_min_compaction_lag and attribution_share are pinned by literal values, because their readers are outside this package or not yet merged. A shared constant would close that once knowledge S2 lands.

## 3. Files changed

```
internal/relay/settings_spec.go      |  23 ++++++-
 internal/relay/settings_spec_test.go | 128 ++++++++++++++++++++++++++++++++++-
 internal/web/static/v2/settings.js   |   7 ++
 3 files changed, 154 insertions(+), 4 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `00734b64-6a89-414f-ac98-75ee1c9d7e5e`._
