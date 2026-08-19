package db

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDBPath_RespectsRelayDBEnv(t *testing.T) {
	t.Setenv("RELAY_DB", "/custom/relay-dev.db")
	got, err := resolveDBPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/relay-dev.db" {
		t.Fatalf("RELAY_DB override ignored: got %q", got)
	}
}

func TestResolveDBPath_DefaultWhenUnset(t *testing.T) {
	t.Setenv("RELAY_DB", "")
	got, err := resolveDBPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(got), ".agent-relay/relay.db") {
		t.Fatalf("default path wrong: %q", got)
	}
}

// TestInsertMessageWithDeliveries_Atomic verifies the message and its delivery
// rows land together and the recipient can read the message from their inbox.
func TestInsertMessageWithDeliveries_Atomic(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	msg, err := d.InsertMessageWithDeliveries("p1", "alice", "bob", "notification", "hi", "body", "{}", "P2", 3600, nil, nil, []string{"bob"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if msg.ID == "" {
		t.Fatal("empty message id")
	}

	inbox, err := d.GetInbox("p1", "bob", true, 10)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	found := false
	for _, m := range inbox {
		if m.ID == msg.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("message+delivery not atomic — message not visible in recipient inbox")
	}
}
