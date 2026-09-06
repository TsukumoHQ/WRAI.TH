package db

import (
	"path/filepath"
	"testing"
)

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// AC1: list_agents excludes status='deleted' rows.
func TestListAgents_ExcludesDeleted(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	const proj = "proj"
	for _, name := range []string{"alice", "ghost"} {
		if _, _, err := d.RegisterAgent(proj, name, "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := d.DeleteAgent(proj, "ghost"); err != nil {
		t.Fatalf("delete ghost: %v", err)
	}

	agents, err := d.ListAgents(proj)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	var names []string
	for _, a := range agents {
		names = append(names, a.Name)
	}
	if !contains(names, "alice") {
		t.Errorf("live agent alice missing from listing: %v", names)
	}
	if contains(names, "ghost") {
		t.Errorf("deleted agent ghost leaked into listing: %v", names)
	}
}

// AC2: every team-roster listing surface excludes a soft-deleted agent while
// keeping live members. Table-driven over the enumerated roster surfaces in
// orgs.go (GetTeamMembers, GetTeamMemberNames, GetAllTeamMemberships) plus an
// explicit check of GetAgentTeams (which is keyed on the agent, not the team).
func TestTeamRosterListings_ExcludeDeleted(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	const proj = "proj"
	for _, name := range []string{"alice", "ghost"} {
		if _, _, err := d.RegisterAgent(proj, name, "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	team, err := d.CreateTeam("Backend", "backend", proj, "", "regular", nil, nil)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	for _, name := range []string{"alice", "ghost"} {
		if err := d.AddTeamMember(team.ID, name, proj, "member"); err != nil {
			t.Fatalf("add member %s: %v", name, err)
		}
	}
	if err := d.DeleteAgent(proj, "ghost"); err != nil {
		t.Fatalf("delete ghost: %v", err)
	}

	surfaces := []struct {
		name    string
		members func() ([]string, error)
	}{
		{"GetTeamMembers", func() ([]string, error) {
			ms, err := d.GetTeamMembers(team.ID)
			if err != nil {
				return nil, err
			}
			var names []string
			for _, m := range ms {
				names = append(names, m.AgentName)
			}
			return names, nil
		}},
		{"GetTeamMemberNames", func() ([]string, error) {
			return d.GetTeamMemberNames(team.ID)
		}},
		{"GetAllTeamMemberships", func() ([]string, error) {
			ms, err := d.GetAllTeamMemberships()
			if err != nil {
				return nil, err
			}
			var names []string
			for _, m := range ms {
				names = append(names, m.AgentName)
			}
			return names, nil
		}},
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			names, err := s.members()
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			if !contains(names, "alice") {
				t.Errorf("%s: live member alice missing: %v", s.name, names)
			}
			if contains(names, "ghost") {
				t.Errorf("%s: deleted member ghost leaked: %v", s.name, names)
			}
		})
	}

	t.Run("GetAgentTeams", func(t *testing.T) {
		// A live agent still sees its team.
		aliceTeams, err := d.GetAgentTeams(proj, "alice")
		if err != nil {
			t.Fatalf("GetAgentTeams alice: %v", err)
		}
		if len(aliceTeams) != 1 {
			t.Errorf("live agent alice: want 1 team, got %d", len(aliceTeams))
		}
		// A deleted agent lists no teams.
		ghostTeams, err := d.GetAgentTeams(proj, "ghost")
		if err != nil {
			t.Fatalf("GetAgentTeams ghost: %v", err)
		}
		if len(ghostTeams) != 0 {
			t.Errorf("deleted agent ghost: want 0 teams, got %d", len(ghostTeams))
		}
	})
}

// AC3: a soft-deleted row is filtered from listings but remains in the table,
// readable directly (audit trail intact) with an explicit status='deleted'.
func TestDeletedAgentRowRemainsReadable(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	const proj = "proj"
	if _, _, err := d.RegisterAgent(proj, "ghost", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := d.DeleteAgent(proj, "ghost"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	a, err := d.GetAgent(proj, "ghost")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if a == nil {
		t.Fatal("deleted agent row was hard-deleted; audit trail lost")
	}
	if a.Status != "deleted" {
		t.Errorf("deleted agent status = %q, want %q", a.Status, "deleted")
	}
}

// AC5 (extra coverage): the profile-based listing surface also excludes deleted
// rows. GetAgentsByProfile resolves dispatch fan-out; a deleted agent must not
// appear as a delivery target.
func TestOtherRegistryListings_ExcludeDeleted(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	const proj = "proj"
	slug := "backend-dev"
	for _, name := range []string{"alice", "ghost"} {
		if _, _, err := d.RegisterAgent(proj, name, "dev", "", nil, &slug, false, nil, "[]", 0, RegisterOptions{ProfileSlugSet: true}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := d.DeleteAgent(proj, "ghost"); err != nil {
		t.Fatalf("delete ghost: %v", err)
	}

	agents, err := d.GetAgentsByProfile(proj, slug)
	if err != nil {
		t.Fatalf("get agents by profile: %v", err)
	}
	var names []string
	for _, a := range agents {
		names = append(names, a.Name)
	}
	if !contains(names, "alice") {
		t.Errorf("live agent alice missing from profile listing: %v", names)
	}
	if contains(names, "ghost") {
		t.Errorf("deleted agent ghost leaked into profile listing: %v", names)
	}
}
