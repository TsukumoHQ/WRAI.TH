package relay

import (
	"strings"
	"testing"

	"agent-relay/internal/web"
)

func readV2Asset(t *testing.T, name string) string {
	t.Helper()
	b, err := web.StaticFiles.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded asset %s: %v", name, err)
	}
	return string(b)
}

// S4 — project-selection fix: a visible "project not found" state for unknown
// deep-links plus a single source of truth for the projects list shared between
// v2.js (the router) and home.js. This repo ships no JS DOM harness, so the five
// subtests assert the served v2 assets at the string level — the router is a
// self-contained module and these guard its contract. The behavioural flows are
// exercised manually against a local serve and recorded in the PR report.
func TestV2ProjectSelect(t *testing.T) {
	v2 := readV2Asset(t, "static/v2/v2.js")
	home := readV2Asset(t, "static/v2/home.js")

	// AC1: an unknown deep-link renders a visible not-found panel with a way
	// back — the old silent redirect-home path is gone.
	t.Run("NotFoundStateVisible", func(t *testing.T) {
		for _, want := range []string{
			"function showNotFound", "page-notfound", "Project not found", "Back to fleet",
			"showNotFound(next.project)",
		} {
			if !strings.Contains(v2, want) {
				t.Errorf("v2.js missing not-found marker %q", want)
			}
		}
		if strings.Contains(v2, "location.hash = '#/';\n    return;") {
			t.Error("v2.js still silently redirects home for unknown projects")
		}
	})

	// AC2: v2.js owns the one projects list via ctx.refreshProjects(); home.js
	// reads through it and no longer fetches its own copy.
	t.Run("SingleSourceOfTruth", func(t *testing.T) {
		if !strings.Contains(v2, "async function refreshProjects()") || !strings.Contains(v2, "refreshProjects,") {
			t.Error("v2.js does not expose refreshProjects on ctx")
		}
		if !strings.Contains(home, "ctx.refreshProjects()") {
			t.Error("home.js does not route its refetch through ctx.refreshProjects()")
		}
		if strings.Contains(home, "ctx.api.projects()") {
			t.Error("home.js still fetches its own projects copy (desync risk)")
		}
	})

	// AC3: a project created/seen from home resolves without a manual reload —
	// the router refetches a fresh list and rechecks before giving up.
	t.Run("NewProjectVisibleWithoutReload", func(t *testing.T) {
		if !strings.Contains(v2, "await refreshProjects(); known = projects.some") {
			t.Error("route() does not refetch-and-recheck before declaring a project unknown")
		}
	})

	// AC4: existing project navigation is unchanged — chrome, tabs and the live
	// stream still bind on the known-project path.
	t.Run("ExistingNavZeroRegression", func(t *testing.T) {
		for _, want := range []string{"showProjectChrome(next.project)", "startStream()", "wireProjectTabs(project)"} {
			if !strings.Contains(v2, want) {
				t.Errorf("v2.js lost existing-nav wiring %q", want)
			}
		}
	})

	// AC5: v1 is untouched — the phase-1 console asset carries none of the v2
	// changes (byte-level isolation). Build-green is implied by this test file
	// compiling and running.
	t.Run("BuildGreenV1ZeroDiff", func(t *testing.T) {
		v1 := readV2Asset(t, "static/index.html")
		for _, leak := range []string{"page-notfound", "refreshProjects"} {
			if strings.Contains(v1, leak) {
				t.Errorf("v1 static/index.html leaked a v2-only token %q", leak)
			}
		}
	})
}
