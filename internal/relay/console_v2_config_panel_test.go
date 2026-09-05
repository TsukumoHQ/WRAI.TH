package relay

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"agent-relay/internal/web"
)

func readCfgAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := web.StaticFiles.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded asset %s: %v", name, err)
	}
	return string(b)
}

// fieldKeyRe extracts the `key: '<k>'` literals from the FIELDS table in
// settings.js. Only the FIELDS entries use that quoted form; runtime uses of the
// key (data-key="${f.key}") carry no colon-then-single-quote, so this matches the
// declared field set exactly.
var fieldKeyRe = regexp.MustCompile(`\bkey:\s*'([^']+)'`)

// TestV2ConfigPanel is the S5 (task 8f4e3172) contract, round 2: the v2 console
// exposes the writable NON-federation settings through the existing server-side
// allowlist (writableSettings, api.go), with no backend change. Where a claim is
// a server round-trip it is exercised behaviorally against the real handlers
// (httptest); the irreducible DOM layer (rendered error banner, reload) is
// asserted at the source level — this repo ships no JS runtime for `go test`, and
// the manual serve-local verification is recorded in the PR body (AC6).
func TestV2ConfigPanel(t *testing.T) {
	cfg := readCfgAsset(t, "static/v2/settings.js")

	// AC1 — cross-boundary set equality: the FIELDS keys the panel exposes are
	// EXACTLY the server allowlist minus federation_peers (federation has its own
	// page). Parsed from the JS, compared to the Go allowlist — not strings.Contains.
	t.Run("FieldsMatchWritableAllowlist", func(t *testing.T) {
		want := map[string]bool{}
		for k := range writableSettings {
			want[k] = true
		}
		delete(want, setFederationPeers) // owned by the federation page, not this panel

		got := map[string]bool{}
		for _, m := range fieldKeyRe.FindAllStringSubmatch(cfg, -1) {
			got[m[1]] = true
		}

		for k := range want {
			if !got[k] {
				t.Errorf("settings.js FIELDS is missing writable key %q", k)
			}
		}
		for k := range got {
			if !want[k] {
				t.Errorf("settings.js FIELDS exposes non-writable / out-of-scope key %q", k)
			}
		}
		if len(got) != len(want) {
			t.Errorf("FIELDS key count %d != allowlist-minus-federation %d", len(got), len(want))
		}
	})

	// AC2 — save through the allowlist: an allowlist key round-trips and persists;
	// a key OUTSIDE the allowlist is refused (403) and nothing is written. Live
	// handlers, same pattern as TestApiPutSetting_Allowlist / TestAPISettings.
	t.Run("SaveThroughAllowlistNonAllowlistRejected", func(t *testing.T) {
		r := testRelay(t)

		// Non-allowlist key → 403, nothing persisted.
		w := doAPI(r, "PUT", "/settings", `{"evil_key":"x"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("non-allowlist PUT: want 403, got %d: %s", w.Code, w.Body.String())
		}
		if got := r.DB.GetSetting("evil_key"); got != "" {
			t.Fatalf("non-allowlist key was written: %q", got)
		}

		// Allowlist key → 200 and persisted (the reload contract reads it back).
		if w := doAPI(r, "PUT", "/settings", `{"sun_type":"3"}`); w.Code != http.StatusOK {
			t.Fatalf("allowlist PUT: want 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := r.DB.GetSetting("sun_type"); got != "3" {
			t.Fatalf("allowlist key not persisted: sun_type=%q", got)
		}
	})

	// AC3 — a server-side rejection is SURFACED, not swallowed: the error string
	// the server emits reaches the rendered banner. Behaviorally the server emits
	// the message (below); the wrapper (api.js) lifts j.error into the thrown
	// Error, and settings.js renders ${e.message} — so the exact server string is
	// what the user sees. api.js:15-18, settings.js catch→render cited in the PR.
	t.Run("ServerErrorSurfaced", func(t *testing.T) {
		r := testRelay(t)
		w := doAPI(r, "PUT", "/settings", `{"evil_key":"x"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("403 body is not JSON: %v (%s)", err, w.Body.String())
		}
		serverMsg, _ := body["error"].(string)
		if !strings.Contains(serverMsg, "not writable") || !strings.Contains(serverMsg, "evil_key") {
			t.Errorf("server error message not descriptive (must name the refused key): %q", serverMsg)
		}

		// The wrapper lifts the server's error field, and the panel renders it.
		apiJS := readCfgAsset(t, "static/v2/api.js")
		if !strings.Contains(apiJS, "j.detail || j.error") {
			t.Error("api.js does not lift the server error body into the thrown Error")
		}
		for _, want := range []string{"save failed:", "${e.message}"} {
			if !strings.Contains(cfg, want) {
				t.Errorf("settings.js does not render the surfaced error (%q missing)", want)
			}
		}
	})

	// AC4 — reload reflects persisted values, and the secret is returned MASKED
	// (never in plaintext). GET-PUT-GET on the panel's keys; the write-only pair
	// (linear_routing/linear_project_map) is not echoed by GET by design, so its
	// persistence is checked at the store.
	t.Run("ReloadReflectsPersistedSecretMasked", func(t *testing.T) {
		r := testRelay(t)

		// GET-PUT-GET over ALL 8 panel keys (the full FIELDS set), not a subset:
		// sun_type + the 6 GET-echoed Linear keys + the 2 write-only Linear keys.
		const secret = "lin_supersecret_ABCD"
		put := `{"sun_type":"7","linear_enabled":"1","linear_team_key":"SYN",` +
			`"linear_project":"growth","linear_reconcile_interval":"5m",` +
			`"linear_api_key":"` + secret + `","linear_routing":"{\"p\":\"a\"}",` +
			`"linear_project_map":"{\"p\":\"proj\"}"}`
		if w := doAPI(r, "PUT", "/settings", put); w.Code != http.StatusOK {
			t.Fatalf("PUT: want 200, got %d: %s", w.Code, w.Body.String())
		}

		// Write-only keys persist at the store (GET never exposes them).
		if got := r.DB.GetSetting("linear_routing"); got == "" {
			t.Error("linear_routing (write-only) not persisted")
		}
		if got := r.DB.GetSetting("linear_project_map"); got == "" {
			t.Error("linear_project_map (write-only) not persisted")
		}

		// Reload: GET must reflect the persisted values and mask the secret.
		w := doAPI(r, "GET", "/settings", "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET: want 200, got %d", w.Code)
		}
		raw := w.Body.String()
		if strings.Contains(raw, secret) {
			t.Fatal("GET /settings leaked the linear_api_key in plaintext")
		}
		var s map[string]any
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatalf("GET body not JSON: %v", err)
		}
		if s["sun_type"] != "7" {
			t.Errorf("reload did not reflect sun_type: got %v", s["sun_type"])
		}
		lin, _ := s["linear"].(map[string]any)
		if lin == nil {
			t.Fatal("GET body has no linear block")
		}
		if lin["team_key"] != "SYN" {
			t.Errorf("reload did not reflect linear team_key: got %v", lin["team_key"])
		}
		if lin["project"] != "growth" {
			t.Errorf("reload did not reflect linear project: got %v", lin["project"])
		}
		// The backend normalizes the duration on read (time.Duration round-trip),
		// so "5m" comes back canonicalized as "5m0s" — assert the persisted value.
		if lin["interval"] != "5m0s" {
			t.Errorf("reload did not reflect linear interval: got %v", lin["interval"])
		}
		if lin["enabled"] != true {
			t.Errorf("reload did not reflect linear enabled: got %v", lin["enabled"])
		}
		masked, _ := lin["api_key_masked"].(string)
		if masked == "" || !strings.HasSuffix(masked, "ABCD") || masked == secret {
			t.Errorf("api key not returned masked: got %q", masked)
		}

		// The panel never logs (a logged secret is a leak).
		if strings.Contains(cfg, "console.log") {
			t.Error("settings.js logs — a secret must never be logged")
		}
	})

	// AC5 — v1 is untouched and build is green: none of the config-panel ids leak
	// into any v1 asset (static/ outside v2/). Build-green is implied by this file
	// compiling and running.
	t.Run("V1ZeroDiffBuildGreen", func(t *testing.T) {
		v1Assets := []string{"static/index.html", "static/style.css", "static/js/main.js"}
		for _, name := range v1Assets {
			a := readCfgAsset(t, name)
			for _, leak := range []string{"cfg-wrap", "initSettings", "cfg-save"} {
				if strings.Contains(a, leak) {
					t.Errorf("v1 asset %s leaked a v2 config-panel id %q", name, leak)
				}
			}
		}
		v2 := readCfgAsset(t, "static/v2/v2.js")
		for _, want := range []string{"initSettings", "settings"} {
			if !strings.Contains(v2, want) {
				t.Errorf("v2.js does not register the settings route (%q missing)", want)
			}
		}
	})

	// AC6 (surface) — the secret is a masked field and the panel never logs, so a
	// token cannot leak to the console. The DOM behavior itself has no JS runtime
	// under `go test` (verified in serve local, PR body); this pins the source
	// guarantees the manual pass rides on.
	t.Run("SecretMaskedNeverLogged", func(t *testing.T) {
		for _, want := range []string{`data-type="secret"`, `type="password"`, "api_key_masked"} {
			if !strings.Contains(cfg, want) {
				t.Errorf("settings.js missing masked-secret marker %q", want)
			}
		}
		if strings.Contains(cfg, "console.log") {
			t.Error("settings.js logs — a secret must never be logged")
		}
	})

	// AC (r4 data-loss guard) — a FAILED load (s === null) must not let a Save
	// wipe the stored config with blanks/'0'. Source-pinned (no JS runtime under
	// `go test`; manual serve-local failed-load pass recorded in the PR body):
	// on null state the panel disables every input, replaces Save with a Retry
	// button, and wire() binds no save handler (early-return on !s), so collect()
	// and the PUT are unreachable.
	t.Run("FailedLoadDisablesSaveNoWipe", func(t *testing.T) {
		// Inputs disabled when there is no snapshot.
		if !strings.Contains(cfg, "!s || (f.linear && linearLocked())") {
			t.Error("fieldHTML does not disable inputs on a failed load (s === null)")
		}
		// Save is swapped for a Retry button (no Save present in the null branch).
		if !strings.Contains(cfg, "cfgRetry") {
			t.Error("render does not offer a Retry-load button on a failed load")
		}
		// wire() returns early on !s, so no save handler exists to wipe config.
		guard := regexp.MustCompile(`if\s*\(!s\)\s*\{`)
		if !guard.MatchString(cfg) {
			t.Error("wire() does not early-return on a failed load (!s) — a Save could wipe config")
		}
	})
}
