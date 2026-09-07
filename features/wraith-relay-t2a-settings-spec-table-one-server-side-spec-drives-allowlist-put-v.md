# [wraith/relay] T2a settings spec table: one server-side spec drives allowlist + PUT validation (400/403 whole-request) + GET /api/settings per-key metadata (frozen contract)

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/settings-spec (from main)
## Relay task : cd34c6ce-1bdf-4eea-8819-26d132b03599
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: writableKeys() is derived from settingSpecs and equals exactly the 24 writable keys = 9 legacy (sun_type, linear_enabled, linear_api_key, linear_team_key, linear_project, linear_reconcile_interval, linear_routing, linear_project_map, federation_peers) + the 15 Operational writable keys listed in the ticket; the legacy var writableSettings is ALSO spec-derived (Writable && group in console|linear|federation), equals exactly those 9 keys and is a strict subset of writableKeys(); apiPutSetting consults the spec (writableKeys), never writableSettings; spec keys are unique and every group is one of console|linear|federation|server|operational|timing (RULING wraith-cto A5 14:22Z, ticket contradiction AC1 vs AC5 reported by doer msg 75477041)
- [ ] 2. AC2 named test: PUT message_retention=1h (below min 24h) returns 400 {error:invalid value,key,detail} and the stored value is unchanged; PUT ack_notify_age=50m ALONE with ack_escalate_age=45m stored returns 400 and nothing is applied; PUT {ack_notify_age:50m, ack_escalate_age:60m} returns 200 and both are stored
- [ ] 3. AC3 named test: GET /api/settings returns the frozen shape: top-level groups in the fixed order and a settings array where EVERY entry has all 12 fields (key, group, kind, value, set, source, writable, secret, env_name, bounds, default, note); with LINEAR_API_KEY set in env the entry has source=env and set=true; a Timing entry (writer_timeout) has source=code, writable=false, value=15s
- [ ] 4. AC4 named test: for linear_api_key the GET value is always empty and set reflects presence; PUT linear_api_key:"" leaves the stored key unchanged; PUT linear_api_key:null clears it (set=false afterwards); the captured server log for these requests contains no fragment of the secret value
- [ ] 5. AC5 named test: PUT with an unknown key (evil_key) alongside a valid key returns 403 and applies nothing (whole-request), and the pre-existing TestV2ConfigPanel passes unchanged; scope: diff touches exactly internal/relay/settings_spec.go, internal/relay/api.go, internal/relay/settings_spec_test.go; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

# Decision — ticket T2a cd34c6ce (settings spec table: allowlist + PUT validation + GET metadata)

## ROOT_CAUSE
Not a bug fix — a capability gap. The v2 config panel exposed only 8 keys (sun_type + 7
Linear); the rest of the configuration surface was env-only / compile-time / hand-SQL with no
server-side description, so nothing could drive an allowlist, PUT validation, or per-key GET
metadata. Fix: one server-side settingSpec table as the single source for all three
(design record trovex 6f73f179).

## SCOPE
3 files: internal/relay/settings_spec.go (new), internal/relay/api.go, internal/relay/settings_spec_test.go (new).

## FIX
- settings_spec.go: settingSpec + settingSpecs (full config surface, D2). Spec-derived
  writableSettings (Writable && group console|linear|federation = 9 panel keys) and
  writableKeys() (all 24 Writable). resolveSetting (env>setting>default; Timing=code).
  validateValue (kind+bounds). validateSettings (whole-request 403/400 + cross-key
  ack_escalate>ack_notify, dl_long>=dl_short vs effective/stored). settingsMetadata
  (frozen 12-field shape).
- api.go: apiGetSettings + PUT-200 return legacy v1-compat fields alongside groups/settings.
  apiPutSetting decodes map[string]*string (null=clear, ""=unchanged on secret), validates
  before any SetSetting (nothing applied on reject), hot-reload hooks unchanged, secrets
  never logged. Old hardcoded writableSettings removed (now spec-derived).
- 403 error="not writable: <key>"+key and 400 error="invalid value: <key>"+key+detail
  (cto ruling A6, msg 8fb42359): symmetric human strings naming the refused key (keeps the
  pre-existing panel ServerErrorSurfaced test green), machine-readable `key`/`detail` fields intact.

## Tests (one per AC)
- AC1 TestSettingsSpecAllowlistDerived: writableKeys()==24 (9 legacy + 15 op), writableSettings==9
  spec-derived subset, PUT of an operational key (not in panel allowlist) applies via spec.
- AC2 TestSettingsPutBoundsAndCrossKey: below-min 400 nothing stored; ack_notify alone vs stored
  escalate 400 nothing applied; valid pair 200 both stored.
- AC3 TestSettingsGetFrozenShape: groups fixed order; every entry 12 fields; env secret source=env
  set=true; writer_timeout source=code writable=false value=15s.
- AC4 TestSettingsSecretHandling: secret value always ""; PUT "" unchanged; PUT null clears (set=false);
  server log never contains the secret.
- AC5 TestSettingsUnknownKeyWholeRequest: unknown key alongside valid → 403, nothing applied; panel
  allowlist stays 9. (Pre-existing TestV2ConfigPanel passes unchanged.)

## review-agent-runtime verdict: SHIP
Scope: internal/relay settings surface (settings_spec.go, api.go, settings_spec_test.go).
§1 no agentColumns/scanAgent touch, no migration, no schema change — reads the existing settings table via GetSetting. §2 settingSpecs/specByKey/writableSettings/settingGroups are immutable package vars built once (no runtime mutation, no new goroutine, no shared-map write). §3 PUT still writes via SetSetting (single writer) at the same frequency (rare dashboard action, whole-request); GET/validate read via GetSetting (ro pool) — no new per-request write on a hot path; fallible reads return "" (GetSetting swallows err), no nil-deref. §4 no new route/bind/auth; PUT allowlist is now stricter (spec-driven), whole-request 403/400 before apply; secrets never returned (value "") or logged (verified by AC4 log-capture). §5/§6 no MCP tool, lifecycle or self-update change. v1 assets untouched; v1 GET fields (linear_mode + nested linear) preserved byte-compatible.
BLOCKERS: none.
NITS: sun_type gains kind=int min=1 validation (was unvalidated) — a deliberate PUT-validation improvement per ticket, rejects non-int sun_type; flagged, not a regression. Server RO group carries 14 env keys (11 + 3 webhook secrets) vs the ticket's "13" tally — no AC asserts the count; noted.
Gate: go test -tags fts5 ./internal/relay/... = 474 pass; vet clean; gofmt clean; 5 AC tests + pre-existing TestV2ConfigPanel green.

## 3. Files changed

```
internal/relay/api.go                |  87 +++++---
 internal/relay/settings_spec.go      | 378 +++++++++++++++++++++++++++++++++++
 internal/relay/settings_spec_test.go | 308 ++++++++++++++++++++++++++++
 3 files changed, 741 insertions(+), 32 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `cd34c6ce-1bdf-4eea-8819-26d132b03599`._
