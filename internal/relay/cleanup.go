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
	// BackupKeep is how many rotated snapshots to retain.
	BackupKeep = 3
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
