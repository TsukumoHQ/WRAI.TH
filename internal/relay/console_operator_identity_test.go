package relay

import (
	"regexp"
	"strings"
	"testing"

	"agent-relay/internal/web"
)

// TestConsoleOperatorIdentity* — founder P1 (ea0cc293): the console has ONE
// operator identity, "human". Every client-side write names the operator as
// "human" (AC1); the operator inbox/thread still shows legacy "user" traffic as
// well as "human" (AC2); and no unrelated asset picks up the change (AC3). The
// JS has no runtime under `go test`, so it is pinned at the source level against
// the embedded assets, exactly as console_v2_config_panel_test.go does.

func readConsoleAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := web.StaticFiles.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded asset %s: %v", name, err)
	}
	return string(b)
}

// Every client file that performs an operator write or self-filter.
var consoleActorFiles = []string{
	"static/js/main.js",
	"static/js/api-client.js",
	"static/v2/api.js",
	"static/v2/board.js",
	"static/v2/messages.js",
	"static/v2/memory.js",
}

// An operator/actor value literally set to "user": an object/ternary value
// `agent: 'user'` / `agent_name: … : 'user'` (`:` then the literal), a `|| "user"`
// default, or a positional 4th arg `, "user")` to a task mutation. Receive-side
// predicates (`msg.to === "user"`, `profile_slug === "user"`, `who === 'user'`)
// use `===`, never `:`/`||`/`,`, so they are NOT flagged and keep matching legacy rows.
var actorUserRe = regexp.MustCompile(`:\s*['"]user['"]|\|\|\s*['"]user['"]|,\s*['"]user['"]\s*\)`)

// AC1 — no console write names the operator as literal "user"; the operator
// constant is "human" in both consoles.
func TestConsoleOperatorSendsAsHuman(t *testing.T) {
	for _, f := range consoleActorFiles {
		if m := actorUserRe.FindString(readConsoleAsset(t, f)); m != "" {
			t.Errorf("%s names the operator as literal user: %q", f, m)
		}
	}
	if !strings.Contains(readConsoleAsset(t, "static/js/main.js"), `const OPERATOR = "human"`) {
		t.Error(`main.js must define the operator constant OPERATOR = "human"`)
	}
	if !strings.Contains(readConsoleAsset(t, "static/v2/messages.js"), `const ME = 'human'`) {
		t.Error(`v2 messages.js operator identity ME must be 'human'`)
	}
}

// AC2 — the operator inbox/thread predicate accepts both legacy "user" and "human".
func TestConsoleInboxAcceptsUserAndHuman(t *testing.T) {
	main := readConsoleAsset(t, "static/js/main.js")
	if !strings.Contains(main, `msg.to === "user"`) || !strings.Contains(main, `msg.to === "human"`) {
		t.Error(`v1 operator-inbox isForUser must accept msg.to "user" AND "human"`)
	}
	msgs := readConsoleAsset(t, "static/v2/messages.js")
	if !strings.Contains(msgs, `who === ME || who === 'user'`) {
		t.Error("v2 messages self-predicate must accept both human (ME) and legacy user")
	}
}

// AC3 — assets outside the operator-identity change carry none of its tokens, so
// the diff stays confined to the console write/filter surface. (The rest of AC3 —
// the full suite green — is this package compiling and passing.)
func TestConsoleOtherAssetsUntouched(t *testing.T) {
	tokens := []string{"OPERATOR", `ME = 'human'`, "isMe("}
	for _, name := range []string{
		"static/index.html", "static/style.css",
		"static/v2/v2.css", "static/v2/settings.js", "static/v2/home.js", "static/v2/team.js",
	} {
		a := readConsoleAsset(t, name)
		for _, tok := range tokens {
			if strings.Contains(a, tok) {
				t.Errorf("asset %s unexpectedly carries operator-identity token %q", name, tok)
			}
		}
	}
}
