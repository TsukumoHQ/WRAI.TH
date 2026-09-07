package relay

import "testing"

// Lowercase-at-write for profile slug / agent name / task assigned_to+profile_slug
// (task df850e85). The referential scan drops its LOWER() (ticket B2) and relies on
// plain equality, so every identifier must be stored lowercase at write time — a
// future mixed-case register must never silently break a lookup. These pin the four
// gap sites the ticket named: register_profile / get_profile, dispatch profile,
// update_task reassign, register_agent profile_slug. Handler contract is unchanged
// (same params, same responses); only the stored value is normalized.

// dispatchWithProfile dispatches a task and returns its id (handlers return a nil Go
// error in practice; the codebase idiom ignores it and lets parseJSON surface an
// IsError result).
func dispatchWithProfile(t *testing.T, h *Handlers, project, as, profile string) string {
	t.Helper()
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": project, "as": as, "profile": profile, "title": "t",
	}))
	return parseJSON(t, res)["task"].(map[string]any)["id"].(string)
}

func taskFields(t *testing.T, h *Handlers, project, taskID string) map[string]any {
	t.Helper()
	res, _ := h.HandleGetTask(ctx, call(map[string]any{"project": project, "task_id": taskID}))
	return parseJSON(t, res)
}

// TestRegisterProfileLowercasesSlug (AC1): register_profile with a mixed-case slug
// stores it lowercase, and get_profile resolves it from the mixed-case input.
func TestRegisterProfileLowercasesSlug(t *testing.T) {
	h := testHandlers(t)

	regRes, _ := h.HandleRegisterProfile(ctx, call(map[string]any{
		"project": "p1", "slug": "Wraith-Backend", "name": "Wraith Backend",
	}))
	if got := parseJSON(t, regRes)["slug"]; got != "wraith-backend" {
		t.Fatalf("register_profile stored slug %q, want %q", got, "wraith-backend")
	}

	// Lookup with the ORIGINAL mixed-case input must still resolve.
	getRes, _ := h.HandleGetProfile(ctx, call(map[string]any{"project": "p1", "slug": "Wraith-Backend"}))
	if got := parseJSON(t, getRes)["slug"]; got != "wraith-backend" {
		t.Fatalf("get_profile(mixed-case) resolved slug %q, want %q", got, "wraith-backend")
	}
}

// TestDispatchTaskLowercasesProfileSlug (AC2, dispatch): dispatch_task with a
// mixed-case profile stores the task's profile_slug lowercase.
func TestDispatchTaskLowercasesProfileSlug(t *testing.T) {
	h := testHandlers(t)

	taskID := dispatchWithProfile(t, h, "p1", "boss", "Wraith-Backend")
	if got := taskFields(t, h, "p1", taskID)["profile_slug"]; got != "wraith-backend" {
		t.Fatalf("dispatched task profile_slug %q, want %q", got, "wraith-backend")
	}
}

// TestUpdateTaskReassignLowercasesAssignedTo (AC2, reassign): update_task with a
// mixed-case assigned_to (+ profile_slug) stores both lowercase.
func TestUpdateTaskReassignLowercasesAssignedTo(t *testing.T) {
	h := testHandlers(t)

	taskID := dispatchWithProfile(t, h, "p1", "boss", "dev")
	// Caller = dispatcher (boss), task still pending -> both fields update freely.
	if _, err := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "boss", "task_id": taskID,
		"assigned_to": "Wraith-Backend-2", "profile_slug": "Wraith-Backend",
	})); err != nil {
		t.Fatalf("update_task: %v", err)
	}

	got := taskFields(t, h, "p1", taskID)
	if got["assigned_to"] != "wraith-backend-2" {
		t.Fatalf("reassigned assigned_to %q, want %q", got["assigned_to"], "wraith-backend-2")
	}
	if got["profile_slug"] != "wraith-backend" {
		t.Fatalf("reassigned profile_slug %q, want %q", got["profile_slug"], "wraith-backend")
	}
}

// TestRegisterAgentLowercasesProfileSlug (AC3): register_agent with a mixed-case
// profile_slug stores it lowercase.
func TestRegisterAgentLowercasesProfileSlug(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "p1", "name": "bot-a", "profile_slug": "Wraith-Backend",
	}))
	agent := parseJSON(t, res)["agent"].(map[string]any)
	if got := agent["profile_slug"]; got != "wraith-backend" {
		t.Fatalf("registered agent profile_slug %q, want %q", got, "wraith-backend")
	}
}

// TestLowercaseInputByteIdenticalStored (AC4): an already-lowercase input at each
// of the four sites stores the exact value it does today — the normalization is a
// no-op on well-formed input, so existing callers see byte-identical rows.
func TestLowercaseInputByteIdenticalStored(t *testing.T) {
	h := testHandlers(t)

	profRes, _ := h.HandleRegisterProfile(ctx, call(map[string]any{
		"project": "p1", "slug": "wraith-backend", "name": "n",
	}))
	if got := parseJSON(t, profRes)["slug"]; got != "wraith-backend" {
		t.Fatalf("profile slug mutated for lowercase input: %q", got)
	}

	agentRes, _ := h.HandleRegisterAgent(ctx, call(map[string]any{
		"project": "p1", "name": "bot-a", "profile_slug": "wraith-backend",
	}))
	agent := parseJSON(t, agentRes)["agent"].(map[string]any)
	if got := agent["profile_slug"]; got != "wraith-backend" {
		t.Fatalf("agent profile_slug mutated for lowercase input: %q", got)
	}

	taskID := dispatchWithProfile(t, h, "p1", "boss", "wraith-backend")
	if _, err := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "boss", "task_id": taskID, "assigned_to": "wraith-backend-2",
	})); err != nil {
		t.Fatalf("update_task: %v", err)
	}
	got := taskFields(t, h, "p1", taskID)
	if got["profile_slug"] != "wraith-backend" || got["assigned_to"] != "wraith-backend-2" {
		t.Fatalf("task fields mutated for lowercase input: profile_slug=%q assigned_to=%q",
			got["profile_slug"], got["assigned_to"])
	}
}
