# [wraith/db+relay] T2b Operational knobs read at tick time: SettingDuration/SettingInt accessors (clamp+default+one WARN) + cleanup.go reads every Operational key inside the tick, env twins keep precedence

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/t2b-tick-reads (from main)
## Relay task : 1c3324e2-fa0b-4de3-8c2f-df859557031f
## Status : 🔵 IN REVIEW

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: SettingDuration/SettingInt return def on unset, the stored value when inside [min,max], the clamped bound when outside (test both sides), def on unparsable; the WARN line for an unparsable/out-of-range key is emitted exactly once across 3 consecutive reads of the same key (captured log)
- [ ] 2. AC2 named test: with the tick body driven directly, a PUT-equivalent SetSetting(message_retention, 48h) between two ticks changes the purge cutoff used by the second tick (rows aged 3d are purged on tick 2 but not on tick 1 at 7d) with no restart and no new StartCleanup call
- [ ] 3. AC3 named test: with RELAY_BACKUP_KEEP=5 and stored backup_keep=9 the tick uses 5; with the env unset and backup_keep=9 it uses 9; same pair for RELAY_REVIEWER_TTL_DAYS / reviewer_ttl_days; an invalid env value falls to the stored setting, then to the const
- [ ] 4. AC4 named test: token_usage_retention_days=3 stored makes the tick purge token_usage rows older than 3d AND run the rollup over a 3-day window (both cutoffs derived from the one key); default 14 with an empty table
- [ ] 5. AC5 named smoke test: with an EMPTY settings table every value the tick reads equals the pre-T2b const (AgentMaxAge, MessageRetention, AuditLogRetention, DeadletterShortRetention, DeadletterLongRetention, TokenUsageRetention(Days), AckNotifyAge, AckEscalateAge, DefaultBackupKeep, DefaultReviewerTTL, ForeignBackupMinAge) and for every Operational key in settingSpecs the spec `default` string equals that const's encoding; scope: diff touches exactly internal/db/projects.go, internal/relay/cleanup.go, internal/relay/cleanup_test.go; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

# T2b — Operational knobs read at tick time

ROOT_CAUSE: The Operational retention/lifecycle knobs T2a promoted to the settings panel (message_retention, audit_log_retention, token_usage_retention_days, agent_max_age, ack_notify/escalate_age, deadletter_short/long_retention, backup_keep, reviewer_ttl_days, foreign_backup_min_age) were only ever read as compile-time consts (or once at StartCleanup boot for backup_keep/reviewer_ttl). A PUT to the settings table therefore did nothing until the relay was restarted — the panel could set values that the cleanup loop never honored.

## Decision
- Add two clamped accessors on `*DB` (internal/db/projects.go): `SettingDuration(key,def,min,max)` and `SettingInt(key,def,min,max)` = `GetSetting` (RO pool) + parse + clamp. Contract: empty/unset -> def silently; unparsable -> def; out-of-range -> nearest bound; one WARN per key per process (sync.Map dedupe, never per-tick spam).
- Extract the per-tick body of `StartCleanup` into `runCleanupTick(database, *cleanupState)` (internal/relay/cleanup.go). Every Operational knob is read inside the tick via the accessor with the pre-T2b const as `def` and the D2 spec bounds as `[min,max]`, so a PUT takes effect on the next 5-min tick with no restart. Ticker intervals stay consts.
- `token_usage_retention_days` drives BOTH the raw-purge duration and the rollup window off the one key (they stay matched, as the rollup invariant requires).
- Env twins keep precedence: `resolveBackupKeep()/resolveReviewerTTL()` keep their env-or-const signatures (and their existing tests) via extracted `envBackupKeep()/envReviewerTTL()` probes; new `tickBackupKeep(db)/tickReviewerTTL(db)` layer the DB setting in as env > setting > const.
- An empty settings table reads byte-identically to today's consts (AC5 asserts both the accessor reads and that every T2b Operational spec `default` equals its const encoding — no drift).

## Rejected alternatives
- Change `resolveBackupKeep/resolveReviewerTTL` signatures to take `*db.DB`: rejected (cto ruling) — it breaks the existing `cleanup_backup_test.go`, a 4th file, contradicting the 3-file scope. Wrapper split keeps the diff at exactly 3 files.
- Add an exported test-only DB seam (`SetMessageExpiredAt`) to backdate `messages.expired_at` for AC2: rejected (cto ruling) — no test-only surface in production. AC2 instead backdates via a second `sql.Open("sqlite3", path)` handle in the test only (closed before the tick; go-sqlite3 is registered as driver `sqlite3`).
- Editing `settings_spec.go` to reconcile a spec/const default mismatch: not needed — spec defaults verified == consts; AC5 would fail and the drift be reported to wraith-cto rather than silently "fixed".

## Verify
`go test -tags fts5 ./internal/relay/...` green (479, +5 AC tests); `go test -tags fts5 ./...` green (880). Diff touches exactly internal/db/projects.go, internal/relay/cleanup.go, internal/relay/cleanup_test.go.

## review-wraith verdict: SHIP
Scope: internal/db/projects.go (SettingDuration/SettingInt accessors), internal/relay/cleanup.go (runCleanupTick extraction + tick-time knob reads + env/tick resolvers), internal/relay/cleanup_test.go (5 AC tests)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (relay 479, repo 880)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- AC2 test opens a second sql.Open("sqlite3", path) handle to backdate messages.expired_at. Test-only, on a t.TempDir DB, closed before the tick fires (no concurrent production writer). Chosen per cto ruling over adding a test-only exported DB seam.
- TokenUsageRetention (exported const) is now unused by production (the tick derives the window from token_usage_retention_days); kept intentionally per the ticket ("keep every const"), exported so the unused linter does not flag it.

## 3. Files changed

```
...s-read-at-tick-time-settingduration-settingi.md |  66 ++++
 internal/db/projects.go                            |  67 ++++
 internal/relay/cleanup.go                          | 320 +++++++++++-------
 internal/relay/cleanup_test.go                     | 359 +++++++++++++++++++++
 4 files changed, 688 insertions(+), 124 deletions(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-1c3324e2-fa0b-4de3-8c2f-df859557031f @ `771c510fb`
- 🟢 AC1: Test pin every contract clause including the warn-dedup via captureLog — evidence: internal/db/projects.go:154-211 adds SettingDuration/SettingInt + warnSettingOnce with sync.Map dedupe — test: TestSettingAccessorsClampDefaultWarnOnce internal/relay/cleanup_test.go:42 (unset→def, in-range→stored, both-side clamp, unparsable→def, 3 reads = 1 WARN)
- 🟢 AC2: Direct tick driver + observed row count delta on the same DB; second sql.Open handle for backdating is closed before tick — evidence: internal/relay/cleanup.go:194-299 extracts runCleanupTick(database,*cleanupState); internal/relay/cleanup_test.go:115-163 inserts+backdates expired_at 3d ago, ticks 1 (default 7d) keeps, SetSetting(48h), tick 2 purges with no StartCleanup — test: TestTickRereadsMessageRetentionNoRestart internal/relay/cleanup_test.go:115
- 🟢 AC3: All 4 precedence branches exercised — evidence: internal/relay/cleanup.go:102-107 tickBackupKeep + 135-141 tickReviewerTTL: env wins if envBackupKeep/envReviewerTTL ok else SettingInt — test: TestTickEnvTwinsBeatStoredBackupReviewer internal/relay/cleanup_test.go:173 (env=5 → 5, env=stored=9, env=invalid → stored=9, no env no stored → const)
- 🟢 AC4: Behavioral assertion on both raw row count + rollup daily count, single key coupling verified — evidence: internal/relay/cleanup.go:231-250 tokenDays drives BOTH RollupTokenUsage(tokenDays) and PurgeOldTokenUsage(tokenDays*24h) — test: TestTickTokenUsageRetentionDaysDrivesPurgeAndRollup internal/relay/cleanup_test.go:224 (default=5d kept+rolled; stored=3 → 5d row purged and excluded from rollup)
- 🟢 AC5: Hard gate: 479 passed in internal/relay; scope drift is .md-only (scribe provenance), see notice above — evidence: internal/relay/cleanup.go consts match settingSpecs defaults (settings_spec.go:98-108 use dur(...) which equals d.String() the same encoding the test builds via dur(constValue)) — test: TestTickEmptySettingsMatchConstsAndSpecDefaults internal/relay/cleanup_test.go:286 (11 consts + spec default == dur(const) for every Operational key in settingSpecs)

## 5. Timeline

- round 1 → **approve** (review-1c3324e2-fa0b-4de3-8c2f-df859557031f)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1c3324e2-fa0b-4de3-8c2f-df859557031f`._
