package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoDynamicJSONErrorLiterals is the AC1 regression guard: no relay handler
// may interpolate a value into a hand-built JSON error literal anymore — every
// dynamic error body must go through jsonError. It scans the package source for
// a `{"error"…}` literal combined with an interpolation marker (fmt.Sprintf into
// a brace literal, strconv.Quote, or string concatenation into the literal).
// Static `{"error":"…"}` literals (already valid JSON) are allowed and left for
// a separate unification follow-up; only DYNAMIC sites are forbidden.
func TestNoDynamicJSONErrorLiterals(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "json_error.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `{"error"`) {
				continue
			}
			dynamic := strings.Contains(line, "Sprintf(`{") ||
				strings.Contains(line, "strconv.Quote(") ||
				(strings.Contains(line, "`{\"error\"") && strings.Contains(line, "+"))
			if dynamic {
				t.Errorf("%s:%d dynamic JSON error literal must use jsonError(): %s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestJSONError pins the helper's contract on adversarial values — the exact
// inputs a hand-built `{"error":%q}` literal mangles. Each must produce a body
// that parses as JSON and round-trips the message verbatim, with the
// application/json content type.
func TestJSONError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"plain", "simple message"},
		{"quote", `he said "stop"`},
		{"backslash", `path C:\temp\node`},
		{"quote_and_backslash", "a \" and a \\ together"},
		{"control_byte", "boom\x1bmore"}, // %q emits \x1b — invalid JSON
		{"bell", "ring\a"},               // %q emits \a — invalid JSON
		{"unicode", "naïve café — 日本語"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			jsonError(w, http.StatusTeapot, tc.msg)

			if w.Code != http.StatusTeapot {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusTeapot)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not valid JSON: %v\nbody: %q", err, w.Body.String())
			}
			ev, ok := got["error"].(string)
			if !ok || ev == "" {
				t.Fatalf(".error missing or empty: %#v", got)
			}
			if ev != tc.msg {
				t.Fatalf(".error = %q, want exact round-trip %q", ev, tc.msg)
			}
		})
	}
}

// TestJSONErrorBeatsQuoteLiteral is the non-tautology: it proves WHY the helper
// exists. For a value carrying a control byte and a bell, the pre-migration
// `fmt.Sprintf("{\"error\":%q}", msg)` shape produces INVALID JSON (Go escaping,
// not JSON escaping), while jsonError produces valid JSON for the same value. If
// this ever stops holding, the migration bought nothing.
func TestJSONErrorBeatsQuoteLiteral(t *testing.T) {
	msg := "boom\x1bmore\aend"

	old := fmt.Sprintf(`{"error":%q}`, msg)
	if json.Valid([]byte(old)) {
		t.Fatalf("expected the %%q literal form to be INVALID JSON, but it parsed: %s", old)
	}

	w := httptest.NewRecorder()
	jsonError(w, http.StatusBadRequest, msg)
	if !json.Valid(w.Body.Bytes()) {
		t.Fatalf("jsonError produced invalid JSON for %q: %s", msg, w.Body.String())
	}
}

// TestJSONErrorReachableSites drives every migrated DYNAMIC error site
// end-to-end through ServeAPI and asserts each returns a parseable JSON body
// with a non-empty .error and application/json — the regression the raw
// literals broke. All 7 jsonError() call sites in handler code are covered:
// apiDispatchTask's *db.TypedTicketError / *db.InvalidTitleError /
// *db.BoardRequiredError branches (api.go), apiPostMemory's ValidateMemoryWrite
// branch (api.go), apiPostMessage's quota and not-authorized branches
// (api_messages.go), and apiSignalWebhook's *db.TypedTicketError branch
// (webhook_signal.go).
func TestJSONErrorReachableSites(t *testing.T) {
	// Set the signal-webhook secret before the relay so the signal case below
	// clears the auth gate; read per-request, but set once up front to mirror
	// the existing signal tests. Auto-restored at test end.
	t.Setenv(RelaySignalWebhookSecretEnv, "shh-secret")
	r := testRelay(t)

	// --- apiDispatchTask, api.go ---

	// TypedTicketError branch: enforced project, bare ticket (no goal/ac/dod).
	tte := doAPI(r, "POST", "/tasks", `{"project":"niwa","profile":"dev","title":"Fix a real, specific thing"}`)
	assertJSONErrorBody(t, tte, http.StatusBadRequest)

	// InvalidTitleError branch: enforced project, complete ticket, placeholder title.
	ite := doAPI(r, "POST", "/tasks",
		`{"project":"niwa","profile":"dev","title":"a4d0f829-3e63-4ae0-96f8-c86d5061f0f9","goal":"g","acceptance_criteria":["a"],"dod":"d"}`)
	assertJSONErrorBody(t, ite, http.StatusBadRequest)

	// BoardRequiredError branch: free-form project with >1 board, no board_id,
	// and a profile whose product board ("backlog") is absent — the resolver
	// can't disambiguate, so DispatchTask refuses.
	if _, err := r.DB.CreateBoard("pboard", "B1", "b1", "", "u"); err != nil {
		t.Fatalf("create board b1: %v", err)
	}
	if _, err := r.DB.CreateBoard("pboard", "B2", "b2", "", "u"); err != nil {
		t.Fatalf("create board b2: %v", err)
	}
	bre := doAPI(r, "POST", "/tasks", `{"project":"pboard","profile":"dev","title":"A real, specific thing to do"}`)
	assertJSONErrorBody(t, bre, http.StatusBadRequest)

	// --- apiPostMemory, api.go ---

	// ValidateMemoryWrite branch: niwa is seeded require_memory_discipline; a
	// layer=context write carries no valid_until on the REST path, so the
	// write-side guard rejects it.
	verr := doAPI(r, "POST", "/memories", `{"project":"niwa","key":"k","value":"v","layer":"context"}`)
	assertJSONErrorBody(t, verr, http.StatusBadRequest)

	// --- apiPostMessage, api_messages.go ---

	// Quota branch: cap sender at 1 message/hour, send one (consumes the quota),
	// then a second send trips CheckQuotaError. to="*" skips the permission gate.
	if err := r.DB.SetAgentQuota("pquota", "sender", 0, 1, 0, 0); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	first := doAPI(r, "POST", "/messages", `{"project":"pquota","from":"sender","to":"*","content":"x"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first send status = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	quota := doAPI(r, "POST", "/messages", `{"project":"pquota","from":"sender","to":"*","content":"x"}`)
	assertJSONErrorBody(t, quota, http.StatusTooManyRequests)

	// Not-authorized branch: teams configured for the project (HasTeams true) and
	// a direct send between two unconnected agents (no shared team / reports_to /
	// notify channel), so CanMessage refuses.
	team, err := r.DB.CreateTeam("Team", "team-x", "pauth", "", "squad", nil, nil)
	if err != nil || team == nil {
		t.Fatalf("create team: %v", err)
	}
	if err := r.DB.AddTeamMember(team.ID, "someone", "pauth", "member"); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	authz := doAPI(r, "POST", "/messages", `{"project":"pauth","from":"a","to":"b","content":"x"}`)
	assertJSONErrorBody(t, authz, http.StatusForbidden)

	// --- apiSignalWebhook, webhook_signal.go ---

	// TypedTicketError branch: route a signed signal to the typed-ticket-enforced
	// niwa project; a signal carries a bare (free-form) ticket, so dispatchCore
	// refuses it with 422 (permanent, no retry).
	r.DB.SetSetting("signal_source:ci", `{"profile":"dev"}`)
	r.DB.SetSetting("signal_webhook_project", "niwa")
	sigTTE := doSignal(r, "shh-secret", "ci", "del-1", `{"title":"t","description":"d"}`, true)
	assertJSONErrorBody(t, sigTTE, http.StatusUnprocessableEntity)
}

func assertJSONErrorBody(t *testing.T, w *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, wantStatus, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json; body=%s", ct, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("error body not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if ev, ok := got["error"].(string); !ok || ev == "" {
		t.Fatalf(".error missing/empty: %#v", got)
	}
}
