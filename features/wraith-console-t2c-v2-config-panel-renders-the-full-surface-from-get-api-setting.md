# [wraith/console] T2c v2 config panel renders the full surface from GET /api/settings metadata: 6 groups, source badges, locks, inline 400 errors, secrets set/unset

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/t2c-config-panel (from main)
## Relay task : 2abc3bef-c6e8-403f-bc70-26e84b3f4c75
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: settings.js renders sections from the response groups array in server order and has NO hardcoded writable key list (no `key: '` FIELDS literals); the LABELS map keys are a subset of the frozen-contract fixture keys and every writable fixture key has a label
- [ ] 2. AC2 named test: settings.js contains the badge text map with exactly the four texts env / setting / default / compile-time (code), and the lock condition covers both writable=false and source=env
- [ ] 3. AC3 named test: on a 400 response settings.js renders the detail next to the offending key (data-err-key attribute path present) and keeps the existing 403 path via j.detail || j.error in v2/api.js
- [ ] 4. AC4 named test: for kind=secret the panel renders a set/unset pill and a replace input with no value attribute, the clear action sends JSON null, an empty replace input omits the key from the PUT body, and no asset contains console.log or a fixture secret string
- [ ] 5. AC5 named smoke test: the federation group renders as a read-only summary with a link to the federation page (no inline JSON editor), the Timing rows render note text when present, and no new v2 id/class leaks into v1 assets; scope: diff touches exactly internal/web/static/v2/settings.js, internal/web/static/v2/v2.css, internal/relay/console_v2_config_panel_test.go; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

# Decision — 2abc3bef T2c v2 config panel (full surface from GET /api/settings)

ROOT_CAUSE: the v2 config panel exposed only 8 keys (sun_type + 7 Linear) from a
hardcoded FIELDS list; founder ask "la page config pas assez bien, il manque bcp
de config". T2a froze the full metadata contract (settings_spec.go); T2c makes
the panel render the whole surface from that metadata.

## Decision
Rewrite internal/web/static/v2/settings.js to render from GET /api/settings
{groups, settings} instead of a hardcoded list: one section per group in server
order, per-key source badge (env/setting/default/compile-time (code)), read-only
lock when !writable || source==='env', per-row note, inline 400 {error,key,detail}
placed next to the offending key via data-err-key (no re-render → form state kept).
Kind-aware inputs (bool/enum/int/duration/json/string/secret). Secrets: set/unset
pill + never-prefilled replace input + clear (sends JSON null); empty replace is
omitted from the PUT (never sends ""). Federation = RO summary (peer count +
labels, tokens never rendered) + link to #/federation; no inline editor. The
existing Boards group (S7b) is preserved unchanged.

## Rulings applied
- A5: editable set derived from the real spec — writableKeys() minus
  federation_peers = 23; the test derives `want` from settingSpecs/writableKeys(),
  never from writableSettings (which stays the 9 legacy keys in api.go).
- A6: 403 error "not writable: <key>" (+key), 400 "invalid value: <key>" (+key,
  +detail); the panel renders `detail` (unchanged) inline and surfaces 403 via
  api.js j.detail||j.error. v1-compat GET flat fields (linear_mode, linear{}) are
  ignored by the panel (it iterates settings[]).
- Q3: Timing rows show badge "compile-time (code)" (source=code), RO.

## Files (3)
- internal/web/static/v2/settings.js — metadata-driven rendering + save/clear.
- internal/web/static/v2/v2.css — badge/lock/pill/err/note/federation styles.
- internal/relay/console_v2_config_panel_test.go — rewritten, 5 AC tests.

## ACs → tests (one per AC; live handler via httptest where possible)
- AC1 TestV2ConfigPanelLabelsSubsetOfSpec — no hardcoded key list; s.groups.map;
  LABELS ⊆ settingSpecs keys; every editable writable key (writableKeys minus
  federation_peers) has a label.
- AC2 TestV2ConfigPanelSourceBadgesAndLock — SOURCE_BADGE exactly 4 texts incl
  "compile-time (code)"; lock condition covers writable=false AND source=env.
- AC3 TestV2ConfigPanelInlineValidationError — live PUT: 400 "invalid value: <key>"
  +key+detail nothing applied, 403 "not writable: <key>" +key nothing applied;
  settings.js targets data-err-key + api.js j.detail||j.error.
- AC4 TestV2ConfigPanelSecretSetUnsetClear — pill; replace input has no value=;
  clear sends JSON null; empty replace omitted; no console.log in assets; live GET
  never returns a secret value (value "" + set true + secret true).
- AC5 TestV2ConfigPanelFederationSummaryAndScope — federation RO summary + link,
  no inline editor; rows render note; no v2 token leaks into v1 assets; live GET
  returns groups in fixed order + 12-field settings entries.

## Rejected alternatives
- Keep the old TestV2ConfigPanel subtests: impossible — they asserted the removed
  hardcoded FIELDS + old GET shape; the ticket mandates the rewrite.
- Touch api.js for the 400 key: rejected (out of 3-file scope) — settings.js does
  its own whole-request PUT to read the structured {error,key,detail}; api.js
  stays unchanged (403 path still greps j.detail||j.error).

## Verify
go build -tags fts5 ./... OK; go vet OK; gofmt clean;
go test -tags fts5 ./internal/relay/... → 471 passed (5 new AC tests).
Live: built binary, GET /api/settings on a local relay (RELAY_DB tmp, PORT 8199,
RELAY_BIND 127.0.0.1, HOME tmp) → valid JSON, groups in order, 57 settings, 24
writable, 6 secret, federation source=default value="", timing source=code.
v1 assets untouched. No LEGACY_OPPORTUNITY.

## review-wraith verdict: SHIP
Scope: internal/web/static/v2/settings.js, internal/web/static/v2/v2.css,
internal/relay/console_v2_config_panel_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (471 passed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

Notes: no DB writer / schema / migration / messaging / dispatch / SSE / updater
surface touched — the fleet-backbone thesis (SSOT, single-writer, backward-compat)
is not in scope. The test's live-handler use is GET (read) + PUT validation only,
no new writer. Secrets never leave the server (live GET asserts masked). v1 assets
byte-identical (AC5 asserts no v2-token leak). Federation tokens never rendered.

## 3. Files changed

```
internal/relay/console_v2_config_panel_test.go | 522 +++++++++++++++----------
 internal/web/static/v2/settings.js             | 378 ++++++++++++------
 internal/web/static/v2/v2.css                  |  34 ++
 3 files changed, 604 insertions(+), 330 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `2abc3bef-c6e8-403f-bc70-26e84b3f4c75`._
