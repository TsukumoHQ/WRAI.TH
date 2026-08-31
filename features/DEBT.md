# Tech-debt backlog — auto-collected by the niwa scribe

- ## [LEGACY_OPPORTUNITY]
- [LEGACY_OPPORTUNITY]: none.
- [LEGACY_OPPORTUNITY]: internal/relay/dispatch_board_priority_test.go's existing multi-board refusal tests use profile "dev" against trovex/wraith-only board sets (no "backlog" board present) — they keep passing unchanged because "dev" maps to the backlog fallback, which doesn't exist in those fixtures. Worth a follow-up note to whoever touches that suite next: adding a "backlog" board to those fixtures would now make "dev" auto-route instead of refuse, which is correct new behavior but would look like a broken test if someone doesn't know why.
- - internal/relay/dispatch_board_priority_test.go's "dev"-profile refusal fixtures have no "backlog" board, so they still exercise the refuse path post-change — see [LEGACY_OPPORTUNITY] above; no action needed now, flagged for whoever touches that file next.
- [LEGACY_OPPORTUNITY]: `relay.analytics.db.parked-2026-08-22` (320MB) sitting on disk is the inert, already-applied backup from the one-time `migrateTokenUsageToAnalytics` backfill (guarded/idempotent, already ran — the live `relay.analytics.db` holds the migrated data). No restore/migrate action needed; safe to `rm` as housekeeping whenever convenient, not blocking this fix.
