package relay

import (
	"regexp"
	"strings"
	"testing"

	"agent-relay/internal/web"
)

// TestV2ConfigPanel* — the S5/T2c contract: the v2 settings panel renders the
// WHOLE relay configuration surface from the frozen GET /api/settings metadata
// (T2a: settings_spec.go). The panel is metadata-driven, so its editable set is
// derived here from the REAL server spec (settingSpecs / writableKeys()), never
// from writableSettings (ruling A5). The JS-only behaviour (this repo ships no JS
// runtime under `go test`) is pinned at the source level; everything that can be
// is exercised against the live handlers (httptest), including the A6 error
// strings. Each test maps to exactly one acceptance criterion (AC1..AC5).

func readPanelAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := web.StaticFiles.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded asset %s: %v", name, err)
	}
	return string(b)
}

// labelsBlock returns the body of the `const LABELS = { ... };` object in
// settings.js (used to parse the declared label keys).
func labelsBlock(t *testing.T, js string) string {
	t.Helper()
	const marker = "const LABELS = {"
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatal("settings.js has no `const LABELS = {` map")
	}
	rest := js[i+len(marker):]
	end := strings.Index(rest, "\n};")
	if end < 0 {
		t.Fatal("settings.js LABELS map is not closed with `\\n};`")
	}
	return rest[:end]
}

// topLevelKeyRe matches an object entry whose value is a nested object, i.e. a
// top-level LABELS key `  sun_type: {`. The nested `label:`/`help:` pairs use a
// string value (`: '`), so they never match this.
var topLevelKeyRe = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*):\s*\{`)

// AC1 — the panel is metadata-driven: it renders sections from the response
// `groups` array (no hardcoded FIELDS list), and its LABELS map is a subset of
// the real spec keys with every EDITABLE writable key labelled. Editable set =
// writableKeys() minus federation_peers (federation is a RO summary; ruling A5).
func TestV2ConfigPanelLabelsSubsetOfSpec(t *testing.T) {
	js := readPanelAsset(t, "static/v2/settings.js")

	// No hardcoded writable key list: the old `key: '<k>'` FIELDS literals are gone.
	if regexp.MustCompile(`\bkey:\s*'`).MatchString(js) {
		t.Error("settings.js still contains a hardcoded `key: '...'` FIELDS literal")
	}
	// Sections come from the server groups order.
	if !strings.Contains(js, "s.groups.map(") {
		t.Error("settings.js does not render sections from the response groups array (s.groups.map)")
	}

	// Real spec key set and the editable-writable set, derived from the spec.
	specKeys := map[string]bool{}
	for _, s := range settingSpecs {
		specKeys[s.Key] = true
	}
	want := writableKeys() // 24 writable keys (map[string]bool)
	delete(want, "federation_peers")

	labels := map[string]bool{}
	for _, m := range topLevelKeyRe.FindAllStringSubmatch(labelsBlock(t, js), -1) {
		labels[m[1]] = true
	}
	if len(labels) == 0 {
		t.Fatal("parsed zero LABELS keys from settings.js")
	}

	// Every LABELS key must be a real spec key (subset).
	for k := range labels {
		if !specKeys[k] {
			t.Errorf("LABELS key %q is not in the server spec", k)
		}
	}
	// Every EDITABLE writable key must have a label (all editable rows are named).
	for k := range want {
		if !labels[k] {
			t.Errorf("editable writable key %q has no LABELS entry", k)
		}
	}
}

// sourceBadgeBlock returns the body of the `const SOURCE_BADGE = { ... };` map.
func sourceBadgeBlock(t *testing.T, js string) string {
	t.Helper()
	const marker = "const SOURCE_BADGE = {"
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatal("settings.js has no `const SOURCE_BADGE = {` map")
	}
	rest := js[i+len(marker):]
	end := strings.Index(rest, "\n};")
	if end < 0 {
		t.Fatal("settings.js SOURCE_BADGE map is not closed")
	}
	return rest[:end]
}

// AC2 — the source badge map has exactly the four texts env / setting / default /
// compile-time (code), and the lock condition covers both writable=false and
// source=env.
func TestV2ConfigPanelSourceBadgesAndLock(t *testing.T) {
	js := readPanelAsset(t, "static/v2/settings.js")
	block := sourceBadgeBlock(t, js)

	entryRe := regexp.MustCompile(`(?m)^\s*[A-Za-z_]+:\s*'([^']*)'`)
	got := entryRe.FindAllStringSubmatch(block, -1)
	if len(got) != 4 {
		t.Fatalf("SOURCE_BADGE must have exactly 4 entries, got %d", len(got))
	}
	want := map[string]bool{"env": true, "setting": true, "default": true, "compile-time (code)": true}
	for _, m := range got {
		if !want[m[1]] {
			t.Errorf("unexpected SOURCE_BADGE text %q", m[1])
		}
		delete(want, m[1])
	}
	for k := range want {
		t.Errorf("SOURCE_BADGE missing required text %q", k)
	}

	// The lock condition is exactly writable=false OR source=env.
	if !strings.Contains(js, "!st.writable || st.source === 'env'") {
		t.Error("lock condition must cover both writable=false and source=env")
	}
}

// AC3 — a 400 validation error carries {error:"invalid value: <key>", key, detail}
// and applies nothing; an unknown key is 403 {error:"not writable: <key>", key}
// (ruling A6, live handler). The panel renders the 400 detail inline next to the
// offending key (data-err-key) and the 403 path still surfaces via api.js.
func TestV2ConfigPanelInlineValidationError(t *testing.T) {
	r := testRelay(t)

	// 400: a duration below its minimum. message_retention min is 24h.
	w := doAPI(r, "PUT", "/settings", `{"message_retention":"1h"}`)
	if w.Code != 400 {
		t.Fatalf("invalid value PUT: want 400, got %d: %s", w.Code, w.Body.String())
	}
	b := decodeJSON(t, w)
	if b["error"] != "invalid value: message_retention" {
		t.Errorf("400 error string (A6): got %q", b["error"])
	}
	if b["key"] != "message_retention" {
		t.Errorf("400 must name the key: got %q", b["key"])
	}
	if d, _ := b["detail"].(string); strings.TrimSpace(d) == "" {
		t.Error("400 must carry a non-empty detail")
	}
	if got := r.DB.GetSetting("message_retention"); got != "" {
		t.Errorf("nothing may be applied on a 400: message_retention=%q", got)
	}

	// 403: an unknown key, whole-request refused, nothing applied.
	w = doAPI(r, "PUT", "/settings", `{"evil_key":"x"}`)
	if w.Code != 403 {
		t.Fatalf("unknown key PUT: want 403, got %d: %s", w.Code, w.Body.String())
	}
	b = decodeJSON(t, w)
	if b["error"] != "not writable: evil_key" {
		t.Errorf("403 error string (A6): got %q", b["error"])
	}
	if b["key"] != "evil_key" {
		t.Errorf("403 must name the key: got %q", b["key"])
	}
	if got := r.DB.GetSetting("evil_key"); got != "" {
		t.Errorf("unknown key was written: %q", got)
	}

	// The panel places the 400 detail into the slot for its key, without a
	// re-render (form state preserved), and the 403 path uses api.js.
	js := readPanelAsset(t, "static/v2/settings.js")
	if !strings.Contains(js, `data-err-key="`) || !strings.Contains(js, `.cfg-err[data-err-key="`) {
		t.Error("settings.js does not target an inline error slot by key")
	}
	if !strings.Contains(js, "e.status === 400") || !strings.Contains(js, "showFieldError(e.key") {
		t.Error("settings.js does not render the 400 detail next to the offending key")
	}
	apiJS := readPanelAsset(t, "static/v2/api.js")
	if !strings.Contains(apiJS, "j.detail || j.error") {
		t.Error("api.js no longer lifts the server error via j.detail || j.error")
	}
}

var secretInputRe = regexp.MustCompile(`<input[^>]*data-type="secret"[^>]*>`)

// AC4 — a secret renders a set/unset pill and a never-prefilled replace input;
// the clear action sends JSON null; an empty replace input is omitted from the
// PUT; no shipped asset logs or embeds a secret; and the live GET never returns a
// secret value (masked, `set` only).
func TestV2ConfigPanelSecretSetUnsetClear(t *testing.T) {
	js := readPanelAsset(t, "static/v2/settings.js")

	// set/unset pill.
	if !strings.Contains(js, "cfg-pill-") || !strings.Contains(js, "st.set ? 'set' : 'unset'") {
		t.Error("settings.js does not render a secret set/unset pill")
	}
	// The replace input must NOT carry a value= attribute (never prefilled).
	sin := secretInputRe.FindString(js)
	if sin == "" {
		t.Fatal("settings.js has no secret replace input")
	}
	if strings.Contains(sin, "value=") {
		t.Errorf("secret replace input must not be prefilled with a value=: %q", sin)
	}
	// Clear sends an explicit JSON null; empty replace omits the key from the PUT.
	if !strings.Contains(js, "putSettings({ [key]: null })") {
		t.Error("the clear action does not send JSON null")
	}
	if !strings.Contains(js, "if (raw) body[st.key] = raw;") {
		t.Error("an empty secret replace is not omitted from the PUT body")
	}
	// No asset logs, and no secret value is embedded anywhere.
	for _, name := range []string{"static/v2/settings.js", "static/v2/api.js", "static/v2/v2.css"} {
		if strings.Contains(readPanelAsset(t, name), "console.log") {
			t.Errorf("asset %s contains console.log (a logged value can leak a secret)", name)
		}
	}

	// Live: a stored secret is never returned by GET; only `set` signals presence.
	r := testRelay(t)
	const secret = "lin_supersecret_ABCD"
	r.DB.SetSetting("linear_api_key", secret)
	g := doAPI(r, "GET", "/settings", "")
	if g.Code != 200 {
		t.Fatalf("GET /settings: want 200, got %d", g.Code)
	}
	if strings.Contains(g.Body.String(), secret) {
		t.Fatal("GET /settings leaked a secret value")
	}
	st := findSetting(t, decodeJSON(t, g), "linear_api_key")
	if st["secret"] != true {
		t.Errorf("linear_api_key metadata not marked secret: %v", st["secret"])
	}
	if v, _ := st["value"].(string); v != "" {
		t.Errorf("secret value must be empty in metadata, got %q", v)
	}
	if st["set"] != true {
		t.Errorf("secret `set` must reflect the stored value: %v", st["set"])
	}
}

// findSetting returns the settings[] entry with the given key from a GET body.
func findSetting(t *testing.T, resp map[string]any, key string) map[string]any {
	t.Helper()
	arr, _ := resp["settings"].([]any)
	if arr == nil {
		t.Fatal("GET /settings has no settings array")
	}
	for _, e := range arr {
		m, _ := e.(map[string]any)
		if m != nil && m["key"] == key {
			return m
		}
	}
	t.Fatalf("settings[] has no entry for key %q", key)
	return nil
}

// AC5 (smoke) — federation is a read-only summary linking to its own page (no
// inline editor); Timing rows render note text when present; no new v2 id/class
// leaks into any v1 asset; and the live GET returns the frozen shape (groups in
// the fixed order + full-metadata settings entries).
func TestV2ConfigPanelFederationSummaryAndScope(t *testing.T) {
	js := readPanelAsset(t, "static/v2/settings.js")

	// RO summary + link to the federation editor page, and no inline editor input
	// in the federation branch.
	if !strings.Contains(js, "cfg-fed-summary") || !strings.Contains(js, `href="#/federation"`) {
		t.Error("federation group is not a RO summary with a link to the federation page")
	}
	fedStart := strings.Index(js, "function federationHTML(")
	fedEnd := strings.Index(js, "function groupHTML(")
	if fedStart < 0 || fedEnd < 0 || fedEnd <= fedStart {
		t.Fatal("cannot isolate federationHTML() in settings.js")
	}
	if strings.Contains(js[fedStart:fedEnd], "data-type=") {
		t.Error("federation summary must not render an inline editor input")
	}

	// Rows render their note when non-empty (e.g. the Timing writerTimeout note).
	if !strings.Contains(js, "st.note") || !strings.Contains(js, "cfg-note-row") {
		t.Error("rows do not render note text when present")
	}

	// Scope: none of the new v2 panel classes/attrs leak into any v1 asset.
	leaks := []string{"cfg-badge", "cfg-fed-summary", "cfg-secret-wrap", "cfg-pill", "data-err-key"}
	for _, name := range []string{"static/index.html", "static/style.css", "static/js/main.js"} {
		a := readPanelAsset(t, name)
		for _, leak := range leaks {
			if strings.Contains(a, leak) {
				t.Errorf("v1 asset %s leaked a v2 panel token %q", name, leak)
			}
		}
	}

	// Live frozen shape: groups in the fixed order, and every settings entry
	// carries all 12 metadata fields.
	r := testRelay(t)
	g := doAPI(r, "GET", "/settings", "")
	if g.Code != 200 {
		t.Fatalf("GET /settings: want 200, got %d", g.Code)
	}
	resp := decodeJSON(t, g)
	groups, _ := resp["groups"].([]any)
	wantGroups := []string{"console", "linear", "federation", "server", "operational", "timing"}
	if len(groups) != len(wantGroups) {
		t.Fatalf("groups: want %v, got %v", wantGroups, groups)
	}
	for i, gname := range wantGroups {
		if groups[i] != gname {
			t.Errorf("groups[%d]: want %q, got %v", i, gname, groups[i])
		}
	}
	settings, _ := resp["settings"].([]any)
	if len(settings) == 0 {
		t.Fatal("GET /settings returned no settings entries")
	}
	fields := []string{"key", "group", "kind", "value", "set", "source", "writable", "secret", "env_name", "bounds", "default", "note"}
	first, _ := settings[0].(map[string]any)
	for _, f := range fields {
		if _, ok := first[f]; !ok {
			t.Errorf("settings entry missing field %q: %v", f, first)
		}
	}
}
