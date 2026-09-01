package relay

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"agent-relay/internal/db"
)

const (
	// PurgeInterval is how often the cleanup runs.
	PurgeInterval = 5 * time.Minute
	// AgentMaxAge is how long an agent can be inactive before being purged.
	AgentMaxAge = 30 * time.Minute
	// ACKCheckInterval is how often we check for unacked tasks.
	ACKCheckInterval = 5 * time.Minute
	// ACKNotifyAge is when to first notify dispatcher about no ACK.
	ACKNotifyAge = 15 * time.Minute
	// ACKEscalateAge is when to escalate the no-ACK notification.
	ACKEscalateAge = 45 * time.Minute
	// BackupInterval is how often a rotated DB snapshot is written.
	BackupInterval = time.Hour
	// DefaultBackupKeep is how many rotated snapshots to retain by default. Each
	// snapshot is a FULL DB copy, so the disk cost is ~snapshot-size × keep — at
	// ~635M/snapshot the old keep=12 alone was ~7.6G, a lead cause of synx-prod's
	// 93% root disk. 3 gives a ~3h recovery window at the hourly cadence; a host
	// that wants a wider window raises RELAY_BACKUP_KEEP (see resolveBackupKeep).
	DefaultBackupKeep = 3
	// ForeignBackupMinAge is how old a one-off (updater/ops) backup must be before
	// the data-dir GC prunes it — a just-upgraded host keeps its pre-upgrade copy
	// as a rollback point for this window; the newest one-off is always kept.
	ForeignBackupMinAge = 24 * time.Hour
	// DefaultReviewerTTL is how long a dead ephemeral reviewer (review-*) may sit
	// inactive before the standing reaper soft-deletes it. Tunable per host via
	// RELAY_REVIEWER_TTL_DAYS (see resolveReviewerTTL).
	DefaultReviewerTTL = 7 * 24 * time.Hour

	// Retention policy (TSU-127). Soft-expiry (ExpireMessages/ExpireDeliveries)
	// only HIDES rows from inboxes; these windows govern HARD reclamation so the
	// tables don't grow unbounded over long-running fleet operation:
	//
	//   messages/deliveries/message_reads — purged MessageRetention after a
	//     message's TTL elapses (ttl_seconds=0 = never expires = never purged).
	//   audit_log — kept AuditLogRetention (accountability record; far longer).
	//   token_usage — 14d raw (PurgeOldTokenUsage), older kept as the daily rollup.
	//   events — bounded by PruneDeliveredEvents(keep).
	//   activity — ephemeral, never persisted (in-memory ingest Detector + SSE).
	//
	// MessageRetention: a soft-expired message stays recoverable/inspectable for
	// a week past its TTL before the row is reclaimed.
	MessageRetention = 7 * 24 * time.Hour
	// AuditLogRetention: 90 days of accountability trail before reclamation.
	AuditLogRetention = 90 * 24 * time.Hour
	// DeadletterShortRetention: non-P0/P1 deadletter journal rows are reclaimed 30
	// days after capture — a generous window to notice a dropped P2/P3 without
	// letting the table grow unbounded.
	DeadletterShortRetention = 30 * 24 * time.Hour
	// DeadletterLongRetention: P0/P1 deadletter records are the critical traces a
	// human may still need to audit, so they are kept far longer (180 days) before
	// reclamation — bounded, but never dropped early.
	DeadletterLongRetention = 180 * 24 * time.Hour
	// TokenUsageRetention: raw telemetry rows are kept 14 days (down from 30). The
	// raw table is the largest in the fleet DB, so a shorter window keeps it small;
	// the daily rollup (token_usage_daily) retains older aggregates for dashboards.
	TokenUsageRetention = 14 * 24 * time.Hour
	// TokenUsageRetentionDays is TokenUsageRetention in days, for the rollup window.
	TokenUsageRetentionDays = 14
)

// resolveBackupKeep returns the rotated-snapshot retention count, from
// RELAY_BACKUP_KEEP when set to a positive integer, else DefaultBackupKeep. A
// non-positive or unparseable value falls back to the default (never 0, which
// would leave no recovery point).
func resolveBackupKeep() int {
	if v := os.Getenv("RELAY_BACKUP_KEEP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("RELAY_BACKUP_KEEP=%q invalid; using default %d", v, DefaultBackupKeep)
	}
	return DefaultBackupKeep
}

// resolveReviewerTTL returns the standing reviewer-reaper TTL, from
// RELAY_REVIEWER_TTL_DAYS when set to a positive integer (days), else
// DefaultReviewerTTL. An invalid or non-positive value falls back to the default.
func resolveReviewerTTL() time.Duration {
	if v := os.Getenv("RELAY_REVIEWER_TTL_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
		log.Printf("RELAY_REVIEWER_TTL_DAYS=%q invalid; using default %s", v, DefaultReviewerTTL)
	}
	return DefaultReviewerTTL
}

// pruneStaleBackups runs the data-dir backup GC and logs what it reclaimed.
// Non-fatal: a prune failure never disrupts the cleanup loop.
func pruneStaleBackups(database *db.DB, keep int) {
	removed, err := database.PruneStaleBackups(keep, ForeignBackupMinAge)
	if err != nil {
		log.Printf("prune stale backups error: %v", err)
		return
	}
	if len(removed) > 0 {
		log.Printf("pruned %d stale backup file(s): %v", len(removed), removed)
	}
}

// StartCleanup runs a background goroutine that marks stale agents as inactive.
// It stops when the done channel is closed.
func StartCleanup(database *db.DB, done <-chan struct{}) {
	ticker := time.NewTicker(PurgeInterval)
	lastBackup := time.Now() // first snapshot fires BackupInterval after boot
	backupKeep := resolveBackupKeep()
	reviewerTTL := resolveReviewerTTL()
	go func() {
		defer ticker.Stop()
		// A healthy boot is the post-upgrade verification the updater lacks: prune
		// stale backups (superseded snapshots + aged one-off updater backups) once
		// at startup so a host that just auto-upgraded reclaims disk without manual
		// cleanup. Never touches the live DB or the newest known-good backup.
		pruneStaleBackups(database, backupKeep)
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				n, err := database.MarkStaleAgentsInactive(AgentMaxAge)
				if err != nil {
					log.Printf("cleanup error: %v", err)
				} else if n > 0 {
					log.Printf("marked %d stale agent(s) inactive", n)
				}
				if expired, err := database.ExpireMessages(); err != nil {
					log.Printf("expire messages error: %v", err)
				} else if expired > 0 {
					log.Printf("expired %d message(s)", expired)
				}
				if expired, err := database.ExpireDeliveries(); err != nil {
					log.Printf("expire deliveries error: %v", err)
				} else if expired > 0 {
					log.Printf("expired %d delivery(ies)", expired)
				}
				if expired, err := database.ExpireFileLocks(); err != nil {
					log.Printf("expire file locks error: %v", err)
				} else if expired > 0 {
					log.Printf("expired %d file lock(s)", expired)
				}
				if expired, err := database.ExpireElevations(); err != nil {
					log.Printf("expire elevations error: %v", err)
				} else if expired > 0 {
					log.Printf("expired %d elevation(s)", expired)
				}
				// Standing TTL reaper: soft-delete dead ephemeral reviewer agents
				// (review-*) that the gate never tears down, once inactive past the
				// TTL, skipping any still holding a live task. Backstop so the roster
				// backlog never re-accumulates (DEC-niwa-reviewer-lifecycle-1).
				if res, err := database.PurgeStaleReviewers(false, reviewerTTL); err != nil {
					log.Printf("reap stale reviewers error: %v", err)
				} else if res.Purged > 0 {
					log.Printf("reaped %d stale review-* reviewer(s)", res.Purged)
				}
				// Refresh the daily rollup BEFORE pruning raw rows, so shortening the
				// raw window never drops aggregate history the dashboards still show.
				if err := database.RollupTokenUsage(TokenUsageRetentionDays); err != nil {
					log.Printf("token usage rollup error: %v", err)
				}
				if purged, err := database.PurgeOldTokenUsage(TokenUsageRetention); err != nil {
					log.Printf("purge token usage error: %v", err)
				} else if purged > 0 {
					log.Printf("purged %d old token usage record(s)", purged)
				}
				// Hard-reclaim soft-expired messages (+ their deliveries/reads) and
				// stale audit rows so the tables stay bounded (TSU-127).
				if purged, err := database.PurgeExpiredMessages(MessageRetention); err != nil {
					log.Printf("purge expired messages error: %v", err)
				} else if purged > 0 {
					log.Printf("purged %d expired message(s)", purged)
				}
				if purged, err := database.PurgeOldAuditLog(AuditLogRetention); err != nil {
					log.Printf("purge audit log error: %v", err)
				} else if purged > 0 {
					log.Printf("purged %d old audit log record(s)", purged)
				}
				// Bound the durable deadletter journal: reclaim aged non-P0/P1 rows,
				// keep P0/P1 far longer (self-GC follow-up to the T6 deadletter).
				if purged, err := database.PurgeOldDeadletter(DeadletterShortRetention, DeadletterLongRetention); err != nil {
					log.Printf("purge deadletter error: %v", err)
				} else if purged > 0 {
					log.Printf("purged %d old deadletter record(s)", purged)
				}
				database.Optimize()

				if time.Since(lastBackup) >= BackupInterval {
					if path, err := database.Backup(backupKeep); err != nil {
						log.Printf("db backup error: %v", err)
					} else {
						lastBackup = time.Now()
						// Verified-restore drill: confirm the snapshot is sound +
						// non-empty before we rely on it (runs on the snapshot file,
						// not the live DB, so it never locks the writer) — TSU-137.
						if counts, verr := db.VerifyDBFile(path); verr != nil {
							log.Printf("db snapshot VERIFY FAILED %s: %v", path, verr)
						} else {
							log.Printf("db snapshot written + verified: %s (agents=%d messages=%d tasks=%d)",
								path, counts["agents"], counts["messages"], counts["tasks"])
						}
						// Prune after a successful new snapshot so numbered slots
						// beyond keep and aged one-off backups don't accumulate.
						pruneStaleBackups(database, backupKeep)
					}
				}
			}
		}
	}()
}

// StartACKChecker runs a background goroutine that checks for unacknowledged tasks.
// 15min → notify dispatcher. 45min → escalate. Never auto-redispatch.
func StartACKChecker(database *db.DB, registry *SessionRegistry, done <-chan struct{}) {
	ticker := time.NewTicker(ACKCheckInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				checkUnackedTasks(database, registry)
			}
		}
	}()
}

func checkUnackedTasks(database *db.DB, registry *SessionRegistry) {
	// Get tasks pending for at least 15 minutes
	tasks, err := database.GetUnackedTasks(ACKNotifyAge)
	if err != nil {
		log.Printf("ACK checker error: %v", err)
		return
	}

	now := time.Now().UTC()
	for _, task := range tasks {
		dispatchedAt, err := time.Parse("2006-01-02T15:04:05Z", task.DispatchedAt)
		if err != nil {
			continue
		}
		age := now.Sub(dispatchedAt)

		if age >= ACKEscalateAge && task.AckEscalatedAt == nil {
			// CAS-guarded mark FIRST: the batch read above can be stale by the time
			// we get here (a run container claimed run_state, or the task moved off
			// 'pending', or a concurrent tick already marked it). ok=false means one
			// of those happened — no-op instead of escalating on data that's no
			// longer true.
			ok, err := database.MarkTaskAckEscalated(task.ID)
			if err != nil {
				log.Printf("ACK escalate mark error: task %s: %v", task.ID, err)
				continue
			}
			if !ok {
				continue
			}
			registry.Notify(task.Project, task.DispatchedBy, "relay",
				fmt.Sprintf("ESCALATED: Task '%s' no ACK for %dmin. Consider re-dispatching.", task.Title, int(age.Minutes())),
				task.ID)
			log.Printf("ACK escalated: task %s (%s) — %dmin", task.ID, task.Title, int(age.Minutes()))
		} else if age >= ACKNotifyAge && task.AckNotifiedAt == nil {
			ok, err := database.MarkTaskAckNotified(task.ID)
			if err != nil {
				log.Printf("ACK notify mark error: task %s: %v", task.ID, err)
				continue
			}
			if !ok {
				continue
			}
			registry.Notify(task.Project, task.DispatchedBy, "relay",
				fmt.Sprintf("Task '%s' no ACK after %dmin. Profile: %s", task.Title, int(age.Minutes()), task.ProfileSlug),
				task.ID)
			log.Printf("ACK notify: task %s (%s) — %dmin", task.ID, task.Title, int(age.Minutes()))
		}
	}
}
