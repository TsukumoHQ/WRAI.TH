package relay

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// F-03 (S3 0b980988): the REST surface is reachable from any browser tab on
// the host, so /api/* refuses what a cross-site page or a DNS-rebinding page
// can send:
//   - a Host that is not a loopback name at the relay's port (or an
//     operator-listed RELAY_ALLOWED_HOSTS name) → 421 — a rebinding page talks
//     to 127.0.0.1 but still sends its own hostname;
//   - a foreign Origin (not the request's own host, not RELAY_CORS_ORIGINS) → 403,
//     enforced for every path by corsMiddleware (outermost);
//   - a state-changing method without Content-Type application/json → 415 — a
//     cross-site form or no-cors fetch cannot set it without a preflight.
//
// Signed server-to-server inbound (signedInboundPaths) is exempt: peers and
// webhook senders reach the relay under their own Host and content type, and
// each request carries its own HMAC / per-peer token, which is what a
// browser-side attacker cannot forge.
var signedInboundPaths = map[string]bool{
	"/api/federation/inbound":        true, // X-Relay-Federation-Token
	"/api/connectors/linear/webhook": true, // LINEAR_WEBHOOK_SECRET HMAC
	"/api/webhooks/github":           true, // GitHub HMAC
	"/api/webhooks/signal":           true, // signal HMAC
}

// apiGuardMiddleware wraps next with the /api/* Host and Content-Type checks.
// port is the relay's listen port; other paths pass through untouched.
func apiGuardMiddleware(port string, allowedHosts []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || signedInboundPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if !hostAllowed(r.Host, port, allowedHosts) {
			http.Error(w, `{"error":"host not allowed"}`, http.StatusMisdirectedRequest)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				http.Error(w, `{"error":"Content-Type must be application/json"}`, http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed: localhost / 127.0.0.1 / [::1] at exactly the listen port, or a
// RELAY_ALLOWED_HOSTS entry (bare host = any port, host:port = that port).
func hostAllowed(hostHeader, port string, allowed []string) bool {
	hostHeader = strings.ToLower(hostHeader)
	host, p, err := net.SplitHostPort(hostHeader)
	if err != nil {
		host, p = strings.Trim(hostHeader, "[]"), ""
	}
	for _, a := range allowed {
		if a == hostHeader || a == host {
			return true
		}
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return p != "" && p == port
	}
	return false
}

// originAllowed: the page is served by this relay (Origin host == Host), or
// the origin is listed in RELAY_CORS_ORIGINS ("*" = any).
func originAllowed(origin, hostHeader string, corsOrigins []string) bool {
	if u, err := url.Parse(origin); err == nil && u.Host != "" && strings.EqualFold(u.Host, hostHeader) {
		return true
	}
	for _, o := range corsOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}
