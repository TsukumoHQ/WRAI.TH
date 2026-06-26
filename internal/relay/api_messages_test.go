package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestApiPostMessage covers the plain-REST send endpoint: a direct message is
// persisted + delivered with send_message semantics, and validation rejects
// missing fields.
func TestApiPostMessage(t *testing.T) {
	r := testRelay(t)

	// Missing content → 400.
	w := httptest.NewRecorder()
	r.apiPostMessage(w, httptest.NewRequest(http.MethodPost, "/api/messages",
		strings.NewReader(`{"project":"p1","from":"bot","to":"cto"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing content want 400, got %d", w.Code)
	}

	// Valid direct send → 2xx + persisted + delivered to recipient inbox.
	w = httptest.NewRecorder()
	r.apiPostMessage(w, httptest.NewRequest(http.MethodPost, "/api/messages",
		strings.NewReader(`{"project":"p1","from":"stale-scanner","to":"cto","priority":"P0","type":"task","subject":"deploy red","content":"prod deploy ERROR"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("valid send want 200, got %d (%s)", w.Code, w.Body.String())
	}

	inbox, err := r.DB.GetInbox("p1", "cto", false, 10)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	found := false
	for _, m := range inbox {
		if m.Content == "prod deploy ERROR" && m.From == "stale-scanner" && m.Priority == "P0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("REST-sent message not delivered to cto inbox (%d msgs)", len(inbox))
	}
}
