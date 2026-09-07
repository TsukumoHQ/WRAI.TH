package relay

import (
	"strings"
	"testing"

	"agent-relay/internal/db"
	"agent-relay/internal/web"
)

// Founder bug (console send identity): a message sent from the web console via
// POST /api/user-response was stored with from_agent "user", but the console's
// human-profile filter and the v2 UI expect "human". The server is the sole
// authority for the sender (the client sends no sender field), so the fix lives
// in apiPostUserResponse. Each test below maps to one acceptance criterion.

// AC1: a console send lands in the target agent's inbox tagged from "human".
func TestUserResponseSenderIsHuman(t *testing.T) {
	r := testRelay(t)

	if _, _, err := r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	w := doAPI(r, "POST", "/user-response", `{"project":"p1","to":"bot-a","content":"hello from the console"}`)
	if w.Code != 200 {
		t.Fatalf("user-response status = %d, body %s", w.Code, w.Body.String())
	}

	msgs, err := r.DB.GetInboxViaDeliveries("p1", "bot-a", false, 50)
	if err != nil {
		t.Fatalf("get inbox: %v", err)
	}

	var found bool
	for _, m := range msgs {
		if m.Content == "hello from the console" {
			found = true
			if m.From != "human" {
				t.Fatalf("inbox message from = %q, want %q", m.From, "human")
			}
		}
	}
	if !found {
		t.Fatalf("console message not delivered to bot-a inbox (%d msgs)", len(msgs))
	}
}

// AC2: "human" and the legacy "user" alias still resolve to the same notify
// target, so the sender rename does not reroute or drop the wake.
func TestUserAliasStillRouted(t *testing.T) {
	n := &Notifier{}

	human := n.resolveTargets("p1", "human", nil)
	user := n.resolveTargets("p1", "user", nil)

	if len(human) != 1 || human[0] != "user" {
		t.Fatalf(`resolveTargets("human") = %v, want ["user"]`, human)
	}
	if len(user) != 1 || user[0] != "user" {
		t.Fatalf(`resolveTargets("user") = %v, want ["user"]`, user)
	}
}

// AC3: the v1 and v2 client assets are untouched by this server-side fix — they
// still POST to /api/user-response and still send NO sender field (the server is
// authoritative). Byte-identity is enforced by the gate's diff-scope check; this
// pins the load-bearing behaviour at the source level.
func TestV1V2AssetsUntouched(t *testing.T) {
	// The two clients that actually call the endpoint.
	for _, name := range []string{"static/js/api-client.js", "static/v2/api.js"} {
		b, err := web.StaticFiles.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		if len(src) == 0 {
			t.Fatalf("%s is empty", name)
		}
		if !strings.Contains(src, "/api/user-response") {
			t.Fatalf("%s no longer POSTs /api/user-response", name)
		}
		if strings.Contains(src, "sender") {
			t.Fatalf("%s now sends a sender field; the server must stay authoritative", name)
		}
	}

	// main.js is a v1 asset in the same bundle: present and non-empty, untouched.
	b, err := web.StaticFiles.ReadFile("static/js/main.js")
	if err != nil {
		t.Fatalf("read static/js/main.js: %v", err)
	}
	if len(b) == 0 {
		t.Fatalf("static/js/main.js is empty")
	}
}
