package db

import "testing"

// TestGetAllRecentMessagesForUser verifies the additive user filter on the
// messages list: it matches sender OR recipient, is case-insensitive, and an
// empty user is identical to the unfiltered query.
func TestGetAllRecentMessagesForUser(t *testing.T) {
	d := testDB(t)

	_, _ = d.InsertMessage("default", "alice", "bob", "notification", "s", "a->b", "{}", "P2", 3600, nil, nil)
	_, _ = d.InsertMessage("default", "bob", "carol", "notification", "s", "b->c", "{}", "P2", 3600, nil, nil)
	_, _ = d.InsertMessage("default", "carol", "dave", "notification", "s", "c->d", "{}", "P2", 3600, nil, nil)

	// Unfiltered: all three.
	all, err := d.GetAllRecentMessagesForUser("default", "", 100)
	if err != nil {
		t.Fatalf("unfiltered: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered expected 3, got %d", len(all))
	}

	// Empty user must equal the legacy unfiltered call.
	legacy, _ := d.GetAllRecentMessages("default", 100)
	if len(legacy) != len(all) {
		t.Fatalf("empty-user (%d) must equal GetAllRecentMessages (%d)", len(all), len(legacy))
	}

	// bob appears as recipient (a->b) and sender (b->c): 2 messages.
	bob, err := d.GetAllRecentMessagesForUser("default", "bob", 100)
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	if len(bob) != 2 {
		t.Fatalf("bob expected 2, got %d", len(bob))
	}

	// Case-insensitive.
	bobUpper, _ := d.GetAllRecentMessagesForUser("default", "BOB", 100)
	if len(bobUpper) != 2 {
		t.Fatalf("BOB expected 2 (case-insensitive), got %d", len(bobUpper))
	}

	// Non-participant: none.
	none, _ := d.GetAllRecentMessagesForUser("default", "zoe", 100)
	if len(none) != 0 {
		t.Fatalf("zoe expected 0, got %d", len(none))
	}
}

// TestListAllConversationsForUser verifies the additive user filter on the
// conversations list: it returns only conversations the user is an active
// member of, is case-insensitive, and an empty user is unfiltered.
func TestListAllConversationsForUser(t *testing.T) {
	d := testDB(t)

	if _, err := d.CreateConversation("default", "ab", "alice", []string{"alice", "bob"}); err != nil {
		t.Fatalf("create ab: %v", err)
	}
	if _, err := d.CreateConversation("default", "cd", "carol", []string{"carol", "dave"}); err != nil {
		t.Fatalf("create cd: %v", err)
	}

	// Unfiltered: both.
	all, err := d.ListAllConversationsForUser("default", "")
	if err != nil {
		t.Fatalf("unfiltered: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered expected 2, got %d", len(all))
	}

	// Empty user must equal the legacy unfiltered call.
	legacy, _ := d.ListAllConversations("default")
	if len(legacy) != len(all) {
		t.Fatalf("empty-user (%d) must equal ListAllConversations (%d)", len(all), len(legacy))
	}

	// alice is only in "ab".
	alice, err := d.ListAllConversationsForUser("default", "alice")
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	if len(alice) != 1 || alice[0].Title != "ab" {
		t.Fatalf("alice expected [ab], got %+v", alice)
	}

	// Case-insensitive.
	aliceUpper, _ := d.ListAllConversationsForUser("default", "ALICE")
	if len(aliceUpper) != 1 {
		t.Fatalf("ALICE expected 1 (case-insensitive), got %d", len(aliceUpper))
	}

	// Non-member: none.
	none, _ := d.ListAllConversationsForUser("default", "zoe")
	if len(none) != 0 {
		t.Fatalf("zoe expected 0, got %d", len(none))
	}
}
