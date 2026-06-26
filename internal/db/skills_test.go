package db

import "testing"

// TestFindBestProfileForSkill_StructuredRegistry guards the skill→profile routing
// regression: FindProfilesBySkill once selected a 15-column set that scanProfile
// couldn't scan, so every joined row was silently skipped and structured
// dispatch-by-skill returned nothing the moment a profile_skills link existed.
// This test creates real links and asserts the expert profile is picked.
func TestFindBestProfileForSkill_StructuredRegistry(t *testing.T) {
	d := testDB(t)
	const project = "p1"

	capable, err := d.RegisterProfile(project, "capable-agent", "Capable", "eng", "[]")
	if err != nil {
		t.Fatalf("register capable: %v", err)
	}
	expert, err := d.RegisterProfile(project, "expert-agent", "Expert", "eng", "[]")
	if err != nil {
		t.Fatalf("register expert: %v", err)
	}

	skill, err := d.UpsertSkill(project, "go", "Go programming", `["go","backend"]`)
	if err != nil {
		t.Fatalf("upsert skill: %v", err)
	}

	if err := d.LinkProfileSkill(capable.ID, skill.ID, "capable"); err != nil {
		t.Fatalf("link capable: %v", err)
	}
	if err := d.LinkProfileSkill(expert.ID, skill.ID, "expert"); err != nil {
		t.Fatalf("link expert: %v", err)
	}

	// Structured registry must return BOTH, expert ordered first.
	profiles, err := d.FindProfilesBySkill(project, "go")
	if err != nil {
		t.Fatalf("FindProfilesBySkill: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("want 2 profiles linked to skill, got %d (the column/scan mismatch swallows rows)", len(profiles))
	}
	if profiles[0].Slug != "expert-agent" {
		t.Fatalf("want expert-agent first (expert > capable ordering), got %q", profiles[0].Slug)
	}

	// FindBestProfileForSkill is what the dispatch handler calls.
	best, err := d.FindBestProfileForSkill(project, "go")
	if err != nil {
		t.Fatalf("FindBestProfileForSkill: %v", err)
	}
	if best == nil {
		t.Fatal("dispatch-by-skill returned nil — structured routing is dead")
	}
	if best.Slug != "expert-agent" {
		t.Fatalf("want expert-agent, got %q", best.Slug)
	}
}
