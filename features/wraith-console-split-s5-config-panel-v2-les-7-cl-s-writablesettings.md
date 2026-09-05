# [wraith/console][split] S5: config panel v2 — les 7 clés writableSettings

## Team : wraith-engine (tsukumo)
## Branch : wraith/console-s5-config-panel (from main)
## Relay task : 8f4e3172-936a-4db7-8306-de8a09a7ec26
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. panneau v2 liste EXACTEMENT les 8 clés writableSettings hors federation_peers (api.go:448): sun_type, linear_enabled, linear_api_key, linear_team_key, linear_project, linear_interval, linear_routing, linear_project_map — avec valeur actuelle; linear_api_key masquée, jamais loggée; test Go qui compare l'ensemble des FIELDS de settings.js à writableSettings moins federation_peers (égalité d'ensemble, pas présence de chaîne)
- [ ] 2. save passe par api.saveSettings existant; clé hors allowlist jamais envoyée — test httptest: PUT /api/settings avec une clé hors allowlist = 403 + corps d'erreur JSON, rien écrit
- [ ] 3. erreur server-side (clé refusée) affichée à l'utilisateur, pas avalée — test: le corps d'erreur 403 renvoyé par le serveur est celui que settings.js rend (même chaîne error), chemin catch->render cité par ligne dans le PR
- [ ] 4. reload reflète les valeurs persistées — test httptest GET-PUT-GET sur les 8 clés (valeur écrite relue, secret relu masqué)
- [ ] 5. go build -tags fts5 ./... vert; v1 + settings modal v1 zéro diff (test: grep des ids du panneau dans static/ hors v2/ = 0)
- [ ] 6. PR body: note explicite 'pas de runtime JS dans la chaîne go test; comportement DOM vérifié en serve local' + les 4 étapes manuelles exécutées (liste, save ok, save refusé, reload)

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/web/static/v2/settings.js (failed-load guard), internal/relay/console_v2_config_panel_test.go (AC4 → all 8 keys + FailedLoadDisablesSaveNoWipe guard pin).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK; TestV2ConfigPanel 7/7. Serve-local failed-load state re-verified (served asset byte-identical, Save swapped for Retry, inputs disabled).

ROOT_CAUSE (r3 REJECT, both findings real per cto ruling): (1) DATA-LOSS — settings.js load() catch set s=null but render() still drew every field empty with Save enabled; a Save then sent '' for plain text and '0' for linear_enabled, wiping the stored config. (2) AC4 was partial — reload test covered 3 of 8 keys.

FIX: (1) on a null snapshot every input renders disabled, Save is replaced by a Retry-load button, and wire() early-returns on !s (no save handler) so collect()/PUT are unreachable — a click after a failed load cannot overwrite config; error banner kept. (2) GET-PUT-GET extended to all 8 panel keys (sun_type, linear_enabled, linear_api_key masked, linear_team_key, linear_project, linear_reconcile_interval [normalized 5m→5m0s], linear_routing + linear_project_map write-only via store). New subtest FailedLoadDisablesSaveNoWipe source-pins the guard.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
...-config-panel-v2-les-7-cl-s-writablesettings.md |  78 +++++++
 internal/relay/api.go                              |   6 +-
 internal/relay/console_v2_config_panel_test.go     | 252 +++++++++++++++++++++
 internal/web/static/v2/settings.js                 | 182 +++++++++++++++
 internal/web/static/v2/v2.css                      |  36 +++
 internal/web/static/v2/v2.js                       |  29 +++
 6 files changed, 582 insertions(+), 1 deletion(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-8f4e3172-936a-4db7-8306-de8a09a7ec26
- 🔴 AC1: [partial] AC text count (7) does not match implementation/backend allowlist (8) — dispatcher-supplied AC typo. JS-side rendering not behaviorally exercised. — evidence: settings.js:22-40 lists 8 FIELDS (sun_type + 7 linear, matches writableSettings-federation_peers in api.go:448-458); AC says 7 (likely typo); secret masked via data-type=secret + type=password + api_key_masked placeholder; console.log absent — test: TestV2ConfigPanel/KeysListedWithCurrentValueMasked asserts string-presence only (strings.Contains) — does NOT execute JS to verify rendering
- 🔴 AC2: [partial] Static pin covers frontend; pre-existing covers backend. No behavioral test for the wrapper round-trip. — evidence: settings.js:156 awaits api.saveSettings(body); federation_peers absent in settings.js (grep 0 matches); api.js:31 routes to PUT /api/settings — test: TestV2ConfigPanel/SaveThroughAllowlistWrapper string-asserts only; backend allowlist enforcement covered pre-existing TestApiPutSetting_Allowlist api_test.go:877 (evil_key → 403, nothing written)
- 🔴 AC3: [partial] Backend rejection is behaviorally tested pre-existing; frontend catch→render is not. — evidence: settings.js:159-163 catch on api.saveSettings failure → msg.kind=err with e.message; rendered to UI via cfg-note-err at 108 — test: TestV2ConfigPanel/ServerErrorSurfaced string-asserts only; pre-existing TestApiPutSetting_Allowlist exercises backend rejection (evil_key 403) but JS-side surfacing path NOT exercised by any test in the diff
- 🔴 AC4: [partial] Backend re-fetch behavior covered pre-existing; frontend reload call not behaviorally tested. — evidence: settings.js:158 await load() called after successful save — reload reflects persisted values — test: TestV2ConfigPanel/ReloadReflectsPersisted string-asserts only; pre-existing TestAPISettings api_test.go:140-160 covers GET-PUT-GET round trip on backend
- 🟢 AC5: No runtime behavior; static file-level checks are the correct test shape. — evidence: v2.js:18+151+160+503-507 imports/register/injects route; v1 static/index.html grep for page-settings|cfg-wrap returns 0 matches (v1 untouched); go build ./... EXIT=0 — test: TestV2ConfigPanel/V1ZeroDiffBuildGreen verifies file-level absence + route registration (string-presence appropriate for static checks); build gate green

### Round 2 — ❌ REJECTED by review-8f4e3172-936a-4db7-8306-de8a09a7ec26
- 🟢 AC1: set-equality vs real Go allowlist — evidence: settings.js:23-38 FIELDS uses the 8 keys matching writableSettings-{federation_peers}; actual key is linear_reconcile_interval (matches backend setLinearInterval=linear_reconcile_interval in linear_manager.go:19) — test: TestV2ConfigPanel/FieldsMatchWritableAllowlist internal/relay/console_v2_config_panel_test.go:39-61
- 🟢 AC2: evil_key->403+empty DB; sun_type->200+persisted — evidence: settings.js collect() only emits FIELDS keys; api.go:467-476 rejects non-allowlist before SetSetting — test: TestV2ConfigPanel/SaveThroughAllowlistNonAllowlistRejected internal/relay/console_v2_config_panel_test.go:65-89
- 🟢 AC3: server body valid JSON, contains not writable + evil_key, wrapper lifts, panel renders — evidence: api.go:472 json.Marshal reject body; api.js:17 lifts j.detail||j.error into thrown Error; settings.js catch->render — test: TestV2ConfigPanel/ServerErrorSurfaced internal/relay/console_v2_config_panel_test.go:93-117
- 🟢 AC4: sun_type=7 + linear.team_key=SYN reread; secret masked; raw body never contains plaintext — evidence: settings.js save.onclick awaits saveSettings then load(); GET /api/settings returns api_key_masked; linear_routing/linear_project_map write-only by design — test: TestV2ConfigPanel/ReloadReflectsPersistedSecretMasked internal/relay/console_v2_config_panel_test.go:121-167
- 🟢 AC5: build green + v1 byte-untouched — evidence: go build -tags fts5 ./... exits 0; v1 assets do not contain cfg-wrap/initSettings/cfg-save; v2.js registers initSettings and settings hash — test: TestV2ConfigPanel/V1ZeroDiffBuildGreen internal/relay/console_v2_config_panel_test.go:170-187
- 🔴 AC6: [partial] AC6 requires enumeration of 4 manual steps in PR body; only the note is present, the list is absent — evidence: c6c3e69e body: No JS runtime in go test; the DOM render/reload path is asserted at source and verified against a local serve. The explicit no-runtime note is present but the 4 manual steps (liste/save ok/save refusé/reload) are NOT enumerated in any commit message — test: NONE - AC6 is a documentation/claim surface; manual pass not on test path

### Round 2 — ❌ REJECTED by human:wraith-cto

### Round 3 — ❌ REJECTED by review-8f4e3172-936a-4db7-8306-de8a09a7ec26
- 🟢 AC1: 8 keys match writableSettings minus federation_peers; linear_reconcile_interval is the real backend key (linear_manager.go setLinearInterval) — evidence: settings.js:22-40 FIELDS key literals enumerated; test parses via fieldKeyRe and asserts set equality both directions plus len equality — test: TestV2ConfigPanel/FieldsMatchWritableAllowlist console_v2_config_panel_test.go:41
- 🟢 AC2: evil_key → 403 + nothing written; sun_type → 200 + persisted at DB — evidence: settings.js:151 await api.saveSettings(body); api.js:31 routes to PUT /api/settings; api.go:467-476 rejects non-allowlist key before SetSetting — test: TestV2ConfigPanel/SaveThroughAllowlistNonAllowlistRejected console_v2_config_panel_test.go:71
- 🟢 AC3: /tmp go run proves pre-fix body was invalid JSON (OLD parses: false) → json.Unmarshal in test would fail → the hunk is genuinely load-bearing — evidence: api.go 403 body json.Marshal encoded (the fix); api.js:17 lifts j.detail||j.error into thrown Error; settings.js:159-163 catch renders save failed: ${e.message} — test: TestV2ConfigPanel/ServerErrorSurfaced console_v2_config_panel_test.go:97
- 🔴 AC4: [partial] AC demands GET-PUT-GET on the 8 keys. Misses exactly the two keys whose stored values GET normalizes (project lowercased, interval 5m→5m0s). — evidence: ReloadReflectsPersistedSecretMasked PUTs 6 keys (sun_type, linear_enabled, linear_team_key, linear_api_key, linear_routing, linear_project_map) and GETs back 3 (sun_type, team_key, masked api_key). linear_project, linear_reconcile_interval, linear_enabled never read back; no pre-PUT GET to assert round-trip fidelity — test: TestV2ConfigPanel/ReloadReflectsPersistedSecretMasked console_v2_config_panel_test.go:128
- 🟢 AC5: build green; v1 assets untouched; v2.js registers initSettings and settings hash route — evidence: go build -tags fts5 ./... exit 0; grep -rn cfg-wrap\|initSettings\|cfg-save\|cfg-in\|cfg-row static/ outside v2/ = 0 hits repo-wide — test: TestV2ConfigPanel/V1ZeroDiffBuildGreen console_v2_config_panel_test.go:184
- 🟢 AC6: R2 reject cited enumeration absence; this submission supplies LIST/SAVE OK/SAVE REFUSED/RELOAD with concrete codes and body — evidence: features/wraith-console-split-s5-config-panel-v2-les-7-cl-s-writablesettings.md:32 carries the explicit no-runtime note; L33-36 enumerate all 4 manual steps with concrete HTTP outcomes and the fixed JSON body — test: N/A (manual verification documented in PR body)

### Round 3 — ❌ REJECTED by human:wraith-cto

## 5. Timeline

- round 1 → **reject** (review-8f4e3172-936a-4db7-8306-de8a09a7ec26)
- round 2 → **reject** (review-8f4e3172-936a-4db7-8306-de8a09a7ec26)
- round 2 → **reject** (human:wraith-cto)
- round 3 → **reject** (review-8f4e3172-936a-4db7-8306-de8a09a7ec26)
- round 3 → **reject** (human:wraith-cto)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `8f4e3172-936a-4db7-8306-de8a09a7ec26`._
