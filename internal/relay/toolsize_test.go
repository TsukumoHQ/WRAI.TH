package relay

import (
	"encoding/json"
	"testing"
)

// Every connected agent pays the serialized tool list in context tokens at
// session start. This budget blocks silent regressions: if a new tool or a
// fatter description pushes the total over the cap, trim descriptions or
// raise the cap deliberately in the same PR.
const toolSchemaBudgetBytes = 48000

// Discovery mode replaces the full list with two tools; their combined
// schema must stay tiny or the mode loses its point.
const discoveryPairBudgetBytes = 2500

func TestToolSchemaBudget(t *testing.T) {
	h := testHandlers(t)
	total := 0
	for _, rt := range h.toolRegistry() {
		b, err := json.Marshal(rt.Tool)
		if err != nil {
			t.Fatalf("marshal %s: %v", rt.Tool.Name, err)
		}
		total += len(b)
		if len(b) > 2300 {
			t.Errorf("tool %s schema is %d bytes (max 2300) — trim its descriptions", rt.Tool.Name, len(b))
		}
	}
	t.Logf("tool schemas: %d tools, %d bytes (~%d tokens)", len(h.toolRegistry()), total, total/4)
	if total > toolSchemaBudgetBytes {
		t.Errorf("total tool schema size %d bytes exceeds budget %d — trim descriptions", total, toolSchemaBudgetBytes)
	}
}

func TestDiscoveryPairBudget(t *testing.T) {
	total := 0
	for _, tool := range []any{discoverToolsTool(), callToolTool()} {
		b, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal discovery tool: %v", err)
		}
		total += len(b)
	}
	t.Logf("discovery pair: %d bytes (~%d tokens)", total, total/4)
	if total > discoveryPairBudgetBytes {
		t.Errorf("discovery pair schema size %d bytes exceeds budget %d", total, discoveryPairBudgetBytes)
	}
}

// Every category must contain at least one tool, and every registered tool
// must belong to a declared category.
func TestToolCategoriesConsistent(t *testing.T) {
	h := testHandlers(t)
	declared := map[string]bool{}
	for _, c := range toolCategories {
		declared[c.name] = true
	}
	seen := map[string]bool{}
	for _, rt := range h.toolRegistry() {
		if !declared[rt.category] {
			t.Errorf("tool %s has undeclared category %q", rt.Tool.Name, rt.category)
		}
		seen[rt.category] = true
	}
	for name := range declared {
		if !seen[name] {
			t.Errorf("category %q declares no tools", name)
		}
	}
}
