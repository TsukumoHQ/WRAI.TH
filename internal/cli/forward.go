package cli

import (
	"bytes"
	"crypto/subtle"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// RunForwarder is the minimal HOST relay-send forwarder. dokan monitors run in
// containers and can't reach the relay's loopback bind; instead of opening the
// relay to the network, this tiny 2-verb, token-gated proxy listens on a
// docker-bridge-reachable address and forwards to the loopback relay. Surface is
// intentionally minimal — only /send and /event — so the backbone stays closed.
//
// Env: FORWARDER_TOKEN (required), FORWARDER_PORT (default 8092),
// RELAY_URL (default http://127.0.0.1:8090).
func RunForwarder() {
	token := strings.TrimSpace(os.Getenv("FORWARDER_TOKEN"))
	if token == "" {
		log.Fatal("FORWARDER_TOKEN is required (the shared secret monitors must present)")
	}
	port := os.Getenv("FORWARDER_PORT")
	if port == "" {
		port = "8092"
	}
	relay := os.Getenv("RELAY_URL")
	if relay == "" {
		relay = "http://127.0.0.1:8090"
	}
	client := &http.Client{Timeout: 10 * time.Second}

	authed := func(r *http.Request) bool {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}
	// forward maps a forwarder verb to a relay loopback endpoint.
	forward := func(relayPath string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"POST only"}`, http.StatusMethodNotAllowed)
				return
			}
			if !authed(r) {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			resp, err := client.Post(relay+relayPath, "application/json", bytes.NewReader(body))
			if err != nil {
				http.Error(w, `{"error":"relay unreachable"}`, http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/send", forward("/api/send"))                 // {from,to,priority,content}
	mux.HandleFunc("/event", forward("/api/notification-events")) // {event,payload}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })

	addr := "0.0.0.0:" + port
	log.Printf("[forwarder] relay-send forwarder on %s → %s (verbs: /send, /event)", addr, relay)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
