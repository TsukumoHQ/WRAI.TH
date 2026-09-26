# [relay/settings] declare the coherence settings in settingSpecs so cto can flip coherence_mode by API

## Team : wraith-engine (tsukumo)
## Branch : wraith/coherence-settings (from main)
## Relay task : a6f347ec-92c2-4308-98c4-6297b9178424
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 PUT /api/settings accepts coherence_mode=advisory|enforce|off and each age within its clamp, persisted and read back; coherence_mode=bogus and out-of-range ages refused with 400 naming the key. Test TestSettingsSpecCoherenceKeys.
- [ ] 2. AC2 clamps in the spec equal the clamps coherence.go applies (test asserts each pair).
- [ ] 3. AC3 v2 config panel lists the keys (TestConsoleV2ConfigPanel green); existing settings tests green.

## 2. Root cause & decisions

ROOT_CAUSE: coherence T1 (7d5683f) reads coherence_mode and three reassess ages but never declared them in settingSpecs, so the settings API refused them (403, not in the allowlist) and cto-tsukumo could not flip a mode it owns (ruling b3a6ab43).
DECISION: declare coherence_mode (enum off|advisory|enforce, default advisory) and coherence_reassess_age / coherence_reassess_breaking_age / coherence_role_age (15m..72h; 24h / 4h / 24h) as writable Operational keys; coherence_cursor_rev listed read-only (internal CAS cursor, 403 on PUT). Clamps pinned against the norms rows (reassess, role) and the rolloutDeadline reader (breaking); v2 panel LABELS added.
REJECTED: leaving coherence_cursor_rev out entirely (listing it read-only makes the sweeper position visible for ops without making it writable); adding a CoherenceModeEnforce const in db/coherence.go (outside this ticket's files; T2 owns the fence that reads it).

## review-wraith verdict: SHIP
Scope: internal/relay/settings_spec.go, internal/relay/settings_spec_test.go, internal/web/static/v2/settings.js
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/relay/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- "enforce" is a literal in the spec and the clamp test until T2 adds the db constant.

## 3. Files changed

```
internal/relay/settings_spec.go      | 12 ++++++-
 internal/relay/settings_spec_test.go | 69 +++++++++++++++++++++++++++++++++---
 internal/web/static/v2/settings.js   |  4 +++
 3 files changed, 80 insertions(+), 5 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `a6f347ec-92c2-4308-98c4-6297b9178424`._
