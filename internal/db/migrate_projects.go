package db

import (
	"database/sql"
	"fmt"
	"log"
)

// migrateNormalizeProjects rewrites every stored project identifier to its
// canonical form — TRIM + LOWER + underscores folded to hyphens — matching
// relay.NormalizeProject, which the handlers now apply to every incoming
// call. Without this boot-time pass, an updated relay would normalize NEW
// writes while the DB kept the old spellings ("testDuSoir", "synergix_prod")
// and every existing agent/project would silently split into two namespaces
// on upgrade.
//
// Runs on every boot, idempotent: once the data is canonical the WHERE
// clauses match nothing. Names starting with "_" (internal pseudo-projects
// like the vault's "_relay") and empty strings are left untouched.
//
// Collisions (both spellings already exist — e.g. an agent registered under
// "synergix_prod" AND "synergix-prod"): UPDATE OR IGNORE keeps the row that
// already holds the canonical name and the follow-up DELETE drops the
// non-canonical duplicate. The canonical row wins; the duplicate was
// unreachable through the normalized handlers anyway.
func migrateNormalizeProjects(conn *sql.DB) {
	tx, err := conn.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Every table carrying a `project` column, discovered live so future
	// tables are covered without touching this migration.
	rows, err := tx.Query(`
		SELECT m.name FROM sqlite_master m
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
		  AND EXISTS (SELECT 1 FROM pragma_table_info(m.name) c WHERE c.name = 'project')`)
	if err != nil {
		return
	}
	var tables []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tables = append(tables, t)
		}
	}
	_ = rows.Close()

	total := int64(0)
	apply := func(table, col string) bool {
		// Canonical form in SQL — must stay in lockstep with
		// relay.NormalizeProject. Exempt: NULL/empty and internal "_" names.
		norm := fmt.Sprintf("LOWER(REPLACE(TRIM(%s), '_', '-'))", col)
		where := fmt.Sprintf(
			` WHERE %s IS NOT NULL AND %s != '' AND %s NOT LIKE '\_%%' ESCAPE '\' AND %s != %s`,
			col, col, col, col, norm)
		res, err := tx.Exec(fmt.Sprintf("UPDATE OR IGNORE %s SET %s = %s%s", table, col, norm, where))
		if err != nil {
			log.Printf("project normalize: %s update failed: %v", table, err)
			return false
		}
		if n, _ := res.RowsAffected(); n > 0 {
			total += n
		}
		// Leftovers = rows whose canonical twin already exists (the OR IGNORE
		// skipped them on a unique constraint). The canonical row wins.
		res, err = tx.Exec(fmt.Sprintf("DELETE FROM %s%s", table, where))
		if err != nil {
			log.Printf("project normalize: %s dedup failed: %v", table, err)
			return false
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("project normalize: %s dropped %d duplicate row(s) whose canonical spelling already existed", table, n)
			total += n
		}
		return true
	}

	for _, t := range tables {
		if !apply(t, "project") {
			return
		}
	}
	// The projects registry itself keys on `name`.
	if !apply("projects", "name") {
		return
	}

	if err := tx.Commit(); err != nil {
		return
	}
	if total > 0 {
		log.Printf("project normalize: %d row(s) migrated to canonical project names", total)
	}
}
