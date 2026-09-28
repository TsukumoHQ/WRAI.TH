package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// F-03 (S3 0b980988) — Host / Origin / Content-Type hardening of /api/*.

func guardCall(h http.Handler, method, path, host, origin, contentType string) (int, bool) {
	var body *strings.Reader
	if method != http.MethodGet {
		body = strings.NewReader(`{}`)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	req.Host = host
	req.RemoteAddr = "127.0.0.1:5555"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Code == http.StatusTeapot
}

// teapot marks "the handler ran" so a test can prove a refusal never reached it.
var teapot = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

// TestAPIGuardRejectsForeignHost: only loopback names at the listen port (or a
// RELAY_ALLOWED_HOSTS entry) reach /api/*; a rebinding hostname is refused.
func TestAPIGuardRejectsForeignHost(t *testing.T) {
	h := apiGuardMiddleware("8090", []string{"relay.example.com"}, teapot)
	for _, host := range []string{"localhost:8090", "127.0.0.1:8090", "[::1]:8090", "relay.example.com", "relay.example.com:443"} {
		if _, ran := guardCall(h, http.MethodGet, "/api/health", host, "", ""); !ran {
			t.Fatalf("host %q: want handler reached", host)
		}
	}
	for _, host := range []string{"evil.test:8090", "localhost:9999", "127.0.0.1", "localhost", "attacker.localhost:8090"} {
		code, ran := guardCall(h, http.MethodGet, "/api/health", host, "", "")
		if ran || code != http.StatusMisdirectedRequest {
			t.Fatalf("host %q: want 421 before the handler, got %d (ran=%v)", host, code, ran)
		}
	}
	// Non-/api paths are out of scope (UI assets, /mcp).
	if _, ran := guardCall(h, http.MethodGet, "/index.html", "evil.test:8090", "", ""); !ran {
		t.Fatal("non-/api path must pass the guard")
	}
}

// TestAPIGuardRequiresJSONOnStateChange: POST/PUT/PATCH/DELETE on /api/* need
// Content-Type application/json (parameters allowed); GET is unaffected.
func TestAPIGuardRequiresJSONOnStateChange(t *testing.T) {
	h := apiGuardMiddleware("8090", nil, teapot)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
			code, ran := guardCall(h, m, "/api/tasks", "localhost:8090", "", ct)
			if ran || code != http.StatusUnsupportedMediaType {
				t.Fatalf("%s with %q: want 415 before the handler, got %d (ran=%v)", m, ct, code, ran)
			}
		}
		for _, ct := range []string{"application/json", "application/json; charset=utf-8"} {
			if _, ran := guardCall(h, m, "/api/tasks", "localhost:8090", "", ct); !ran {
				t.Fatalf("%s with %q: want handler reached", m, ct)
			}
		}
	}
	if _, ran := guardCall(h, http.MethodGet, "/api/tasks", "localhost:8090", "", ""); !ran {
		t.Fatal("GET without Content-Type must pass")
	}
}

// TestCORSNeverPassesDisallowedOrigin: a foreign Origin is refused 403 and the
// handler never runs — with or without RELAY_CORS_ORIGINS configured. The
// relay's own origin and listed origins pass.
func TestCORSNeverPassesDisallowedOrigin(t *testing.T) {
	for _, origins := range [][]string{nil, {"https://ui.example.com"}} {
		h := corsMiddleware(origins, teapot)
		code, ran := guardCall(h, http.MethodPost, "/api/tasks", "localhost:8090", "https://evil.test", "application/json")
		if ran || code != http.StatusForbidden {
			t.Fatalf("origins %v: foreign origin want 403 before the handler, got %d (ran=%v)", origins, code, ran)
		}
		code, ran = guardCall(h, http.MethodPost, "/mcp", "localhost:8090", "http://evil.test:8090", "application/json")
		if ran || code != http.StatusForbidden {
			t.Fatalf("origins %v: foreign origin on /mcp want 403, got %d (ran=%v)", origins, code, ran)
		}
		if _, ran := guardCall(h, http.MethodPost, "/api/tasks", "localhost:8090", "http://localhost:8090", "application/json"); !ran {
			t.Fatalf("origins %v: same-origin UI request must pass", origins)
		}
		if _, ran := guardCall(h, http.MethodPost, "/api/tasks", "localhost:8090", "", "application/json"); !ran {
			t.Fatalf("origins %v: no-Origin (non-browser) request must pass", origins)
		}
	}
	h := corsMiddleware([]string{"https://ui.example.com"}, teapot)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Host = "localhost:8090"
	req.Header.Set("Origin", "https://ui.example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTeapot || rr.Header().Get("Access-Control-Allow-Origin") != "https://ui.example.com" {
		t.Fatalf("listed origin: want handler + ACAO, got %d %q", rr.Code, rr.Header().Get("Access-Control-Allow-Origin"))
	}
}

// TestMiddlewareChainWiresAPIGuard: the served chain applies the /api/* guard
// at the listen port.
func TestMiddlewareChainWiresAPIGuard(t *testing.T) {
	r := &Relay{}
	h := r.buildMiddlewareChain(teapot, "8090")
	if code, ran := guardCall(h, http.MethodGet, "/api/health", "evil.test:8090", "", ""); ran || code != http.StatusMisdirectedRequest {
		t.Fatalf("chain: foreign host want 421, got %d (ran=%v)", code, ran)
	}
	if code, ran := guardCall(h, http.MethodPost, "/api/tasks", "localhost:8090", "", "text/plain"); ran || code != http.StatusUnsupportedMediaType {
		t.Fatalf("chain: text/plain POST want 415, got %d (ran=%v)", code, ran)
	}
	if _, ran := guardCall(h, http.MethodPost, "/api/tasks", "localhost:8090", "", "application/json"); !ran {
		t.Fatal("chain: well-formed loopback JSON POST must reach the handler")
	}
}

// TestAPIGuardExemptsSignedInbound: federation and webhook receivers carry
// their own HMAC / peer token and arrive under the sender's Host and content
// type (GitHub defaults to form encoding), so the guard lets them through.
func TestAPIGuardExemptsSignedInbound(t *testing.T) {
	h := apiGuardMiddleware("8090", nil, teapot)
	for path := range signedInboundPaths {
		if _, ran := guardCall(h, http.MethodPost, path, "peer.lan:8090", "", "application/x-www-form-urlencoded"); !ran {
			t.Fatalf("%s: signed inbound must reach its handler", path)
		}
	}
	if code, ran := guardCall(h, http.MethodPost, "/api/webhooks/other", "peer.lan:8090", "", "application/json"); ran || code != http.StatusMisdirectedRequest {
		t.Fatalf("unlisted webhook path: want 421, got %d (ran=%v)", code, ran)
	}
}
