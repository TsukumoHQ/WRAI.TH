package db

import (
	"testing"
	"time"
)

// setLease stamps a held (unexpired) lease on a task so the cascade's "leased"
// branch fires.
func setLease(t *testing.T, d *DB, taskID, holder string) {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(
		`UPDATE tasks SET lease_holder = ?, lease_expires_at = ?, lease_heartbeat_at = ? WHERE id = ?`,
		holder, future, future, taskID,
	); err != nil {
		t.Fatalf("set lease on %s: %v", taskID, err)
	}
}

func taskStatus(t *testing.T, d *DB, id string) string {
	t.Helper()
	var s string
	if err := d.conn.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("status %s: %v", id, err)
	}
	return s
}

func leaseHolder(t *testing.T, d *DB, id string) string {
	t.Helper()
	var h *string
	if err := d.conn.QueryRow(`SELECT lease_holder FROM tasks WHERE id = ?`, id).Scan(&h); err != nil {
		t.Fatalf("lease_holder %s: %v", id, err)
	}
	if h == nil {
		return ""
	}
	return *h
}

// TestCascadeReleasesLeasedMarksAssignedClosesMemberships is the core Phase 2
// soft-cascade: a deactivated agent's LEASED tasks are released to pending, its
// non-leased ASSIGNED tasks are marked limbo (not moved), its memberships are
// soft-closed, and a DIFFERENT agent's task is untouched — nothing is deleted.
func TestCascadeReleasesLeasedMarksAssignedClosesMemberships(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedAgent(t, c, "p1", "holder", "active", "backend", "", 0)
	seedAgent(t, c, "p1", "other", "active", "backend", "", 0)

	// leased task held by 'holder'
	seedTask(t, c, "t-leased", "p1", "in-progress", "cto", "holder", "holder", "backend", "", "", false)
	setLease(t, d, "t-leased", "holder")
	// non-leased task merely ASSIGNED to 'holder'
	seedTask(t, c, "t-assigned", "p1", "pending", "cto", "holder", "", "backend", "", "", false)
	// a DIFFERENT live agent's leased task — must NOT be touched
	seedTask(t, c, "t-other", "p1", "in-progress", "cto", "other", "other", "backend", "", "", false)
	setLease(t, d, "t-other", "other")

	// memberships for 'holder'
	if _, err := c.Exec(`INSERT INTO team_members (team_id, agent_name, project, role, joined_at) VALUES ('tm1','holder','p1','member','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(`INSERT INTO conversations (id, title, created_by, created_at, project) VALUES ('cv1','c','cto','2026-01-01T00:00:00Z','p1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(`INSERT INTO conversation_members (conversation_id, agent_name, joined_at) VALUES ('cv1','holder','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	// deactivate then cascade (mirrors the handler)
	if err := d.DeactivateAgent("p1", "holder"); err != nil {
		t.Fatal(err)
	}
	res, err := d.CascadeAgentDeactivation("p1", "holder")
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}

	// leased task released to pending, lease cleared
	if s := taskStatus(t, d, "t-leased"); s != "pending" {
		t.Errorf("t-leased status = %q, want pending (released)", s)
	}
	if h := leaseHolder(t, d, "t-leased"); h != "" {
		t.Errorf("t-leased lease_holder = %q, want cleared", h)
	}
	if len(res.Released) != 1 || res.Released[0].TaskID != "t-leased" {
		t.Errorf("Released = %+v, want [t-leased]", res.Released)
	}

	// non-leased assigned task NOT transitioned, but MARKED limbo
	if s := taskStatus(t, d, "t-assigned"); s != "pending" {
		t.Errorf("t-assigned status changed to %q — a non-leased assignment must NOT be transitioned", s)
	}
	if taskAssignee(t, d, "t-assigned") != "holder" {
		t.Error("t-assigned assignee must be left intact (marked, not rerouted)")
	}
	if !quarantineRowExists(t, c, "limbo", "t-assigned") {
		t.Error("t-assigned must be marked limbo")
	}
	if res.MarkedLimbo != 1 {
		t.Errorf("MarkedLimbo = %d, want 1", res.MarkedLimbo)
	}

	// the other agent's task is untouched
	if s := taskStatus(t, d, "t-other"); s != "in-progress" {
		t.Errorf("t-other status = %q — a different agent's task must be untouched", s)
	}
	if leaseHolder(t, d, "t-other") != "other" {
		t.Error("t-other lease must be untouched")
	}

	// memberships soft-closed (left_at set), NOT deleted
	var tmLeft, cvLeft *string
	_ = c.QueryRow(`SELECT left_at FROM team_members WHERE team_id='tm1' AND agent_name='holder'`).Scan(&tmLeft)
	_ = c.QueryRow(`SELECT left_at FROM conversation_members WHERE conversation_id='cv1' AND agent_name='holder'`).Scan(&cvLeft)
	if tmLeft == nil {
		t.Error("team membership must be soft-closed (left_at set), row kept")
	}
	if cvLeft == nil {
		t.Error("conversation membership must be soft-closed (left_at set), row kept")
	}
	if res.LeftTeams != 1 || res.LeftConvos != 1 {
		t.Errorf("LeftTeams=%d LeftConvos=%d, want 1/1", res.LeftTeams, res.LeftConvos)
	}
}

// TestCascadeSkipsLiveRacedClaim proves the CAS guard: if a leased task was
// re-claimed by a live agent between the list and the release (simulated by
// changing the holder), the cascade does NOT clobber it.
func TestCascadeSkipsLiveRacedClaim(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedAgent(t, c, "p1", "holder", "inactive", "backend", "", 0) // already deactivated
	seedAgent(t, c, "p1", "newowner", "active", "backend", "", 0)

	seedTask(t, c, "t-raced", "p1", "in-progress", "cto", "newowner", "newowner", "backend", "", "", false)
	setLease(t, d, "t-raced", "newowner") // now held by a DIFFERENT (live) agent

	// Cascade for 'holder' must not touch a task the CAS guard shows is held by
	// someone else.
	res, err := d.CascadeAgentDeactivation("p1", "holder")
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if len(res.Released) != 0 {
		t.Errorf("Released = %+v, want none (task held by another agent)", res.Released)
	}
	if leaseHolder(t, d, "t-raced") != "newowner" {
		t.Error("raced task must keep its live holder")
	}
}

// TestReassignOnWriteMarksUnresolvedAssignee covers Phase 2 §7.1: reassigning to
// a non-resolving agent stamps a quarantine marker (never rejects); reassigning
// to a live agent stamps nothing.
func TestReassignOnWriteMarksUnresolvedAssignee(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedAgent(t, c, "p1", "live-agent", "active", "backend", "", 0)
	seedTask(t, c, "t-a", "p1", "in-progress", "cto", "live-agent", "live-agent", "backend", "", "", false)
	seedTask(t, c, "t-b", "p1", "in-progress", "cto", "live-agent", "live-agent", "backend", "", "", false)

	// reassign to a NON-existent agent → marked, but the reassign still stands
	if _, err := d.ReassignTask("t-a", "p1", "ghost-agent"); err != nil {
		t.Fatalf("reassign to ghost: %v", err)
	}
	if taskAssignee(t, d, "t-a") != "ghost-agent" {
		t.Error("reassignment must stand even for an unresolved agent (never rejected)")
	}
	if !quarantineRowExists(t, c, "orphan_assignee", "t-a") {
		t.Error("reassigning to a non-resolving agent must stamp orphan_assignee")
	}

	// reassign to a LIVE agent → no marker
	if _, err := d.ReassignTask("t-b", "p1", "live-agent"); err != nil {
		t.Fatalf("reassign to live: %v", err)
	}
	if quarantineRowExists(t, c, "orphan_assignee", "t-b") {
		t.Error("reassigning to a live agent must not stamp a marker")
	}
}
