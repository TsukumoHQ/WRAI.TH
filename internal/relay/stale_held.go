package relay

import (
	"fmt"
	"log"
	"time"

	"agent-relay/internal/db"
)

// StaleTaskAge is the stale_task_age default: how long an accepted /
// in-progress task may go without heartbeat or activity before its
// dispatcher is alerted (task 887351ac).
const StaleTaskAge = 2 * time.Hour

// evaluateStaleHeldTasks alerts the dispatcher, once per stale episode, of
// every accepted / in-progress task silent for stale_task_age: the stall the
// no-ACK ladder cannot see because the task already left pending. P1 "do",
// durable, pushed live.
func evaluateStaleHeldTasks(database *db.DB, notifier ackNotifier, now time.Time) {
	age := database.SettingDuration("stale_task_age", StaleTaskAge, 5*time.Minute, 48*time.Hour)
	stale, err := database.StaleHeldTasks(now.Add(-age))
	if err != nil {
		log.Printf("stale-held check error: %v", err)
		return
	}
	for _, s := range stale {
		ok, err := database.MarkStaleNotified(s.ID, s.Project, s.Seen, now)
		if err != nil || !ok {
			continue
		}
		minutes := 0
		if seen, perr := time.Parse(time.RFC3339Nano, s.Seen); perr == nil {
			minutes = int(now.Sub(seen).Minutes())
		}
		text := fmt.Sprintf("STALE: task '%s' is %s by %s with no heartbeat or activity for %dmin — check the holder, reassign or reclaim.",
			s.Title, s.Status, orNoneStr(s.Holder), minutes)
		meta := fmt.Sprintf(`{"task_id":%q,"alert":"stale_held"}`, s.ID)
		msg, _, err := database.InsertMessageWithDeliveries(s.Project, "relay", s.DispatchedBy, "notification", text, text, meta,
			"P1", -1, nil, nil, []string{s.DispatchedBy}, "do")
		if err != nil {
			log.Printf("stale-held message error: task %s: %v", s.ID, err)
			continue
		}
		notifier.Notify(s.Project, s.DispatchedBy, "relay", text, msg.ID)
		log.Printf("STALE: task %s (%s) %s by %s -> %s — %dmin", s.ID, s.Title, s.Status, s.Holder, s.DispatchedBy, minutes)
	}
}

func orNoneStr(s string) string {
	if s == "" {
		return "(nobody)"
	}
	return s
}
