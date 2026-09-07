package relay

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// newCleanupTestDB opens an isolated on-disk relay DB for the cleanup tests.
func newCleanupTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.NewTestDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// captureLog redirects the standard logger to a buffer for the duration of fn and
// returns everything it wrote, so a WARN line can be counted.
func captureLog(fn func()) string {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	fn()
	return buf.String()
}

// TestSettingAccessorsClampDefaultWarnOnce is AC1: SettingDuration/SettingInt
// return def on unset, the stored value inside [min,max], the clamped bound when
// outside (both sides), def on unparsable — and the WARN for an unparsable/out-of-
// range key is emitted exactly once across three consecutive reads of that key.
func TestSettingAccessorsClampDefaultWarnOnce(t *testing.T) {
	d := newCleanupTestDB(t)
	const (
		durDef, durMin, durMax = time.Hour, time.Minute, 24 * time.Hour
		intDef, intMin, intMax = 10, 1, 100
	)

	// Unset → def, silently (no WARN).
	if out := captureLog(func() {
		if got := d.SettingDuration("ac1_unset", durDef, durMin, durMax); got != durDef {
			t.Fatalf("unset duration: got %s want %s", got, durDef)
		}
		if got := d.SettingInt("ac1_unset_i", intDef, intMin, intMax); got != intDef {
			t.Fatalf("unset int: got %d want %d", got, intDef)
		}
	}); out != "" {
		t.Fatalf("unset must not WARN, got: %q", out)
	}

	// In-range stored → the stored value.
	d.SetSetting("ac1_ok", "2h")
	if got := d.SettingDuration("ac1_ok", durDef, durMin, durMax); got != 2*time.Hour {
		t.Fatalf("in-range duration: got %s want 2h", got)
	}
	d.SetSetting("ac1_ok_i", "42")
	if got := d.SettingInt("ac1_ok_i", intDef, intMin, intMax); got != 42 {
		t.Fatalf("in-range int: got %d want 42", got)
	}

	// Below/above range → clamped to the nearest bound (both sides, both kinds).
	d.SetSetting("ac1_lo", "10s") // < 1m
	if got := d.SettingDuration("ac1_lo", durDef, durMin, durMax); got != durMin {
		t.Fatalf("below-min duration: got %s want %s", got, durMin)
	}
	d.SetSetting("ac1_hi", "48h") // > 24h
	if got := d.SettingDuration("ac1_hi", durDef, durMin, durMax); got != durMax {
		t.Fatalf("above-max duration: got %s want %s", got, durMax)
	}
	d.SetSetting("ac1_lo_i", "0") // < 1
	if got := d.SettingInt("ac1_lo_i", intDef, intMin, intMax); got != intMin {
		t.Fatalf("below-min int: got %d want %d", got, intMin)
	}
	d.SetSetting("ac1_hi_i", "999") // > 100
	if got := d.SettingInt("ac1_hi_i", intDef, intMin, intMax); got != intMax {
		t.Fatalf("above-max int: got %d want %d", got, intMax)
	}

	// Unparsable → def.
	d.SetSetting("ac1_bad", "not-a-duration")
	if got := d.SettingDuration("ac1_bad", durDef, durMin, durMax); got != durDef {
		t.Fatalf("unparsable duration: got %s want %s", got, durDef)
	}
	d.SetSetting("ac1_bad_i", "not-an-int")
	if got := d.SettingInt("ac1_bad_i", intDef, intMin, intMax); got != intDef {
		t.Fatalf("unparsable int: got %d want %d", got, intDef)
	}

	// WARN dedupe: three reads of one bad key emit exactly one WARN line for it.
	d.SetSetting("ac1_warnonce", "1000h") // > 24h, out of range
	out := captureLog(func() {
		for i := 0; i < 3; i++ {
			_ = d.SettingDuration("ac1_warnonce", durDef, durMin, durMax)
		}
	})
	if n := strings.Count(out, "ac1_warnonce"); n != 1 {
		t.Fatalf("expected exactly 1 WARN for ac1_warnonce across 3 reads, got %d\nlog:\n%s", n, out)
	}
}

// TestTickRereadsMessageRetentionNoRestart is AC2: a PUT-equivalent
// SetSetting(message_retention, 48h) between two ticks changes the purge cutoff
// the next tick uses — a message soft-expired 3 days ago survives tick 1 at the
// 7d default but is purged on tick 2 — with no restart and no new StartCleanup.
func TestTickRereadsMessageRetentionNoRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.NewTestDB(dbPath)
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	msg, err := database.InsertMessage("p-ac2", "a", "b", "notification", "s", "c", "{}", "P2", 60, nil, nil)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	// Backdate expired_at to 3 days ago through a second "sqlite3" handle (the
	// go-sqlite3 driver is registered by the db package's blank import). The tick
	// reads/purges through the primary DB; a WAL commit here is visible to it.
	threeDaysAgo := time.Now().UTC().AddDate(0, 0, -3).Format("2006-01-02T15:04:05.000000Z")
	raw, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	if _, err := raw.Exec("UPDATE messages SET expired_at = ? WHERE id = ?", threeDaysAgo, msg.ID); err != nil {
		t.Fatalf("backdate expired_at: %v", err)
	}
	_ = raw.Close()

	count := func() int {
		r, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
		if err != nil {
			t.Fatalf("open count handle: %v", err)
		}
		defer func() { _ = r.Close() }()
		var n int
		if err := r.QueryRow("SELECT COUNT(*) FROM messages WHERE id = ?", msg.ID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	st := &cleanupState{lastBackup: time.Now()} // lastBackup=now → no snapshot fires

	// Tick 1 at the 7d default: a 3-day-old expiry is inside the window → kept.
	runCleanupTick(database, st)
	if count() != 1 {
		t.Fatalf("tick 1 (default 7d) must keep the 3-day-old expired message")
	}

	// PUT message_retention=48h, then tick 2: 3-day-old expiry now past the
	// window → purged, with no restart and no new StartCleanup call.
	database.SetSetting("message_retention", "48h")
	runCleanupTick(database, st)
	if count() != 0 {
		t.Fatalf("tick 2 (48h, post-PUT) must purge the 3-day-old expired message")
	}
}

// TestTickEnvTwinsBeatStoredBackupReviewer is AC3: for backup_keep and
// reviewer_ttl_days the env twin wins when valid, else the stored setting, else
// the const — and an invalid env falls through to the setting.
func TestTickEnvTwinsBeatStoredBackupReviewer(t *testing.T) {
	d := newCleanupTestDB(t)
	d.SetSetting("backup_keep", "9")
	d.SetSetting("reviewer_ttl_days", "9")

	// Valid env wins over the stored setting.
	t.Setenv("RELAY_BACKUP_KEEP", "5")
	t.Setenv("RELAY_REVIEWER_TTL_DAYS", "5")
	if got := tickBackupKeep(d); got != 5 {
		t.Fatalf("valid env backup_keep: got %d want 5", got)
	}
	if got := tickReviewerTTL(d); got != 5*24*time.Hour {
		t.Fatalf("valid env reviewer_ttl: got %s want 120h", got)
	}

	// Env unset → the stored setting.
	t.Setenv("RELAY_BACKUP_KEEP", "")
	t.Setenv("RELAY_REVIEWER_TTL_DAYS", "")
	if got := tickBackupKeep(d); got != 9 {
		t.Fatalf("stored backup_keep: got %d want 9", got)
	}
	if got := tickReviewerTTL(d); got != 9*24*time.Hour {
		t.Fatalf("stored reviewer_ttl: got %s want 216h", got)
	}

	// Invalid env → falls through to the stored setting.
	t.Setenv("RELAY_BACKUP_KEEP", "nope")
	t.Setenv("RELAY_REVIEWER_TTL_DAYS", "nope")
	if got := tickBackupKeep(d); got != 9 {
		t.Fatalf("invalid env backup_keep falls to setting: got %d want 9", got)
	}
	if got := tickReviewerTTL(d); got != 9*24*time.Hour {
		t.Fatalf("invalid env reviewer_ttl falls to setting: got %s want 216h", got)
	}

	// No env, no stored setting → the const default.
	fresh := newCleanupTestDB(t)
	t.Setenv("RELAY_BACKUP_KEEP", "")
	t.Setenv("RELAY_REVIEWER_TTL_DAYS", "")
	if got := tickBackupKeep(fresh); got != DefaultBackupKeep {
		t.Fatalf("empty backup_keep: got %d want const %d", got, DefaultBackupKeep)
	}
	if got := tickReviewerTTL(fresh); got != DefaultReviewerTTL {
		t.Fatalf("empty reviewer_ttl: got %s want const %s", got, DefaultReviewerTTL)
	}
}

// TestEnvTwinInvalidValueWarnsOncePerProcess is AC1: an invalid RELAY_BACKUP_KEEP
// / RELAY_REVIEWER_TTL_DAYS logs its invalid line exactly once across repeated
// tick reads (the env>stored>const precedence is unchanged — the reads fall
// through to the stored setting); a valid env logs nothing and wins.
func TestEnvTwinInvalidValueWarnsOncePerProcess(t *testing.T) {
	// envWarnedKeys is package-global: another test may already have warned these
	// names this process, so reset for an order-independent count.
	envWarnedKeys.Delete("RELAY_BACKUP_KEEP")
	envWarnedKeys.Delete("RELAY_REVIEWER_TTL_DAYS")

	d := newCleanupTestDB(t)
	d.SetSetting("backup_keep", "9")
	d.SetSetting("reviewer_ttl_days", "9")

	// Invalid env → falls through to the stored setting; WARN once across 3 ticks.
	t.Setenv("RELAY_BACKUP_KEEP", "abc")
	t.Setenv("RELAY_REVIEWER_TTL_DAYS", "abc")
	out := captureLog(func() {
		for i := 0; i < 3; i++ {
			if got := tickBackupKeep(d); got != 9 {
				t.Fatalf("invalid env backup_keep tick %d: got %d want 9", i, got)
			}
			if got := tickReviewerTTL(d); got != 9*24*time.Hour {
				t.Fatalf("invalid env reviewer_ttl tick %d: got %s want 216h", i, got)
			}
		}
	})
	if n := strings.Count(out, `RELAY_BACKUP_KEEP="abc" invalid`); n != 1 {
		t.Errorf("RELAY_BACKUP_KEEP invalid WARN count = %d, want 1\nlog:\n%s", n, out)
	}
	if n := strings.Count(out, `RELAY_REVIEWER_TTL_DAYS="abc" invalid`); n != 1 {
		t.Errorf("RELAY_REVIEWER_TTL_DAYS invalid WARN count = %d, want 1\nlog:\n%s", n, out)
	}

	// A valid env logs nothing and wins over the stored setting.
	envWarnedKeys.Delete("RELAY_BACKUP_KEEP")
	envWarnedKeys.Delete("RELAY_REVIEWER_TTL_DAYS")
	t.Setenv("RELAY_BACKUP_KEEP", "5")
	t.Setenv("RELAY_REVIEWER_TTL_DAYS", "5")
	out = captureLog(func() {
		if got := tickBackupKeep(d); got != 5 {
			t.Fatalf("valid env backup_keep: got %d want 5", got)
		}
		if got := tickReviewerTTL(d); got != 5*24*time.Hour {
			t.Fatalf("valid env reviewer_ttl: got %s want 120h", got)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("valid env should log nothing, got:\n%s", out)
	}
}

// TestTickTokenUsageRetentionDaysDrivesPurgeAndRollup is AC4: the one key
// token_usage_retention_days drives BOTH the raw-purge window and the rollup
// window. A 5-day-old row survives + is rolled up at the default (14) but is
// purged + excluded from the rollup at 3.
func TestTickTokenUsageRetentionDaysDrivesPurgeAndRollup(t *testing.T) {
	const project = "p-ac4"
	fiveDaysAgo := time.Now().UTC().AddDate(0, 0, -5)
	fiveDayRow := []db.TokenRecord{{
		Project: project, Agent: "a1", Input: 1000, CreatedAt: fiveDaysAgo.Format(time.RFC3339),
	}}
	// lastBackup=now so the tick never attempts a snapshot; lastRollupCatchup=""
	// so the once-per-day full-window rollup fires on this tick.
	newState := func() *cleanupState { return &cleanupState{lastBackup: time.Now()} }

	rawRows := func(d *db.DB) int64 {
		sums, err := d.GetTokenUsageByProject("1970-01-01T00:00:00Z")
		if err != nil {
			t.Fatalf("GetTokenUsageByProject: %v", err)
		}
		var n int64
		for _, s := range sums {
			n += s.CallCount
		}
		return n
	}
	rolledUp := func(d *db.DB) bool {
		sums, err := d.GetTokenUsageDailyByProject(fiveDaysAgo.Format("2006-01-02"))
		if err != nil {
			t.Fatalf("GetTokenUsageDailyByProject: %v", err)
		}
		return len(sums) > 0
	}

	// Default (empty settings → 14 days): the 5-day row survives and is rolled up.
	def := newCleanupTestDB(t)
	if err := def.InsertTokenUsageBatch(fiveDayRow); err != nil {
		t.Fatalf("insert: %v", err)
	}
	runCleanupTick(def, newState())
	if got := rawRows(def); got != 1 {
		t.Fatalf("default 14d must keep the 5-day raw row, got %d rows", got)
	}
	if !rolledUp(def) {
		t.Fatalf("default 14d must roll up the 5-day row")
	}

	// token_usage_retention_days=3: the 5-day row is purged and excluded from the
	// rollup — both cutoffs move together off the one key.
	short := newCleanupTestDB(t)
	short.SetSetting("token_usage_retention_days", "3")
	if err := short.InsertTokenUsageBatch(fiveDayRow); err != nil {
		t.Fatalf("insert: %v", err)
	}
	runCleanupTick(short, newState())
	if got := rawRows(short); got != 0 {
		t.Fatalf("3d must purge the 5-day raw row, got %d rows", got)
	}
	if rolledUp(short) {
		t.Fatalf("3d must exclude the 5-day row from the rollup window")
	}
}

// TestTickEmptySettingsMatchConstsAndSpecDefaults is AC5: with an empty settings
// table every knob the tick reads equals its pre-T2b const, and for each T2b
// Operational key the settingSpecs default string equals that const's encoding —
// so promoting the knobs to the panel introduced no behavior drift.
func TestTickEmptySettingsMatchConstsAndSpecDefaults(t *testing.T) {
	d := newCleanupTestDB(t)
	t.Setenv("RELAY_BACKUP_KEEP", "")
	t.Setenv("RELAY_REVIEWER_TTL_DAYS", "")

	// Empty table → the accessor calls the tick makes read back the consts.
	if got := d.SettingDuration("agent_max_age", AgentMaxAge, 5*time.Minute, 24*time.Hour); got != AgentMaxAge {
		t.Errorf("agent_max_age: got %s want %s", got, AgentMaxAge)
	}
	if got := d.SettingDuration("message_retention", MessageRetention, 24*time.Hour, 90*24*time.Hour); got != MessageRetention {
		t.Errorf("message_retention: got %s want %s", got, MessageRetention)
	}
	if got := d.SettingDuration("audit_log_retention", AuditLogRetention, 7*24*time.Hour, 365*24*time.Hour); got != AuditLogRetention {
		t.Errorf("audit_log_retention: got %s want %s", got, AuditLogRetention)
	}
	if got := d.SettingDuration("deadletter_short_retention", DeadletterShortRetention, 24*time.Hour, 180*24*time.Hour); got != DeadletterShortRetention {
		t.Errorf("deadletter_short_retention: got %s want %s", got, DeadletterShortRetention)
	}
	if got := d.SettingDuration("deadletter_long_retention", DeadletterLongRetention, 24*time.Hour, 730*24*time.Hour); got != DeadletterLongRetention {
		t.Errorf("deadletter_long_retention: got %s want %s", got, DeadletterLongRetention)
	}
	if got := d.SettingInt("token_usage_retention_days", TokenUsageRetentionDays, 1, 90); got != TokenUsageRetentionDays {
		t.Errorf("token_usage_retention_days: got %d want %d", got, TokenUsageRetentionDays)
	}
	if got := d.SettingDuration("ack_notify_age", ACKNotifyAge, time.Minute, 24*time.Hour); got != ACKNotifyAge {
		t.Errorf("ack_notify_age: got %s want %s", got, ACKNotifyAge)
	}
	if got := d.SettingDuration("ack_escalate_age", ACKEscalateAge, time.Minute, 24*time.Hour); got != ACKEscalateAge {
		t.Errorf("ack_escalate_age: got %s want %s", got, ACKEscalateAge)
	}
	if got := d.SettingDuration("foreign_backup_min_age", ForeignBackupMinAge, time.Hour, 30*24*time.Hour); got != ForeignBackupMinAge {
		t.Errorf("foreign_backup_min_age: got %s want %s", got, ForeignBackupMinAge)
	}
	if got := tickBackupKeep(d); got != DefaultBackupKeep {
		t.Errorf("tickBackupKeep: got %d want %d", got, DefaultBackupKeep)
	}
	if got := tickReviewerTTL(d); got != DefaultReviewerTTL {
		t.Errorf("tickReviewerTTL: got %s want %s", got, DefaultReviewerTTL)
	}

	// Each T2b Operational key's spec default equals its const's encoding.
	wantDefault := map[string]string{
		"agent_max_age":              dur(AgentMaxAge),
		"message_retention":          dur(MessageRetention),
		"audit_log_retention":        dur(AuditLogRetention),
		"deadletter_short_retention": dur(DeadletterShortRetention),
		"deadletter_long_retention":  dur(DeadletterLongRetention),
		"token_usage_retention_days": strconv.Itoa(TokenUsageRetentionDays),
		"ack_notify_age":             dur(ACKNotifyAge),
		"ack_escalate_age":           dur(ACKEscalateAge),
		"backup_keep":                strconv.Itoa(DefaultBackupKeep),
		"reviewer_ttl_days":          strconv.Itoa(int(DefaultReviewerTTL / (24 * time.Hour))),
		"foreign_backup_min_age":     dur(ForeignBackupMinAge),
	}
	seen := map[string]bool{}
	for _, s := range settingSpecs {
		want, ok := wantDefault[s.Key]
		if !ok {
			continue
		}
		seen[s.Key] = true
		if s.Group != groupOperational {
			t.Errorf("spec %q: group %q want %q", s.Key, s.Group, groupOperational)
		}
		if s.Default != want {
			t.Errorf("spec %q default %q != const encoding %q (drift — report to wraith-cto, do not edit the spec)", s.Key, s.Default, want)
		}
	}
	for k := range wantDefault {
		if !seen[k] {
			t.Errorf("T2b key %q missing from settingSpecs", k)
		}
	}
}
