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

// pushInReviewAsync fires the connector's one owned write-back (→ In Review +
// comment) for a Linear-sourced task, after the local in_review_at stamp has
// already succeeded. It is a no-op in native mode (the no-op connector reports
// Active()==false) or for tasks without a Linear issue id. Best-effort: a failed
// push never affects the local transition (Linear reconcile is the backstop).
func pushInReviewAsync(conn connector.TaskConnector, task *models.Task, agent string, comment *string) {
	if conn == nil || !conn.Active() || task == nil {
		return
	}
	if task.Source != "linear" || task.LinearIssueID == nil || *task.LinearIssueID == "" {
		return
	}
	issueID := *task.LinearIssueID
	body := buildReviewComment(task, agent, comment)
	go func() {
		if err := conn.PushInReview(issueID, body); err != nil {
			log.Printf("[linear] push in-review %s: %v", issueID, err)
		}
	}()
}

// buildReviewComment composes the Linear comment posted on the In Review
// write-back: who moved it plus the optional result/PR note the agent attached.
func buildReviewComment(task *models.Task, agent string, note *string) string {
	var b strings.Builder
	b.WriteString("Moved to In Review by ")
	if agent != "" {
		b.WriteString(agent)
	} else {
		b.WriteString("a relay agent")
	}
	b.WriteString(".")
	if note != nil && strings.TrimSpace(*note) != "" {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(*note))
	}
	return b.String()
}
