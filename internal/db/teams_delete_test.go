package db

import (
	"path/filepath"
	"testing"
)

// Issue #150: delete_team must actually retire a team — row, members, inbox refs.
func TestDeleteTeam(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}

	team, err := d.CreateTeam("Backend", "backend", "proj", "", "regular", nil, nil)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := d.AddTeamMember(team.ID, "alice", "proj", "member"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := d.AddToTeamInbox(team.ID, "msg-1"); err != nil {
		t.Fatalf("add inbox: %v", err)
	}

	if err := d.DeleteTeam("proj", "backend"); err != nil {
		t.Fatalf("delete team: %v", err)
	}

	// Team is gone.
	if got, _ := d.GetTeam("proj", "backend"); got != nil {
		t.Errorf("team still resolvable after delete: %+v", got)
	}
	// Membership gone.
	if members, _ := d.GetTeamMemberNames(team.ID); len(members) != 0 {
		t.Errorf("members not cleared: %v", members)
	}

	// Deleting a missing team is an explicit error, not a silent success.
	if err := d.DeleteTeam("proj", "backend"); err == nil {
		t.Error("expected error deleting non-existent team")
	}
}

func TestUnreadCountForAgent(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.RegisterAgent("proj", "bob", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if n, err := d.UnreadCountForAgent("proj", "bob"); err != nil || n != 0 {
		t.Fatalf("empty inbox: got %d, err %v", n, err)
	}

	if _, err := d.InsertMessageWithDeliveries("proj", "alice", "bob", "note", "hi", "body", "", "normal", 0, nil, nil, []string{"bob"}); err != nil {
		t.Fatalf("insert msg: %v", err)
	}
	// Count must not mutate delivery state, so a second call is still 1.
	for i := 0; i < 2; i++ {
		n, err := d.UnreadCountForAgent("proj", "bob")
		if err != nil || n != 1 {
			t.Fatalf("call %d: expected 1 unread, got %d, err %v", i, n, err)
		}
	}
}
