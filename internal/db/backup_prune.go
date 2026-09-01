package db

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// numberedSnapshotRe matches a rotated coordination-DB snapshot's slot number:
// relay.db.bak.<N>.
var numberedSnapshotRe = regexp.MustCompile(`\.bak\.(\d+)$`)

// backupMarkerRe recognizes a file as a DB backup artifact (as opposed to some
// unrelated file that merely shares the relay.db prefix). Matches the relay's own
// rotated snapshots (.bak.N) and the one-off copies the host updater leaves behind
// (bak-preupg, PRE-RESET, v115-broken, *.backup, *.old). Case-insensitive.
var backupMarkerRe = regexp.MustCompile(`(?i)(\.bak\b|\.bak\.|-bak|bak-|broken|pre-?reset|preupg|\.backup\b|\.old\b)`)

// PruneStaleBackups bounds the relay data directory, which otherwise fills with
// full-DB copies that nothing retires (synx-prod hit 93% root disk: ~11G of
// unpruned relay.db backups). It removes:
//   - rotated coordination snapshots relay.db.bak.<N> with N >= keep (superseded,
//     e.g. left over after the retention count was lowered), and
//   - one-off updater/ops backups (relay.db.bak-preupg, relay.db.PRE-RESET,
//     relay.db.v115-broken, and analytics siblings) once older than minForeignAge.
//
// Safety (constraints from the ticket): the live coordination + analytics DBs and
// their -wal/-shm are NEVER touched; the single newest one-off backup is always
// kept as the last known-good rollback point; one-off backups are removed only
// after minForeignAge so a just-upgraded host keeps its pre-upgrade copy for a
// recovery window. Only ever unlinks superseded copies (never the writer's live
// DB), so it is idempotent and safe to run on a live host.
func (d *DB) PruneStaleBackups(keep int, minForeignAge time.Duration) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	dir := filepath.Dir(d.path)
	coordBase := filepath.Base(d.path)
	analyticsBase := filepath.Base(analyticsDBPath(d.path))

	// Live files that must never be removed (basenames, matched against dir entries).
	protected := map[string]bool{}
	for _, live := range []string{d.path, analyticsDBPath(d.path)} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			protected[filepath.Base(live)+suffix] = true
		}
	}

	isBackupName := func(name string) bool {
		hasDBPrefix := false
		for _, base := range []string{coordBase, analyticsBase} {
			if strings.HasPrefix(name, base+".") || strings.HasPrefix(name, base+"-") {
				hasDBPrefix = true
				break
			}
		}
		return hasDBPrefix && backupMarkerRe.MatchString(name)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read backup dir %s: %w", dir, err)
	}

	type foreignBackup struct {
		name  string
		mtime time.Time
	}
	var foreign []foreignBackup
	var removed []string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if protected[name] || !isBackupName(name) {
			continue
		}
		full := filepath.Join(dir, name)

		// Rotated coordination snapshot: keep slots [0, keep), drop the rest.
		if strings.HasPrefix(name, coordBase+".bak.") {
			if m := numberedSnapshotRe.FindStringSubmatch(name); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					if n >= keep {
						if os.Remove(full) == nil {
							removed = append(removed, name)
						}
					}
					continue
				}
			}
		}

		info, err := e.Info()
		if err != nil {
			continue
		}
		foreign = append(foreign, foreignBackup{name: name, mtime: info.ModTime()})
	}

	// Keep the newest one-off backup (last known-good); prune the aged remainder.
	sort.Slice(foreign, func(i, j int) bool { return foreign[i].mtime.After(foreign[j].mtime) })
	for i, fb := range foreign {
		if i == 0 {
			continue // newest = rollback point, always kept
		}
		if time.Since(fb.mtime) < minForeignAge {
			continue // still inside the recovery window
		}
		if os.Remove(filepath.Join(dir, fb.name)) == nil {
			removed = append(removed, fb.name)
		}
	}

	return removed, nil
}
