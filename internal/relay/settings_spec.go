package relay

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// settingSpec is the single server-side description of one configuration key. It
// drives three things at once so they can never drift: (i) the PUT allowlist
// (writableKeys / writableSettings), (ii) PUT validation by kind + bounds +
// cross-key rules, (iii) the GET /api/settings per-key metadata (the frozen
// contract in trovex 6f73f179). Design record: wraith-v2-config-surface-20260907.
type settingSpec struct {
	Key      string      // snake_case, unique
	Group    string      // one of settingGroups
	Kind     settingKind // bool|int|duration|enum|string|json|secret
	Source   string      // classification: db | env | code (code = compile-time const)
	EnvName  string      // env twin, "" when none
	Writable bool        // settable via PUT /api/settings
	Secret   bool        // value never returned/logged; GET exposes `set` only
	Min      string      // int: decimal; duration: time.Duration.String(); "" = unbounded
	Max      string      // same encoding as Min; "" = unbounded
	Enum     []string    // enum: allowed values
	Default  string      // same encoding as value; "" when none
	Note     string      // free text for the panel
}

type settingKind string

const (
	kindBool     settingKind = "bool"
	kindInt      settingKind = "int"
	kindDuration settingKind = "duration"
	kindEnum     settingKind = "enum"
	kindString   settingKind = "string"
	kindJSON     settingKind = "json"
	kindSecret   settingKind = "secret"
)

const (
	groupConsole     = "console"
	groupLinear      = "linear"
	groupFederation  = "federation"
	groupServer      = "server"
	groupOperational = "operational"
	groupTiming      = "timing"
)

// settingGroups is the fixed render order returned as GET's top-level `groups`.
var settingGroups = []string{groupConsole, groupLinear, groupFederation, groupServer, groupOperational, groupTiming}

// dur is a tiny helper so bounds/defaults are written as real durations and
// encoded exactly as GET returns them (time.Duration.String()).
func dur(d time.Duration) string { return d.String() }

// settingSpecs covers every key of the configuration surface (design record D2).
// Durations are encoded via time.Duration.String(); ints as decimal strings.
var settingSpecs = []settingSpec{
	// ---- Console ----
	{Key: "sun_type", Group: groupConsole, Kind: kindInt, Source: "db", Writable: true, Min: "1", Default: "1", Note: "console sun/dyson variant index"},

	// ---- Linear (7 writable + the RO webhook secret) ----
	{Key: "linear_enabled", Group: groupLinear, Kind: kindBool, Source: "env", EnvName: "RELAY_LINEAR_MODE", Writable: true, Default: "0"},
	{Key: "linear_api_key", Group: groupLinear, Kind: kindSecret, Source: "env", EnvName: "LINEAR_API_KEY", Writable: true, Secret: true},
	{Key: "linear_team_key", Group: groupLinear, Kind: kindString, Source: "env", EnvName: "LINEAR_TEAM_KEY", Writable: true},
	{Key: "linear_project", Group: groupLinear, Kind: kindString, Source: "db", Writable: true},
	{Key: "linear_reconcile_interval", Group: groupLinear, Kind: kindDuration, Source: "env", EnvName: "RELAY_LINEAR_RECONCILE_INTERVAL", Writable: true, Min: dur(30 * time.Second), Max: dur(time.Hour), Default: dur(time.Minute)},
	{Key: "linear_routing", Group: groupLinear, Kind: kindJSON, Source: "db", Writable: true},
	{Key: "linear_project_map", Group: groupLinear, Kind: kindJSON, Source: "db", Writable: true},
	{Key: "linear_webhook_secret", Group: groupLinear, Kind: kindSecret, Source: "env", EnvName: "LINEAR_WEBHOOK_SECRET", Secret: true, Note: "HMAC secret for inbound Linear webhooks"},

	// ---- Federation (RO summary in panel; still API-writable via the federation editor) ----
	{Key: "federation_peers", Group: groupFederation, Kind: kindJSON, Source: "env", EnvName: "RELAY_FEDERATION_PEERS", Writable: true, Note: "edited on the federation page"},

	// ---- Server (all RO env) ----
	{Key: "bind", Group: groupServer, Kind: kindString, Source: "env", EnvName: "RELAY_BIND", Default: "127.0.0.1"},
	{Key: "port", Group: groupServer, Kind: kindInt, Source: "env", EnvName: "PORT", Default: "8090"},
	{Key: "api_key", Group: groupServer, Kind: kindSecret, Source: "env", EnvName: "RELAY_API_KEY", Secret: true, Note: "unset = auth off"},
	{Key: "cors_origins", Group: groupServer, Kind: kindString, Source: "env", EnvName: "RELAY_CORS_ORIGINS"},
	{Key: "max_body", Group: groupServer, Kind: kindInt, Source: "env", EnvName: "RELAY_MAX_BODY", Default: "1048576"},
	{Key: "rate_limit", Group: groupServer, Kind: kindInt, Source: "env", EnvName: "RELAY_RATE_LIMIT", Default: "0"},
	{Key: "db_path", Group: groupServer, Kind: kindString, Source: "env", EnvName: "RELAY_DB", Default: "~/.agent-relay/relay.db"},
	{Key: "ui_dir", Group: groupServer, Kind: kindString, Source: "env", EnvName: "RELAY_UI_DIR", Note: "embedded when unset"},
	{Key: "log_level", Group: groupServer, Kind: kindString, Source: "env", EnvName: "RELAY_LOG_LEVEL", Default: "info"},
	{Key: "log_format", Group: groupServer, Kind: kindString, Source: "env", EnvName: "RELAY_LOG_FORMAT", Default: "text"},
	{Key: "trust_loopback", Group: groupServer, Kind: kindBool, Source: "env", EnvName: "RELAY_TRUST_LOOPBACK", Default: "1"},
	{Key: "webhook_secret", Group: groupServer, Kind: kindSecret, Source: "env", EnvName: "RELAY_WEBHOOK_SECRET", Secret: true},
	{Key: "github_webhook_secret", Group: groupServer, Kind: kindSecret, Source: "env", EnvName: "RELAY_GITHUB_WEBHOOK_SECRET", Secret: true},
	{Key: "signal_webhook_secret", Group: groupServer, Kind: kindSecret, Source: "env", EnvName: "RELAY_SIGNAL_WEBHOOK_SECRET", Secret: true},

	// ---- Operational writable (read at tick time in T2b) ----
	{Key: "agent_max_age", Group: groupOperational, Kind: kindDuration, Source: "db", Writable: true, Min: dur(5 * time.Minute), Max: dur(24 * time.Hour), Default: dur(30 * time.Minute)},
	{Key: "message_retention", Group: groupOperational, Kind: kindDuration, Source: "db", Writable: true, Min: dur(24 * time.Hour), Max: dur(90 * 24 * time.Hour), Default: dur(7 * 24 * time.Hour)},
	{Key: "audit_log_retention", Group: groupOperational, Kind: kindDuration, Source: "db", Writable: true, Min: dur(7 * 24 * time.Hour), Max: dur(365 * 24 * time.Hour), Default: dur(90 * 24 * time.Hour)},
	{Key: "deadletter_short_retention", Group: groupOperational, Kind: kindDuration, Source: "db", Writable: true, Min: dur(24 * time.Hour), Max: dur(180 * 24 * time.Hour), Default: dur(30 * 24 * time.Hour)},
	{Key: "deadletter_long_retention", Group: groupOperational, Kind: kindDuration, Source: "db", Writable: true, Min: dur(24 * time.Hour), Max: dur(730 * 24 * time.Hour), Default: dur(180 * 24 * time.Hour), Note: "must be >= deadletter_short_retention"},
	{Key: "token_usage_retention_days", Group: groupOperational, Kind: kindInt, Source: "db", Writable: true, Min: "1", Max: "90", Default: "14", Note: "drives both token-usage purge and rollup window"},
	{Key: "ack_notify_age", Group: groupOperational, Kind: kindDuration, Source: "db", Writable: true, Min: dur(time.Minute), Max: dur(24 * time.Hour), Default: dur(15 * time.Minute)},
	{Key: "ack_escalate_age", Group: groupOperational, Kind: kindDuration, Source: "db", Writable: true, Min: dur(time.Minute), Max: dur(24 * time.Hour), Default: dur(45 * time.Minute), Note: "must be > ack_notify_age"},
	{Key: "backup_keep", Group: groupOperational, Kind: kindInt, Source: "env", EnvName: "RELAY_BACKUP_KEEP", Writable: true, Min: "1", Max: "24", Default: "3", Note: "env wins over setting"},
	{Key: "reviewer_ttl_days", Group: groupOperational, Kind: kindInt, Source: "env", EnvName: "RELAY_REVIEWER_TTL_DAYS", Writable: true, Min: "1", Max: "90", Default: "7", Note: "env wins over setting"},
	{Key: "foreign_backup_min_age", Group: groupOperational, Kind: kindDuration, Source: "db", Writable: true, Min: dur(time.Hour), Max: dur(30 * 24 * time.Hour), Default: dur(24 * time.Hour)},
	{Key: "activity_idle_seconds", Group: groupOperational, Kind: kindInt, Source: "db", Writable: true, Min: "5", Max: "86400", Default: "120"},
	{Key: "activity_waiting_seconds", Group: groupOperational, Kind: kindInt, Source: "db", Writable: true, Min: "5", Max: "86400", Default: "10"},
	{Key: "activity_exit_seconds", Group: groupOperational, Kind: kindInt, Source: "db", Writable: true, Min: "5", Max: "86400", Default: "300"},
	{Key: "cost_default_model", Group: groupOperational, Kind: kindString, Source: "db", Writable: true, Default: "opus"},

	// ---- Operational RO (destructive toggles / dynamic keys stay out of the panel) ----
	{Key: "limbo_sweep_apply", Group: groupOperational, Kind: kindBool, Source: "db", Default: "0", Note: "destructive; env/SQL only"},
	{Key: "dangling_board_apply", Group: groupOperational, Kind: kindBool, Source: "db", Default: "0", Note: "destructive; env/SQL only"},
	{Key: "cron_schedules", Group: groupOperational, Kind: kindJSON, Source: "db"},
	{Key: "github_webhook_project", Group: groupOperational, Kind: kindString, Source: "db"},
	{Key: "signal_webhook_project", Group: groupOperational, Kind: kindString, Source: "db"},

	// ---- Timing (RO, compile-time consts) ----
	{Key: "purge_interval", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(5 * time.Minute)},
	{Key: "ack_check_interval", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(5 * time.Minute)},
	{Key: "backup_interval", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(time.Hour)},
	{Key: "writer_timeout", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(15 * time.Second), Note: "compile-time (code); must stay < referential_scan_timeout"},
	{Key: "writer_slow_wait", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(2 * time.Second)},
	{Key: "referential_scan_timeout", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(30 * time.Second), Note: "compile-time (code); must stay > writer_timeout"},
	{Key: "task_sweep_interval", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(2 * time.Minute)},
	{Key: "notify_sweep_interval", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(1500 * time.Millisecond)},
	{Key: "digest_tick", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(time.Minute)},
	{Key: "cron_tick", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(30 * time.Second)},
	{Key: "ingest_tick", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(2 * time.Second)},
	{Key: "sse_ping", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(25 * time.Second)},
	{Key: "dashboard_poll", Group: groupTiming, Kind: kindDuration, Source: "code", Default: dur(5 * time.Second)},
}

// specByKey indexes settingSpecs. Built once; settingSpecs is never mutated.
var specByKey = func() map[string]settingSpec {
	m := make(map[string]settingSpec, len(settingSpecs))
	for _, s := range settingSpecs {
		m[s.Key] = s
	}
	return m
}()

// writableSettings is the PANEL-facing allowlist (console/linear/federation
// groups) kept in lockstep with v2/settings.js FIELDS via the
// FieldsMatchWritableAllowlist contract test. The FULL API PUT allowlist is
// writableKeys() — every Writable spec, including Operational keys the panel
// does not render yet (widened in T2c). Var name preserved so the panel test
// keeps compiling.
var writableSettings = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range settingSpecs {
		if s.Writable && (s.Group == groupConsole || s.Group == groupLinear || s.Group == groupFederation) {
			m[s.Key] = true
		}
	}
	return m
}()

// writableKeys is the full PUT allowlist derived from the spec: every key with
// Writable=true (the 9 legacy panel keys + the 15 Operational knobs).
func writableKeys() map[string]bool {
	m := make(map[string]bool, len(settingSpecs))
	for _, s := range settingSpecs {
		if s.Writable {
			m[s.Key] = true
		}
	}
	return m
}

// boundsJSON renders the per-kind `bounds` object of the frozen contract.
func (s settingSpec) boundsJSON() map[string]any {
	switch s.Kind {
	case kindInt, kindDuration:
		return map[string]any{"min": s.Min, "max": s.Max}
	case kindEnum:
		vals := s.Enum
		if vals == nil {
			vals = []string{}
		}
		return map[string]any{"values": vals}
	default:
		return map[string]any{}
	}
}

// resolveSetting computes the effective (value, set, source) for one key.
// Precedence: env > setting > default; Timing consts resolve to source=code.
// `set` is true whenever the value comes from env/setting/code (i.e. not the
// bare default). For secrets `value` is always "" — `set` is the only signal.
func (r *Relay) resolveSetting(s settingSpec) (value string, set bool, source string) {
	if s.Source == "code" {
		return s.Default, true, "code"
	}
	if s.EnvName != "" {
		if ev := os.Getenv(s.EnvName); ev != "" {
			if s.Secret {
				return "", true, "env"
			}
			return ev, true, "env"
		}
	}
	if dv := r.DB.GetSetting(s.Key); dv != "" {
		if s.Secret {
			return "", true, "setting"
		}
		return dv, true, "setting"
	}
	if s.Secret {
		return "", false, "default"
	}
	return s.Default, false, "default"
}

// validateValue checks a single non-null value against its spec's kind + bounds.
// Returns "" when valid, else a human-readable detail for the 400 response.
func validateValue(s settingSpec, val string) string {
	switch s.Kind {
	case kindBool:
		if val != "0" && val != "1" {
			return "must be 0 or 1"
		}
	case kindInt:
		n, err := strconv.Atoi(val)
		if err != nil {
			return "must be an integer"
		}
		if s.Min != "" {
			if mn, _ := strconv.Atoi(s.Min); n < mn {
				return "below minimum " + s.Min
			}
		}
		if s.Max != "" {
			if mx, _ := strconv.Atoi(s.Max); n > mx {
				return "above maximum " + s.Max
			}
		}
	case kindDuration:
		d, err := time.ParseDuration(val)
		if err != nil {
			return "must be a duration (e.g. 30s, 5m, 24h)"
		}
		if s.Min != "" {
			if mn, _ := time.ParseDuration(s.Min); d < mn {
				return "below minimum " + s.Min
			}
		}
		if s.Max != "" {
			if mx, _ := time.ParseDuration(s.Max); d > mx {
				return "above maximum " + s.Max
			}
		}
	case kindEnum:
		for _, v := range s.Enum {
			if v == val {
				return ""
			}
		}
		return "must be one of " + strings.Join(s.Enum, ", ")
	case kindJSON:
		if !json.Valid([]byte(val)) {
			return "must be valid JSON"
		}
	case kindString, kindSecret:
		// no per-kind constraint; bounds/cross-key handled elsewhere
	}
	return ""
}

// validateSettings runs the WHOLE-REQUEST validation for a PUT body before any
// value is applied. body values are *string: nil = explicit clear (JSON null).
// Returns (200,"","") when the whole body is applicable, else the status
// (403 unknown/non-writable, 400 bad value/bounds/cross-key), offending key and
// detail. Nothing is written here — the caller applies only on 200.
func (r *Relay) validateSettings(body map[string]*string) (status int, key, detail string) {
	// 1. Allowlist — any unknown or non-writable key rejects the whole request.
	for k := range body {
		s, ok := specByKey[k]
		if !ok || !s.Writable {
			return http.StatusForbidden, k, ""
		}
	}
	// 2. Per-key kind + bounds.
	for k, v := range body {
		s := specByKey[k]
		if v == nil {
			continue // explicit clear
		}
		if s.Secret && *v == "" {
			continue // "" on a secret = unchanged
		}
		if d := validateValue(s, *v); d != "" {
			return http.StatusBadRequest, k, d
		}
	}
	// 3. Cross-key rules, evaluated against the effective value (body value if
	// present — null → default — else the current stored value, else default).
	eff := func(k string) string {
		if v, ok := body[k]; ok {
			if v == nil || *v == "" {
				return specByKey[k].Default
			}
			return *v
		}
		if cur := r.DB.GetSetting(k); cur != "" {
			return cur
		}
		return specByKey[k].Default
	}
	if _, in1 := body["ack_notify_age"]; in1 {
		if d := crossDurationGreater(eff, "ack_escalate_age", "ack_notify_age"); d != "" {
			return http.StatusBadRequest, "ack_escalate_age", d
		}
	} else if _, in2 := body["ack_escalate_age"]; in2 {
		if d := crossDurationGreater(eff, "ack_escalate_age", "ack_notify_age"); d != "" {
			return http.StatusBadRequest, "ack_escalate_age", d
		}
	}
	if _, in3 := body["deadletter_short_retention"]; in3 {
		if d := crossDurationGreaterEqual(eff, "deadletter_long_retention", "deadletter_short_retention"); d != "" {
			return http.StatusBadRequest, "deadletter_long_retention", d
		}
	} else if _, in4 := body["deadletter_long_retention"]; in4 {
		if d := crossDurationGreaterEqual(eff, "deadletter_long_retention", "deadletter_short_retention"); d != "" {
			return http.StatusBadRequest, "deadletter_long_retention", d
		}
	}
	return http.StatusOK, "", ""
}

func crossDurationGreater(eff func(string) string, hi, lo string) string {
	h, herr := time.ParseDuration(eff(hi))
	l, lerr := time.ParseDuration(eff(lo))
	if herr != nil || lerr != nil {
		return ""
	}
	if !(h > l) {
		return hi + " must be greater than " + lo
	}
	return ""
}

func crossDurationGreaterEqual(eff func(string) string, hi, lo string) string {
	h, herr := time.ParseDuration(eff(hi))
	l, lerr := time.ParseDuration(eff(lo))
	if herr != nil || lerr != nil {
		return ""
	}
	if h < l {
		return hi + " must be >= " + lo
	}
	return ""
}

// settingsMetadata builds the frozen-contract `settings` array: one entry per
// spec in group/spec order, every entry carrying all 12 fields.
func (r *Relay) settingsMetadata() []map[string]any {
	out := make([]map[string]any, 0, len(settingSpecs))
	for _, s := range settingSpecs {
		value, set, source := r.resolveSetting(s)
		out = append(out, map[string]any{
			"key":      s.Key,
			"group":    s.Group,
			"kind":     string(s.Kind),
			"value":    value,
			"set":      set,
			"source":   source,
			"writable": s.Writable,
			"secret":   s.Secret,
			"env_name": s.EnvName,
			"bounds":   s.boundsJSON(),
			"default":  s.Default,
			"note":     s.Note,
		})
	}
	return out
}
