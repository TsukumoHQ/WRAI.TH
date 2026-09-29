package db

import (
	"testing"
	"time"
)

// Task d43d844e: human tasks already pending when founder_gates lands get a
// gate seeded as alerted, so the first sweep after deploy sends no burst of
// founder alerts; a human task dispatched later still alerts once.
func TestFounderGateMigrationSeedsExistingAsAlerted(t *testing.T) {
	d := testDB(t)
	old := time.Now().UTC().Add(-3 * time.Hour).Format(memoryTimeFmt)
	insert := func(id string) {
		if _, err := d.writerExec(`INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, labels, blocked_periods)
			VALUES (?, 'human', 'cto', ?, 'pending', 'p1', ?, '[]', '[]')`, id, "title "+id, old); err != nil {
			t.Fatal(err)
		}
	}
	insert("old-human")
	if _, err := d.writerExec(`DROP TABLE founder_gates`); err != nil {
		t.Fatal(err)
	}
	migrateParking(d.conn)
	migrateParking(d.conn) // idempotent: no second seed

	insert("new-human")
	unalerted, err := d.SyncFounderGates(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(unalerted) != 1 || unalerted[0].TaskID != "new-human" {
		t.Fatalf("want only new-human to alert, got %+v", unalerted)
	}
	open, err := d.OpenFounderGates("p1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].TaskID != "old-human" || open[0].AgeSeconds < 3*3600-60 {
		t.Fatalf("digest: want old-human (age ~3h) then new-human, got %+v", open)
	}
}
