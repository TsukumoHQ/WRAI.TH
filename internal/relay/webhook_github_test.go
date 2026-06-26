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

func ghSign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func ghRequest(secret, event, delivery, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github?project=p1", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if secret != "" {
		req.Header.Set("X-Hub-Signature-256", ghSign(secret, body))
	}
	return req
}

func TestGitHubWebhook(t *testing.T) {
	const secret = "topsecret"
	const body = `{"action":"completed","workflow_run":{"conclusion":"failure","name":"CI","html_url":"http://x"},"repository":{"full_name":"org/repo"},"sender":{"login":"bob"}}`

	t.Run("no secret configured → 503", func(t *testing.T) {
		r := testRelay(t)
		w := httptest.NewRecorder()
		r.apiGitHubWebhook(w, ghRequest("", "workflow_run", "d0", body))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503, got %d", w.Code)
		}
	})

	t.Run("bad signature → 401", func(t *testing.T) {
		t.Setenv(RelayGitHubWebhookSecretEnv, secret)
		r := testRelay(t)
		req := ghRequest("", "workflow_run", "d1", body) // no signature header
		req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
		w := httptest.NewRecorder()
		r.apiGitHubWebhook(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", w.Code)
		}
	})

	t.Run("valid signature → queued + dedup", func(t *testing.T) {
		t.Setenv(RelayGitHubWebhookSecretEnv, secret)
		r := testRelay(t)

		w := httptest.NewRecorder()
		r.apiGitHubWebhook(w, ghRequest(secret, "workflow_run", "dGUID", body))
		if w.Code != http.StatusAccepted {
			t.Fatalf("want 202, got %d (%s)", w.Code, w.Body.String())
		}

		evs, err := r.DB.RecentEvents("p1", 10)
		if err != nil {
			t.Fatalf("recent: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("want 1 event, got %d", len(evs))
		}
		e := evs[0]
		if e.EventType != "event:github.workflow_run" {
			t.Fatalf("event type = %q", e.EventType)
		}
		if e.DeliveryID != "dGUID" {
			t.Fatalf("delivery_id = %q, want the GitHub GUID", e.DeliveryID)
		}
		if !strings.Contains(e.Payload, `"conclusion":"failure"`) {
			t.Fatalf("payload missing conclusion: %s", e.Payload)
		}

		// Redelivery with the same GUID dedupes (INSERT OR IGNORE) → still 1 row.
		w2 := httptest.NewRecorder()
		r.apiGitHubWebhook(w2, ghRequest(secret, "workflow_run", "dGUID", body))
		if w2.Code != http.StatusAccepted {
			t.Fatalf("redelivery want 202, got %d", w2.Code)
		}
		evs2, _ := r.DB.RecentEvents("p1", 10)
		if len(evs2) != 1 {
			t.Fatalf("redelivery must dedupe, got %d rows", len(evs2))
		}
	})
}
