package db

import (
	"context"
	"database/sql"
	"time"
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

// HoldPoolsForTest checks out `readers` reader-pool connections and, when
// writer is true, the single writer connection, and keeps them until release
// is called — a deterministic stand-in for the 2026-10-02 wedge (task
// 55323073), where slow queries pinned every pooled connection. Test-only.
func (d *DB) HoldPoolsForTest(readers int, writer bool) (release func(), err error) {
	var held []*sql.Conn
	release = func() {
		for _, c := range held {
			_ = c.Close()
		}
		held = nil
	}
	ctx := context.Background()
	for i := 0; i < readers; i++ {
		c, err := d.reader.Conn(ctx)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, c)
	}
	if writer {
		c, err := d.conn.Conn(ctx)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, c)
	}
	return release, nil
}

// SetReaderTimeoutForTest shrinks readerTimeout so a test can exercise the
// starved-reader path without a real 10s wait. Returns the restore func.
func SetReaderTimeoutForTest(t time.Duration) (restore func()) {
	orig := readerTimeout
	readerTimeout = t
	return func() { readerTimeout = orig }
}

// SetWriterTimeoutForTest is the writerTimeout sibling of
// SetReaderTimeoutForTest. Returns the restore func.
func SetWriterTimeoutForTest(t time.Duration) (restore func()) {
	orig := writerTimeout
	writerTimeout = t
	return func() { writerTimeout = orig }
}
