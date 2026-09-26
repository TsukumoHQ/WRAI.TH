package relay

import (
	"bytes"
	"database/sql"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// getSettingEntry returns the settings[] metadata entry for key from a GET
// /api/settings response, and whether it was found.
func getSettingEntry(t *testing.T, body map[string]any, key string) (map[string]any, bool) {
	t.Helper()
	arr, ok := body["settings"].([]any)
	if !ok {
		t.Fatalf("GET /settings: `settings` is not an array: %T", body["settings"])
	}
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("settings entry is not an object: %T", e)
		}
		if m["key"] == key {
			return m, true
		}
	}
	return nil, false
}

// TestSettingsSpecAllowlistDerived (T2a AC1): writableKeys() is the spec-derived
// full PUT allowlist (31 = 9 legacy + 22 Operational), contains no evil_key;
// apiPutSetting consults the spec — a legacy key and an Operational key both
// apply via PUT; spec keys are unique and every group is valid.
func TestSettingsSpecAllowlistDerived(t *testing.T) {
	legacy := []string{
		"sun_type", "linear_enabled", "linear_api_key", "linear_team_key",
		"linear_project", "linear_reconcile_interval", "linear_routing",
		"linear_project_map", "federation_peers",
	}
	operational := []string{
		"agent_max_age", "message_retention", "audit_log_retention",
		"deadletter_short_retention", "deadletter_long_retention",
		"token_usage_retention_days", "ack_notify_age", "ack_escalate_age",
		"backup_keep", "reviewer_ttl_days", "foreign_backup_min_age",
		"activity_idle_seconds", "activity_waiting_seconds",
		"activity_exit_seconds", "cost_default_model",
		"ack_manager_age", "ack_human_age", "answer_reply_age", "answer_role_age",
		"class_budget_mode", "attribution_share", "knowledge_min_compaction_lag",
	}

	wk := writableKeys()
	wantWK := map[string]bool{}
	for _, k := range append(append([]string{}, legacy...), operational...) {
		wantWK[k] = true
	}
	if len(wk) != 31 || len(wantWK) != 31 {
		t.Fatalf("writableKeys size %d, expected set size %d, want 31", len(wk), len(wantWK))
	}
	for k := range wantWK {
		if !wk[k] {
			t.Errorf("writableKeys missing %q", k)
		}
	}
	for k := range wk {
		if !wantWK[k] {
			t.Errorf("writableKeys has unexpected %q", k)
		}
	}

	// evil_key is not in the allowlist.
	if wk["evil_key"] {
		t.Error("writableKeys must not contain evil_key")
	}

	// Spec keys unique; every group valid.
	validGroup := map[string]bool{
		groupConsole: true, groupLinear: true, groupFederation: true,
		groupServer: true, groupOperational: true, groupTiming: true,
	}
	seen := map[string]bool{}
	for _, s := range settingSpecs {
		if seen[s.Key] {
			t.Errorf("duplicate spec key %q", s.Key)
		}
		seen[s.Key] = true
		if !validGroup[s.Group] {
			t.Errorf("spec key %q has invalid group %q", s.Key, s.Group)
		}
	}

	// apiPutSetting consults the spec: a legacy panel key and an Operational key
	// both apply via PUT through the same allowlist.
	r := testRelay(t)
	if w := doAPI(r, http.MethodPut, "/settings", `{"sun_type":"2"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT legacy key: status %d, want 200\nbody: %s", w.Code, w.Body.String())
	}
	if got := r.DB.GetSetting("sun_type"); got != "2" {
		t.Errorf("sun_type stored %q, want 2", got)
	}
	if w := doAPI(r, http.MethodPut, "/settings", `{"message_retention":"48h"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT operational key: status %d, want 200\nbody: %s", w.Code, w.Body.String())
	}
	if got := r.DB.GetSetting("message_retention"); got != "48h" {
		t.Errorf("message_retention stored %q, want 48h", got)
	}
}

// TestSettingsPutBoundsAndCrossKey (T2a AC2): out-of-bounds and cross-key PUTs
// are rejected 400 whole-request with nothing applied; a valid pair applies.
func TestSettingsPutBoundsAndCrossKey(t *testing.T) {
	r := testRelay(t)

	// Below min 24h → 400, nothing stored.
	w := doAPI(r, http.MethodPut, "/settings", `{"message_retention":"1h"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("message_retention=1h: status %d, want 400\nbody: %s", w.Code, w.Body.String())
	}
	j := decodeJSON(t, w)
	errStr, _ := j["error"].(string)
	if !strings.Contains(errStr, "invalid value") || j["key"] != "message_retention" || j["detail"] == "" {
		t.Errorf("400 body = %v, want error containing \"invalid value\" + key:message_retention + detail:non-empty", j)
	}
	if got := r.DB.GetSetting("message_retention"); got != "" {
		t.Errorf("message_retention should be unchanged (empty), got %q", got)
	}

	// ack_escalate_age=45m stored; PUT ack_notify_age=50m ALONE → 400 (50m !< 45m),
	// nothing applied.
	r.DB.SetSetting("ack_escalate_age", "45m")
	w = doAPI(r, http.MethodPut, "/settings", `{"ack_notify_age":"50m"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ack_notify_age=50m alone: status %d, want 400\nbody: %s", w.Code, w.Body.String())
	}
	if got := r.DB.GetSetting("ack_notify_age"); got != "" {
		t.Errorf("ack_notify_age should not be applied, got %q", got)
	}
	if got := r.DB.GetSetting("ack_escalate_age"); got != "45m" {
		t.Errorf("ack_escalate_age must stay 45m, got %q", got)
	}

	// Valid pair → 200, both stored.
	w = doAPI(r, http.MethodPut, "/settings", `{"ack_notify_age":"50m","ack_escalate_age":"60m"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("valid ack pair: status %d, want 200\nbody: %s", w.Code, w.Body.String())
	}
	if got := r.DB.GetSetting("ack_notify_age"); got != "50m" {
		t.Errorf("ack_notify_age stored %q, want 50m", got)
	}
	if got := r.DB.GetSetting("ack_escalate_age"); got != "60m" {
		t.Errorf("ack_escalate_age stored %q, want 60m", got)
	}
}

// TestSettingsGetFrozenShape (T2a AC3): GET returns the frozen shape — groups in
// fixed order, every settings entry carrying all 12 fields; an env-set secret
// resolves source=env/set=true; a Timing const resolves source=code, RO, value=15s.
func TestSettingsGetFrozenShape(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "env-linear-key-xyz")
	r := testRelay(t)

	w := doAPI(r, http.MethodGet, "/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings: status %d, want 200", w.Code)
	}
	body := decodeJSON(t, w)

	// groups in the fixed order.
	gs, ok := body["groups"].([]any)
	if !ok {
		t.Fatalf("`groups` is not an array: %T", body["groups"])
	}
	wantGroups := []string{"console", "linear", "federation", "server", "operational", "timing"}
	if len(gs) != len(wantGroups) {
		t.Fatalf("groups len %d, want %d", len(gs), len(wantGroups))
	}
	for i, g := range wantGroups {
		if gs[i] != g {
			t.Errorf("groups[%d] = %v, want %q", i, gs[i], g)
		}
	}

	// Every entry has all 12 fields.
	fields := []string{"key", "group", "kind", "value", "set", "source", "writable", "secret", "env_name", "bounds", "default", "note"}
	arr, ok := body["settings"].([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("`settings` missing/empty: %T", body["settings"])
	}
	for _, e := range arr {
		m := e.(map[string]any)
		if len(m) != len(fields) {
			t.Errorf("entry %v has %d fields, want %d", m["key"], len(m), len(fields))
		}
		for _, f := range fields {
			if _, present := m[f]; !present {
				t.Errorf("entry %v missing field %q", m["key"], f)
			}
		}
	}

	// linear_api_key: env-resolved secret.
	if e, ok := getSettingEntry(t, body, "linear_api_key"); ok {
		if e["source"] != "env" || e["set"] != true || e["value"] != "" {
			t.Errorf("linear_api_key = {source:%v set:%v value:%v}, want {env,true,\"\"}", e["source"], e["set"], e["value"])
		}
	} else {
		t.Error("linear_api_key entry missing")
	}

	// writer_timeout: compile-time const.
	if e, ok := getSettingEntry(t, body, "writer_timeout"); ok {
		if e["source"] != "code" || e["writable"] != false || e["value"] != "15s" {
			t.Errorf("writer_timeout = {source:%v writable:%v value:%v}, want {code,false,15s}", e["source"], e["writable"], e["value"])
		}
	} else {
		t.Error("writer_timeout entry missing")
	}
}

// TestSettingsSecretHandling (T2a AC4): a secret's GET value is always empty and
// `set` tracks presence; PUT "" leaves it unchanged; PUT null clears it; and the
// secret value never appears in the server log for any of these requests.
func TestSettingsSecretHandling(t *testing.T) {
	// No LINEAR_API_KEY in env → DB is the resolved source for this test.
	t.Setenv("LINEAR_API_KEY", "")
	r := testRelay(t)

	const secret = "sk-super-secret-value-9f3a"

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	// Set it.
	if w := doAPI(r, http.MethodPut, "/settings", `{"linear_api_key":"`+secret+`"}`); w.Code != http.StatusOK {
		t.Fatalf("set linear_api_key: status %d\nbody: %s", w.Code, w.Body.String())
	}
	body := decodeJSON(t, doAPI(r, http.MethodGet, "/settings", ""))
	if e, ok := getSettingEntry(t, body, "linear_api_key"); ok {
		if e["value"] != "" || e["set"] != true || e["source"] != "setting" {
			t.Errorf("after set: {value:%v set:%v source:%v}, want {\"\",true,setting}", e["value"], e["set"], e["source"])
		}
	} else {
		t.Fatal("linear_api_key entry missing")
	}

	// PUT "" = unchanged.
	if w := doAPI(r, http.MethodPut, "/settings", `{"linear_api_key":""}`); w.Code != http.StatusOK {
		t.Fatalf("put empty secret: status %d", w.Code)
	}
	if got := r.DB.GetSetting("linear_api_key"); got != secret {
		t.Errorf("PUT \"\" must leave secret unchanged, got %q", got)
	}

	// PUT null = explicit clear.
	if w := doAPI(r, http.MethodPut, "/settings", `{"linear_api_key":null}`); w.Code != http.StatusOK {
		t.Fatalf("clear secret: status %d", w.Code)
	}
	body = decodeJSON(t, doAPI(r, http.MethodGet, "/settings", ""))
	if e, ok := getSettingEntry(t, body, "linear_api_key"); ok {
		if e["set"] != false || e["value"] != "" {
			t.Errorf("after clear: {set:%v value:%v}, want {false,\"\"}", e["set"], e["value"])
		}
	} else {
		t.Fatal("linear_api_key entry missing after clear")
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("server log leaked the secret value:\n%s", buf.String())
	}
}

// TestSettingsUnknownKeyWholeRequest (T2a AC5): a PUT carrying an unknown key
// alongside a valid one is rejected 403 whole-request and applies nothing.
func TestSettingsUnknownKeyWholeRequest(t *testing.T) {
	r := testRelay(t)

	w := doAPI(r, http.MethodPut, "/settings", `{"sun_type":"2","evil_key":"x"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown key: status %d, want 403\nbody: %s", w.Code, w.Body.String())
	}
	j := decodeJSON(t, w)
	errStr, _ := j["error"].(string)
	if !strings.Contains(errStr, "not writable") || j["key"] != "evil_key" {
		t.Errorf("403 body = %v, want error containing \"not writable\" + key:evil_key", j)
	}
	// Whole-request: the valid key was NOT applied.
	if got := r.DB.GetSetting("sun_type"); got != "" {
		t.Errorf("sun_type must be unchanged after a rejected whole request, got %q", got)
	}
}

// TestNoWritableSettingsIdentifierInSource (T2d-a AC2): the dead legacy
// writableSettings map is gone — a scan of every non-_test.go source file in the
// package dir finds zero occurrences of the identifier. writableKeys() is the
// sole PUT allowlist. (Precedent: console_v2_config_panel_test.go scans embedded
// assets; here we scan the package's own .go source.)
func TestNoWritableSettingsIdentifierInSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), "writableSettings") {
			t.Errorf("%s still references dead identifier writableSettings", name)
		}
	}
}

// TestGetNormalisesStoredDurationValue (T2d-c AC1): after PUT message_retention=48h,
// GET /api/settings echoes the value in Go time.Duration.String() form (48h0m0s),
// not the raw stored string, on the source=setting path.
func TestGetNormalisesStoredDurationValue(t *testing.T) {
	r := testRelay(t)
	if w := doAPI(r, http.MethodPut, "/settings", `{"message_retention":"48h"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT message_retention=48h: status %d\nbody: %s", w.Code, w.Body.String())
	}
	e, ok := getSettingEntry(t, decodeJSON(t, doAPI(r, http.MethodGet, "/settings", "")), "message_retention")
	if !ok {
		t.Fatal("message_retention entry missing")
	}
	if e["value"] != "48h0m0s" || e["source"] != "setting" {
		t.Errorf("message_retention = {value:%v source:%v}, want {48h0m0s, setting}", e["value"], e["source"])
	}
}

// TestGetEchoesUnparseableStoredDurationRaw (T2d-c AC2): a stored duration that
// does not parse (hand-SQL garbage written straight to the DB, bypassing PUT
// validation) is echoed raw with source=setting and GET still returns 200 — no
// panic, no 500.
func TestGetEchoesUnparseableStoredDurationRaw(t *testing.T) {
	r := testRelay(t)
	r.DB.SetSetting("message_retention", "garbage")
	w := doAPI(r, http.MethodGet, "/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET with garbage stored duration: status %d, want 200 (no 500)\nbody: %s", w.Code, w.Body.String())
	}
	e, ok := getSettingEntry(t, decodeJSON(t, w), "message_retention")
	if !ok {
		t.Fatal("message_retention entry missing")
	}
	if e["value"] != "garbage" || e["source"] != "setting" {
		t.Errorf("garbage stored = {value:%v source:%v}, want {garbage, setting}", e["value"], e["source"])
	}
}

// newNormKeys are the settings the norms added since v1.21 read (task 00734b64).
var newNormKeys = []string{
	"ack_manager_age", "ack_human_age", "answer_reply_age", "answer_role_age",
	"class_budget_mode", "attribution_share", "knowledge_min_compaction_lag",
}

// TestSettingsSpecNewNormKeys (00734b64 AC1 + AC3): each new key is PUT-writable
// and reads back equal; out-of-range values are refused with 400 naming the key
// and nothing is stored; GET lists every key in the operational group, and
// budget_epoch is listed read-only.
func TestSettingsSpecNewNormKeys(t *testing.T) {
	r := testRelay(t)
	valid := map[string]string{
		"ack_manager_age": "2h", "ack_human_age": "6h", "answer_reply_age": "30m", "answer_role_age": "3h",
		"class_budget_mode": "on", "attribution_share": "0.75", "knowledge_min_compaction_lag": "240h",
	}
	for _, k := range newNormKeys {
		w := doAPI(r, http.MethodPut, "/settings", `{"`+k+`":"`+valid[k]+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s=%s: status %d\nbody: %s", k, valid[k], w.Code, w.Body.String())
		}
		if got := r.DB.GetSetting(k); got != valid[k] {
			t.Errorf("%s read back %q, want %q", k, got, valid[k])
		}
	}
	bad := []struct{ key, val string }{
		{"ack_manager_age", "49h"}, {"ack_human_age", "30s"}, {"answer_reply_age", "25h"}, {"answer_role_age", "0s"},
		{"class_budget_mode", "bogus"}, {"attribution_share", "0"}, {"attribution_share", "1.5"},
		{"attribution_share", "abc"}, {"attribution_share", "NaN"}, {"attribution_share", "-0.1"},
		{"attribution_share", "Inf"}, {"knowledge_min_compaction_lag", "24h"},
	}
	for _, b := range bad {
		before := r.DB.GetSetting(b.key)
		w := doAPI(r, http.MethodPut, "/settings", `{"`+b.key+`":"`+b.val+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s=%s: status %d, want 400", b.key, b.val, w.Code)
		}
		if j := decodeJSON(t, w); j["key"] != b.key {
			t.Errorf("PUT %s=%s: 400 names key %v", b.key, b.val, j["key"])
		}
		if got := r.DB.GetSetting(b.key); got != before {
			t.Errorf("PUT %s=%s changed the stored value to %q", b.key, b.val, got)
		}
	}
	// The bounds of attribution_share's (0, 1] are accepted.
	for _, v := range []string{"0.6", "1"} {
		if w := doAPI(r, http.MethodPut, "/settings", `{"attribution_share":"`+v+`"}`); w.Code != http.StatusOK {
			t.Errorf("PUT attribution_share=%s: status %d, want 200", v, w.Code)
		}
	}
	// budget_epoch is migration-stamped: listed, never writable.
	if w := doAPI(r, http.MethodPut, "/settings", `{"budget_epoch":"2026-01-01T00:00:00Z"}`); w.Code != http.StatusForbidden {
		t.Errorf("PUT budget_epoch: status %d, want 403", w.Code)
	}
	body := decodeJSON(t, doAPI(r, http.MethodGet, "/settings", ""))
	for _, k := range append(append([]string{}, newNormKeys...), "budget_epoch") {
		e, ok := getSettingEntry(t, body, k)
		if !ok {
			t.Fatalf("GET /settings does not list %s", k)
		}
		if e["group"] != groupOperational {
			t.Errorf("%s group = %v, want operational", k, e["group"])
		}
		if wantW := k != "budget_epoch"; e["writable"] != wantW {
			t.Errorf("%s writable = %v, want %v", k, e["writable"], wantW)
		}
	}
}

// TestSettingsSpecClampsMatchReaders (00734b64 AC2): the spec's bounds and
// defaults equal what each reader applies, so the panel can never offer a value
// the relay silently clamps. Norm-backed ages are checked against the norms row
// the obligations engine reads; the ACK ladder defaults against its constants;
// the budget mode against the db enum.
func TestSettingsSpecClampsMatchReaders(t *testing.T) {
	r := testRelay(t)
	raw, err := sql.Open("sqlite3", "file:"+r.DB.Path()+"?mode=ro")
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	defer func() { _ = raw.Close() }()
	secs := func(d string) int64 {
		v, err := time.ParseDuration(d)
		if err != nil {
			t.Fatalf("parse %q: %v", d, err)
		}
		return int64(v / time.Second)
	}
	for _, k := range []string{"ack_notify_age", "ack_escalate_age", "ack_manager_age", "ack_human_age", "answer_reply_age", "answer_role_age"} {
		var def, mn, mx int64
		if err := raw.QueryRow(`SELECT deadline_default_s, deadline_min_s, deadline_max_s FROM norms WHERE deadline_setting = ?`, k).
			Scan(&def, &mn, &mx); err != nil {
			t.Fatalf("norms row for %s: %v", k, err)
		}
		s := specByKey[k]
		if secs(s.Default) != def || secs(s.Min) != mn || secs(s.Max) != mx {
			t.Errorf("%s spec %s [%s..%s] != norms reader %ds [%ds..%ds]", k, s.Default, s.Min, s.Max, def, mn, mx)
		}
	}
	for k, c := range map[string]time.Duration{
		"ack_notify_age": ACKNotifyAge, "ack_escalate_age": ACKEscalateAge,
		"ack_manager_age": ACKManagerAge, "ack_human_age": ACKHumanAge,
	} {
		if specByKey[k].Default != dur(c) {
			t.Errorf("%s spec default %s != reader constant %s", k, specByKey[k].Default, dur(c))
		}
	}
	mode := specByKey[db.SettingClassBudgetMode]
	if strings.Join(mode.Enum, ",") != strings.Join([]string{db.ClassBudgetModeOff, db.ClassBudgetModeShadow, db.ClassBudgetModeOn}, ",") ||
		mode.Default != db.ClassBudgetModeShadow {
		t.Errorf("class_budget_mode spec %v/%s != db enum", mode.Enum, mode.Default)
	}
	// Readers outside this package's reach: attribution_share (db/class_budgets.go,
	// default 0.6, accepts 0 < v <= 1) and knowledge_min_compaction_lag
	// (compactKnowledgeLogs, knowledge S2: 30 d, clamp 7..365 d).
	if s := specByKey["attribution_share"]; s.Default != "0.6" || validateValue(s, "1") != "" || validateValue(s, "0") == "" {
		t.Errorf("attribution_share spec drifted: %+v", s)
	}
	if s := specByKey["knowledge_min_compaction_lag"]; s.Min != dur(7*24*time.Hour) || s.Max != dur(365*24*time.Hour) || s.Default != dur(30*24*time.Hour) {
		t.Errorf("knowledge_min_compaction_lag spec %s [%s..%s], want 720h [168h..8760h]", s.Default, s.Min, s.Max)
	}
}
