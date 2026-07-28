//go:build windows

package main

import "errors"

// errLockHeld mirrors the unix declaration so main.go's errors.Is check
// compiles on Windows too (never returned here).
var errLockHeld = errors.New("serve lock held by another process")

// acquireServeLock is a no-op on Windows (flock is unix-only). Windows isn't a
// relay host in this deployment; revisit with LockFileEx if that changes.
func acquireServeLock(_ string) (func(), error) {
	return func() {}, nil
}
