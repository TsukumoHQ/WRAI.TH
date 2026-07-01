package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agent-relay/internal/config"
	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/server"
)

// newFedRelay builds a wired Relay with a Federation registry for the given
// peers, mirroring relay.New's wiring (Handlers and Relay share one Federation).
func newFedRelay(t *testing.T, peers []config.FederationPeer) *Relay {
	t.Helper()
	database, err := db.NewTestDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mcpSrv := server.NewMCPServer("test", "0.0.0")
	events := NewEventBus()
	registry := NewSessionRegistry(mcpSrv)
	handlers := NewHandlers(database, registry, nil, events)
	fed := NewFederation(peers)
	handlers.federation = fed

	return &Relay{
		MCPServer:  mcpSrv,
		DB:         database,
		Registry:   registry,
		Events:     events,
		Handlers:   handlers,
		Federation: fed,
		Config:     config.Config{},
	}
}

func TestSplitPeerAddr(t *testing.T) {
	cases := []struct {
		in         string
		name, peer string
		ok         bool
	}{
		{"bob@relayb", "bob", "relayb", true},
		{"plain", "", "", false},
		{"*", "", "", false},
		{"team:eng", "", "", false},
		{"conversation:abc", "", "", false},
		{"@nope", "", "", false},
		{"trailing@", "", "", false},
		{"a@b@c", "a@b", "c", true}, // last @ wins
	}
	for _, c := range cases {
		name, peer, ok := splitPeerAddr(c.in)
		if ok != c.ok || name != c.name || peer != c.peer {
			t.Errorf("splitPeerAddr(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, name, peer, ok, c.name, c.peer, c.ok)
		}
	}
}

func TestPeerLookup(t *testing.T) {
	fed := NewFederation([]config.FederationPeer{
		{Label: "RelayB", URL: "http://x", Token: "tok-secret", Project: "default"},
	})
	if !fed.Enabled() {
		t.Fatal("federation should be enabled with one peer")
	}
	if _, ok := fed.PeerByLabel("relayb"); !ok { // label lowercased at build
		t.Error("PeerByLabel should resolve case-insensitively")
	}
	if _, ok := fed.PeerByToken("tok-secret"); !ok {
		t.Error("PeerByToken should match the configured token")
	}
	if _, ok := fed.PeerByToken("wrong"); ok {
		t.Error("PeerByToken must reject an unknown token")
	}
	if _, ok := fed.PeerByToken(""); ok {
		t.Error("PeerByToken must reject an empty token")
	}
}

func TestFederationDisabledByDefault(t *testing.T) {
	fed := NewFederation(nil)
	if fed.Enabled() {
		t.Fatal("federation must be disabled with no peers")
	}
	r := newFedRelay(t, nil)
	w := doAPI(r, http.MethodPost, "/federation/inbound", `{"to":"x","content":"y"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled inbound should 404, got %d", w.Code)
	}
}

func TestFederationInboundAuthAndDelivery(t *testing.T) {
	r := newFedRelay(t, []config.FederationPeer{
		{Label: "relaya", URL: "http://a", Token: "shared-tok", Project: "default"},
	})
	// Recipient must exist locally.
	if _, _, err := r.DB.RegisterAgent("default", "bob", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register bob: %v", err)
	}

	body := `{"from":"alice","to":"bob","project":"default","subject":"hi","content":"hello bob","priority":"P1"}`

	// Bad token → 401.
	req := httptest.NewRequest(http.MethodPost, "/api/federation/inbound", strings.NewReader(body))
	req.Header.Set("X-Relay-Federation-Token", "nope")
	w := httptest.NewRecorder()
	r.ServeAPI(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token should 401, got %d: %s", w.Code, w.Body.String())
	}

	// Unknown recipient → 404.
	req = httptest.NewRequest(http.MethodPost, "/api/federation/inbound", strings.NewReader(`{"from":"alice","to":"ghost","content":"x"}`))
	req.Header.Set("X-Relay-Federation-Token", "shared-tok")
	w = httptest.NewRecorder()
	r.ServeAPI(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown recipient should 404, got %d", w.Code)
	}

	// Happy path → delivered.
	req = httptest.NewRequest(http.MethodPost, "/api/federation/inbound", strings.NewReader(body))
	req.Header.Set("X-Relay-Federation-Token", "shared-tok")
	w = httptest.NewRecorder()
	r.ServeAPI(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delivery should 200, got %d: %s", w.Code, w.Body.String())
	}

	inbox, err := r.DB.GetInboxViaDeliveries("default", "bob", false, 10)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("bob should have 1 message, got %d", len(inbox))
	}
	m := inbox[0]
	if m.From != "alice@relaya" { // stamped with the origin peer's local label
		t.Errorf("from = %q, want alice@relaya", m.From)
	}
	if m.Content != "hello bob" || m.Priority != "P1" {
		t.Errorf("unexpected message: content=%q priority=%q", m.Content, m.Priority)
	}
	if !strings.Contains(m.Metadata, `"source_relay":"relaya"`) || !strings.Contains(m.Metadata, `"federated":true`) {
		t.Errorf("metadata missing federation markers: %s", m.Metadata)
	}
}

// TestFederationRoundTrip drives a real two-relay hop: alice on relay A sends to
// "bob@relayb"; A forwards over HTTP to B's inbound route; bob's inbox on B
// surfaces it with a reply-addressable sender.
func TestFederationRoundTrip(t *testing.T) {
	// Relay B — receiver. Its label for A is "relaya".
	relayB := newFedRelay(t, []config.FederationPeer{
		{Label: "relaya", URL: "http://unused", Token: "pair-tok", Project: "default"},
	})
	if _, _, err := relayB.DB.RegisterAgent("default", "bob", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register bob: %v", err)
	}
	bServer := httptest.NewServer(http.HandlerFunc(relayB.ServeAPI))
	defer bServer.Close()

	// Relay A — sender. Its peer "relayb" points at B's server.
	relayA := newFedRelay(t, []config.FederationPeer{
		{Label: "relayb", URL: bServer.URL, Token: "pair-tok", Project: "default"},
	})
	if _, _, err := relayA.DB.RegisterAgent("default", "alice", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register alice: %v", err)
	}

	res, err := relayA.Handlers.HandleSendMessage(context.Background(), call(map[string]any{
		"project": "default",
		"as":      "alice",
		"to":      "bob@relayb",
		"subject": "ping",
		"content": "hi from alice",
	}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.IsError {
		t.Fatalf("federated send errored: %s", expectError(t, res))
	}

	inbox, err := relayB.DB.GetInboxViaDeliveries("default", "bob", false, 10)
	if err != nil {
		t.Fatalf("read bob inbox: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("bob should have 1 federated message, got %d", len(inbox))
	}
	if inbox[0].From != "alice@relaya" {
		t.Errorf("from = %q, want alice@relaya (reply-addressable)", inbox[0].From)
	}
	if inbox[0].Content != "hi from alice" {
		t.Errorf("content = %q", inbox[0].Content)
	}

	// Reply routes back symmetrically: bob → "alice@relaya" lands on relay A.
	// Point B's peer "relaya" at A's server so the reverse hop resolves.
	aServer := httptest.NewServer(http.HandlerFunc(relayA.ServeAPI))
	defer aServer.Close()
	relayB.Federation.Reload([]config.FederationPeer{
		{Label: "relaya", URL: aServer.URL, Token: "pair-tok", Project: "default"},
	})

	res, err = relayB.Handlers.HandleSendMessage(context.Background(), call(map[string]any{
		"project": "default",
		"as":      "bob",
		"to":      "alice@relaya",
		"content": "pong",
	}))
	if err != nil || res.IsError {
		t.Fatalf("reply send failed: err=%v result=%+v", err, res)
	}
	aInbox, err := relayA.DB.GetInboxViaDeliveries("default", "alice", false, 10)
	if err != nil {
		t.Fatalf("read alice inbox: %v", err)
	}
	if len(aInbox) != 1 || aInbox[0].From != "bob@relayb" || aInbox[0].Content != "pong" {
		t.Fatalf("reply not delivered symmetrically: %+v", aInbox)
	}
}

// jsonMarshalable guards that fedMessage round-trips cleanly (schema sanity).
func TestFedMessageJSON(t *testing.T) {
	fm := fedMessage{From: "a", To: "b", Project: "p", Type: "task", Subject: "s", Content: "c", Priority: "P0", TTL: 60, ReplyTo: "r"}
	b, err := json.Marshal(fm)
	if err != nil {
		t.Fatal(err)
	}
	var back fedMessage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != fm {
		t.Errorf("round-trip mismatch: %+v vs %+v", back, fm)
	}
}
