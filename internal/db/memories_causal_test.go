package db

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-relay/internal/models"
)

// Causal-context memory writes (DEC-wraith-memory-causal-1, task df33d619).

// memRow reads one memory row by id, archived or not.
func memRow(t *testing.T, d *DB, id string) models.Memory {
	t.Helper()
	mems, err := d.queryMemories(fmt.Sprintf(`SELECT %s FROM memories WHERE id = ?`, memorySelectCols), id)
	if err != nil || len(mems) != 1 {
		t.Fatalf("read memory %s: %v (n=%d)", id, err, len(mems))
	}
	return mems[0]
}

func memCount(t *testing.T, d *DB, key string) int {
	t.Helper()
	var n int
	if err := d.ro().QueryRow(`SELECT count(*) FROM memories WHERE key = ?`, key).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func mustSet(t *testing.T, d *DB, agent, key, value string, opts SetMemoryOpts) *models.Memory {
	t.Helper()
	m, err := d.SetMemoryWith("p1", agent, key, value, "[]", "project", "stated", "behavior", true, opts)
	if err != nil {
		t.Fatalf("set %s=%q: %v", key, value, err)
	}
	return m
}

func TestCausal(t *testing.T) {
	t.Run("LostUpdateReproBecomesSibling", func(t *testing.T) {
		d := testDB(t)
		v1 := mustSet(t, d, "a", "auth-policy", "sessions: cookie", SetMemoryOpts{})
		// B read v1 and writes: fast-forward.
		v2 := mustSet(t, d, "b", "auth-policy", "JWT prohibited", SetMemoryOpts{BasedOn: v1.ID})
		// A still works from v1: must NOT archive B's v2.
		v3 := mustSet(t, d, "a", "auth-policy", "sessions: cookie, 30d", SetMemoryOpts{BasedOn: v1.ID})

		if got := memRow(t, d, v2.ID); got.ArchivedAt != nil || got.Status != "live" {
			t.Fatalf("v2 was archived by a stale writer: status=%s archived_at=%s", got.Status, deref(got.ArchivedAt))
		}
		got := memRow(t, d, v3.ID)
		if deref(got.ConflictWith) != v2.ID || deref(got.Supersedes) != v1.ID || got.Version != v2.Version+1 {
			t.Fatalf("sibling = conflict_with %s supersedes %s version %d; want %s / %s / %d",
				deref(got.ConflictWith), deref(got.Supersedes), got.Version, v2.ID, v1.ID, v2.Version+1)
		}
		live, err := d.GetMemory("p1", "a", "auth-policy", "project")
		if err != nil || len(live) != 2 {
			t.Fatalf("GetMemory = %d rows (err %v), want both siblings", len(live), err)
		}
	})

	t.Run("BasedOnNewWithLiveIsSibling", func(t *testing.T) {
		d := testDB(t)
		cur := mustSet(t, d, "a", "k", "first", SetMemoryOpts{})
		sib := mustSet(t, d, "b", "k", "second", SetMemoryOpts{BasedOn: BasedOnNew})
		if memRow(t, d, cur.ID).ArchivedAt != nil {
			t.Fatal("create-only write archived the live row")
		}
		got := memRow(t, d, sib.ID)
		if deref(got.ConflictWith) != cur.ID || got.Supersedes != nil {
			t.Fatalf("create-only sibling = conflict_with %s supersedes %s; want %s / <nil>",
				deref(got.ConflictWith), deref(got.Supersedes), cur.ID)
		}
		// BasedOnNew on an empty key is a plain create.
		fresh := mustSet(t, d, "a", "empty", "v", SetMemoryOpts{BasedOn: BasedOnNew})
		if fresh.Version != 1 || fresh.ConflictWith != nil || fresh.Supersedes != nil {
			t.Fatalf("create-only on empty key = %+v", fresh)
		}
	})

	t.Run("FastForwardArchivesAsToday", func(t *testing.T) {
		d := testDB(t)
		v1 := mustSet(t, d, "a", "k", "one", SetMemoryOpts{})
		v2 := mustSet(t, d, "a", "k", "two", SetMemoryOpts{BasedOn: v1.ID})
		old := memRow(t, d, v1.ID)
		if old.Status != "archived" || deref(old.ArchivedBy) != "upsert" || deref(old.ArchivedReason) != "superseded" {
			t.Fatalf("predecessor = status %s by %s reason %s", old.Status, deref(old.ArchivedBy), deref(old.ArchivedReason))
		}
		got := memRow(t, d, v2.ID)
		if got.Version != 2 || deref(got.Supersedes) != v1.ID || got.ConflictWith != nil {
			t.Fatalf("successor = version %d supersedes %s conflict_with %s", got.Version, deref(got.Supersedes), deref(got.ConflictWith))
		}
	})

	t.Run("NoBasedOnUnchanged", func(t *testing.T) {
		// Legacy last-writer-wins, field for field as before this change. The
		// only new write is H1's valid_until on the archived predecessor
		// (asserted in the H1 tests).
		d := testDB(t)
		v1, err := d.SetMemory("p1", "a", "k", "one", "[]", "project", "stated", "behavior")
		if err != nil {
			t.Fatal(err)
		}
		v2, err := d.SetMemory("p1", "b", "k", "two", `["x"]`, "project", "observed", "constraints")
		if err != nil {
			t.Fatal(err)
		}
		old := memRow(t, d, v1.ID)
		if old.Status != "archived" || deref(old.ArchivedBy) != "upsert" || deref(old.ArchivedReason) != "superseded" ||
			old.ArchivedAt == nil || *old.ArchivedAt != v2.CreatedAt || old.Value != "one" || old.Version != 1 {
			t.Fatalf("legacy predecessor changed: %+v", old)
		}
		got := memRow(t, d, v2.ID)
		want := models.Memory{Key: "k", Value: "two", Tags: `["x"]`, Scope: "project", Project: "p1", AgentName: "b",
			Confidence: "observed", Version: 2, Layer: "constraints", Status: "live"}
		if got.Key != want.Key || got.Value != want.Value || got.Tags != want.Tags || got.Scope != want.Scope ||
			got.Project != want.Project || got.AgentName != want.AgentName || got.Confidence != want.Confidence ||
			got.Version != want.Version || got.Layer != want.Layer || got.Status != want.Status ||
			deref(got.Supersedes) != v1.ID || got.ConflictWith != nil || got.ArchivedAt != nil ||
			deref(got.ValidFrom) != got.CreatedAt || got.ValidUntil != nil || got.CreatedAt != got.UpdatedAt {
			t.Fatalf("legacy successor changed: %+v", got)
		}
		if n := memCount(t, d, "k"); n != 2 {
			t.Fatalf("rows = %d, want 2", n)
		}
	})

	t.Run("UpsertFalseUnchanged", func(t *testing.T) {
		d := testDB(t)
		v1, _ := d.SetMemory("p1", "a", "k", "one", "[]", "project", "stated", "behavior", false)
		v2, err := d.SetMemory("p1", "b", "k", "two", "[]", "project", "stated", "behavior", false)
		if err != nil {
			t.Fatal(err)
		}
		if memRow(t, d, v1.ID).ArchivedAt != nil {
			t.Fatal("conflict mode archived the live row")
		}
		got := memRow(t, d, v2.ID)
		if deref(got.ConflictWith) != v1.ID || deref(got.Supersedes) != v1.ID || got.Version != 2 {
			t.Fatalf("conflict row = conflict_with %s supersedes %s version %d", deref(got.ConflictWith), deref(got.Supersedes), got.Version)
		}
		// based_on is ignored in explicit conflict mode.
		v3, err := d.SetMemoryWith("p1", "c", "k", "three", "[]", "project", "stated", "behavior", false, SetMemoryOpts{BasedOn: v2.ID})
		if err != nil || deref(v3.ConflictWith) != v2.ID {
			t.Fatalf("upsert=false with based_on = %+v, %v", v3, err)
		}
	})

	t.Run("BasedOnWrongKeyRejectedNothingWritten", func(t *testing.T) {
		d := testDB(t)
		other := mustSet(t, d, "a", "other-key", "x", SetMemoryOpts{})
		mustSet(t, d, "a", "k", "one", SetMemoryOpts{})
		mine, err := d.SetMemoryWith("p1", "a", "mine", "v", "[]", "agent", "stated", "behavior", true, SetMemoryOpts{})
		if err != nil {
			t.Fatal(err)
		}
		cases := map[string]struct{ scope, agent, basedOn string }{
			"other key":       {"project", "a", other.ID},
			"unknown id":      {"project", "a", "no-such-id"},
			"other scope":     {"agent", "a", other.ID},
			"other agent row": {"agent", "b", mine.ID},
		}
		for name, c := range cases {
			key := "k"
			if c.scope == "agent" {
				key = "mine"
			}
			before := memCount(t, d, key)
			_, err := d.SetMemoryWith("p1", c.agent, key, "new value", "[]", c.scope, "stated", "behavior", true, SetMemoryOpts{BasedOn: c.basedOn})
			if !errors.Is(err, ErrBasedOnMismatch) {
				t.Errorf("%s: err = %v, want ErrBasedOnMismatch", name, err)
			}
			if after := memCount(t, d, key); after != before {
				t.Errorf("%s: rows %d -> %d, want nothing written", name, before, after)
			}
		}
	})

	t.Run("H1PredecessorValidUntilEqualsSuccessorValidFrom", func(t *testing.T) {
		d := testDB(t)
		v1 := mustSet(t, d, "a", "k", "one", SetMemoryOpts{})
		v2 := mustSet(t, d, "a", "k", "two", SetMemoryOpts{})
		if got := memRow(t, d, v1.ID); got.ValidUntil == nil || *got.ValidUntil != deref(v2.ValidFrom) {
			t.Fatalf("predecessor valid_until = %s, want successor valid_from %s", deref(got.ValidUntil), deref(v2.ValidFrom))
		}

		// resolve_conflict closes the losers at the resolution instant, which
		// is the new resolution row's valid_from.
		d.SetMemory("p1", "a", "c", "left", "[]", "project", "stated", "behavior", false)
		d.SetMemory("p1", "b", "c", "right", "[]", "project", "stated", "behavior", false)
		live, _ := d.GetMemory("p1", "a", "c", "project")
		res, err := d.ResolveConflict("p1", "a", "c", "merged", "project")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range live {
			if got := memRow(t, d, m.ID); deref(got.ValidUntil) != deref(res.ValidFrom) {
				t.Errorf("loser %s valid_until = %s, want %s", m.Value, deref(got.ValidUntil), deref(res.ValidFrom))
			}
		}
	})

	t.Run("H1EarlierExplicitExpiryKept", func(t *testing.T) {
		d := testDB(t)
		early := mustSet(t, d, "a", "early", "one", SetMemoryOpts{})
		past := time.Now().UTC().Add(-time.Hour).Format(memoryTimeFmt)
		if err := d.SetMemoryValidity("p1", "a", "early", "project", "", past); err != nil {
			t.Fatal(err)
		}
		mustSet(t, d, "a", "early", "two", SetMemoryOpts{})
		if got := memRow(t, d, early.ID); deref(got.ValidUntil) != past {
			t.Fatalf("earlier explicit expiry overwritten: %s, want %s", deref(got.ValidUntil), past)
		}

		// A later explicit expiry is pulled in to the supersede instant.
		late := mustSet(t, d, "a", "late", "one", SetMemoryOpts{})
		future := time.Now().UTC().Add(time.Hour).Format(memoryTimeFmt)
		if err := d.SetMemoryValidity("p1", "a", "late", "project", "", future); err != nil {
			t.Fatal(err)
		}
		succ := mustSet(t, d, "a", "late", "two", SetMemoryOpts{})
		if got := memRow(t, d, late.ID); deref(got.ValidUntil) != deref(succ.ValidFrom) {
			t.Fatalf("later expiry not closed at supersede: %s, want %s", deref(got.ValidUntil), deref(succ.ValidFrom))
		}
	})

	t.Run("H2SameValueSameMetaTouchesOnly", func(t *testing.T) {
		d := testDB(t)
		v1 := mustSet(t, d, "a", "k", "same", SetMemoryOpts{})
		time.Sleep(2 * time.Millisecond) // distinct updated_at
		again := mustSet(t, d, "b", "k", "same", SetMemoryOpts{})
		if again.ID != v1.ID || memCount(t, d, "k") != 1 {
			t.Fatalf("convergent write versioned: id %s vs %s, rows %d", again.ID, v1.ID, memCount(t, d, "k"))
		}
		got := memRow(t, d, v1.ID)
		if got.UpdatedAt <= v1.UpdatedAt || got.Tags != "[]" || got.Confidence != "stated" || got.Layer != "behavior" {
			t.Fatalf("convergent write = %+v", got)
		}
	})

	t.Run("H2SameValueNewTagsVersions", func(t *testing.T) {
		d := testDB(t)
		v1, _ := d.SetMemory("p1", "a", "k", "same", `["old"]`, "project", "stated", "behavior")
		v2, err := d.SetMemory("p1", "a", "k", "same", `["new"]`, "project", "observed", "behavior")
		if err != nil {
			t.Fatal(err)
		}
		if v2.ID == v1.ID || v2.Version != 2 || deref(v2.Supersedes) != v1.ID {
			t.Fatalf("metadata change not versioned: %+v", v2)
		}
		old := memRow(t, d, v1.ID)
		if old.Status != "archived" || old.Tags != `["old"]` || old.Confidence != "stated" {
			t.Fatalf("old metadata lost: %+v", old)
		}
		if got := memRow(t, d, v2.ID); got.Tags != `["new"]` || got.Confidence != "observed" {
			t.Fatalf("new metadata = %+v", got)
		}
	})

	t.Run("H2LayerChangeNoLongerDropped", func(t *testing.T) {
		d := testDB(t)
		v1, _ := d.SetMemory("p1", "a", "k", "same", "[]", "project", "stated", "constraints")
		v2, err := d.SetMemory("p1", "a", "k", "same", "[]", "project", "stated", "behavior")
		if err != nil {
			t.Fatal(err)
		}
		live, _ := d.GetMemory("p1", "a", "k", "project")
		if len(live) != 1 || live[0].ID != v2.ID || live[0].Layer != "behavior" {
			t.Fatalf("layer change dropped: %+v", live)
		}
		if old := memRow(t, d, v1.ID); old.Layer != "constraints" || old.Status != "archived" {
			t.Fatalf("old layer not kept in history: %+v", old)
		}
	})

	t.Run("ConcurrentWritersSerialize", func(t *testing.T) {
		d := testDB(t)
		base := mustSet(t, d, "a", "k", "base", SetMemoryOpts{})
		var wg sync.WaitGroup
		results := make([]*models.Memory, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = d.SetMemoryWith("p1", fmt.Sprintf("w%d", i), "k", fmt.Sprintf("value-%d", i),
					"[]", "project", "stated", "behavior", true, SetMemoryOpts{BasedOn: base.ID})
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("writer %d: %v", i, err)
			}
		}
		var ff, sib *models.Memory
		for _, r := range results {
			if r.ConflictWith == nil {
				ff = r
			} else {
				sib = r
			}
		}
		if ff == nil || sib == nil {
			t.Fatalf("want one fast-forward and one sibling, got %+v / %+v", results[0], results[1])
		}
		if deref(ff.Supersedes) != base.ID || deref(sib.ConflictWith) != ff.ID || deref(sib.Supersedes) != base.ID {
			t.Fatalf("ff supersedes %s; sibling conflict_with %s supersedes %s", deref(ff.Supersedes), deref(sib.ConflictWith), deref(sib.Supersedes))
		}
		live, _ := d.GetMemory("p1", "a", "k", "project")
		if len(live) != 2 {
			t.Fatalf("live rows = %d, want 2 (both concurrent values kept)", len(live))
		}
	})
}
