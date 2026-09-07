package relay

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"
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
// full PUT allowlist (24 = 9 legacy + 15 Operational); writableSettings is the
// spec-derived panel subset (9, console|linear|federation) and a strict subset;
// apiPutSetting consults the spec (an Operational key not in writableSettings
// still applies); spec keys are unique and every group is valid.
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
	}

	wk := writableKeys()
	wantWK := map[string]bool{}
	for _, k := range append(append([]string{}, legacy...), operational...) {
		wantWK[k] = true
	}
	if len(wk) != 24 || len(wantWK) != 24 {
		t.Fatalf("writableKeys size %d, expected set size %d, want 24", len(wk), len(wantWK))
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

	// writableSettings = the 9 panel keys, strict subset of writableKeys.
	if len(writableSettings) != len(legacy) {
		t.Errorf("writableSettings size %d, want %d", len(writableSettings), len(legacy))
	}
	for _, k := range legacy {
		if !writableSettings[k] {
			t.Errorf("writableSettings missing panel key %q", k)
		}
	}
	for k := range writableSettings {
		if !wk[k] {
			t.Errorf("writableSettings key %q not in writableKeys (not a subset)", k)
		}
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

	// apiPutSetting consults the spec, not writableSettings: an Operational key
	// (writable in the spec, absent from writableSettings) applies via PUT.
	r := testRelay(t)
	if _, ok := writableSettings["message_retention"]; ok {
		t.Fatal("precondition: message_retention must NOT be in the panel allowlist")
	}
	w := doAPI(r, http.MethodPut, "/settings", `{"message_retention":"48h"}`)
	if w.Code != http.StatusOK {
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
// alongside a valid one is rejected 403 whole-request and applies nothing; the
// panel-facing writableSettings stays the 9-key contract the v2 panel test pins.
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
	// Panel contract (guarded end-to-end by the pre-existing TestV2ConfigPanel):
	// writableSettings excludes evil_key and every Operational key.
	if writableSettings["evil_key"] {
		t.Error("writableSettings must not contain evil_key")
	}
	if writableSettings["message_retention"] {
		t.Error("writableSettings (panel) must not contain Operational keys")
	}
}
