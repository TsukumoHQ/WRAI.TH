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
// projects registry itself keys on `name`.
//
// Called ONCE, gated by the 'purge_default_project' settings marker in migrate()
// — deliberately NOT on every boot: until every non-MCP path (REST, connectors)
// is migrated off its own = "default" fallback, a row can still be written to
// 'default' at runtime, and an every-boot purge would silently delete it on the
// next restart. The function itself is idempotent (a re-run matches nothing), so
// it is also safe to call directly in tests.
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

	// Tables where 'default' is NOT the retired catch-all but a load-bearing
	// sentinel with its own meaning — never purge these. In notification_rules a
	// rule with project='default' is the GLOBAL rule (the evaluator fires it for
	// every project, see relay/notifications.go), not a rule that landed in the
	// catch-all. Deleting them would silently disable the global/autonomy
	// notification rules.
	skip := map[string]bool{"notification_rules": true}

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

	// ID-linked child tables that carry no `project` column of their own, so the
	// sqlite_master discovery above cannot see them. Scope each to 'default' via
	// its parent and purge FIRST — otherwise the parent's deletion below strands
	// them as orphans (no enforced FK catches it; the link is a bare TEXT id).
	for _, q := range []string{
		"DELETE FROM conversation_members WHERE conversation_id IN (SELECT id FROM conversations WHERE project = 'default')",
		"DELETE FROM conversation_reads WHERE conversation_id IN (SELECT id FROM conversations WHERE project = 'default')",
		"DELETE FROM team_inbox WHERE team_id IN (SELECT id FROM teams WHERE project = 'default')",
		"DELETE FROM message_reads WHERE message_id IN (SELECT id FROM messages WHERE project = 'default')",
	} {
		res, err := tx.Exec(q)
		if err != nil {
			log.Printf("purge default: junction delete failed: %v", err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			total += n
		}
	}

	// The 'mauvais waterfall': the ONLY enforced FK among project-column tables is
	// deliveries.message_id -> messages(id) (RESTRICT, no ON DELETE CASCADE, see
	// db.go:380). sqlite_master discovery order is table-CREATION order, which
	// lists `messages` BEFORE `deliveries`, so a naive in-order purge deletes the
	// parent first and fails with "FOREIGN KEY constraint failed", aborting the
	// whole purge. Defer the FK parents to a trailing pass so every child is gone
	// first. All other project-column tables carry no enforced FK and are
	// order-independent.
	deferred := map[string]bool{"messages": true}
	for _, t := range tables {
		if skip[t] || deferred[t] {
			continue
		}
		if !del(t, "project") {
			return
		}
	}
	for _, t := range tables {
		if deferred[t] && !del(t, "project") {
			return
		}
	}
	// The projects registry keys the project on `name`, not `project`.
	if !del("projects", "name") {
		return
	}

	// FTS shadow tables are trigger-synced on every base-row DELETE, so the purges
	// above already removed the matching index rows; this defensive rebuild
	// guarantees no dangling index row survives (e.g. from a row ever removed out
	// of band). Only the two content-backed FTS tables exist — memories_fts and
	// vault_docs_fts; messages/tasks/conversations have no FTS. Cheap here: the
	// retired 'default' data is small and this migration runs once.
	for _, fts := range []string{"memories_fts", "vault_docs_fts"} {
		if _, err := tx.Exec("INSERT INTO " + fts + "(" + fts + ") VALUES('rebuild')"); err != nil { //nolint:gosec // fts is an internal identifier, never user input
			log.Printf("purge default: %s rebuild failed: %v", fts, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		return
	}
	if total > 0 {
		log.Printf("purge default: removed %d row(s) belonging to the retired 'default' project", total)
	}
}
