package db

import (
	"fmt"
	"testing"
	"time"
)

// ListBootMemories must surface global + project-scope memories written by
// OTHER agents (the buildSessionContext regression: ListMemories with
// agentName filtered agent_name on all scopes, hiding shared knowledge).
func TestListBootMemories_CrossScope(t *testing.T) {
	d := testDB(t)

	mustSet := func(agent, key, scope, layer string) {
		t.Helper()
		if _, err := d.SetMemory("proj", agent, key, "v-"+key, "", scope, "", layer); err != nil {
			t.Fatalf("SetMemory(%s): %v", key, err)
		}
	}

	mustSet("cto", "shared-rule", "project", "constraints") // other agent, project scope
	mustSet("cto", "global-fact", "global", "behavior")     // other agent, global scope
	mustSet("fe-2", "my-note", "agent", "context")          // own agent scope
	mustSet("fe-3", "private-note", "agent", "context")     // other agent's private scope

	mems, err := d.ListBootMemories("proj", "fe-2", 50)
	if err != nil {
		t.Fatalf("ListBootMemories: %v", err)
	}

	got := map[string]bool{}
	for _, m := range mems {
		got[m.Key] = true
	}
	for _, want := range []string{"shared-rule", "global-fact", "my-note"} {
		if !got[want] {
			t.Fatalf("boot view missing %q (got %v)", want, got)
		}
	}
	if got["private-note"] {
		t.Fatal("boot view must not include another agent's agent-scope memory")
	}

	// Constraints sort first so budget projection keeps them.
	if len(mems) == 0 || mems[0].Key != "shared-rule" {
		t.Fatalf("constraints memory must sort first, got %q", mems[0].Key)
	}
}

// seedBootLayers writes n project-scope memories of one layer with strictly
// increasing updated_at (prefix-00 oldest), so "newest first" is deterministic.
func seedBootLayers(t *testing.T, d *DB, prefix, layer string, n int, base time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%s-%02d", prefix, i)
		if _, err := d.SetMemory("proj", "cto", key, "v-"+key, "", "project", "", layer); err != nil {
			t.Fatalf("SetMemory(%s): %v", key, err)
		}
		ts := base.Add(time.Duration(i) * time.Second).UTC().Format(memoryTimeFmt)
		if _, err := d.writerExec(`UPDATE memories SET updated_at = ? WHERE key = ?`, ts, key); err != nil {
			t.Fatalf("stamp %s: %v", key, err)
		}
	}
}

// TestListBootMemoriesReservesLayers is f4efab3a AC1: a constraint-heavy project
// no longer starves behavior/context out of boot. Each layer gets its reserved
// slots (newest first), unused quota is refilled, the total stays <= limit.
func TestListBootMemoriesReservesLayers(t *testing.T) {
	d := testDB(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// Constraints are the newest rows overall: under the old single LIMIT they
	// alone would fill all 50 slots.
	seedBootLayers(t, d, "b", "behavior", 20, base)
	seedBootLayers(t, d, "x", "context", 5, base.Add(time.Hour))
	seedBootLayers(t, d, "c", "constraints", 80, base.Add(2*time.Hour))

	mems, err := d.ListBootMemories("proj", "a", 50)
	if err != nil {
		t.Fatalf("ListBootMemories: %v", err)
	}
	if len(mems) != 50 {
		t.Fatalf("total = %d, want the full limit 50", len(mems))
	}
	byLayer := map[string][]string{}
	for _, m := range mems {
		byLayer[m.Layer] = append(byLayer[m.Layer], m.Key)
	}
	if n := len(byLayer["behavior"]); n < BootQuotaBehavior {
		t.Fatalf("behavior = %d, want >= quota %d", n, BootQuotaBehavior)
	}
	if n := len(byLayer["context"]); n != 5 {
		t.Fatalf("context = %d, want all 5", n)
	}
	// Newest first per layer: the reserved behavior slots are b-19..b-10.
	if got := byLayer["behavior"][0]; got != "b-19" {
		t.Fatalf("newest behavior first: got %s, want b-19", got)
	}
	for _, keys := range byLayer {
		for i := 1; i < len(keys); i++ {
			if keys[i] > keys[i-1] {
				t.Fatalf("layer not newest-first: %v", keys)
			}
		}
	}
	// Constraints still sort first (projectMemories' floor relies on it) and
	// take the reserved slots plus the refill.
	if mems[0].Layer != "constraints" {
		t.Fatalf("constraints must sort first, got %s", mems[0].Layer)
	}
	if n := len(byLayer["constraints"]); n != 50-BootQuotaBehavior-5 {
		t.Fatalf("constraints = %d, want %d (quota + refill)", n, 50-BootQuotaBehavior-5)
	}
}

// TestListBootMemoriesQuotaRefill: with no behavior/context memories the
// constraints fill the whole limit, and a small project returns everything.
func TestListBootMemoriesQuotaRefill(t *testing.T) {
	d := testDB(t)
	seedBootLayers(t, d, "c", "constraints", 60, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	mems, err := d.ListBootMemories("proj", "a", 50)
	if err != nil {
		t.Fatalf("ListBootMemories: %v", err)
	}
	if len(mems) != 50 {
		t.Fatalf("constraints-only project: got %d, want 50 (refill)", len(mems))
	}
	if mems[0].Key != "c-59" || mems[49].Key != "c-10" {
		t.Fatalf("refill order: first %s last %s, want c-59..c-10", mems[0].Key, mems[49].Key)
	}

	small := testDB(t)
	seedBootLayers(t, small, "b", "behavior", 3, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if got, _ := small.ListBootMemories("proj", "a", 50); len(got) != 3 {
		t.Fatalf("small project: got %d, want 3", len(got))
	}
}
