package relay

import (
	"io"
	"log"
	"net/http"
	"strings"

	"agent-relay/internal/connector"
	linearconn "agent-relay/internal/connector/linear"
	"agent-relay/internal/models"
)

// apiLinearWebhook handles POST /api/connectors/linear/webhook.
//
// Inertness: when the Linear connector is not active the route 404s with the
// exact same body as any unknown route, so behavior is byte-identical to native
// mode. When active it verifies the HMAC signature + timestamp freshness
// synchronously (rejecting unsigned/stale/oversized), returns 200 fast, and
// processes the payload asynchronously (upsert + emit semantic events).
func (r *Relay) apiLinearWebhook(w http.ResponseWriter, req *http.Request) {
	conn := r.LinearConnector()
	if conn == nil || !conn.Active() {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	// Size cap: never read more than the connector's max body.
	body, err := io.ReadAll(io.LimitReader(req.Body, linearconn.MaxWebhookBody+1))
	if err != nil {
		http.Error(w, `{"error":"read error"}`, http.StatusBadRequest)
		return
	}
	if len(body) > linearconn.MaxWebhookBody {
		http.Error(w, `{"error":"payload too large"}`, http.StatusRequestEntityTooLarge)
		return
	}

	sig := req.Header.Get("Linear-Signature")

	// Synchronous verification gate (cheap): reject bad signatures/stale/oversized.
	if err := conn.VerifySignature(body, sig); err != nil {
		http.Error(w, `{"error":"signature verification failed"}`, http.StatusUnauthorized)
		return
	}

	// Acknowledge fast; do the heavier upsert + emit off the request path.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))

	go func() {
		events, err := conn.Ingest(body, sig)
		if err != nil {
			log.Printf("[linear] webhook ingest: %v", err)
			return
		}
		for _, e := range events {
			r.Events.EmitSemantic(e.Type, e.Project, e.Agent, e.Payload)
		}
	}()
}

// pushStatusAsync mirrors a relay status change to the Linear issue (move state +
// optional comment), after the local transition has already succeeded. No-op in
// native mode or for tasks without a Linear issue id. Best-effort: a failed push
// never affects the local transition (Linear reconcile is the backstop). The
// comment is posted only when a note is supplied, so routine claim/start moves
// stay quiet in Linear.
func pushStatusAsync(conn connector.TaskConnector, task *models.Task, status string, note *string) {
	if conn == nil || !conn.Active() || task == nil {
		return
	}
	if task.Source != "linear" || task.LinearIssueID == nil || *task.LinearIssueID == "" {
		return
	}
	issueID := *task.LinearIssueID
	comment := ""
	if note != nil {
		comment = strings.TrimSpace(*note)
	}
	go func() {
		if err := conn.PushStatus(issueID, status, comment); err != nil {
			log.Printf("[linear] push status %s %s: %v", status, issueID, err)
		}
	}()
}
