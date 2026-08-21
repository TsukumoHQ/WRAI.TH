package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"testing"
)

// TestDispatchTask_TraceID_MintsWhenAbsent: an omitted trace_id gets a fresh
// 32-lowercase-hex mint — never left blank, never a silent no-op.
func TestDispatchTask_TraceID_MintsWhenAbsent(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if task.TraceID == nil || !ValidTraceID(*task.TraceID) {
		t.Fatalf("expected a minted 32-hex trace_id, got %v", task.TraceID)
	}
}

// TestDispatchTask_TraceID_SubtaskInheritsParent: a subtask dispatched with
// parent_task_id set, no explicit trace_id, inherits the parent's — the
// grouping key stays the same across a whole causal chain.
func TestDispatchTask_TraceID_SubtaskInheritsParent(t *testing.T) {
	d := testDB(t)
	parent, err := d.DispatchTask("p1", "dev", "cto", "parent", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch parent: %v", err)
	}
	sub, err := d.DispatchTask("p1", "dev", "cto", "child", "", "P2", &parent.ID, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch subtask: %v", err)
	}
	if sub.TraceID == nil || parent.TraceID == nil || *sub.TraceID != *parent.TraceID {
		t.Fatalf("subtask trace_id = %v, want parent's %v", sub.TraceID, parent.TraceID)
	}
}

// TestDispatchTask_TraceID_CallerSuppliedWins: an explicit valid trace_id is
// used verbatim, not overridden by a fresh mint.
func TestDispatchTask_TraceID_CallerSuppliedWins(t *testing.T) {
	d := testDB(t)
	want := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false, &want)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if task.TraceID == nil || *task.TraceID != want {
		t.Fatalf("trace_id = %v, want %q", task.TraceID, want)
	}
}

// TestGetTask_SurfacesTraceID: the single-task read API returns the same
// trace_id DispatchTask minted, via GetTask's dedicated lookup (not
// taskColumns/scanTask).
func TestGetTask_SurfacesTraceID(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, err := d.GetTask(task.ID, "p1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.TraceID == nil || *got.TraceID != *task.TraceID {
		t.Fatalf("GetTask trace_id = %v, want %v", got.TraceID, task.TraceID)
	}
}

// TestTraceIDColumnAdditive: a fresh DB boots with the trace_id column
// present and nullable — a row inserted without it (simulating an older
// binary / pre-migration row) reads back NULL without erroring, so an older
// binary + a newer DB (and vice versa) both boot.
func TestTraceIDColumnAdditive(t *testing.T) {
	d := testDB(t)
	_, err := d.conn.Exec(
		`INSERT INTO tasks (id, profile_slug, dispatched_by, title, description, priority, status, project, dispatched_at, source, last_activity_at)
		 VALUES ('legacy-1', 'dev', 'cto', 'legacy row', '', 'P2', 'pending', 'p1', '2026-01-01T00:00:00.000000Z', 'native', '2026-01-01T00:00:00.000000Z')`,
	)
	if err != nil {
		t.Fatalf("insert legacy row without trace_id: %v", err)
	}
	var tid sql.NullString
	if err := d.conn.QueryRow("SELECT trace_id FROM tasks WHERE id = 'legacy-1'").Scan(&tid); err != nil {
		t.Fatalf("select trace_id: %v", err)
	}
	if tid.Valid {
		t.Errorf("expected NULL trace_id on a legacy row, got %q", tid.String)
	}
}

// TestDeriveTraceID_ReplyInheritsParentMessage: a response inherits its
// parent message's trace_id.
func TestDeriveTraceID_ReplyInheritsParentMessage(t *testing.T) {
	d := testDB(t)
	parent, err := d.InsertMessage("p1", "cto", "worker", "notification", "s", "hello", "{}", "P2", 3600, nil, nil)
	if err != nil {
		t.Fatalf("insert parent message: %v", err)
	}
	if _, err := d.conn.Exec("UPDATE messages SET trace_id = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' WHERE id = ?", parent.ID); err != nil {
		t.Fatalf("seed parent trace_id: %v", err)
	}
	reply, err := d.InsertMessage("p1", "worker", "cto", "response", "s", "reply", "{}", "P2", 3600, &parent.ID, nil)
	if err != nil {
		t.Fatalf("insert reply: %v", err)
	}
	if reply.TraceID == nil || *reply.TraceID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("reply trace_id = %v, want the parent's", reply.TraceID)
	}
}

// TestDeriveTraceID_TaskAnnouncementInheritsTask: the per-agent announcement
// message dispatchCore/announceClaimable sends carries the task's trace_id —
// the metadata {"task_id":"..."} shape deriveTraceID reads.
func TestDeriveTraceID_TaskAnnouncementInheritsTask(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	metadata := `{"task_id":"` + task.ID + `"}`
	msg, err := d.InsertMessage("p1", "cto", "worker", "task", "New task", "...", metadata, "P2", 3600, nil, nil)
	if err != nil {
		t.Fatalf("insert announcement: %v", err)
	}
	if msg.TraceID == nil || task.TraceID == nil || *msg.TraceID != *task.TraceID {
		t.Fatalf("announcement trace_id = %v, want task's %v", msg.TraceID, task.TraceID)
	}
}

// TestRecordAudit_DerivesTaskTraceID: an audit entry against a task
// auto-picks up that task's trace_id when the caller leaves it unset.
func TestRecordAudit_DerivesTaskTraceID(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := d.RecordAudit(models.AuditEntry{
		Project: "p1", Actor: "cto", Action: "contract_updated",
		ResourceType: "task", ResourceID: task.ID, Summary: "test",
	}); err != nil {
		t.Fatalf("record audit: %v", err)
	}
	entries, err := d.ListAudit("p1", task.ID, 0)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].TraceID != *task.TraceID {
		t.Errorf("audit trace_id = %q, want %q", entries[0].TraceID, *task.TraceID)
	}
}

func TestValidTraceID(t *testing.T) {
	cases := map[string]bool{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": true,
		"0123456789abcdef0123456789abcdef": true,
		"":                                 false,
		"tooshort":                         false,
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA": false, // uppercase not allowed
		"gggggggggggggggggggggggggggggggg": false, // non-hex
	}
	for in, want := range cases {
		if got := ValidTraceID(in); got != want {
			t.Errorf("ValidTraceID(%q) = %v, want %v", in, got, want)
		}
	}
}
