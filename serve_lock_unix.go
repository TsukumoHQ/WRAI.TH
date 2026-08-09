//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// errLockHeld marks "another live relay holds the serve lock" — the ONLY
// case the caller should report as a second-writer refusal. Every other
// error (missing state dir, permissions) is an environment problem and
// must surface as itself, not as a phantom second relay.
var errLockHeld = errors.New("serve lock held by another process")

// acquireServeLock takes an exclusive, non-blocking advisory lock on a lockfile
// next to the DB so two relay processes can NEVER serve the same database at
// once — the failure that wiped agents+teams when a second (launchd) relay came
// up on the same SQLite file. Returns a release func, or errLockHeld if another
// live relay already holds it (caller should refuse to start). The lock is held
// for the process lifetime and released by the OS if the process dies.
func acquireServeLock(path string) (func(), error) {
	// Fresh host: the state dir may not exist yet (serve locks before db.New
	// creates it). Creating it here must not read as "another relay is
	// serving" — found live: every brand-new install died on the ENOENT.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errLockHeld
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
