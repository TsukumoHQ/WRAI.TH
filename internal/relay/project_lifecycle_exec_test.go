package relay

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// The project-lifecycle executive arm (task 05936947): archive/unarchive/
// delete_project act ON a target project the caller need not be registered in.
// guardIdentity's default rule (caller must be registered in the resolved ==
// target project) makes a retired catch-all like 'default' — where registration
// is refused by design — unarchivable forever. An ACTIVE executive registered
// anywhere may run these three, audited; every other unregistered caller is
// refused exactly as before.

// expectNotRegistered asserts the unchanged bare-string refusal a caller not
// registered in the target project gets.
func expectNotRegistered(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	msg := expectError(t, res)
	if want := "is not registered in project"; !strings.Contains(msg, want) {
		t.Fatalf("want %q refusal, got: %s", want, msg)
	}
}

// lifecycleAdminAudit returns the project.lifecycle.admin audit rows recorded
// against the target project (ResourceID is empty for these).
func lifecycleAdminAudit(t *testing.T, h *Handlers, project string) []struct{ actor, proj, reason string } {
	t.Helper()
	rows, err := h.db.ListAudit(project, "", 50)
	if err != nil {
		t.Fatalf("ListAudit(%s): %v", project, err)
	}
	var out []struct{ actor, proj, reason string }
	for _, e := range rows {
		if e.Action == "project.lifecycle.admin" {
			out = append(out, struct{ actor, proj, reason string }{e.Actor, e.Project, e.Reason})
		}
	}
	return out
}

// TestLifecycleArm_RemoteExecutiveArchives (AC1): an is_executive agent
// registered only in project A archives project B (no registration in B) and
// an audit_log row Action=project.lifecycle.admin names actor + target + home.
// Revert-check: deleting the projectLifecycleTools branch in guardIdentity turns
// this red with the 'is not registered in project' refusal.
func TestLifecycleArm_RemoteExecutiveArchives(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "home", "chief", map[string]any{"is_executive": true})
	h.db.EnsureProject("target-b")

	archive := guardedHandler(t, h, "archive_project")
	res, _ := archive(ctx, call(map[string]any{"project": "target-b", "as": "chief"}))
	if res.IsError {
		t.Fatalf("remote executive must archive an unregistered project: %s", expectError(t, res))
	}
	if got := parseJSON(t, res)["archived"]; got != true {
		t.Fatalf("archived != true: %v", got)
	}
	if !h.db.IsProjectArchived("target-b") {
		t.Fatal("target-b should be archived")
	}

	audits := lifecycleAdminAudit(t, h, "target-b")
	if len(audits) != 1 {
		t.Fatalf("want exactly one project.lifecycle.admin audit row, got %d", len(audits))
	}
	a := audits[0]
	if a.actor != "chief" || a.proj != "target-b" {
		t.Fatalf("audit row must name actor=chief target=target-b, got actor=%q project=%q", a.actor, a.proj)
	}
	if !strings.Contains(a.reason, "home") {
		t.Fatalf("audit reason must name the home project, got: %q", a.reason)
	}
}

// TestLifecycleArm_RemoteExecutiveArchivesDefault (AC1, default variant): the
// target may be 'default' — the exact project registration refuses by design,
// so the arm is the only way to retire it.
func TestLifecycleArm_RemoteExecutiveArchivesDefault(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "home", "chief", map[string]any{"is_executive": true})
	h.db.EnsureProject("default")

	archive := guardedHandler(t, h, "archive_project")
	res, _ := archive(ctx, call(map[string]any{"project": "default", "as": "chief"}))
	if res.IsError {
		t.Fatalf("remote executive must archive 'default': %s", expectError(t, res))
	}
	if !h.db.IsProjectArchived("default") {
		t.Fatal("default should be archived")
	}
}

// TestLifecycleArm_NonExecutiveRefused (AC2): a NON-executive registered only in
// A calling archive_project project=B is refused with the unchanged
// 'is not registered in project' text, and no audit row is written.
func TestLifecycleArm_NonExecutiveRefused(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "home", "worker", nil) // not executive
	h.db.EnsureProject("target-b")

	archive := guardedHandler(t, h, "archive_project")
	res, _ := archive(ctx, call(map[string]any{"project": "target-b", "as": "worker"}))
	expectNotRegistered(t, res)

	if h.db.IsProjectArchived("target-b") {
		t.Fatal("non-executive must not have archived target-b")
	}
	if n := len(lifecycleAdminAudit(t, h, "target-b")); n != 0 {
		t.Fatalf("no arm audit expected for a refused non-executive, got %d", n)
	}
}

// TestLifecycleArm_UnregisteredExecutiveOnlyPath: the arm is the ONLY new path.
// An executive REGISTERED in the target project archives exactly as today (the
// agent!=nil branch), and the executive arm does not fire (no arm audit row).
func TestLifecycleArm_RegisteredCallerUnchanged(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "target-b", "chief", map[string]any{"is_executive": true})

	archive := guardedHandler(t, h, "archive_project")
	res, _ := archive(ctx, call(map[string]any{"project": "target-b", "as": "chief"}))
	if res.IsError {
		t.Fatalf("caller registered in the target archives as today: %s", expectError(t, res))
	}
	if !h.db.IsProjectArchived("target-b") {
		t.Fatal("target-b should be archived")
	}
	if n := len(lifecycleAdminAudit(t, h, "target-b")); n != 0 {
		t.Fatalf("registered-caller path must not take the executive arm (no arm audit), got %d", n)
	}
}

// TestLifecycleArm_UnarchiveAndDelete (AC4): unarchive_project and delete_project
// take the same arm. A remote executive unarchives an archived target; and
// delete_project by a remote executive purges an archived target but still
// refuses a non-archived one ('archive_project it first').
func TestLifecycleArm_UnarchiveAndDelete(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "home", "chief", map[string]any{"is_executive": true})

	// unarchive an archived target
	h.db.EnsureProject("target-u")
	if err := h.db.ArchiveProject("target-u"); err != nil {
		t.Fatalf("archive target-u: %v", err)
	}
	unarchive := guardedHandler(t, h, "unarchive_project")
	if r, _ := unarchive(ctx, call(map[string]any{"project": "target-u", "as": "chief"})); r.IsError {
		t.Fatalf("remote executive must unarchive: %s", expectError(t, r))
	}
	if h.db.IsProjectArchived("target-u") {
		t.Fatal("target-u should be unarchived")
	}

	// delete an archived target → purge
	h.db.EnsureProject("target-d")
	if err := h.db.ArchiveProject("target-d"); err != nil {
		t.Fatalf("archive target-d: %v", err)
	}
	del := guardedHandler(t, h, "delete_project")
	if r, _ := del(ctx, call(map[string]any{"project": "target-d", "as": "chief"})); r.IsError {
		t.Fatalf("remote executive must delete an archived project: %s", expectError(t, r))
	}
	if h.db.IsProjectArchived("target-d") {
		t.Fatal("target-d should be purged")
	}

	// delete a NON-archived target → the arm passes the guard but the handler
	// still enforces archived-first.
	h.db.EnsureProject("target-active")
	res, _ := del(ctx, call(map[string]any{"project": "target-active", "as": "chief"}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "archive_project it first") {
		t.Fatalf("delete on a non-archived target must still refuse 'archive first', got: %s", msg)
	}
}
