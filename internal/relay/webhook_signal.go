package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"agent-relay/internal/db"
)

// RelaySignalWebhookSecretEnv holds the shared HMAC secret for the generic
// inbound-signal webhook. As with the GitHub webhook, an unset secret makes the
// endpoint refuse every request — an unauthenticated task-injection sink would
// let anyone create fleet work.
const RelaySignalWebhookSecretEnv = "RELAY_SIGNAL_WEBHOOK_SECRET"

// signalWebhookMaxBody caps the request body (a signal payload is tiny; the cap
// bounds memory against a malicious sender).
const signalWebhookMaxBody = 1 << 20 // 1 MiB

// signalSourceConfig is the per-source routing config an operator stores under
// the setting key "signal_source:<source>". Its presence is the config-gate:
// a source with no config is rejected, so the endpoint is inert until an
// operator explicitly enables a source and picks the lane it routes to. The
// routing lane comes from operator config, NEVER from the untrusted payload —
// an external signal must not be able to target an arbitrary profile.
type signalSourceConfig struct {
	Profile  string `json:"profile"`  // required — the lane the ticket routes to
	Priority string `json:"priority"` // optional default priority (P0..P3)
	BoardID  string `json:"board_id"` // optional board
}

// signalPayload is the small, source-agnostic envelope a caller POSTs. A caller
// maps its own event (CI failure, Sentry alert, …) into this before signing. The
// title is required; everything else is optional. Priority here overrides the
// source config's default when valid.
type signalPayload struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
}

// apiSignalWebhook receives a signed inbound signal, verifies its HMAC-SHA256
// signature BEFORE parsing, dedupes on the delivery id (so an at-least-once
// redelivery creates no duplicate ticket), and turns it into a typed ticket that
// enters the normal dispatch pipeline (steal #9). Inbound-only: the relay never
// reaches back out — a host-side forwarder delivers signals here.
//
// Required headers: X-Signal-Source (the config key), X-Signal-Delivery (the
// idempotency key), X-Signal-Signature-256 (sha256=<hex> of the body).
func (r *Relay) apiSignalWebhook(w http.ResponseWriter, req *http.Request) {
	secret := strings.TrimSpace(os.Getenv(RelaySignalWebhookSecretEnv))
	if secret == "" {
		http.Error(w, `{"error":"signal webhook secret not configured"}`, http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, signalWebhookMaxBody))
	if err != nil {
		http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
		return
	}
	// Reuse the shared HMAC verifier (same sha256=<hex> scheme as GitHub).
	if !verifyGitHubSignature(secret, req.Header.Get("X-Signal-Signature-256"), body) {
		http.Error(w, `{"error":"invalid signature"}`, http.StatusUnauthorized)
		return
	}

	source := strings.TrimSpace(req.Header.Get("X-Signal-Source"))
	if source == "" {
		http.Error(w, `{"error":"missing X-Signal-Source"}`, http.StatusBadRequest)
		return
	}
	deliveryID := strings.TrimSpace(req.Header.Get("X-Signal-Delivery"))
	if deliveryID == "" {
		http.Error(w, `{"error":"missing X-Signal-Delivery (idempotency key)"}`, http.StatusBadRequest)
		return
	}

	// Config-gate: a source is only accepted if an operator stored routing config
	// for it. Absent/invalid config → 403, never a silent create.
	cfg, ok := r.signalSourceConfig(source)
	if !ok {
		http.Error(w, `{"error":"unknown or disabled signal source"}`, http.StatusForbidden)
		return
	}

	var sig signalPayload
	if err := json.Unmarshal(body, &sig); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(sig.Title)
	if title == "" {
		http.Error(w, `{"error":"payload title is required"}`, http.StatusBadRequest)
		return
	}

	project := req.URL.Query().Get("project")
	if project == "" {
		if s := strings.TrimSpace(r.DB.GetSetting("signal_webhook_project")); s != "" {
			project = s
		} else {
			project = "default"
		}
	}

	// Quota-gate the signal identity BEFORE reserving the delivery id, so a signed
	// source can't flood the board past the project task quota (the same guard the
	// MCP dispatch path enforces). Checked pre-reservation so a rejected signal
	// leaves no dedup row and can be retried after the quota window resets.
	signalAgent := "signal:" + source
	if qErr := r.DB.CheckQuotaError(project, signalAgent, "tasks"); qErr != "" {
		http.Error(w, `{"error":"task quota exceeded for signal source"}`, http.StatusTooManyRequests)
		return
	}

	// Idempotency: the events outbox INSERT-OR-IGNOREs on delivery_id. A duplicate
	// delivery is a no-op — no second ticket. The event is also a durable journal
	// of every accepted signal. This is a reservation: if the dispatch below
	// fails, we compensate-delete the row so a redelivery re-attempts rather than
	// deduping to a silent no-op (record-then-act: no lost signal, still no dup).
	payloadJSON, _ := json.Marshal(map[string]any{"source": source, "title": title, "priority": sig.Priority})
	_, inserted, err := r.DB.InsertEvent(deliveryID, project, "signal:"+source, signalAgent, string(payloadJSON))
	if err != nil {
		http.Error(w, `{"error":"persist signal"}`, http.StatusInternalServerError)
		return
	}
	if !inserted {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"duplicate": true, "delivery_id": deliveryID})
		return
	}

	priority := resolveSignalPriority(sig.Priority, cfg.Priority)
	var boardID *string
	if cfg.BoardID != "" {
		b := cfg.BoardID
		boardID = &b
	}

	// Create the ticket through the shared dispatch pipeline, so a signal-created
	// task is indistinguishable from a hand-dispatched one (notifies the lane,
	// emits task.dispatched). A signal carries a free-form ticket: it can't supply
	// goal/AC/dod, so on a typed-ticket project (require_typed_ticket) the guard —
	// hoisted into dispatchCore, with db.DispatchTask as the backstop — refuses it;
	// bare tickets are impossible on every path (founder ruling). That refusal is
	// PERMANENT: a redelivery would fail
	// identically, so we keep the delivery marker (no compensating release) and
	// return 422, never spin an at-least-once retry into a poison-message loop.
	task, _, err := r.Handlers.dispatchCore(project, signalAgent, cfg.Profile, title, sig.Description, priority, nil, boardID, db.TypedTicket{}, false, nil)
	if err != nil {
		var tte *db.TypedTicketError
		if errors.As(err, &tte) {
			jsonError(w, http.StatusUnprocessableEntity, tte.Error())
			return
		}
		// Transient failure: release the delivery-id reservation so an
		// at-least-once redelivery can retry instead of deduping to a no-op.
		r.DB.DeleteEventByDelivery(project, deliveryID)
		http.Error(w, `{"error":"create ticket"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"queued": true, "task_id": task.ID, "profile": cfg.Profile})
}

// signalSourceConfig loads and validates the per-source routing config. Returns
// ok=false when the source has no config or the config lacks a profile (the
// config-gate: no valid config → source rejected).
func (r *Relay) signalSourceConfig(source string) (signalSourceConfig, bool) {
	raw := strings.TrimSpace(r.DB.GetSetting("signal_source:" + source))
	if raw == "" {
		return signalSourceConfig{}, false
	}
	var cfg signalSourceConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return signalSourceConfig{}, false
	}
	if strings.TrimSpace(cfg.Profile) == "" {
		return signalSourceConfig{}, false
	}
	return cfg, true
}

// resolveSignalPriority picks the payload priority when it is a valid P0..P3,
// else the source-config default, else P2.
func resolveSignalPriority(payload, configDefault string) string {
	if isValidPriority(payload) {
		return payload
	}
	if isValidPriority(configDefault) {
		return configDefault
	}
	return "P2"
}

func isValidPriority(p string) bool {
	switch p {
	case "P0", "P1", "P2", "P3":
		return true
	}
	return false
}
