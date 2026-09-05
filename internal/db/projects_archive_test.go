package db

import (
	"testing"

	"agent-relay/internal/models"
)

// AC1: archived_at column added via ensureColumns; a fresh project row is active
// (archived_at NULL). testDB() runs migrate(), so a clean open raises no error.
func TestArchiveProjectSchemaColumn(t *testing.T) {
	d := testDB(t)
	seedProject(t, d.conn, "alpha")
	var archivedAt interface{}
	if err := d.conn.QueryRow(`SELECT archived_at FROM projects WHERE name = 'alpha'`).Scan(&archivedAt); err != nil {
		t.Fatalf("select archived_at (column missing?): %v", err)
	}
	if archivedAt != nil {
		t.Fatalf("fresh project should be active (archived_at NULL), got %v", archivedAt)
	}
}

// AC2: archive flips archived_at in one tx; already-archived -> error; unknown ->
// error; unarchive resets to NULL; not-archived / unknown unarchive -> error.
func TestArchiveUnarchiveProjectVerbs(t *testing.T) {
	d := testDB(t)
	seedProject(t, d.conn, "alpha")

	if err := d.ArchiveProject("alpha"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	var at interface{}
	_ = d.conn.QueryRow(`SELECT archived_at FROM projects WHERE name='alpha'`).Scan(&at)
	if at == nil {
		t.Fatal("archived_at should be set after archive")
	}
	if err := d.ArchiveProject("alpha"); err == nil {
		t.Fatal("re-archiving an archived project should error")
	}
	if err := d.ArchiveProject("ghost"); err == nil {
		t.Fatal("archiving an unknown project should error")
	}

	if err := d.UnarchiveProject("alpha"); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	_ = d.conn.QueryRow(`SELECT archived_at FROM projects WHERE name='alpha'`).Scan(&at)
	if at != nil {
		t.Fatalf("archived_at should be NULL after unarchive, got %v", at)
	}
	if err := d.UnarchiveProject("alpha"); err == nil {
		t.Fatal("unarchiving an active project should error")
	}
	if err := d.UnarchiveProject("ghost"); err == nil {
		t.Fatal("unarchiving an unknown project should error")
	}
}

// AC2: archive/unarchive touch ONLY the projects row — zero cascade. Row counts
// in every project-scoped data table are identical before and after.
func TestArchiveProjectZeroCascade(t *testing.T) {
	d := testDB(t)
	seedProject(t, d.conn, "alpha")
	seedAgent(t, d.conn, "alpha", "alice", "active", "", "", 0)
	seedTask(t, d.conn, "t1", "alpha", "accepted", "", "", "", "", "", "", false)
	seedMemory(t, d.conn, "m1", "alpha", false)
	seedMessage(t, d.conn, "msg1", "alpha", "alice", "bob")

	tables := []string{"agents", "tasks", "memories", "messages"}
	before := map[string]int{}
	for _, tbl := range tables {
		before[tbl] = countProjectRows(t, d, tbl, "alpha")
	}
	if err := d.ArchiveProject("alpha"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := d.UnarchiveProject("alpha"); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	for _, tbl := range tables {
		if got := countProjectRows(t, d, tbl, "alpha"); got != before[tbl] {
			t.Fatalf("%s row count changed: before=%d after=%d (cascade!)", tbl, before[tbl], got)
		}
	}
}

// AC3: ListProjects + ListProjectsWithInfo exclude archived by default; the
// includeArchived variants return them, with archived_at populated.
func TestListProjectsExcludeArchived(t *testing.T) {
	d := testDB(t)
	seedProject(t, d.conn, "alpha")
	seedProject(t, d.conn, "beta")
	// ListProjects derives names from agents/messages/conversations.
	seedAgent(t, d.conn, "alpha", "a1", "active", "", "", 0)
	seedAgent(t, d.conn, "beta", "b1", "active", "", "", 0)
	if err := d.ArchiveProject("beta"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	names, err := d.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if containsStr(names, "beta") || !containsStr(names, "alpha") {
		t.Fatalf("ListProjects default should exclude archived beta, include alpha: %v", names)
	}
	all, err := d.ListProjectsFiltered(true)
	if err != nil {
		t.Fatalf("ListProjectsFiltered: %v", err)
	}
	if !containsStr(all, "beta") {
		t.Fatalf("ListProjectsFiltered(true) should include beta: %v", all)
	}

	infos, err := d.ListProjectsWithInfo()
	if err != nil {
		t.Fatalf("ListProjectsWithInfo: %v", err)
	}
	if projectInfoByName(infos, "beta") != nil {
		t.Fatal("ListProjectsWithInfo default should exclude archived beta")
	}
	infosAll, err := d.ListProjectsWithInfoFiltered(true)
	if err != nil {
		t.Fatalf("ListProjectsWithInfoFiltered: %v", err)
	}
	b := projectInfoByName(infosAll, "beta")
	if b == nil {
		t.Fatal("ListProjectsWithInfoFiltered(true) should include beta")
	}
	if b.ArchivedAt == "" {
		t.Fatal("archived project's ArchivedAt should be populated")
	}
}

// AC4: archive_project is fail-closed on a Linear-backed project (a value in
// linear_project_map) until S7; a non-mirrored project is unaffected.
func TestArchiveProjectRefusesLinearBacked(t *testing.T) {
	d := testDB(t)
	seedProject(t, d.conn, "mirrored")
	d.SetSetting("linear_project_map", `{"lp-1":"mirrored"}`)
	if err := d.ArchiveProject("mirrored"); err == nil {
		t.Fatal("archiving a Linear-backed project should be refused")
	}
	var at interface{}
	_ = d.conn.QueryRow(`SELECT archived_at FROM projects WHERE name='mirrored'`).Scan(&at)
	if at != nil {
		t.Fatal("refused archive must not have flipped archived_at")
	}

	seedProject(t, d.conn, "solo")
	if err := d.ArchiveProject("solo"); err != nil {
		t.Fatalf("archiving a non-Linear project should succeed: %v", err)
	}
}

// AC5 (§4 invariant regression): archiving a project with live tasks + memories
// must NOT make their project refs orphans — the projects row still exists, so
// the existence-only orphan_*_project checks stay clean. A future edit that adds
// an archived_at filter to those subqueries would break this test.
func TestArchivedProjectRefsNotOrphaned(t *testing.T) {
	d := testDB(t)
	seedProject(t, d.conn, "alpha")
	seedTask(t, d.conn, "t1", "alpha", "accepted", "", "", "", "", "", "", false)
	seedMemory(t, d.conn, "m1", "alpha", false)

	if err := d.ArchiveProject("alpha"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	counts, err := d.RunReferentialScan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if counts["orphan_task_project"] != 0 {
		t.Fatalf("archived project's task must not be orphaned: %d", counts["orphan_task_project"])
	}
	if counts["orphan_memory_project"] != 0 {
		t.Fatalf("archived project's memory must not be orphaned: %d", counts["orphan_memory_project"])
	}
	if quarantineRowExists(t, d.conn, "orphan_task_project", "t1") {
		t.Fatal("task t1 must not be quarantined after its project is archived")
	}
	if quarantineRowExists(t, d.conn, "orphan_memory_project", "m1") {
		t.Fatal("memory m1 must not be quarantined after its project is archived")
	}
}

func countProjectRows(t *testing.T, d *DB, table, project string) int {
	t.Helper()
	var n int
	if err := d.conn.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE project = ?", project).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func projectInfoByName(xs []models.ProjectInfo, name string) *models.ProjectInfo {
	for i := range xs {
		if xs[i].Name == name {
			return &xs[i]
		}
	}
	return nil
}
