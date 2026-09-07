package relay

import (
	"strings"
	"testing"

	"agent-relay/internal/web"
)

// TestDysonAlias* — founder ruling B (design record 6f73f179 rule e): the v1
// dyson picker (static/js/main.js) speaks the legacy key `dyson_type`; the
// server aliases it to the `sun_type` spec key SERVER-SIDE so the v1 assets stay
// byte-identical. Each test maps to exactly one acceptance criterion.

// AC1: PUT /api/settings translates dyson_type -> sun_type before validation.
func TestDysonAliasPut(t *testing.T) {
	r := testRelay(t)

	// "off" -> sun_type "0"
	if w := doAPI(r, "PUT", "/settings", `{"dyson_type":"off"}`); w.Code != 200 {
		t.Fatalf(`dyson_type=off: want 200, got %d: %s`, w.Code, w.Body.String())
	}
	if got := r.DB.GetSetting("sun_type"); got != "0" {
		t.Fatalf(`dyson_type=off: sun_type = %q, want "0"`, got)
	}

	// "5" -> sun_type "5"
	if w := doAPI(r, "PUT", "/settings", `{"dyson_type":"5"}`); w.Code != 200 {
		t.Fatalf(`dyson_type=5: want 200, got %d: %s`, w.Code, w.Body.String())
	}
	if got := r.DB.GetSetting("sun_type"); got != "5" {
		t.Fatalf(`dyson_type=5: sun_type = %q, want "5"`, got)
	}

	// "auto" -> clear (sun_type back to "")
	r.DB.SetSetting("sun_type", "3")
	if w := doAPI(r, "PUT", "/settings", `{"dyson_type":"auto"}`); w.Code != 200 {
		t.Fatalf(`dyson_type=auto: want 200, got %d: %s`, w.Code, w.Body.String())
	}
	if got := r.DB.GetSetting("sun_type"); got != "" {
		t.Fatalf(`dyson_type=auto: sun_type = %q, want "" (cleared)`, got)
	}

	// bad value -> 400 naming dyson_type
	w := doAPI(r, "PUT", "/settings", `{"dyson_type":"x"}`)
	if w.Code != 400 {
		t.Fatalf(`dyson_type=x: want 400, got %d: %s`, w.Code, w.Body.String())
	}
	body := decodeJSON(t, w)
	if body["key"] != "dyson_type" {
		t.Fatalf(`dyson_type=x: key = %v, want "dyson_type"`, body["key"])
	}
	if body["error"] != "invalid value: dyson_type" {
		t.Fatalf(`dyson_type=x: error = %v, want "invalid value: dyson_type"`, body["error"])
	}

	// both keys -> ambiguous 400
	if w := doAPI(r, "PUT", "/settings", `{"dyson_type":"1","sun_type":"2"}`); w.Code != 400 {
		t.Fatalf(`dyson_type+sun_type: want 400 (ambiguous), got %d: %s`, w.Code, w.Body.String())
	}
}

// AC2: GET /api/settings exposes the flat dyson_type mirror while keeping every
// v1 field (sun_type default "1", linear_mode, linear, groups, settings).
func TestDysonAliasGet(t *testing.T) {
	r := testRelay(t)

	resp := decodeJSON(t, doAPI(r, "GET", "/settings", ""))
	if resp["dyson_type"] != "auto" {
		t.Fatalf(`unset: dyson_type = %v, want "auto"`, resp["dyson_type"])
	}
	if resp["sun_type"] != "1" {
		t.Fatalf(`unset: sun_type = %v, want "1"`, resp["sun_type"])
	}
	for _, k := range []string{"linear_mode", "linear", "groups", "settings"} {
		if _, ok := resp[k]; !ok {
			t.Fatalf("GET /settings missing v1-compatible field %q", k)
		}
	}

	r.DB.SetSetting("sun_type", "0")
	resp = decodeJSON(t, doAPI(r, "GET", "/settings", ""))
	if resp["dyson_type"] != "off" {
		t.Fatalf(`sun_type=0: dyson_type = %v, want "off"`, resp["dyson_type"])
	}

	r.DB.SetSetting("sun_type", "6")
	resp = decodeJSON(t, doAPI(r, "GET", "/settings", ""))
	if resp["dyson_type"] != "6" {
		t.Fatalf(`sun_type=6: dyson_type = %v, want "6"`, resp["dyson_type"])
	}
}

// AC3: v1 assets stay byte-identical — the alias is server-side only. The v1
// picker string is still present in main.js (untouched), and none of the 3 v1
// assets carries any server-only token from this change.
func TestV1AssetsUntouched(t *testing.T) {
	mainJS, err := web.StaticFiles.ReadFile("static/js/main.js")
	if err != nil {
		t.Fatalf("read static/js/main.js: %v", err)
	}
	if !strings.Contains(string(mainJS), "dyson_type") {
		t.Fatal("static/js/main.js no longer contains the v1 dyson_type picker key")
	}

	// The server-only alias error strings must never leak into a shipped asset.
	serverOnly := []string{
		"invalid value: dyson_type",
		"send dyson_type or sun_type, not both",
		"must be auto, off, or 1..7",
	}
	for _, name := range []string{"static/js/main.js", "static/index.html", "static/style.css"} {
		b, err := web.StaticFiles.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, tok := range serverOnly {
			if strings.Contains(string(b), tok) {
				t.Fatalf("v1 asset %s leaked server-only token %q", name, tok)
			}
		}
	}
}
