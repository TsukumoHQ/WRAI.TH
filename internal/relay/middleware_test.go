package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddleware_LoopbackExemption(t *testing.T) {
	t.Setenv("RELAY_TRUST_LOOPBACK", "") // default: trust loopback
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := authMiddleware("secret", ok)

	call := func(remote, token string) int {
		req := httptest.NewRequest("GET", "/api/health", nil)
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if c := call("127.0.0.1:5555", ""); c != 200 {
		t.Fatalf("loopback IPv4 no-token: want 200, got %d", c)
	}
	if c := call("[::1]:5555", ""); c != 200 {
		t.Fatalf("loopback IPv6 no-token: want 200, got %d", c)
	}
	if c := call("10.1.2.3:5555", ""); c != 401 {
		t.Fatalf("remote no-token: want 401, got %d", c)
	}
	if c := call("10.1.2.3:5555", "secret"); c != 200 {
		t.Fatalf("remote good token: want 200, got %d", c)
	}
	if c := call("10.1.2.3:5555", "wrong"); c != 401 {
		t.Fatalf("remote bad token: want 401, got %d", c)
	}
}

func TestAuthMiddleware_TrustLoopbackDisabled(t *testing.T) {
	t.Setenv("RELAY_TRUST_LOOPBACK", "0")
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := authMiddleware("secret", ok)

	req := httptest.NewRequest("GET", "/api/health", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("loopback with trust disabled: want 401, got %d", rr.Code)
	}
}

func TestAuthMiddleware_NoKeyPassesThrough(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := authMiddleware("", ok)
	req := httptest.NewRequest("GET", "/api/health", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("no key should pass through: want 200, got %d", rr.Code)
	}
}
