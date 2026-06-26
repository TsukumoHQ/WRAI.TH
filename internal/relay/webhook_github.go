package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

// RelayGitHubWebhookSecretEnv holds the shared secret configured on the GitHub
// webhook; the relay verifies X-Hub-Signature-256 against it.
const RelayGitHubWebhookSecretEnv = "RELAY_GITHUB_WEBHOOK_SECRET"

// githubWebhookMaxBody caps the request body we'll read (GitHub payloads are
// well under this; the cap bounds memory on a malicious sender).
const githubWebhookMaxBody = 2 << 20 // 2 MiB

// apiGitHubWebhook receives a GitHub webhook, verifies its HMAC-SHA256 signature
// BEFORE parsing, and persists the event into the durable outbox (TSU-52
// slice-D). The sweeper then fires matching rules — e.g. a rule on
// "event:github.workflow_run" with match {"conclusion":"failure"} pings the
// author, killing the /loop CI-polling. Dedup is by the GitHub delivery GUID
// (X-GitHub-Delivery) so an at-least-once redelivery is a no-op.
//
// GitHub cannot POST to a loopback bind, so in production a host-side relay
// (smee.io SSE client, or a poller) forwards deliveries to this endpoint —
// the backbone stays loopback. The endpoint is ingress-agnostic.
func (r *Relay) apiGitHubWebhook(w http.ResponseWriter, req *http.Request) {
	secret := strings.TrimSpace(os.Getenv(RelayGitHubWebhookSecretEnv))
	if secret == "" {
		// Refuse rather than accept unsigned events — an unauthenticated event
		// sink would let anything inject pings.
		http.Error(w, `{"error":"github webhook secret not configured"}`, http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, githubWebhookMaxBody))
	if err != nil {
		http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
		return
	}
	if !verifyGitHubSignature(secret, req.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, `{"error":"invalid signature"}`, http.StatusUnauthorized)
		return
	}

	ghEvent := req.Header.Get("X-GitHub-Event")
	if ghEvent == "" {
		http.Error(w, `{"error":"missing X-GitHub-Event"}`, http.StatusBadRequest)
		return
	}
	deliveryID := strings.TrimSpace(req.Header.Get("X-GitHub-Delivery"))

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	project := req.URL.Query().Get("project")
	if project == "" {
		if s := strings.TrimSpace(r.DB.GetSetting("github_webhook_project")); s != "" {
			project = s
		} else {
			project = "default"
		}
	}

	semantic := githubSemantic(ghEvent, raw)
	payload, _ := json.Marshal(semantic)
	eventType := "event:github." + ghEvent

	// delivery_id = the GitHub GUID → INSERT OR IGNORE dedupes redeliveries.
	if _, _, err := r.DB.InsertEvent(deliveryID, project, eventType, strVal(semantic["agent"]), string(payload)); err != nil {
		http.Error(w, `{"error":"persist event"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"queued": true, "event": eventType})
}

// verifyGitHubSignature constant-time compares the X-Hub-Signature-256 header
// ("sha256=<hex>") against HMAC-SHA256(body, secret).
func verifyGitHubSignature(secret, header string, body []byte) bool {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	want := strings.TrimPrefix(header, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}

// githubSemantic flattens the fields rules and templates care about from a raw
// GitHub payload into a compact semantic map (the full payload is not stored).
func githubSemantic(ghEvent string, raw map[string]any) map[string]any {
	s := map[string]any{"gh_event": ghEvent}
	if v := strVal(raw["action"]); v != "" {
		s["action"] = v
	}
	if repo, ok := raw["repository"].(map[string]any); ok {
		s["repo"] = strVal(repo["full_name"])
	}
	if sender, ok := raw["sender"].(map[string]any); ok {
		s["sender"] = strVal(sender["login"])
	}
	// pull_request: title, url, author, merged
	if pr, ok := raw["pull_request"].(map[string]any); ok {
		s["title"] = strVal(pr["title"])
		s["url"] = strVal(pr["html_url"])
		if u, ok := pr["user"].(map[string]any); ok {
			s["agent"] = strVal(u["login"])
		}
		if m, ok := pr["merged"].(bool); ok {
			s["merged"] = m
		}
	}
	// CI: workflow_run / check_run conclusion (success/failure/...)
	for _, key := range []string{"workflow_run", "check_run", "check_suite"} {
		if run, ok := raw[key].(map[string]any); ok {
			if c := strVal(run["conclusion"]); c != "" {
				s["conclusion"] = c
			}
			if s["title"] == nil || strVal(s["title"]) == "" {
				s["title"] = strVal(run["name"])
			}
			if u := strVal(run["html_url"]); u != "" {
				s["url"] = u
			}
		}
	}
	// line: a default human-readable summary for templates that omit one.
	line := ghEvent
	if a := strVal(s["action"]); a != "" {
		line += " " + a
	}
	if c := strVal(s["conclusion"]); c != "" {
		line += " (" + c + ")"
	}
	if t := strVal(s["title"]); t != "" {
		line += ": " + t
	}
	s["line"] = line
	return s
}
