package relay

import (
	"encoding/json"
	"testing"
)

// Every connected agent pays the serialized tool list in context tokens at
// session start. This budget blocks silent regressions: if a new tool or a
// fatter description pushes the total over the cap, trim descriptions or
// raise the cap deliberately in the same PR.
//
// Raised 48000 -> 49152 -> 51200 -> 52224 -> 54272 -> 55296 (54 KiB) across the
// WRAITH sprint, which added genuinely new SURFACE (not fat descriptions):
// reclaim_task (T1), is_eligible (T2), T5 temporal params, delivery_status (T4),
// deadletter (T6), identity_check (identity-failclosed), rank=mempalace
// (MemPalace S2), link_pr (PR-link S1), set_run + get_run (changeset-per-run S1),
// reconcile_pr (PR-link S3 poll convergence). Descriptions are trimmed to the
// bone each time; the growth is real tools the fleet needs, so the cap moves
// deliberately in-PR.
const toolSchemaBudgetBytes = 55296

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
