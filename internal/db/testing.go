package db

import (
	"database/sql"
)

// NewTestDB creates a database at the given path for testing. It mirrors New():
// every connection ATTACHes the sibling analytics DB (where token_usage lives),
// and the reader pool is opened after migrate() so the analytics file exists for
// its read-only attach.
func NewTestDB(path string) (*DB, error) {
	driverName := registerAttachDriver(analyticsDBPath(path))

	conn, err := sql.Open(driverName, path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	if err := migrate(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader, err := sql.Open(driverName, path+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON")
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(10)
	return &DB{conn: conn, reader: reader, path: path}, nil
}
