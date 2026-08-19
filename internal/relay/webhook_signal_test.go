package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func hmacSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// doSignal POSTs a signed signal with the given headers, through ServeAPI.
func doSignal(r *Relay, secret, source, delivery, body string, sign bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/signal", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if source != "" {
		req.Header.Set("X-Signal-Source", source)
	}
	if delivery != "" {
		req.Header.Set("X-Signal-Delivery", delivery)
	}
	if sign {
		req.Header.Set("X-Signal-Signature-256", hmacSig(secret, body))
	}
	w := httptest.NewRecorder()
	r.ServeAPI(w, req)
	return w
}

func TestSignalWebhookCreatesTicketAndDedupes(t *testing.T) {
	t.Setenv(RelaySignalWebhookSecretEnv, "shh-secret")
	r := testRelay(t)
	r.DB.SetSetting("signal_source:ci", `{"profile":"wraith-backend","priority":"P1"}`)

	body := `{"title":"CI failed on main","description":"workflow_run conclusion=failure"}`
	w := doSignal(r, "shh-secret", "ci", "delivery-1", body, true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	out := decodeJSON(t, w)
	id, _ := out["task_id"].(string)
	if id == "" {
		t.Fatalf("no task_id: %v", out)
	}
	if out["profile"] != "wraith-backend" {
		t.Errorf("profile = %v, want wraith-backend", out["profile"])
	}

	task, err := r.DB.GetTask(id, "default")
	if err != nil || task == nil {
		t.Fatalf("created task not found: %v", err)
	}
	if task.Title != "CI failed on main" {
		t.Errorf("title = %q", task.Title)
	}
	if task.ProfileSlug != "wraith-backend" {
		t.Errorf("profile = %q, want wraith-backend (routed from config, not payload)", task.ProfileSlug)
	}
	if task.Priority != "P1" {
		t.Errorf("priority = %q, want P1 (config default)", task.Priority)
	}

	// Same delivery id → idempotent no-op, no second ticket.
	w2 := doSignal(r, "shh-secret", "ci", "delivery-1", body, true)
	if w2.Code != http.StatusOK {
		t.Fatalf("dup status = %d, want 200", w2.Code)
	}
	if dup := decodeJSON(t, w2); dup["duplicate"] != true {
		t.Errorf("expected duplicate:true, got %v", dup)
	}

	// Different delivery id → a new ticket.
	w3 := doSignal(r, "shh-secret", "ci", "delivery-2", body, true)
	if w3.Code != http.StatusAccepted {
		t.Fatalf("second signal status = %d, want 202", w3.Code)
	}
	if id2, _ := decodeJSON(t, w3)["task_id"].(string); id2 == "" || id2 == id {
		t.Errorf("second delivery should create a distinct task, got %q (first %q)", id2, id)
	}
}

// Payload priority overrides the config default when valid.
func TestSignalWebhookPayloadPriorityOverride(t *testing.T) {
	t.Setenv(RelaySignalWebhookSecretEnv, "s")
	r := testRelay(t)
	r.DB.SetSetting("signal_source:ci", `{"profile":"wraith-backend","priority":"P2"}`)

	body := `{"title":"prod down","priority":"P0"}`
	w := doSignal(r, "s", "ci", "d1", body, true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	id, _ := decodeJSON(t, w)["task_id"].(string)
	task, _ := r.DB.GetTask(id, "default")
	if task == nil || task.Priority != "P0" {
		t.Errorf("priority = %v, want P0 (payload override)", task)
	}
}

func TestSignalWebhookRejections(t *testing.T) {
	r := testRelay(t)
	body := `{"title":"x"}`

	// No secret configured → 503.
	if w := doSignal(r, "s", "ci", "d", body, true); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no-secret status = %d, want 503", w.Code)
	}

	t.Setenv(RelaySignalWebhookSecretEnv, "s")
	r.DB.SetSetting("signal_source:ci", `{"profile":"wraith-backend"}`)

	// Bad signature → 401.
	if w := doSignal(r, "WRONG", "ci", "d", body, true); w.Code != http.StatusUnauthorized {
		t.Errorf("bad-sig status = %d, want 401", w.Code)
	}
	// Unsigned → 401.
	if w := doSignal(r, "s", "ci", "d", body, false); w.Code != http.StatusUnauthorized {
		t.Errorf("unsigned status = %d, want 401", w.Code)
	}
	// Missing source → 400.
	if w := doSignal(r, "s", "", "d", body, true); w.Code != http.StatusBadRequest {
		t.Errorf("no-source status = %d, want 400", w.Code)
	}
	// Missing delivery id → 400.
	if w := doSignal(r, "s", "ci", "", body, true); w.Code != http.StatusBadRequest {
		t.Errorf("no-delivery status = %d, want 400", w.Code)
	}
	// Unknown/disabled source → 403.
	if w := doSignal(r, "s", "nope", "d", body, true); w.Code != http.StatusForbidden {
		t.Errorf("unknown-source status = %d, want 403", w.Code)
	}
	// Missing title → 400.
	if w := doSignal(r, "s", "ci", "d", `{"description":"no title"}`, true); w.Code != http.StatusBadRequest {
		t.Errorf("no-title status = %d, want 400", w.Code)
	}
}

// On a typed-ticket project a signal can't supply goal/AC/dod, so the single
// guard in db.DispatchTask refuses it (bare tickets impossible on every path).
// That refusal is PERMANENT — the handler must return 422 and KEEP the delivery
// marker so an at-least-once redelivery dedupes to a no-op instead of spinning a
// poison-message retry loop (contrast the transient/quota path, which releases).
func TestSignalWebhookTypedTicketRefusedPermanently(t *testing.T) {
	t.Setenv(RelaySignalWebhookSecretEnv, "s")
	r := testRelay(t)
	r.DB.SetSetting("signal_source:ci", `{"profile":"dev","priority":"P1"}`)

	body := `{"title":"CI failed on niwa","description":"no typed fields"}`
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/signal?project=niwa", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signal-Source", "ci")
	req.Header.Set("X-Signal-Delivery", "niwa-d1")
	req.Header.Set("X-Signal-Signature-256", hmacSig("s", body))
	w := httptest.NewRecorder()
	r.ServeAPI(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "typed ticket required") {
		t.Errorf("body should name the typed-ticket refusal, got %s", w.Body.String())
	}

	// Redelivery of the SAME delivery id must dedupe to a no-op (marker kept),
	// never retry into a new create attempt — proving no poison loop.
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/signal?project=niwa", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Signal-Source", "ci")
	req2.Header.Set("X-Signal-Delivery", "niwa-d1")
	req2.Header.Set("X-Signal-Signature-256", hmacSig("s", body))
	w2 := httptest.NewRecorder()
	r.ServeAPI(w2, req2)
	if w2.Code != http.StatusOK || decodeJSON(t, w2)["duplicate"] != true {
		t.Errorf("redelivery should dedupe (200 duplicate:true), got %d %s", w2.Code, w2.Body.String())
	}
}

// A signed source is quota-gated: past the project task quota it is rejected
// with 429, and the rejected delivery leaves NO dedup row (retryable later).
func TestSignalWebhookQuotaExceeded(t *testing.T) {
	t.Setenv(RelaySignalWebhookSecretEnv, "s")
	r := testRelay(t)
	r.DB.SetSetting("signal_source:ci", `{"profile":"wraith-backend"}`)
	// Cap the signal identity at 1 task.
	if err := r.DB.SetAgentQuota("default", "signal:ci", 0, 0, 1, 0); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	body := `{"title":"CI failed"}`
	if w := doSignal(r, "s", "ci", "d1", body, true); w.Code != http.StatusAccepted {
		t.Fatalf("first signal status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	// Second distinct signal is over quota → 429.
	if w := doSignal(r, "s", "ci", "d2", body, true); w.Code != http.StatusTooManyRequests {
		t.Errorf("over-quota status = %d, want 429", w.Code)
	}
}
