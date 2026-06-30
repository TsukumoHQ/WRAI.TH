package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPublicSkill(t *testing.T) {
	home := t.TempDir()
	if err := InstallPublicSkill(home); err != nil {
		t.Fatalf("InstallPublicSkill: %v", err)
	}

	path := filepath.Join(home, ".claude", "skills", PublicSkillName, "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("skill not written to %s: %v", path, err)
	}
	got := string(data)

	// Valid SKILL.md frontmatter with the public skill name.
	if !strings.HasPrefix(got, "---\nname: "+PublicSkillName+"\n") {
		t.Errorf("missing/!valid frontmatter; head=%q", got[:min(60, len(got))])
	}
	if !strings.Contains(got, "description:") {
		t.Error("frontmatter missing description (skills activate on it)")
	}

	// Teaches real setup + usage, not just prose.
	for _, want := range []string{"agent-relay hooks install", "register_agent", "get_inbox", "self-hosted"} {
		if !strings.Contains(got, want) {
			t.Errorf("skill should mention %q", want)
		}
	}

	// Idempotent re-run.
	if err := InstallPublicSkill(home); err != nil {
		t.Fatalf("re-run InstallPublicSkill: %v", err)
	}
}

// TestPublicSkillNoPrivateLeak guards the public/AGPL split: the shipped skill
// must not carry private fleet/persona/internal references.
func TestPublicSkillNoPrivateLeak(t *testing.T) {
	lower := strings.ToLower(PublicSkillMD)
	for _, bad := range []string{
		"trovex-growth", "wraith-dev", "copy-gate", "release-cto",
		"tsukumohq/skills", "linear_routing", "loicmancino", "helios-code",
	} {
		if strings.Contains(lower, bad) {
			t.Errorf("public skill leaks private reference %q", bad)
		}
	}
}
