# [wraith/relay] T2d-c GET /api/settings normalises duration values on the source=setting path: echo Duration.String() (48h0m0s) not the raw stored string (48h)

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/t2d-c (from main)
## Relay task : ac5314a5-831a-43e8-9c15-41eece95a391
## Status : 🔵 IN REVIEW

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 GET after PUT message_retention=48h returns value "48h0m0s" source=setting (test drives apiPutSetting then apiGetSettings via httptest and asserts the duration item)
- [ ] 2. AC2 an unparseable stored duration (raw UPDATE settings SET value='garbage' through the test DB) is echoed raw with source=setting and GET still returns 200 (no panic, no 500)
- [ ] 3. AC3 existing TestSettingsSpec* and TestV2ConfigPanel stay green; go test -tags fts5 ./internal/relay/... passes

## 2. Root cause & decisions

# T2d-c — GET /api/settings normalises stored duration/int values

## ROOT_CAUSE
`resolveSetting` (settings_spec.go) returned the raw stored string on the
`source=setting` branch, while the env and default branches already yield
canonical forms. So `PUT message_retention=48h` then `GET` returned `"48h"`
whereas the default path returns `"168h0m0s"` — a client cannot compare/parse
durations uniformly (breaks frozen contract 6f73f179 A1).

## CHANGE (2 files, no behaviour change beyond the returned string form)
- `internal/relay/settings_spec.go`: add `normalizeStoredValue(s settingSpec, raw string) string`
  — kindDuration → `time.ParseDuration(raw).String()`, kindInt → `Atoi`/`Itoa`,
  any other kind → raw; an unparseable value returns raw unchanged (GET never 500s).
  The `source=setting` non-secret return now goes through it. Secret guard still
  returns `""` first; env/default/code branches untouched; only settingsMetadata
  (v2 array) calls resolveSetting — v1 flat fields, PUT validation, and storage
  format are untouched.
- `internal/relay/settings_spec_test.go`: AC1 `TestGetNormalisesStoredDurationValue`
  (PUT 48h → GET value=="48h0m0s" source=="setting"); AC2
  `TestGetEchoesUnparseableStoredDurationRaw` (garbage stored via DB SetSetting,
  bypassing PUT → GET 200, value echoed raw, source=="setting", no panic/500).

## review-wraith verdict: SHIP
Scope: internal/relay/settings_spec.go, settings_spec_test.go (GET read-path value normalisation).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (479 pass, +2 new tests).

BLOCKERS: none.
NITS: none.

Note: branch is off main pre-T2d-a (writableSettings removal not yet merged), so
it shares settings_spec_test.go with T2d-a #190 — a 1-round rebase is expected
once #190 lands. Not touched: sqlite writer/lock, schema, task transitions,
deliveries/inbox, auth, MCP registry, ingest/SSE, updater/release, api.go, cleanup.go, settings.js.

## 3. Files changed

```
...normalises-duration-values-on-the-source-set.md | 67 ++++++++++++++++++++++
 internal/relay/settings_spec.go                    | 22 ++++++-
 internal/relay/settings_spec_test.go               | 37 ++++++++++++
 3 files changed, 125 insertions(+), 1 deletion(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-ac5314a5-831a-43e8-9c15-41eece95a391 @ `d32ce7783`
- 🟢 AC1: Real httptest PUT then GET, observable output asserted, not mock — evidence: settings_spec.go:208 routes source=setting non-secret through normalizeStoredValue; settings_spec.go:225-227 time.ParseDuration(raw).String() — test: TestGetNormalisesStoredDurationValue settings_spec_test.go:313 (PUT 48h → GET asserts 48h0m0s/setting; pre-fix returns raw 48h → would fail)
- 🟢 AC2: Hits the parse-error fall-through branch via real handler; no panic/500 verified by status==200 — evidence: settings_spec.go:222-233; kindDuration parse-error returns raw unchanged; no panic — test: TestGetEchoesUnparseableStoredDurationRaw settings_spec_test.go:331 (DB.SetSetting garbage → GET 200, value=garbage, source=setting)
- 🟢 AC3: Suite green; no test deleted or weakened — evidence: go test -tags fts5 ./internal/relay/... = 479 passed including TestV2ConfigPanel and existing TestSettingsSpec* — test: TestV2ConfigPanel and TestSettingsSpec* (pre-existing, all green)

## 5. Timeline

- round 1 → **approve** (review-ac5314a5-831a-43e8-9c15-41eece95a391)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ac5314a5-831a-43e8-9c15-41eece95a391`._
