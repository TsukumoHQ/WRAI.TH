package db

import (
	"database/sql"
	"log"
)

// migratePurgeDefaultProject deletes the retired 'default' catch-all project and
// every row attributed to it. The implicit 'default' project was where any call
// that named no project silently landed; it collapsed unrelated teams into one
// shared namespace. The handlers now reject an unresolved project outright
// (guardIdentity), so the leftover 'default' data is unreachable noise — this
// removes it.
//
// Tables are discovered live from sqlite_master (any table carrying a `project`
// column), so future tables are covered without editing this migration. The
// projects registry itself keys on `name`. Runs on every boot; idempotent —
// once purged the WHERE clauses match nothing.
//
// Internal pseudo-projects (names starting with '_', e.g. the vault's '_relay')
// are never touched. This only ever targets the literal 'default'.
func migratePurgeDefaultProject(conn *sql.DB) {
	tx, err := conn.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()

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
	del := func(table, col string) bool {
		res, err := tx.Exec("DELETE FROM " + table + " WHERE " + col + " = 'default'") //nolint:gosec // table/col are internal identifiers, never user input
		if err != nil {
			log.Printf("purge default: %s delete failed: %v", table, err)
			return false
		}
		if n, _ := res.RowsAffected(); n > 0 {
			total += n
		}
		return true
	}

	for _, t := range tables {
		if !del(t, "project") {
			return
		}
	}
	// The projects registry keys the project on `name`, not `project`.
	if !del("projects", "name") {
		return
	}

	if err := tx.Commit(); err != nil {
		return
	}
	if total > 0 {
		log.Printf("purge default: removed %d row(s) belonging to the retired 'default' project", total)
	}
}
