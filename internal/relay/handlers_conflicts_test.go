package relay

import (
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"
)

// resolve_conflict's action form and the legacy key/chosen_value form
// (design 8d107daa T2, ruling ed744dee OQ5).

func TestResolveConflictTool(t *testing.T) {
	t.Run("LegacyResolveConflictUnchangedForCallers", func(t *testing.T) {
		d, _ := memDB(t)
		h := memHandlersAt(t, d)
		setMem(t, h, "a", map[string]any{"value": "sessions: cookie"})
		sib := setMem(t, h, "b", map[string]any{"value": "JWT prohibited", "based_on": "new"})
		if sib["conflict"] != true || sib["conflict_with"] == nil {
			t.Fatalf("sibling result = %v, want conflict + conflict_with as before", sib)
		}
		res, _ := h.HandleResolveConflict(ctx, call(map[string]any{"project": "p1", "as": "a", "key": "auth-policy", "chosen_value": "JWT prohibited"}))
		got := parseJSON(t, res)
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "memory,resolved" || got["resolved"] != true {
			t.Fatalf("legacy result keys %v (%v), want exactly memory,resolved", keys, got)
		}
		if mem := got["memory"].(map[string]any); mem["value"] != "JWT prohibited" || mem["key"] != "auth-policy" {
			t.Fatalf("legacy winner = %v", mem)
		}
		if id := getMemID(t, h, "a"); id == "" {
			t.Fatal("key has no single live value after the legacy resolve")
		}
		for args, want := range map[string]map[string]any{
			"key is required":          {"project": "p1", "as": "a", "chosen_value": "x"},
			"chosen_value is required": {"project": "p1", "as": "a", "key": "auth-policy"},
		} {
			res, _ := h.HandleResolveConflict(ctx, call(want))
			if msg := expectError(t, res); !strings.Contains(msg, args) {
				t.Fatalf("legacy refusal = %q, want %q", msg, args)
			}
		}
	})

	t.Run("ActionsListClaimResolveRevertAndContestedBoot", func(t *testing.T) {
		d, path := memDB(t)
		h := memHandlersAt(t, d)
		for _, a := range []string{"alice", "boss"} {
			args := map[string]any{"project": "p1", "name": a}
			if a == "alice" {
				args["reports_to"] = "boss"
			}
			if res, _ := h.HandleRegisterAgent(ctx, call(args)); res.IsError {
				t.Fatalf("register %s: %s", a, expectError(t, res))
			}
		}
		m1 := memID(setMem(t, h, "alice", map[string]any{"key": "pg-version", "value": "foo runs PostgreSQL 16", "layer": "constraints"}))
		m2 := memID(setMem(t, h, "boss", map[string]any{"key": "foo-postgres", "value": "foo runs PostgreSQL 16 in prod", "layer": "constraints"}))
		raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		members := []string{m1, m2}
		sort.Strings(members)
		if _, err := raw.Exec(`INSERT INTO knowledge_conflicts (id, project, members, members_hash, kind, gate_evidence, detected_by, state, created_at)
			VALUES ('cf1', 'p1', ?, 'h-cf1', 'candidate', '{}', 'relay-sweeper', 'detected', ?)`,
			`["`+members[0]+`","`+members[1]+`"]`, time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")); err != nil {
			t.Fatal(err)
		}
		act := func(as string, args map[string]any) (map[string]any, string) {
			t.Helper()
			args["project"], args["as"] = "p1", as
			res, _ := h.HandleResolveConflict(ctx, call(args))
			if res.IsError {
				return nil, expectError(t, res)
			}
			return parseJSON(t, res), ""
		}
		contested := func() map[string]bool {
			t.Helper()
			res, _ := h.HandleGetSessionContext(ctx, call(map[string]any{"project": "p1", "as": "alice"}))
			out := map[string]bool{}
			for _, m := range parseJSON(t, res)["relevant_memories"].([]any) {
				row := m.(map[string]any)
				out[row["key"].(string)] = row["contested"] == true
			}
			return out
		}
		if c := contested(); !c["pg-version"] || !c["foo-postgres"] {
			t.Fatalf("boot contested flags = %v, want both members flagged", c)
		}

		list, _ := act("alice", map[string]any{"action": "list"})
		if list["count"].(float64) != 1 {
			t.Fatalf("list = %v", list)
		}
		if _, msg := act("alice", map[string]any{"action": "resolve", "conflict_id": "cf1", "resolution": "merge", "keep": m1}); !strings.Contains(msg, "claim it first") {
			t.Fatalf("resolve before claim = %q", msg)
		}
		if _, msg := act("alice", map[string]any{"action": "claim"}); !strings.Contains(msg, "conflict_id is required") {
			t.Fatalf("claim without id = %q", msg)
		}
		if got, msg := act("alice", map[string]any{"action": "claim", "conflict_id": "cf1"}); got["claimed"] != true {
			t.Fatalf("claim = %v %q", got, msg)
		}
		if _, msg := act("boss", map[string]any{"action": "claim", "conflict_id": "cf1"}); !strings.Contains(msg, "not claimable") {
			t.Fatalf("second claim = %q", msg)
		}
		got, msg := act("alice", map[string]any{"action": "resolve", "conflict_id": "cf1", "resolution": "merge", "keep": m1, "rationale": "same fact"})
		if got == nil || got["state"] != "resolved" || got["kept"] != m1 {
			t.Fatalf("resolve = %v %q", got, msg)
		}
		if c := contested(); c["pg-version"] {
			t.Fatalf("resolved conflict still contested at boot: %v", c)
		}
		if _, msg := act("alice", map[string]any{"action": "revert", "conflict_id": "cf1"}); !strings.Contains(msg, "revert is for") {
			t.Fatalf("resolver's own revert = %q", msg)
		}
		got, msg = act("boss", map[string]any{"action": "revert", "conflict_id": "cf1"})
		if got == nil || got["state"] != "detected" {
			t.Fatalf("lead's revert = %v %q", got, msg)
		}
		if c := contested(); !c["foo-postgres"] {
			t.Fatalf("reverted conflict not contested again: %v", c)
		}
		if _, msg := act("alice", map[string]any{"action": "nope", "conflict_id": "cf1"}); !strings.Contains(msg, "action must be") {
			t.Fatalf("bad action = %q", msg)
		}
	})
}
