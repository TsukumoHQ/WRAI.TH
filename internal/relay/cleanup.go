package relay

import (
	"fmt"
	"log"
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
	// BackupKeep is how many rotated snapshots to retain. 12 hourly snapshots =
	// a ~12h recovery window — wide enough that an incident isn't rotated out
	// before it's noticed (the data-loss restore leaned on a 07:53 snapshot that
	// was the oldest of only 3, nearly gone). Disk: ~snapshot-size × 12.
	BackupKeep = 12

	// Retention policy (TSU-127). Soft-expiry (ExpireMessages/ExpireDeliveries)
	// only HIDES rows from inboxes; these windows govern HARD reclamation so the
	// tables don't grow unbounded over long-running fleet operation:
	//
	//   messages/deliveries/message_reads — purged MessageRetention after a
	//     message's TTL elapses (ttl_seconds=0 = never expires = never purged).
	//   audit_log — kept AuditLogRetention (accountability record; far longer).
	//   token_usage — 30d (PurgeOldTokenUsage).
	//   events — bounded by PruneDeliveredEvents(keep).
	//   activity — ephemeral, never persisted (in-memory ingest Detector + SSE).
	//
	// MessageRetention: a soft-expired message stays recoverable/inspectable for
	// a week past its TTL before the row is reclaimed.
	MessageRetention = 7 * 24 * time.Hour
	// AuditLogRetention: 90 days of accountability trail before reclamation.
	AuditLogRetention = 90 * 24 * time.Hour
)

// StartCleanup runs a background goroutine that marks stale agents as inactive.
// It stops when the done channel is closed.
func StartCleanup(database *db.DB, done <-chan struct{}) {
	ticker := time.NewTicker(PurgeInterval)
	lastBackup := time.Now() // first snapshot fires BackupInterval after boot
	go func() {
		defer ticker.Stop()
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
				if purged, err := database.PurgeOldTokenUsage(30 * 24 * time.Hour); err != nil {
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
				database.Optimize()

				if time.Since(lastBackup) >= BackupInterval {
					if path, err := database.Backup(BackupKeep); err != nil {
						log.Printf("db backup error: %v", err)
					} else {
						lastBackup = time.Now()
						log.Printf("db snapshot written: %s", path)
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
			// Escalate
			registry.Notify(task.Project, task.DispatchedBy, "relay",
				fmt.Sprintf("ESCALATED: Task '%s' no ACK for %dmin. Consider re-dispatching.", task.Title, int(age.Minutes())),
				task.ID)
			_ = database.MarkTaskAckEscalated(task.ID)
			log.Printf("ACK escalated: task %s (%s) — %dmin", task.ID, task.Title, int(age.Minutes()))
		} else if age >= ACKNotifyAge && task.AckNotifiedAt == nil {
			// First notification
			registry.Notify(task.Project, task.DispatchedBy, "relay",
				fmt.Sprintf("Task '%s' no ACK after %dmin. Profile: %s", task.Title, int(age.Minutes()), task.ProfileSlug),
				task.ID)
			_ = database.MarkTaskAckNotified(task.ID)
			log.Printf("ACK notify: task %s (%s) — %dmin", task.ID, task.Title, int(age.Minutes()))
		}
	}
}
