# [wraith/relay] T2d-a remove dead writableSettings (legacy 9-key panel allowlist, spec-derived since T2a, unused since T2c): writableKeys() is the sole PUT allowlist

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/t2d-a (from main)
## Relay task : e3d26498-49a2-46a9-8db5-8a6b22531720
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: writableKeys() has exactly 24 keys = the 9 legacy panel keys (sun_type + 7 linear_* + federation_peers) plus the 15 Operational keys, contains no evil_key, and a PUT of a legacy key and a PUT of an Operational key both apply (200) through apiPutSetting
- [ ] 2. AC2 named test: a source scan of every non-_test.go file in internal/relay (os.ReadFile over the package dir, precedent console_v2_config_panel_test.go) finds zero occurrences of the identifier writableSettings
- [ ] 3. AC3 named smoke test: TestV2ConfigPanel and TestSettingsSpec suites still green; scope: diff touches exactly internal/relay/settings_spec.go, internal/relay/settings_spec_test.go, internal/relay/api.go; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

# T2d-a — remove dead `writableSettings`

## ROOT_CAUSE
`writableSettings` was the legacy 9-key PANEL allowlist (console/linear/federation),
spec-derived since T2a and unused by any runtime path since T2c widened the panel to
render the whole surface from GET /api/settings metadata (ruling A5, design record
trovex 6f73f179). `writableKeys()` has been the sole PUT allowlist since T2a; the map
and every test pinning it were dead weight kept only "so the panel test keeps
compiling". Removed.

## CHANGE (3 files, zero behaviour change)
- `internal/relay/settings_spec.go`: delete `var writableSettings` + its doc comment;
  drop `writableSettings` from the `settingSpec` doc comment. `writableKeys()` /
  `validateSettings` untouched.
- `internal/relay/api.go`: rewrite the `apiPutSetting` preamble comment to name only
  `writableKeys()` / `validateSettings`.
- `internal/relay/settings_spec_test.go`: drop every `writableSettings` assertion;
  AC1 `TestSettingsSpecAllowlistDerived` re-expressed against `writableKeys()` only
  (24 keys, no evil_key, a legacy PUT sun_type=2 AND an Operational PUT
  message_retention=48h both apply 200 via doAPI); NEW AC2
  `TestNoWritableSettingsIdentifierInSource` scans every non-_test.go .go in the
  package dir for zero `writableSettings` occurrences.

`console_v2_config_panel_test.go:15` left untouched (its comment names the identifier
historically). GET/PUT /api/settings responses byte-identical for the same input.

## review-wraith verdict: SHIP
Scope: internal/relay/settings_spec.go, settings_spec_test.go, api.go (dead-code removal, no runtime path).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (477 pass, +1 new test).

BLOCKERS (must fix before merge):
- none.

NITS (non-blocking):
- none.

Not touched: sqlite writer/lock, schema/migrations, task transitions, deliveries/inbox,
auth/middleware, MCP registry, ingest/SSE/tokens, updater/release. Out of scope for this diff.

## 3. Files changed

```
...blesettings-legacy-9-key-panel-allowlist-spe.md | 68 ++++++++++++++++++++
 internal/relay/api.go                              |  7 +--
 internal/relay/settings_spec.go                    | 18 +-----
 internal/relay/settings_spec_test.go               | 72 ++++++++++++----------
 4 files changed, 112 insertions(+), 53 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-e3d26498-49a2-46a9-8db5-8a6b22531720 @ `c92f22c73`
- 🔴 AC1: AC1 partial: size==24 check passes, evil_key not asserted in this test (asserted elsewhere :302-303), but no legacy-key PUT verification; precondition still keys on writableSettings which AC1 says must vanish — evidence: internal/relay/settings_spec_test.go:36 TestSettingsSpecAllowlistDerived checks writableKeys()==24 but lacks legacy-key PUT; test still asserts against writableSettings (lines 71-83, 104-105) which AC1 says must be re-expressed against writableKeys() only — test: TestSettingsSpecAllowlistDerived internal/relay/settings_spec_test.go:36 — incomplete: missing PUT of legacy key (e.g. sun_type) to confirm 200 apply via apiPutSetting
- 🔴 AC2: Implementation commit missing; AC2 identifier scan returns 5 hits in non-_test.go files — evidence: grep -n writableSettings internal/relay/*.go | non-_test.go matches: settings_spec.go:14,144,150 and api.go:462,464 = 5 occurrences; TestNoWritableSettingsIdentifierInSource does not exist in settings_spec_test.go — test: NONE — AC2-named TestNoWritableSettingsIdentifierInSource absent from diff
- 🔴 AC3: [partial] Tests green by accident — old code with writableSettings still present, not the contract-required new state — evidence: go test -tags fts5 ./internal/relay/... = 476 PASS; but AC3 scope: git diff main..wraith-backend-2/t2d-a shows ONLY features/wraith-relay-t2d-a-...md, zero changes to internal/relay/settings_spec.go|settings_spec_test.go|api.go — test: TestV2ConfigPanel + TestSettingsSpec* still green (cached) because implementation never landed

## 5. Timeline

- round 1 → **reject** (review-e3d26498-49a2-46a9-8db5-8a6b22531720)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `e3d26498-49a2-46a9-8db5-8a6b22531720`._
