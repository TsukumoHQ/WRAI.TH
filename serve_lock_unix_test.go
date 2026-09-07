//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestServeLockFreshHostENOENT pins the PR #149 fix: on a fresh host the lock's
// parent directory does not exist yet (serve takes the lock before db.New
// creates the state dir). Acquiring must CREATE the parent and succeed, not
// report a phantom second writer — while a real second holder on the same path
// is still refused, so the single-writer guarantee is intact.
func TestServeLockFreshHostENOENT(t *testing.T) {
	base := t.TempDir()
	lockPath := filepath.Join(base, "nonexistent", "sub", "relay.db.lock")
	if _, err := os.Stat(filepath.Dir(lockPath)); !os.IsNotExist(err) {
		t.Fatalf("precondition: parent dir must not exist, stat err = %v", err)
	}

	// Fresh host: parent missing → acquire creates it and succeeds.
	release, err := acquireServeLock(lockPath)
	if err != nil {
		t.Fatalf("fresh-host acquire should succeed (create parent), got %v", err)
	}
	if release == nil {
		t.Fatal("expected a non-nil release func")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should exist after acquire: %v", err)
	}

	// Real second writer on the SAME path is still refused (errLockHeld), a
	// separate open file description → flock LOCK_NB conflicts.
	if _, err := acquireServeLock(lockPath); !errors.Is(err, errLockHeld) {
		t.Fatalf("second acquire must return errLockHeld, got %v", err)
	}

	// After release the OS drops the flock, so the path is lockable again.
	release()
	release2, err := acquireServeLock(lockPath)
	if err != nil {
		t.Fatalf("re-acquire after release should succeed, got %v", err)
	}
	release2()
}

// TestServeLockDiffScope backs AC2. The diff-scope + author-preserved half of
// AC2 is enforced at the gate (git-level); the testable half is the contract
// the whole fix turns on: ONLY EWOULDBLOCK/EAGAIN is the errLockHeld
// second-writer sentinel, every other (environment) error surfaces as itself —
// otherwise a fresh-host problem reads as a phantom second writer, the bug this
// PR fixes. Here MkdirAll fails because the lock's parent is a regular file, so
// acquire must return a plain error, never errLockHeld.
func TestServeLockDiffScope(t *testing.T) {
	base := t.TempDir()
	fileNotDir := filepath.Join(base, "afile")
	if err := os.WriteFile(fileNotDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(fileNotDir, "sub", "relay.db.lock") // parent is a file

	_, err := acquireServeLock(lockPath)
	if err == nil {
		t.Fatal("expected an error when the lock parent cannot be created")
	}
	if errors.Is(err, errLockHeld) {
		t.Fatalf("an environment error must NOT be reported as errLockHeld, got %v", err)
	}
}
