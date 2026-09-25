package relay

import (
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// TestACKEquivalence (DEC-wraith-obligations-1 slice 1, task f77efe18) runs
// the legacy ACK checker — kept below VERBATIM as the oracle, only renamed and
// with its registry parameter typed as the ackNotifier interface
// *SessionRegistry already satisfies — and evaluateObligations on twin DBs
// over the same scenarios. Each tick must emit the same notices
// (project, recipient, from, text, task_id) and leave the same
// ack_notified_at / ack_escalated_at values on every task, quirks Q1-Q4
// included. It is part of the default `go test` run on purpose: every PR that
// touches obligations runs it until slice 2 retires a quirk explicitly.
//
// Slice 2a (task 6b4369f0) retired Q1/Q2/Q4. Notices are compared without the
// message id (the engine now pushes the durable message's id, the legacy push
// carried the task id), and exactly one expected difference is applied to the
// oracle: a legacy notify that follows an escalate for the same task (Q1) is
// dropped, together with the ack_notified_at mark it set.

// --- oracle: legacy checkUnackedTasks from main 5845c2f, verbatim ---

func legacyCheckUnackedTasks(database *db.DB, registry ackNotifier) {
	// Read the ack knobs at check time so a PUT takes effect on the next ACK
	// tick without restart (const default, D2 bounds clamp).
	notifyAge := database.SettingDuration("ack_notify_age", ACKNotifyAge, time.Minute, 24*time.Hour)
	escalateAge := database.SettingDuration("ack_escalate_age", ACKEscalateAge, time.Minute, 24*time.Hour)
	// Get tasks pending for at least the notify age
	tasks, err := database.GetUnackedTasks(notifyAge)
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

		if age >= escalateAge && task.AckEscalatedAt == nil {
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
		} else if age >= notifyAge && task.AckNotifiedAt == nil {
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

// --- harness ---

type recordingNotifier struct {
	mu      sync.Mutex
	notices []string
}

func (r *recordingNotifier) Notify(project, agentName, from, subject, messageID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, strings.Join([]string{project, agentName, from, subject, messageID}, "|"))
}

// drain returns this tick's notices sorted: the legacy candidate read has no
// ORDER BY, so only the per-tick set (at most one notice per task) is defined.
func (r *recordingNotifier) drain() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.notices
	r.notices = nil
	sort.Strings(out)
	return out
}

// withoutMessageID strips the trailing message id from recorded notices.
func withoutMessageID(notices []string) []string {
	out := make([]string, len(notices))
	for i, n := range notices {
		out[i] = n[:strings.LastIndex(n, "|")]
	}
	return out
}

// retireQ1 applies the one expected slice-2a difference to the oracle's tick:
// it drops a legacy notify for a task already escalated on an earlier tick,
// and records the task so its ack_notified_at is not compared. escalated holds
// the task ids the oracle escalated so far (its push carries the task id).
func retireQ1(notices []string, escalated, retired map[string]bool) []string {
	var kept []string
	for _, n := range notices {
		taskID := n[strings.LastIndex(n, "|")+1:]
		if strings.Contains(n, "|Task '") && escalated[taskID] {
			retired[taskID] = true
			continue
		}
		kept = append(kept, n)
	}
	for _, n := range notices {
		if strings.Contains(n, "|ESCALATED: ") {
			escalated[n[strings.LastIndex(n, "|")+1:]] = true
		}
	}
	return kept
}

// twin is one side of the comparison: a relay DB plus a raw handle for test
// fixtures (backdating, run_state, settings) that the DB API does not expose.
type twin struct {
	d   *db.DB
	raw *sql.DB
	rec *recordingNotifier
}

func newTwin(t *testing.T, name string) *twin {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	d, err := db.NewTestDB(path)
	if err != nil {
		t.Fatalf("new test db: %v", err)
	}
	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close(); _ = d.Close() })
	return &twin{d: d, raw: raw, rec: &recordingNotifier{}}
}

func (w *twin) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := w.raw.Exec(q, args...); err != nil {
		t.Fatalf("fixture %q: %v", q, err)
	}
}

func (w *twin) marks(t *testing.T, retiredQ1 map[string]bool) string {
	t.Helper()
	rows, err := w.raw.Query(`SELECT id, COALESCE(ack_notified_at, '-'), COALESCE(ack_escalated_at, '-') FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatalf("marks: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var id, n, e string
		if err := rows.Scan(&id, &n, &e); err != nil {
			t.Fatalf("scan marks: %v", err)
		}
		if retiredQ1[id] {
			n = "(Q1 retired)"
		}
		fmt.Fprintf(&b, "%s:notified=%s,escalated=%s;", id, n, e)
	}
	return b.String()
}

// ago renders now-d in the DB timestamp format, +30s off any minute boundary
// so the "%dmin" in the texts cannot differ between the two sides.
func ago(d time.Duration) string {
	return time.Now().UTC().Add(-d - 30*time.Second).Format("2006-01-02T15:04:05.000000Z")
}

type step func(t *testing.T, w *twin)

func seedTask(id, status, dispatchedAt string) step {
	return func(t *testing.T, w *twin) {
		w.exec(t, `INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, labels, blocked_periods, goal, acceptance_criteria, dod)
			VALUES (?, 'dev', 'cto', ?, ?, 'p1', ?, '[]', '[]', '', '[]', '')`, id, "title "+id, status, dispatchedAt)
	}
}

func setSQL(q string, args ...any) step {
	return func(t *testing.T, w *twin) { w.exec(t, q, args...) }
}

func setSetting(key, value string) step {
	return func(t *testing.T, w *twin) { w.d.SetSetting(key, value) }
}

func TestACKEquivalence(t *testing.T) {
	scenarios := []struct {
		name  string
		ticks [][]step // fixture steps applied to both twins before each tick
	}{
		{"FreshUnder15m", [][]step{{seedTask("t1", "pending", ago(10*time.Minute))}, nil}},
		{"Between15And45mNotifies", [][]step{{seedTask("t1", "pending", ago(20*time.Minute))}, nil}},
		{"Over45mFirstSightQ1", [][]step{{seedTask("t1", "pending", ago(50*time.Minute))}, nil, nil}},
		{"NotifyThenEscalate", [][]step{
			{seedTask("t1", "pending", ago(20*time.Minute))},
			{setSQL(`UPDATE tasks SET dispatched_at = ? WHERE id = 't1'`, ago(50*time.Minute))},
			nil,
		}},
		{"ClaimedBetweenTicks", [][]step{
			{seedTask("t1", "pending", ago(20*time.Minute))},
			{setSQL(`UPDATE tasks SET status = 'accepted', dispatched_at = ? WHERE id = 't1'`, ago(50*time.Minute))},
		}},
		{"Cancelled", [][]step{{seedTask("t1", "cancelled", ago(50*time.Minute))}}},
		{"CancelledAfterNotify", [][]step{
			{seedTask("t1", "pending", ago(20*time.Minute))},
			{setSQL(`UPDATE tasks SET status = 'cancelled', dispatched_at = ? WHERE id = 't1'`, ago(50*time.Minute))},
		}},
		{"Archived", [][]step{{seedTask("t1", "pending", ago(50*time.Minute)), setSQL(`UPDATE tasks SET archived_at = ? WHERE id = 't1'`, ago(time.Minute))}}},
		{"BecameRunContainer", [][]step{
			{seedTask("t1", "pending", ago(20*time.Minute))},
			{setSQL(`UPDATE tasks SET run_state = 'running', dispatched_at = ? WHERE id = 't1'`, ago(50*time.Minute))},
		}},
		{"SettingsChangedBetweenTicks", [][]step{
			{seedTask("t1", "pending", ago(20*time.Minute)), setSetting("ack_notify_age", "30m")},
			{setSetting("ack_notify_age", "10m")},
			{setSetting("ack_escalate_age", "15m")},
			nil,
		}},
		{"NotifyAgeAtOrAboveEscalateAgeQ1", [][]step{
			{setSetting("ack_notify_age", "40m"), setSetting("ack_escalate_age", "20m"), seedTask("t1", "pending", ago(45*time.Minute))},
			nil,
			nil,
		}},
		{"UnparseableDispatchedAt", [][]step{{seedTask("t1", "pending", "2000-01-01 not-a-time")}, nil}},
		{"PreexistingMarksNeverRefire", [][]step{
			{seedTask("t1", "pending", ago(50*time.Minute)), setSQL(`UPDATE tasks SET ack_notified_at = ?, ack_escalated_at = ? WHERE id = 't1'`, ago(10*time.Minute), ago(5*time.Minute))},
			nil,
		}},
		{"PreexistingNotifyOnlyThenEscalate", [][]step{
			{seedTask("t1", "pending", ago(50*time.Minute)), setSQL(`UPDATE tasks SET ack_notified_at = ? WHERE id = 't1'`, ago(30*time.Minute))},
			nil,
		}},
		{"SeveralTasksOneTick", [][]step{
			{
				seedTask("a", "pending", ago(20*time.Minute)),
				seedTask("b", "pending", ago(50*time.Minute)),
				seedTask("c", "pending", ago(5*time.Minute)),
				seedTask("d", "accepted", ago(50*time.Minute)),
			},
			{setSQL(`UPDATE tasks SET dispatched_at = ? WHERE id = 'c'`, ago(20*time.Minute))},
			nil,
		}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			legacy, engine := newTwin(t, "legacy"), newTwin(t, "engine")
			escalated, retired := map[string]bool{}, map[string]bool{}
			for i, steps := range sc.ticks {
				for _, s := range steps {
					s(t, legacy)
					s(t, engine)
				}
				// One instant per tick: the legacy marks read db.Now, the
				// engine gets the same value, so mark timestamps must match
				// byte-for-byte, not just in nullness.
				tick := time.Now().UTC()
				legacy.d.SetClock(func() time.Time { return tick })
				legacyCheckUnackedTasks(legacy.d, legacy.rec)
				evaluateObligations(engine.d, engine.rec, tick)

				ln := withoutMessageID(retireQ1(legacy.rec.drain(), escalated, retired))
				en := withoutMessageID(engine.rec.drain())
				if strings.Join(ln, "\n") != strings.Join(en, "\n") {
					t.Fatalf("tick %d notices differ:\nlegacy %q\nengine %q", i+1, ln, en)
				}
				if lm, em := legacy.marks(t, retired), engine.marks(t, retired); lm != em {
					t.Fatalf("tick %d marks differ:\nlegacy %s\nengine %s", i+1, lm, em)
				}
			}
		})
	}
}

// The scenarios above must actually exercise both sanctions, or equivalence
// would hold vacuously.
func TestACKEquivalenceCoversBothSanctions(t *testing.T) {
	w := newTwin(t, "cover")
	seedTask("n", "pending", ago(20*time.Minute))(t, w)
	seedTask("e", "pending", ago(50*time.Minute))(t, w)
	evaluateObligations(w.d, w.rec, time.Now().UTC())
	got := strings.Join(w.rec.drain(), "\n")
	if !strings.Contains(got, "p1|cto|relay|Task 'title n' no ACK after 20min. Profile: dev|") ||
		!strings.Contains(got, "p1|cto|relay|ESCALATED: Task 'title e' no ACK for 50min. Consider re-dispatching.|") {
		t.Fatalf("expected one notify and one escalate, got:\n%s", got)
	}
}

// TestObligationsOneSanctionPerTick: a task past both thresholds gets exactly
// one sanction per tick (the escalate), never two in the same tick (Q3). Since
// slice 2a the late notify of Q1 is retired, so later ticks stay silent.
func TestObligationsOneSanctionPerTick(t *testing.T) {
	w := newTwin(t, "one")
	seedTask("t1", "pending", ago(50*time.Minute))(t, w)
	var perTick [][]string
	for i := 0; i < 3; i++ {
		evaluateObligations(w.d, w.rec, time.Now().UTC())
		perTick = append(perTick, w.rec.drain())
	}
	if len(perTick[0]) != 1 || !strings.Contains(perTick[0][0], "ESCALATED") {
		t.Fatalf("tick 1 = %q, want exactly the escalate", perTick[0])
	}
	if len(perTick[1]) != 0 || len(perTick[2]) != 0 {
		t.Fatalf("ticks 2-3 = %q / %q, want nothing (Q1 retired)", perTick[1], perTick[2])
	}
}
