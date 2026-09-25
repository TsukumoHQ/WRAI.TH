package relay

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-relay/internal/db"
)

// envWarnedKeys dedupes the invalid-env-twin WARN to one line per env name per
// process. The cleanup tick calls envBackupKeep/envReviewerTTL every
// PurgeInterval (ruling A7 moved the probes into the tick), so without the guard
// a single bad env value would re-log on every tick. Mirrors
// db.settingWarnedKeys (projects.go).
var envWarnedKeys sync.Map // env name string -> struct{}{}

// warnEnvOnce logs one WARN for an invalid env twin the first time name is seen
// this process; subsequent calls for the same name are silent.
func warnEnvOnce(name, format string, args ...interface{}) {
	if _, seen := envWarnedKeys.LoadOrStore(name, struct{}{}); seen {
		return
	}
	log.Printf(format, args...)
}

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
	// ACKManagerAge is when the ACK chain climbs to rung 2 (reports_to /
	// executive / founder), setting ack_manager_age (slice 2a).
	ACKManagerAge = 90 * time.Minute
	// ACKHumanAge is when the ACK chain reaches the human (rung 3, last),
	// setting ack_human_age.
	ACKHumanAge = 4 * time.Hour
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

// envBackupKeep parses the RELAY_BACKUP_KEEP env twin. ok is false when the env
// is unset OR invalid (non-positive / unparseable); an invalid value logs one
// line (the pre-existing message) before falling through.
func envBackupKeep() (int, bool) {
	v := os.Getenv("RELAY_BACKUP_KEEP")
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n, true
	}
	warnEnvOnce("RELAY_BACKUP_KEEP", "RELAY_BACKUP_KEEP=%q invalid; using default %d", v, DefaultBackupKeep)
	return 0, false
}

// resolveBackupKeep returns the rotated-snapshot retention count: the env twin
// when valid, else DefaultBackupKeep (never 0, which would leave no recovery
// point). Env-or-const only — used at boot-independent call sites and by the
// standing tests; the tick uses tickBackupKeep to layer the DB setting in.
func resolveBackupKeep() int {
	if n, ok := envBackupKeep(); ok {
		return n
	}
	return DefaultBackupKeep
}

// tickBackupKeep is the tick-time resolver: env twin wins when valid, else the
// backup_keep DB setting (read now, clamped to its D2 bounds), else the const —
// so a PUT takes effect on the next tick while the env override still wins.
func tickBackupKeep(database *db.DB) int {
	if n, ok := envBackupKeep(); ok {
		return n
	}
	return database.SettingInt("backup_keep", DefaultBackupKeep, 1, 24)
}

// envReviewerTTL parses the RELAY_REVIEWER_TTL_DAYS env twin (days). ok is false
// when unset OR invalid; an invalid value logs one line before falling through.
func envReviewerTTL() (time.Duration, bool) {
	v := os.Getenv("RELAY_REVIEWER_TTL_DAYS")
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * 24 * time.Hour, true
	}
	warnEnvOnce("RELAY_REVIEWER_TTL_DAYS", "RELAY_REVIEWER_TTL_DAYS=%q invalid; using default %s", v, DefaultReviewerTTL)
	return 0, false
}

// resolveReviewerTTL returns the standing reviewer-reaper TTL: the env twin when
// valid, else DefaultReviewerTTL. Env-or-const only; the tick uses
// tickReviewerTTL to layer the DB setting in.
func resolveReviewerTTL() time.Duration {
	if ttl, ok := envReviewerTTL(); ok {
		return ttl
	}
	return DefaultReviewerTTL
}

// tickReviewerTTL is the tick-time resolver: env twin wins when valid, else the
// reviewer_ttl_days DB setting (days, clamped to its D2 bounds), else the const.
func tickReviewerTTL(database *db.DB) time.Duration {
	if ttl, ok := envReviewerTTL(); ok {
		return ttl
	}
	days := database.SettingInt("reviewer_ttl_days", int(DefaultReviewerTTL/(24*time.Hour)), 1, 90)
	return time.Duration(days) * 24 * time.Hour
}

// pruneStaleBackups runs the data-dir backup GC and logs what it reclaimed.
// Non-fatal: a prune failure never disrupts the cleanup loop. minAge is the
// foreign_backup_min_age knob, read at call time by the caller.
func pruneStaleBackups(database *db.DB, keep int, minAge time.Duration) {
	removed, err := database.PruneStaleBackups(keep, minAge)
	if err != nil {
		log.Printf("prune stale backups error: %v", err)
		return
	}
	if len(removed) > 0 {
		log.Printf("pruned %d stale backup file(s): %v", len(removed), removed)
	}
}

// cleanupState carries the mutable state a StartCleanup tick advances across
// ticks. Extracting it (with runCleanupTick) lets tests drive one tick body
// directly without a running goroutine or a real ticker.
type cleanupState struct {
	lastBackup        time.Time // first snapshot fires BackupInterval after this
	lastRollupCatchup string    // UTC day of the last full-window rollup ("" => run next tick)
}

// StartCleanup runs a background goroutine that marks stale agents as inactive.
// It stops when the done channel is closed.
func StartCleanup(database *db.DB, done <-chan struct{}) {
	ticker := time.NewTicker(PurgeInterval)
	st := &cleanupState{lastBackup: time.Now()}
	go func() {
		defer ticker.Stop()
		// A healthy boot is the post-upgrade verification the updater lacks: prune
		// stale backups (superseded snapshots + aged one-off updater backups) once
		// at startup so a host that just auto-upgraded reclaims disk without manual
		// cleanup. Never touches the live DB or the newest known-good backup.
		pruneStaleBackups(database, tickBackupKeep(database),
			database.SettingDuration("foreign_backup_min_age", ForeignBackupMinAge, time.Hour, 30*24*time.Hour))
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				runCleanupTick(database, st)
			}
		}
	}()
}

// runCleanupTick is one cleanup pass. Every Operational knob is read here (not
// hoisted to StartCleanup), so a PUT to the settings table takes effect on the
// next tick with no restart; the const is the default and the D2 spec bounds are
// the clamp. Env twins (backup_keep, reviewer_ttl_days) keep precedence via the
// resolve* helpers. An empty settings table reads byte-identically to the consts.
func runCleanupTick(database *db.DB, st *cleanupState) {
	agentMaxAge := database.SettingDuration("agent_max_age", AgentMaxAge, 5*time.Minute, 24*time.Hour)
	if n, err := database.MarkStaleAgentsInactive(agentMaxAge); err != nil {
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
	if res, err := database.PurgeStaleReviewers(false, tickReviewerTTL(database)); err != nil {
		log.Printf("reap stale reviewers error: %v", err)
	} else if res.Purged > 0 {
		log.Printf("reaped %d stale review-* reviewer(s)", res.Purged)
	}
	// token_usage_retention_days drives BOTH the rollup window (days) and the
	// raw purge window (days as a duration) — one key, two derived cutoffs.
	tokenDays := database.SettingInt("token_usage_retention_days", TokenUsageRetentionDays, 1, 90)
	// Refresh the daily rollup BEFORE pruning raw rows, so shortening the
	// raw window never drops aggregate history the dashboards still show.
	// Routine tick: only days that can still change (yesterday 00:00Z on).
	if err := database.RollupTokenUsageRoutine(); err != nil {
		log.Printf("token usage rollup error: %v", err)
	}
	// Once per UTC day, re-sum the full retention window to fold in rows
	// that arrived for older days after the routine window passed them.
	if today := time.Now().UTC().Format("2006-01-02"); today != st.lastRollupCatchup {
		if err := database.RollupTokenUsage(tokenDays); err != nil {
			log.Printf("token usage rollup (daily catch-up) error: %v", err)
		} else {
			st.lastRollupCatchup = today
		}
	}
	if purged, err := database.PurgeOldTokenUsage(time.Duration(tokenDays) * 24 * time.Hour); err != nil {
		log.Printf("purge token usage error: %v", err)
	} else if purged > 0 {
		log.Printf("purged %d old token usage record(s)", purged)
	}
	// Hard-reclaim soft-expired messages (+ their deliveries/reads) and
	// stale audit rows so the tables stay bounded (TSU-127).
	messageRetention := database.SettingDuration("message_retention", MessageRetention, 24*time.Hour, 90*24*time.Hour)
	if purged, tombstoned, err := database.PurgeExpiredMessagesWithTombstones(messageRetention); err != nil {
		log.Printf("purge expired messages error: %v", err)
	} else if purged > 0 {
		log.Printf("purged %d expired message(s), %d tombstone(s) written", purged, tombstoned)
	}
	auditRetention := database.SettingDuration("audit_log_retention", AuditLogRetention, 7*24*time.Hour, 365*24*time.Hour)
	if purged, err := database.PurgeOldAuditLog(auditRetention); err != nil {
		log.Printf("purge audit log error: %v", err)
	} else if purged > 0 {
		log.Printf("purged %d old audit log record(s)", purged)
	}
	// Bound the durable deadletter journal: reclaim aged non-P0/P1 rows,
	// keep P0/P1 far longer (self-GC follow-up to the T6 deadletter).
	deadletterShort := database.SettingDuration("deadletter_short_retention", DeadletterShortRetention, 24*time.Hour, 180*24*time.Hour)
	deadletterLong := database.SettingDuration("deadletter_long_retention", DeadletterLongRetention, 24*time.Hour, 730*24*time.Hour)
	if purged, err := database.PurgeOldDeadletter(deadletterShort, deadletterLong); err != nil {
		log.Printf("purge deadletter error: %v", err)
	} else if purged > 0 {
		log.Printf("purged %d old deadletter record(s)", purged)
	}
	database.Optimize()

	if time.Since(st.lastBackup) >= BackupInterval {
		backupKeep := tickBackupKeep(database)
		if path, err := database.Backup(backupKeep); err != nil {
			log.Printf("db backup error: %v", err)
		} else {
			st.lastBackup = time.Now()
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
			pruneStaleBackups(database, backupKeep,
				database.SettingDuration("foreign_backup_min_age", ForeignBackupMinAge, time.Hour, 30*24*time.Hour))
		}
	}
}

// StartACKChecker runs a background goroutine that checks for unacknowledged tasks.
// 15min → notify dispatcher. 45min → escalate. Never auto-redispatch.
// Since DEC-wraith-obligations-1 slice 1 each check runs on the obligations
// engine (evaluateObligations), with the legacy checker's exact behaviour.
func StartACKChecker(database *db.DB, registry *SessionRegistry, done <-chan struct{}) {
	ticker := time.NewTicker(ACKCheckInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				evaluateObligations(database, registry, database.Now())
			}
		}
	}()
}

// ackNotifier is the push channel the ACK sanctions use (*SessionRegistry in
// production, a recorder in the equivalence test).
type ackNotifier interface {
	Notify(project, agentName, from, subject, messageID string)
}

// evaluateObligations is the ACK checker on the obligations engine: an
// escalation chain of four rungs per unclaimed pending task
// (DEC-wraith-obligations-1 slice 2a). Rung 0 (ack_notify_age) tells the
// dispatcher with a durable fyi P2 no-wake message; rung 1 (ack_escalate_age)
// a P1 message; rung 2 (ack_manager_age) goes to the dispatcher's reports_to,
// else the project's executive, else the founder; rung 3 (ack_human_age) to
// the human, unless rung 2 already reached them. Per task the highest due rung
// fires, at most one per tick, and firing it closes the lower rungs (no notify
// after an escalate). Messages are durable (Q2) and also pushed live.
func evaluateObligations(database *db.DB, notifier ackNotifier, now time.Time) {
	// Answer chain (task a01d0b87): its own message branch, first so an ACK
	// early return never skips it. ACK code below stays task-only.
	evaluateAnswerObligations(database, notifier, now)

	// Read the ack knobs at check time so a PUT takes effect on the next ACK
	// tick without restart (const default, D2 bounds clamp).
	notifyAge := database.SettingDuration("ack_notify_age", ACKNotifyAge, time.Minute, 24*time.Hour)
	escalateAge := database.SettingDuration("ack_escalate_age", ACKEscalateAge, time.Minute, 24*time.Hour)
	thresholds := map[string]time.Duration{
		db.NormAckNotify:   notifyAge,
		db.NormAckEscalate: escalateAge,
		db.NormAckManager:  database.SettingDuration("ack_manager_age", ACKManagerAge, time.Minute, 48*time.Hour),
		db.NormAckHuman:    database.SettingDuration("ack_human_age", ACKHumanAge, time.Minute, 48*time.Hour),
	}
	cutoff := now.Add(-notifyAge)

	// Close first so a task claimed in the gap reads fulfilled, not breached.
	if _, err := database.CloseMootTaskObligations(now); err != nil {
		log.Printf("obligations close error: %v", err)
	}
	if _, err := database.StampLateDone(now); err != nil {
		log.Printf("obligations late-done error: %v", err)
	}
	if _, err := database.InstantiateTaskAck(cutoff, now); err != nil {
		log.Printf("ACK checker error: %v", err)
		return
	}
	obligations, err := database.ActiveTaskObligations(cutoff)
	if err != nil {
		log.Printf("ACK checker error: %v", err)
		return
	}

	sanctioned := map[string]bool{} // one rung per task per tick
	for _, o := range obligations {
		if sanctioned[o.TaskID] {
			continue
		}
		dispatchedAt, err := time.Parse("2006-01-02T15:04:05Z", o.DispatchedAt)
		if err != nil {
			continue
		}
		age := now.Sub(dispatchedAt)
		threshold, known := thresholds[o.NormID]
		if !known || age < threshold {
			continue
		}
		// Branch taken: a refused CAS below still consumes this task's tick.
		sanctioned[o.TaskID] = true
		minutes := int(age.Minutes())
		sn := ackSanction(database, o, minutes)

		ok, err := database.TransitionTaskAck(o.ID, o.NormID, o.TaskID, dispatchedAt.Add(threshold), now, sn.alsoClose...)
		if err != nil {
			log.Printf("ACK %s mark error: task %s: %v", o.NormID, o.TaskID, err)
			continue
		}
		if !ok {
			continue
		}
		sendAckSanction(database, notifier, o, sn, minutes)
	}
}

// ackRungSanction is who a fired ACK rung tells, and how.
type ackRungSanction struct {
	target, msgType, priority, action, text string
	alsoClose                               []string
}

// ackSanction words and routes the sanction of one ACK rung: rung 0 a fyi P2
// no-wake to the dispatcher, rung 1 a P1 to the dispatcher, rung 2 the
// resolved reports_to / executive / founder (closing rung 3 when it is the
// founder), rung 3 the human.
func ackSanction(database *db.DB, o db.TaskObligation, minutes int) ackRungSanction {
	sn := ackRungSanction{target: o.DispatchedBy, msgType: "notification", priority: "P1", action: "do"}
	switch o.NormID {
	case db.NormAckNotify:
		sn.msgType, sn.priority, sn.action = "fyi", "P2", "none"
		sn.text = fmt.Sprintf(o.SanctionTemplate, o.Title, minutes, o.ProfileSlug)
	case db.NormAckEscalate:
		sn.text = fmt.Sprintf(o.SanctionTemplate, o.Title, minutes)
	case db.NormAckManager:
		var rule string
		sn.target, rule = resolveAckRung2(database, o.Project, o.DispatchedBy)
		log.Printf("[obligations] rung=2 task=%s rule=%s target=%s", o.TaskID, rule, sn.target)
		if sn.target == ackFounder {
			sn.alsoClose = []string{db.NormAckHuman} // the human is reached: rung 3 never fires
		}
		sn.text = fmt.Sprintf(o.SanctionTemplate, o.Title, minutes, o.DispatchedBy)
	case db.NormAckHuman:
		sn.target = ackFounder
		sn.text = fmt.Sprintf(o.SanctionTemplate, o.Title, minutes)
	}
	return sn
}

// sendAckSanction persists the rung's notice (durable, Q2) and pushes it live.
func sendAckSanction(database *db.DB, notifier ackNotifier, o db.TaskObligation, sn ackRungSanction, minutes int) {
	meta := fmt.Sprintf(`{"task_id":%q,"obligation_id":%q,"norm":%q}`, o.TaskID, o.ID, o.NormID)
	msg, _, err := database.InsertMessageWithDeliveries(o.Project, "relay", sn.target, sn.msgType, sn.text, sn.text, meta,
		sn.priority, -1, nil, nil, []string{sn.target}, sn.action)
	if err != nil {
		log.Printf("ACK %s message error: task %s: %v", o.NormID, o.TaskID, err)
		return
	}
	notifier.Notify(o.Project, sn.target, "relay", sn.text, msg.ID)
	log.Printf("ACK %s: task %s (%s) -> %s — %dmin", o.NormID, o.TaskID, o.Title, sn.target, minutes)
}

// ackFounder is the human end of the ACK chain: the operator inbox.
const ackFounder = "user"

// resolveAckRung2 picks the rung-2 target: the dispatcher's reports_to when
// that agent is active, else the project's first active executive (not the
// dispatcher), else the founder. rule names which fired, for the journal.
func resolveAckRung2(database *db.DB, project, dispatcher string) (target, rule string) {
	if d, err := database.GetAgent(project, dispatcher); err == nil && d != nil && d.ReportsTo != nil && *d.ReportsTo != "" {
		if m, err := database.GetAgent(project, *d.ReportsTo); err == nil && m != nil && m.Status == "active" {
			return m.Name, "reports_to"
		}
	}
	if agents, err := database.ListAgents(project); err == nil {
		for _, a := range agents {
			if a.IsExecutive && a.Status == "active" && a.Name != dispatcher {
				return a.Name, "executive"
			}
		}
	}
	return ackFounder, "founder"
}

// evaluateAnswerObligations breaches every answer obligation past its deadline
// (task a01d0b87): the obligation closes unfulfilled and the next rung opens
// in the same tx. answer.reply escalates to the recipient's role (reports_to,
// else an active executive), answer.role to the human. A role that resolves to
// nobody but the founder skips straight to answer.human: the human is only
// ever the max-depth rung. Each opened rung sends one P1 message to its bearer
// quoting the ask, as a reply to it so the bearer's answer fulfils the rung.
// The original message and its deliveries are never touched.
func evaluateAnswerObligations(database *db.DB, notifier ackNotifier, now time.Time) {
	due, err := database.DueAnswerObligations(now)
	if err != nil {
		log.Printf("[obligations] answer due error: %v", err)
		return
	}
	for _, a := range due {
		childNorm, bearer := a.NextNorm, ""
		switch childNorm {
		case db.NormAnswerRole:
			var rule string
			bearer, rule = resolveAckRung2(database, a.Project, a.Recipient)
			if bearer == ackFounder || strings.EqualFold(bearer, a.Recipient) {
				childNorm, bearer = db.NormAnswerHuman, ackFounder
			}
			log.Printf("[obligations] answer rung=1 msg=%s recipient=%s rule=%s target=%s", a.MessageID, a.Recipient, rule, bearer)
		case db.NormAnswerHuman:
			bearer = ackFounder
		}
		child, ok, err := database.BreachAnswerObligation(a, childNorm, bearer, now)
		if err != nil {
			log.Printf("[obligations] answer breach %s: %v", a.ID, err)
			continue
		}
		if !ok || child == nil {
			continue
		}
		sendAnswerSanction(database, notifier, a, child, now)
	}
}

// answerQuote is the ask as the sanction quotes it: subject and the first
// 300 characters of the body, or a note when the message was purged.
func answerQuote(a db.AnswerDue) string {
	body := strings.TrimSpace(a.Body)
	if body == "" {
		return fmt.Sprintf("%q (message %s purged; its tombstone keeps the thread)", a.Subject, a.MessageID)
	}
	if r := []rune(body); len(r) > 300 {
		body = string(r[:300]) + "…"
	}
	if a.Subject != "" {
		return fmt.Sprintf("%q: %s", a.Subject, body)
	}
	return body
}

// sendAnswerSanction persists the opened rung's notice to its bearer (P1,
// action do, reply_to = the ask) and pushes it live.
func sendAnswerSanction(database *db.DB, notifier ackNotifier, a db.AnswerDue, child *db.AnswerChild, now time.Time) {
	minutes := 0
	if at, err := time.Parse("2006-01-02T15:04:05.000000Z", a.AskedAt); err == nil {
		minutes = int(now.Sub(at).Minutes())
	}
	action := a.Action
	if action == "" {
		action = "ask"
	}
	var text string
	if child.NormID == db.NormAnswerRole {
		text = fmt.Sprintf(child.SanctionTemplate, action, a.Asker, a.Recipient, minutes, a.Recipient, answerQuote(a))
	} else {
		text = fmt.Sprintf(child.SanctionTemplate, action, a.Asker, a.Recipient, minutes, answerQuote(a))
	}
	meta := fmt.Sprintf(`{"obligation_id":%q,"norm":%q,"message_id":%q}`, child.ID, child.NormID, a.MessageID)
	replyTo := a.MessageID
	msg, _, err := database.InsertMessageWithDeliveries(a.Project, "relay", child.Bearer, "notification", text, text, meta,
		"P1", -1, &replyTo, nil, []string{child.Bearer}, "do")
	if err != nil {
		log.Printf("[obligations] answer sanction %s: %v", child.ID, err)
		return
	}
	notifier.Notify(a.Project, child.Bearer, "relay", text, msg.ID)
	log.Printf("[obligations] answer %s: msg %s (%s -> %s) -> %s", child.NormID, a.MessageID, a.Asker, a.Recipient, child.Bearer)
}
