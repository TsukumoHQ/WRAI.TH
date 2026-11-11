package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"agent-relay/internal/models"

	"github.com/mark3labs/mcp-go/mcp"
)

// tokens L3 (ffd35635): the relay responses agents read every turn shrink.

func longTicket() map[string]any {
	return map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "compact echo",
		"description":         strings.Repeat("long description ", 40),
		"goal":                "the goal",
		"acceptance_criteria": `["AC1: one","AC2: two"]`,
		"dod":                 "merged via the gate",
		"priority":            "P1",
	}
}

// AC1: dispatch_task echoes what the relay decided, not the ticket the caller
// just sent; verbose=true restores the full record.
func TestCompactEcho_Dispatch(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleDispatchTask(ctx, call(longTicket()))
	if res.IsError {
		t.Fatalf("dispatch: %s", expectError(t, res))
	}
	task := parseJSON(t, res)["task"].(map[string]any)
	if task["id"] == nil || task["status"] != "pending" || task["priority"] != "P1" || task["profile_slug"] != "dev" {
		t.Fatalf("compact dispatch echo lost identity/status: %+v", task)
	}
	for _, k := range []string{"description", "goal", "acceptance_criteria", "dod", "title", "dispatched_at"} {
		if _, ok := task[k]; ok {
			t.Errorf("compact dispatch echo repeats %q", k)
		}
	}
	taskBytes := func(res *mcp.CallToolResult) int {
		var r struct {
			Task json.RawMessage `json:"task"`
		}
		_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &r)
		return len(r.Task)
	}
	compactBytes := taskBytes(res)

	args := longTicket()
	args["title"] = "compact echo verbose"
	args["verbose"] = true
	vres, _ := h.HandleDispatchTask(ctx, call(args))
	vtask := parseJSON(t, vres)["task"].(map[string]any)
	if vtask["goal"] != "the goal" || vtask["description"] == nil {
		t.Fatalf("verbose dispatch must echo the full task: %+v", vtask)
	}
	verboseBytes := taskBytes(vres)
	t.Logf("dispatch_task echo: verbose=%dB compact=%dB", verboseBytes, compactBytes)
	if compactBytes*3 > verboseBytes {
		t.Errorf("compact dispatch echo %dB not < 1/3 of verbose %dB", compactBytes, verboseBytes)
	}
}

// AC1: update_task names the fields it set and repeats only the short ones.
func TestCompactEcho_Update(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	id := parseJSON(t, result(h.HandleDispatchTask(ctx, call(longTicket()))))["task"].(map[string]any)["id"].(string)

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": id,
		"description": strings.Repeat("new description ", 40), "priority": "P0",
		"progress_note": "noted",
	}))
	if res.IsError {
		t.Fatalf("update: %s", expectError(t, res))
	}
	got := parseJSON(t, res)
	if got["id"] != id || got["status"] != "pending" || got["priority"] != "P0" {
		t.Fatalf("compact update echo: %+v", got)
	}
	updated, _ := got["updated"].([]any)
	if fmt.Sprint(updated) != "[description priority progress_note]" {
		t.Errorf("updated = %v, want [description priority progress_note]", updated)
	}
	for _, k := range []string{"description", "goal", "acceptance_criteria", "dod", "title"} {
		if _, ok := got[k]; ok {
			t.Errorf("compact update echo repeats %q", k)
		}
	}
	// The write landed: get_task has it.
	full := parseJSON(t, result(h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": id}))))
	if !strings.HasPrefix(full["description"].(string), "new description") {
		t.Errorf("description not persisted: %v", full["description"])
	}

	vres, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": id, "title": "renamed", "verbose": true,
	}))
	v := parseJSON(t, vres)
	if v["title"] != "renamed" || v["description"] == nil || v["goal"] != "the goal" {
		t.Fatalf("verbose update must echo the full task: %+v", v)
	}
	t.Logf("update_task echo: verbose=%dB compact=%dB", len(vres.Content[0].(mcp.TextContent).Text), len(res.Content[0].(mcp.TextContent).Text))
}

// AC1: cancel_task returns id + status.
func TestCompactEcho_Cancel(t *testing.T) {
	h := testHandlers(t)
	id := parseJSON(t, result(h.HandleDispatchTask(ctx, call(longTicket()))))["task"].(map[string]any)["id"].(string)
	res, _ := h.HandleCancelTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": id, "reason": "obsolete"}))
	got := parseJSON(t, res)
	if got["id"] != id || got["status"] != "cancelled" {
		t.Fatalf("compact cancel echo: %+v", got)
	}
	if _, ok := got["description"]; ok {
		t.Error("compact cancel echo repeats description")
	}

	id2 := parseJSON(t, result(h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "second",
	}))))["task"].(map[string]any)["id"].(string)
	v := parseJSON(t, result(h.HandleCancelTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": id2, "verbose": true}))))
	if v["status"] != "cancelled" || v["title"] != "second" || v["dispatched_by"] != "cto" {
		t.Fatalf("verbose cancel must echo the full task: %+v", v)
	}
}

// AC2: a re-register of a live agent boots minimal by default; a first boot,
// a comeback from inactive, and an explicit 'full' stay full.
func TestRegister_ReRegisterOfLiveAgentDefaultsMinimal(t *testing.T) {
	h := testHandlers(t)
	reg := func(extra map[string]any) map[string]any {
		args := map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}
		for k, v := range extra {
			args[k] = v
		}
		res, _ := h.HandleRegisterAgent(ctx, call(args))
		if res.IsError {
			t.Fatalf("register: %s", expectError(t, res))
		}
		return parseJSON(t, res)["session_context"].(map[string]any)
	}
	isFull := func(sc map[string]any) bool {
		_, ok := sc["relevant_memories"]
		return ok
	}

	first := reg(nil)
	if !isFull(first) {
		t.Fatalf("first boot must be full: %+v", first)
	}
	if _, ok := first["mode"]; ok {
		t.Error("first boot must not carry the minimal mode note")
	}

	again := reg(nil)
	if isFull(again) {
		t.Fatalf("re-register of a live agent must default to minimal: %+v", again)
	}
	if again["mode"] == nil || again["is_respawn"] != true {
		t.Errorf("minimal re-register should say why and how to get full: %+v", again)
	}

	if explicit := reg(map[string]any{"session_context": "full"}); !isFull(explicit) {
		t.Fatal("explicit session_context='full' must stay full on re-register")
	}

	// A comeback from inactive is not a live re-register: full boot.
	if err := h.db.DeactivateAgent("p1", "dev-a"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if back := reg(nil); !isFull(back) {
		t.Fatal("re-register after deactivation must boot full")
	}
}

// AC1 (audit T5, 6ead6bed): the default discover_tools payload is terse — one
// "(required params) one-line description" string per tool — and stays under
// discoverTerseMaxBytes for every category. It lists every tool of the
// category (core discovery tools by name only) and its required params exactly
// as the verbose schema declares them; verbose=true keeps the full schemas.
func TestDiscoverTerse_SizeAndRequired(t *testing.T) {
	h := testHandlers(t)
	bg := context.Background()
	for _, c := range toolCategories {
		vres, _ := h.HandleDiscoverTools(bg, call(map[string]any{"category": c.name, "verbose": true}))
		tres, _ := h.HandleDiscoverTools(bg, call(map[string]any{"category": c.name}))
		vtext, ttext := vres.Content[0].(mcp.TextContent).Text, tres.Content[0].(mcp.TextContent).Text
		t.Logf("discover_tools %-14s verbose=%5dB terse=%5dB", c.name, len(vtext), len(ttext))
		if len(ttext) > discoverTerseMaxBytes {
			t.Errorf("category %s: terse payload %dB > %dB", c.name, len(ttext), discoverTerseMaxBytes)
		}

		var verbose struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Type       string                    `json:"type"`
					Properties map[string]map[string]any `json:"properties"`
					Required   []string                  `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		}
		var terse struct {
			Tools       map[string]string `json:"tools"`
			OnToolsList []string          `json:"on_tools_list"`
		}
		if err := json.Unmarshal([]byte(vtext), &verbose); err != nil {
			t.Fatalf("verbose %s: %v", c.name, err)
		}
		if err := json.Unmarshal([]byte(ttext), &terse); err != nil {
			t.Fatalf("terse %s: %v", c.name, err)
		}
		onList := map[string]bool{}
		for _, n := range terse.OnToolsList {
			onList[n] = true
			if !coreDiscoveryTools[n] {
				t.Errorf("%s listed as on tools/list but is not a core discovery tool", n)
			}
		}
		if got, want := len(terse.Tools)+len(terse.OnToolsList), len(verbose.Tools); got != want {
			t.Errorf("category %s: terse names %d tools, verbose %d", c.name, got, want)
		}
		for _, vt := range verbose.Tools {
			if vt.InputSchema.Type != "object" {
				t.Errorf("verbose %s inputSchema.type = %q", vt.Name, vt.InputSchema.Type)
			}
			if len(vt.InputSchema.Properties) == 0 && len(vt.InputSchema.Required) > 0 {
				t.Errorf("verbose %s lost its properties", vt.Name)
			}
			if onList[vt.Name] {
				continue
			}
			line, ok := terse.Tools[vt.Name]
			if !ok {
				t.Errorf("terse %s lost tool %s", c.name, vt.Name)
				continue
			}
			req := append([]string(nil), vt.InputSchema.Required...)
			sort.Strings(req)
			if prefix := "(" + strings.Join(req, ",") + ") "; !strings.HasPrefix(line, prefix) {
				t.Errorf("%s: terse %q does not start with required params %q", vt.Name, line, prefix)
			}
			if desc := strings.TrimPrefix(line, "("+strings.Join(req, ",")+") "); desc == "" || strings.Contains(desc, "\n") || len(desc) > discoverToolDescMax {
				t.Errorf("%s: description %q not one line <= %dB", vt.Name, desc, discoverToolDescMax)
			}
		}
	}
}

// oneLine keeps the first sentence only.
func TestDiscoverTerse_OneLine(t *testing.T) {
	for in, want := range map[string]string{
		"Send a message. Use to=* for broadcast.": "Send a message",
		"Claim a task (accepted).\nMore detail.":  "Claim a task",
		"Hold a task; no ACK pages":               "Hold a task",
		"Read state — see deliveries":             "Read state",
		"  Archive done tasks.  ":                 "Archive done tasks",
		"Move: a -> b":                            "Move: a -> b",
	} {
		if got := oneLine(in); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// AC4: constraints memories are counted in sessionMemoryBudget — they consume
// it, so a later memory that would fit an empty budget is dropped — and even
// the guaranteed floor cannot push relevant_memories past the hard ceiling.
func TestProjectMemories_ConstraintsCountInBudget(t *testing.T) {
	var mems []models.Memory
	for i := 0; i < 3; i++ {
		mems = append(mems, models.Memory{
			Key: fmt.Sprintf("rule-%d", i), Value: strings.Repeat("c", 400),
			Scope: "project", Layer: "constraints",
		})
	}
	// Two context memories: the first is the context floor, the second competes.
	mems = append(mems,
		models.Memory{Key: "ctx-floor", Value: "x", Scope: "project", Layer: "context"},
		models.Memory{Key: "ctx-extra", Value: strings.Repeat("y", 100), Scope: "project", Layer: "context"},
	)
	extra := memorySummaryBytes(summarizeMemory(mems[4]))
	constraintBytes := 0
	for _, m := range mems[:3] {
		constraintBytes += memorySummaryBytes(summarizeMemory(m))
	}
	floorCtx := memorySummaryBytes(summarizeMemory(mems[3]))
	// A budget the extra fits on its own, but not after the constraints spent it.
	budget := constraintBytes + floorCtx + extra - 1
	out := projectMemories(mems, budget)
	for _, s := range out {
		if s.Key == "ctx-extra" {
			t.Fatalf("ctx-extra (%dB) surfaced although constraints (%dB) spent the %dB budget", extra, constraintBytes, budget)
		}
	}
	if len(out) != 4 {
		t.Fatalf("want the 3 constraints + context floor, got %d", len(out))
	}
	// With room for it, it surfaces: the drop above is the constraints' doing.
	if got := projectMemories(mems, budget+1); len(got) != 5 {
		t.Fatalf("budget+1: want 5 memories, got %d", len(got))
	}

	// Fat floor: 8 constraints with 2 KB keys blow the hard ceiling; the floor
	// stops at it (first entry always surfaces).
	var fat []models.Memory
	for i := 0; i < sessionConstraintFloor; i++ {
		fat = append(fat, models.Memory{
			Key: fmt.Sprintf("%02d-%s", i, strings.Repeat("k", 2000)), Value: strings.Repeat("v", 400),
			Scope: "project", Layer: "constraints",
		})
	}
	got := projectMemories(fat, sessionMemoryBudget)
	used := 0
	for _, s := range got {
		used += memorySummaryBytes(s)
	}
	if len(got) == 0 {
		t.Fatal("the first constraint must always surface")
	}
	if len(got) > 1 && used > sessionMemoryBudget*budgetHardMultiplier {
		t.Fatalf("floor pushed relevant_memories to %dB > hard ceiling %dB", used, sessionMemoryBudget*budgetHardMultiplier)
	}
}

// AC4 round 1 review: the floor ceiling must not let a constraint-heavy boot
// starve the guaranteed behavior/context floors. Repro: 8 constraints whose
// floor rows alone exceed sessionFloorCeiling, then one behavior and one
// context memory — both must still surface, and the total stays bounded.
func TestProjectMemories_FloorCeilingKeepsBehaviorContextFloors(t *testing.T) {
	var mems []models.Memory
	for i := 0; i < sessionConstraintFloor; i++ {
		mems = append(mems, models.Memory{
			Key: fmt.Sprintf("%02d-%s", i, strings.Repeat("k", 500)), Value: strings.Repeat("v", 400),
			Scope: "project", Layer: "constraints",
		})
	}
	mems = append(mems,
		models.Memory{Key: "beh-floor", Value: strings.Repeat("b", 150), Scope: "project", Layer: "behavior"},
		models.Memory{Key: "ctx-floor", Value: strings.Repeat("x", 150), Scope: "project", Layer: "context"},
	)
	all := 0
	for _, m := range mems[:sessionConstraintFloor] {
		all += memorySummaryBytes(summarizeMemory(m))
	}
	if all <= sessionFloorCeiling {
		t.Fatalf("fixture too small: constraint floor %dB must exceed ceiling %dB", all, sessionFloorCeiling)
	}
	out := projectMemories(mems, sessionMemoryBudget)
	layers := map[string]int{}
	used := 0
	for _, s := range out {
		used += memorySummaryBytes(s)
		switch {
		case s.Key == "beh-floor":
			layers["behavior"]++
		case s.Key == "ctx-floor":
			layers["context"]++
		default:
			layers["constraints"]++
		}
	}
	if layers["behavior"] != 1 || layers["context"] != 1 {
		t.Fatalf("behavior/context floors dropped by fat constraints: layers %v", layers)
	}
	if layers["constraints"] == 0 {
		t.Fatalf("constraint floor emptied: layers %v", layers)
	}
	if used > sessionFloorCeiling {
		t.Fatalf("relevant_memories %dB > floor ceiling %dB (layers %v)", used, sessionFloorCeiling, layers)
	}
}
