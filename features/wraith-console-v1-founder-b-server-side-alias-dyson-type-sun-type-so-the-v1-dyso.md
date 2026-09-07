# [wraith/console-v1][founder B] server-side alias dyson_type <-> sun_type so the v1 dyson picker works again; v1 assets untouched

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/v1-dyson-alias (from main)
## Relay task : 1ab761c2-d4d7-40dc-be5e-11cfdebf1fcd
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestDysonAliasPut: PUT {dyson_type:"off"} -> 200 and stored sun_type == "0"; PUT {dyson_type:"5"} -> stored sun_type == "5"; PUT {dyson_type:"auto"} -> sun_type cleared; PUT {dyson_type:"x"} -> 400 naming key dyson_type; body with both dyson_type and sun_type -> 400
- [ ] 2. AC2 TestDysonAliasGet: flat GET /api/settings exposes dyson_type mapped auto/off/n from stored sun_type, and the v1 fields (sun_type default "1", linear_mode, linear{}) plus groups/settings stay byte-compatible
- [ ] 3. AC3 TestV1AssetsUntouched: internal/web/static/js/main.js, index.html, style.css byte-identical to main; existing TestSettingsSpec*/TestV2ConfigPanel green; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

# T1ab761c2 — server-side dyson_type ⇄ sun_type alias (founder ruling B)

## Root cause
v1 dyson picker (static/js/main.js) reads `settings.dyson_type` from GET and PUTs
`dyson_type`, but the config surface was reshaped to the `sun_type` spec key (T2a).
The legacy key was neither accepted on PUT nor emitted on GET → v1 picker inert.

## Decision
Founder ruling B (design record 6f73f179 rule e): keep the feature, bridge the key
names SERVER-SIDE so v1 assets stay byte-identical. No v1/v2 asset edits.
- PUT: translate `dyson_type` → `sun_type` BEFORE validation. auto/null→clear,
  off→"0", "1".."7"→same, else→400(dyson_type). Both keys→400 (ambiguous).
- GET: add flat `dyson_type` mirror of stored sun_type (""→auto, "0"→off, else raw).
  Flat `sun_type` keeps "1" default unchanged.
- settings_spec.go: sun_type Min "0" (0 = off) so the off state validates.

## ACs → tests
- AC1 PUT translation/errors → TestDysonAliasPut
- AC2 GET flat mirror + v1 fields intact → TestDysonAliasGet
- AC3 v1 assets untouched (picker key present, no server-only token leak) → TestV1AssetsUntouched

## Rejected alternatives
- Option A (edit main.js to speak sun_type): violates byte-identical v1 assets.
- Client-side shim: same asset-edit problem; server bridge is the single source.

## review-wraith verdict: SHIP
Scope: internal/relay/api.go (settingsResponse + apiPutSetting alias), internal/relay/settings_spec.go (sun_type Min "0"), NEW internal/relay/console_v1_dyson_alias_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (483 pass)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- dyson_type raw "0" → 400 by design (picker sends "off", never "0"); GET maps stored "0"→"off" so round-trip stays consistent.

## 3. Files changed

```
internal/relay/api.go                         |  37 +++++++-
 internal/relay/console_v1_dyson_alias_test.go | 123 ++++++++++++++++++++++++++
 internal/relay/settings_spec.go               |   2 +-
 3 files changed, 160 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1ab761c2-d4d7-40dc-be5e-11cfdebf1fcd`._
