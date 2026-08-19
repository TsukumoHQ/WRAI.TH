package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecisionDependsOnStored(t *testing.T) {
	d := testDB(t)
	base, err := d.RememberDecision("p1", "a", "relay/wake", "guard-first wake predicate", "", nil, "", nil)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	dep, err := d.RememberDecision("p1", "a", "relay/metrics", "no-wake share metric", "", nil, "", []string{base.Key})
	if err != nil {
		t.Fatalf("dep: %v", err)
	}
	var dv DecisionValue
	if err := json.Unmarshal([]byte(dep.Value), &dv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dv.DependsOn) != 1 || dv.DependsOn[0] != base.Key {
		t.Errorf("depends_on = %v, want [%s]", dv.DependsOn, base.Key)
	}
}

func TestDecisionGraphMermaid(t *testing.T) {
	d := testDB(t)
	m1, _ := d.RememberDecision("p1", "a", "relay/wake", "wake on P0 only", "", nil, "", nil)
	// Supersede m1, and depend on it — exercises both edge kinds + archived class.
	_, err := d.RememberDecision("p1", "a", "relay/wake", "guard-first wake predicate", "", nil, m1.Key, nil)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	_, err = d.RememberDecision("p1", "a", "relay/metrics", "no-wake share metric", "", nil, "", []string{"DEC-relay-wake-2"})
	if err != nil {
		t.Fatalf("dep: %v", err)
	}

	g, err := d.DecisionGraphMermaid("p1")
	if err != nil {
		t.Fatalf("mermaid: %v", err)
	}
	for _, want := range []string{"graph TD", "-->|supersedes|", "-.->|depends|", "classDef archived", "DEC_relay_wake_1"} {
		if !strings.Contains(g, want) {
			t.Errorf("graph missing %q\n%s", want, g)
		}
	}
}

func TestDecisionGraphEmpty(t *testing.T) {
	d := testDB(t)
	g, err := d.DecisionGraphMermaid("empty")
	if err != nil {
		t.Fatalf("mermaid: %v", err)
	}
	if !strings.Contains(g, "graph TD") || !strings.Contains(g, "no decisions yet") {
		t.Errorf("empty graph = %q", g)
	}
}

func TestRelevantDecisionsAreaFilter(t *testing.T) {
	d := testDB(t)
	_, _ = d.RememberDecision("p1", "a", "relay/comms-discipline", "action_required tag", "", nil, "", nil)
	_, _ = d.RememberDecision("p1", "a", "trovex/retrieval", "local rerank default", "", nil, "", nil)

	// Prefix match: "relay/comms" should hit "relay/comms-discipline".
	rel, err := d.RelevantDecisions("p1", "relay/comms")
	if err != nil {
		t.Fatalf("relevant: %v", err)
	}
	if len(rel) != 1 {
		t.Fatalf("want 1 relevant, got %d", len(rel))
	}

	// Empty area → all live decisions.
	all, _ := d.RelevantDecisions("p1", "")
	if len(all) != 2 {
		t.Errorf("empty-area want 2, got %d", len(all))
	}

	// Unrelated area → none.
	none, _ := d.RelevantDecisions("p1", "yoru/dashboard")
	if len(none) != 0 {
		t.Errorf("unrelated area want 0, got %d", len(none))
	}
}
