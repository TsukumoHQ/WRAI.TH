package relay

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"agent-relay/internal/db"
)

// Cron scheduling (niwa steal #8). A config-defined 6-field cron creates typed
// tickets on a schedule; each ticket flows through the SAME dispatch pipeline as
// a hand-dispatched one (dispatchCore). The relay owns only the scheduling — it
// never runs the work. Inert until the "cron_schedules" setting is populated, so
// the feature is additive and off by default.

const (
	cronTickInterval = 30 * time.Second // sub-minute so no matching minute is missed
	cronSettingKey   = "cron_schedules" // JSON array of CronSchedule
	cronLastFirePfx  = "cron:lastfire:" // per-schedule dedup marker (minute key)
	cronDispatchedBy = "cron"
)

// CronSchedule is one recurring typed-ticket definition, loaded from the
// "cron_schedules" setting. A disabled or malformed entry is skipped.
type CronSchedule struct {
	ID                 string   `json:"id"`   // stable identity (dedup key); required
	Cron               string   `json:"cron"` // 6-field: sec min hour dom month dow
	Project            string   `json:"project"`
	Profile            string   `json:"profile"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	Priority           string   `json:"priority"`
	Goal               string   `json:"goal"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Dod                string   `json:"dod"`
	Enabled            bool     `json:"enabled"`
}

// --- 6-field cron parser ---

// cronField matches one time component: any (*) or a fixed set of allowed values.
type cronField struct {
	any    bool
	values map[int]bool
}

func (f cronField) match(v int) bool {
	if f.any {
		return true
	}
	return f.values[v]
}

// cronSpec is a parsed 6-field expression: sec min hour dom month dow.
type cronSpec struct {
	sec, min, hour, dom, month, dow cronField
	domRestricted, dowRestricted    bool
}

// parseCron parses a 6-field expression (sec min hour day-of-month month
// day-of-week). Each field supports '*', 'n', 'a-b', '*/step', 'a-b/step', and
// comma lists of those. Evaluation is minute-granular (the scheduler ticks the
// minute boundary), so a seconds field should be 0 or '*'.
func parseCron(expr string) (cronSpec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 6 {
		return cronSpec{}, fmt.Errorf("cron: want 6 fields (sec min hour dom month dow), got %d in %q", len(parts), expr)
	}
	bounds := [6][2]int{{0, 59}, {0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	var fields [6]cronField
	for i, p := range parts {
		f, err := parseCronField(p, bounds[i][0], bounds[i][1])
		if err != nil {
			return cronSpec{}, fmt.Errorf("cron field %d (%q): %w", i, p, err)
		}
		fields[i] = f
	}
	return cronSpec{
		sec: fields[0], min: fields[1], hour: fields[2],
		dom: fields[3], month: fields[4], dow: fields[5],
		domRestricted: !fields[3].any, dowRestricted: !fields[5].any,
	}, nil
}

func parseCronField(s string, min, max int) (cronField, error) {
	values := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return cronField{}, fmt.Errorf("empty term")
		}
		// step: base/step
		step := 1
		base := part
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			base = part[:slash]
			st, err := strconv.Atoi(part[slash+1:])
			if err != nil || st <= 0 {
				return cronField{}, fmt.Errorf("bad step %q", part)
			}
			step = st
		}
		lo, hi := min, max
		switch {
		case base == "*":
			// full range with step
		case strings.IndexByte(base, '-') >= 0:
			dash := strings.IndexByte(base, '-')
			a, err1 := strconv.Atoi(base[:dash])
			b, err2 := strconv.Atoi(base[dash+1:])
			if err1 != nil || err2 != nil {
				return cronField{}, fmt.Errorf("bad range %q", base)
			}
			lo, hi = a, b
		default:
			n, err := strconv.Atoi(base)
			if err != nil {
				return cronField{}, fmt.Errorf("bad number %q", base)
			}
			lo, hi = n, n
		}
		if lo < min || hi > max || lo > hi {
			return cronField{}, fmt.Errorf("value out of range [%d,%d]: %q", min, max, part)
		}
		for v := lo; v <= hi; v += step {
			values[v] = true
		}
	}
	// '*' with no step and no list → any (cheaper match).
	if s == "*" {
		return cronField{any: true}, nil
	}
	return cronField{values: values}, nil
}

// matches reports whether t (evaluated at the minute boundary, seconds forced to
// 0) satisfies the spec. Day-of-month and day-of-week follow standard cron OR
// semantics: when BOTH are restricted the tick matches if EITHER matches; when
// only one is restricted that one governs.
func (s cronSpec) matches(t time.Time) bool {
	if !s.sec.match(t.Second()) || !s.min.match(t.Minute()) || !s.hour.match(t.Hour()) || !s.month.match(int(t.Month())) {
		return false
	}
	domOK := s.dom.match(t.Day())
	dowOK := s.dow.match(int(t.Weekday()))
	switch {
	case s.domRestricted && s.dowRestricted:
		return domOK || dowOK
	case s.domRestricted:
		return domOK
	case s.dowRestricted:
		return dowOK
	default:
		return true
	}
}

// --- Scheduler ---

// StartCronScheduler launches the cron goroutine. It ticks sub-minute, and on
// each new minute fires every enabled schedule whose expression matches, exactly
// once per minute (dedup persisted in the settings KV, so a restart never
// re-fires an already-fired minute). Stops when done is closed.
func (r *Relay) StartCronScheduler(done <-chan struct{}) {
	if r.Handlers == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(cronTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.runCronTick(time.Now().UTC())
			}
		}
	}()
}

// loadCronSchedules reads and parses the configured schedules. A malformed
// document yields none (logged) rather than a partial fleet surprise.
func (r *Relay) loadCronSchedules() []CronSchedule {
	raw := strings.TrimSpace(r.DB.GetSetting(cronSettingKey))
	if raw == "" {
		return nil
	}
	var out []CronSchedule
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		log.Printf("cron: parse %s: %v", cronSettingKey, err)
		return nil
	}
	return out
}

// runCronTick evaluates every schedule against the minute containing now and
// fires the matches once. Exported-to-package for deterministic testing.
func (r *Relay) runCronTick(now time.Time) {
	schedules := r.loadCronSchedules()
	if len(schedules) == 0 {
		return
	}
	minute := now.Truncate(time.Minute)
	minuteKey := minute.Format(time.RFC3339)
	for _, sc := range schedules {
		if !sc.Enabled || sc.ID == "" {
			continue
		}
		spec, err := parseCron(sc.Cron)
		if err != nil {
			log.Printf("cron: schedule %q: %v", sc.ID, err)
			continue
		}
		if !spec.matches(minute) {
			continue
		}
		// Idempotency: a schedule fires at most once per minute, across restarts.
		fireKey := cronLastFirePfx + sc.ID
		if r.DB.GetSetting(fireKey) == minuteKey {
			continue
		}
		r.fireCronSchedule(sc, minuteKey)
	}
}

// persistFireMarker writes the per-schedule dedup marker and confirms it is
// durable by reading it back. SetSetting swallows its write error (projects.go),
// so the read-back is the guarantee: a true result means the same-minute guard
// will see the marker on the next tick, so the fire cannot be duplicated. A
// false result (the write silently failed) tells the caller to skip the fire.
func (r *Relay) persistFireMarker(key, minuteKey string) bool {
	r.DB.SetSetting(key, minuteKey)
	return r.DB.GetSetting(key) == minuteKey
}

// fireCronSchedule creates the typed ticket through the normal dispatch pipeline
// and records the dedup marker. The marker is written FIRST, then read back and
// CONFIRMED durable BEFORE the dispatch: SetSetting swallows its write error
// (projects.go), so a silent write failure would leave the same-minute guard
// blind and the next 30s tick would re-fire a duplicate. Gating the dispatch on
// a confirmed-persisted marker makes at-most-once hold even if the write fails —
// we would rather skip a fire (a missing ticket, self-corrects next occurrence)
// than emit a duplicate. A confirmed marker means the next tick's guard sees it.
func (r *Relay) fireCronSchedule(sc CronSchedule, minuteKey string) {
	project := sc.Project
	if project == "" {
		project = "default"
	}
	priority := sc.Priority
	if priority == "" {
		priority = "P2"
	}
	ac := "[]"
	if len(sc.AcceptanceCriteria) > 0 {
		if b, err := json.Marshal(sc.AcceptanceCriteria); err == nil {
			ac = string(b)
		}
	}
	ticket := db.TypedTicket{Goal: sc.Goal, AcceptanceCriteria: ac, Dod: sc.Dod}

	if !r.persistFireMarker(cronLastFirePfx+sc.ID, minuteKey) {
		// The dedup marker did not persist — firing now risks a duplicate on the
		// next tick, so skip this occurrence instead.
		log.Printf("cron: dedup marker for %q did not persist; skipping fire to avoid a duplicate ticket", sc.ID)
		return
	}
	if _, _, err := r.Handlers.dispatchCore(project, cronDispatchedBy, sc.Profile, sc.Title, sc.Description, priority, nil, nil, ticket, false); err != nil {
		log.Printf("cron: dispatch schedule %q: %v", sc.ID, err)
		return
	}
	log.Printf("cron: fired schedule %q → ticket on profile %q (%s)", sc.ID, sc.Profile, project)
}
