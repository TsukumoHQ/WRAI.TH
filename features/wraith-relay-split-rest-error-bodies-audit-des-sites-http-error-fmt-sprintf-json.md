# [wraith/relay][split] REST error bodies: audit des sites http.Error + fmt.Sprintf JSON — un seul helper jsonError, plus jamais de JSON invalide

## Team : wraith-backend (tsukumo)
## Branch : wraith/rest-jsonerror (from main)
## Relay task : dc93de28-0131-4c38-94a1-b1e02e36b8a3
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 AMENDÉ (wraith-cto 14:22Z, option C): zéro site à interpolation DYNAMIQUE dans un littéral JSON reste dans internal/relay hors api.go:468 (S5): grep de `http.Error(w, fmt.Sprintf(`{`, de `strconv.Quote`/`%q` injecté dans un littéral `{"error"`, et de concaténation de chaîne (`'`+x+`'` ou `"`+x+`"`) dans un tel littéral = 0 — tous migrés vers le helper. Les http.Error(w, `{...}`) STATIQUES (JSON déjà valide) peuvent rester; le PR liste leur compte par fichier comme candidats du follow-up d'unification (ticket séparé, pas celui-ci).
- [ ] 2. helper jsonError: corps via json.Marshal, header Content-Type: application/json, status inchangé par site (table des sites migrés + status dans le PR)
- [ ] 3. test table-driven: chaque site migré atteignable en httptest renvoie un corps JSON valide avec .error non vide, y compris une entrée dont la valeur contient un guillemet et un backslash (et un \x/\a que %q aurait émis)
- [ ] 4. aucun status code ni clé de corps modifié (diff cité)
- [ ] 5. go test -tags fts5 ./internal/relay/ && go test -tags fts5 ./... verts (chiffre cité); ≤5 fichiers (helper + api.go + api_messages.go + webhook_signal.go + test)

## 2. Root cause & decisions

# dc93de28 — REST error bodies: jsonError helper

ROOT_CAUSE: the dynamic REST error sites built a JSON body by hand — a
`fmt.Sprintf("{\"error\":%q}", err.Error())` literal (api.go x4, webhook_signal.go),
`` `{"error":`+strconv.Quote(q)+`}` `` (api_messages.go quota), and a raw
`` `{"error":"...'"+to+"'..."}` `` concatenation (api_messages.go not-authorized).
Go's `%q` / `strconv.Quote` use Go string escaping, not JSON: a control byte in
the value is emitted as `\x1b` and a bell as `\a` — neither is legal JSON — and
the raw-concat site never escaped `to` at all. The v2 console's `sendJSON` runs
`res.json()` in a try/catch and falls back to `"<status> <url>"`, so a malformed
error body silently swallows the refusal reason for every caller of that endpoint.

DECISION (ticket option C, cto 14:22Z): add one `jsonError(w, status, msg)` helper
(internal/relay/json_error.go) — body via `json.Marshal` (correct escaping for
quotes, backslashes, control bytes), `Content-Type: application/json`, status
unchanged per site. Migrate ONLY the 7 DYNAMIC-interpolation sites. Static
`{"error":"..."}` literals (already valid JSON) are left as-is; the PR lists them
per file as candidates for a separate unification follow-up ticket. No status code
or body key (`error`) changed.

REJECTED ALTERNATIVE: reuse the existing `apiError(w, status, msg, err)`. It logs,
adds a `detail` field from `err`, and writes via `http.Error` (text/plain) — a
different body shape and content type than these sites emit. The sites put the
reason directly in `error`, so a dedicated minimal helper preserves behavior
exactly where `apiError` would have changed it.

ROOT_CAUSE of AC1 being a real regression guard, not a grep receipt:
`TestNoDynamicJSONErrorLiterals` scans the package source and fails if any
`{"error"…}` literal is combined with an interpolation marker (`Sprintf(\`{`,
`strconv.Quote`, or backtick-literal `+` concat) — so a future dynamic error body
that skips the helper reds the build.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/relay/json_error.go (new), json_error_test.go (new), api.go,
api_messages.go, webhook_signal.go — REST error-body formatting only.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... = 768 passed.

BLOCKERS (must fix before merge):
- none. No sqlite/schema/migration, task-transition, delivery-inbox, auth/permission,
  MCP-registry, ingest/SSE, or updater surface touched. Statuses (400/403/422/429)
  and the `error` body key are preserved verbatim per site; the not-authorized
  message string is byte-identical.

NITS (non-blocking):
- Behavioral note: migrated error responses now carry `Content-Type: application/json`
  instead of `http.Error`'s `text/plain; charset=utf-8`. This is the correct type
  and what the ticket asks for; the v2 console parses JSON regardless of type, so no
  consumer regresses. Flagged only because it is an observable header change.
- json_error.go: unlike `http.Error`, jsonError does not set `X-Content-Type-Options:
  nosniff`. Negligible for an application/json body (not MIME-sniffable to anything
  executable); could be added for parity with `http.Error` in the follow-up.

## Migrated site coverage (AC3)

All 7 migrated dynamic sites are driven end-to-end through `ServeAPI` by
`TestJSONErrorReachableSites` (each: parseable JSON body, non-empty `.error`,
`Content-Type: application/json`, status unchanged).

| # | file:line | handler | error type / cause | status |
|---|-----------|---------|--------------------|--------|
| 1 | api.go:1038 | apiPostMemory | ValidateMemoryWrite (layer=context, no valid_until) | 400 |
| 2 | api.go:1554 | apiDispatchTask | *db.TypedTicketError | 400 |
| 3 | api.go:1559 | apiDispatchTask | *db.InvalidTitleError | 400 |
| 4 | api.go:1564 | apiDispatchTask | *db.BoardRequiredError | 400 |
| 5 | api_messages.go:191 | apiPostMessage | CheckQuotaError | 429 |
| 6 | api_messages.go:199 | apiPostMessage | CanMessage not-authorized | 403 |
| 7 | webhook_signal.go:158 | apiSignalWebhook | *db.TypedTicketError | 422 |

## Static literal follow-up candidates (AC1)

Static `{"error":"…"}` `http.Error` literals left as-is (already valid JSON) —
candidates for the separate unification follow-up ticket, NOT this one:

| file | static `{"error"…}` http.Error literals |
|------|------------------------------------------|
| internal/relay/api.go | 89 |
| internal/relay/api_messages.go | 12 |
| internal/relay/webhook_signal.go | 11 |

The AC1 guard `TestNoDynamicJSONErrorLiterals` allows these static forms and
fails only on dynamic interpolation into an `{"error"…}` literal.

## 3. Files changed

```
...-audit-des-sites-http-error-fmt-sprintf-json.md |  90 +++++++++
 internal/relay/api.go                              |   8 +-
 internal/relay/api_messages.go                     |   4 +-
 internal/relay/json_error.go                       |  28 +++
 internal/relay/json_error_test.go                  | 212 +++++++++++++++++++++
 internal/relay/webhook_signal.go                   |   3 +-
 6 files changed, 337 insertions(+), 8 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-dc93de28-0131-4c38-94a1-b1e02e36b8a3
- 🟢 AC1: grep internal/relay returns 0 hits for fmt.Sprintf into a { literal combined with error, 0 strconv.Quote into such a literal outside the json_error_test sentinel, 0 backtick-concat into a { literal outside json_error_test. S5 exception api.go:472 uses json.Marshal (valid JSON). — test: TestNoDynamicJSONErrorLiterals internal/relay/json_error_test.go:31 PASS
- 🟢 AC2: json_error.go:20 sets Content-Type application/json before WriteHeader; line 22 body via json.Marshal(map[string]string{error: msg}). Status per site unchanged: api.go 400 x4, api_messages.go 429 + 403, webhook_signal.go 422. — test: TestJSONError internal/relay/json_error_test.go:50 asserts Content-Type and JSON round-trip on 7 subtests including quote+backslash, control byte, bell. Doc lacks explicit per-site status table (minor).
- 🔴 AC3: [partial] TestJSONError table covers quote+backslash+control byte+bell (PASS). TestJSONErrorReachableSites drives only apiDispatchTask tte + ite via ServeAPI. 5 reachable migrated sites untested end-to-end: apiPostMemory (POST /memory ValidateMemoryWrite failure), apiDispatchTask BoardRequiredError, apiPostMessage quota (CheckQuotaError), apiPostMessage not-authorized (HasTeams + CanMessage), apiSignalWebhook TypedTicketError. — test: TestJSONErrorReachableSites internal/relay/json_error_test.go:118 covers 2 of 7 reachable sites
- 🟢 AC4: diff preserves status at all 7 migrated sites. Body key error preserved by json.Marshal map key. — test: TestJSONErrorReachableSites asserts .error key on reachable sites tested
- 🟢 AC5: go test -tags fts5 ./internal/relay/ ok. go test -tags fts5 ./... all packages ok, 0 FAIL, 768 RUN / 669 top-level PASS. 5 Go code files (api.go, api_messages.go, json_error.go, json_error_test.go, webhook_signal.go). gofmt and vet clean. — test: TestNoDynamicJSONErrorLiterals + TestJSONError + TestJSONErrorBeatsQuoteLiteral + TestJSONErrorReachableSites all PASS

## 5. Timeline

- round 1 → **reject** (review-dc93de28-0131-4c38-94a1-b1e02e36b8a3)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `dc93de28-0131-4c38-94a1-b1e02e36b8a3`._
