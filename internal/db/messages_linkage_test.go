package db

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
)

// DEC-wraith-linkage-1 (task da1945d5): messages.task_id is derived at insert
// from a relay-written metadata.task_id or a same-project reply_to parent —
// never from prose — and historical rows are backfilled with the same rule.

func linkageTask(t *testing.T, d *DB, project string) string {
	t.Helper()
	task, err := d.DispatchTask(project, "lead", "cto", "linkage", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch task: %v", err)
	}
	return task.ID
}

func storedTaskID(t *testing.T, d *DB, msgID string) string {
	t.Helper()
	var v *string
	if err := d.conn.QueryRow("SELECT task_id FROM messages WHERE id = ?", msgID).Scan(&v); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func deliveryCount(t *testing.T, d *DB, msgID string) int {
	t.Helper()
	var n int
	if err := d.conn.QueryRow("SELECT COUNT(*) FROM deliveries WHERE message_id = ?", msgID).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return n
}

func TestLinkage(t *testing.T) {
	t.Run("MetadataTaskIDPromoted", func(t *testing.T) {
		d := testDB(t)
		taskID := linkageTask(t, d, "p1")
		meta := fmt.Sprintf(`{"task_id":%q}`, taskID)

		m1, _, err := d.InsertMessageWithDeliveries("p1", "cto", "bot", "task", "s", "c", meta, "P2", 0, nil, nil, []string{"bot"}, "")
		if err != nil {
			t.Fatalf("insert with deliveries: %v", err)
		}
		m2, err := d.InsertMessage("p1", "cto", "bot", "notification", "s", "c", meta, "P2", 0, nil, nil)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		for _, m := range []string{m1.ID, m2.ID} {
			if got := storedTaskID(t, d, m); got != taskID {
				t.Fatalf("message %s: task_id = %q, want %q", m, got, taskID)
			}
		}
		if m1.TaskID == nil || *m1.TaskID != taskID {
			t.Fatalf("returned message TaskID = %v, want %s", m1.TaskID, taskID)
		}
	})

	t.Run("EmptyMetadataTaskIDStaysNull", func(t *testing.T) {
		d := testDB(t)
		for _, meta := range []string{`{"task_id":"","event":"agent.joined"}`, `{}`, `not json`} {
			m, _, err := d.InsertMessageWithDeliveries("p1", "notifier", "bot", "notification", "s", "c", meta, "P2", 0, nil, nil, []string{"bot"}, "")
			if err != nil {
				t.Fatalf("insert %s: %v", meta, err)
			}
			if got := storedTaskID(t, d, m.ID); got != "" {
				t.Fatalf("metadata %s: task_id = %q, want NULL", meta, got)
			}
		}
	})

	t.Run("UnknownOrCrossProjectTaskIDStaysNull", func(t *testing.T) {
		d := testDB(t)
		other := linkageTask(t, d, "p2")
		for _, taskID := range []string{"00000000-0000-0000-0000-000000000000", other} {
			m, _, err := d.InsertMessageWithDeliveries("p1", "cto", "bot", "task", "s", "c", fmt.Sprintf(`{"task_id":%q}`, taskID), "P2", 0, nil, nil, []string{"bot"}, "")
			if err != nil {
				t.Fatalf("insert must still succeed for %s: %v", taskID, err)
			}
			if got := storedTaskID(t, d, m.ID); got != "" {
				t.Fatalf("task %s: task_id = %q, want NULL", taskID, got)
			}
			if n := deliveryCount(t, d, m.ID); n != 1 {
				t.Fatalf("task %s: %d delivery rows, want 1", taskID, n)
			}
		}
	})

	t.Run("ReplyInheritsParentTaskID", func(t *testing.T) {
		d := testDB(t)
		taskID := linkageTask(t, d, "p1")
		parent, _, err := d.InsertMessageWithDeliveries("p1", "cto", "bot", "task", "s", "c", fmt.Sprintf(`{"task_id":%q}`, taskID), "P2", 0, nil, nil, []string{"bot"}, "")
		if err != nil {
			t.Fatalf("insert parent: %v", err)
		}
		reply, _, err := d.InsertMessageWithDeliveries("p1", "bot", "cto", "response", "re", "ok", "{}", "P2", 0, &parent.ID, nil, []string{"cto"}, "")
		if err != nil {
			t.Fatalf("insert reply: %v", err)
		}
		if got := storedTaskID(t, d, reply.ID); got != taskID {
			t.Fatalf("reply task_id = %q, want inherited %q", got, taskID)
		}
	})

	t.Run("CrossProjectParentNotInherited", func(t *testing.T) {
		d := testDB(t)
		taskID := linkageTask(t, d, "p2")
		parent, _, err := d.InsertMessageWithDeliveries("p2", "cto", "bot", "task", "s", "c", fmt.Sprintf(`{"task_id":%q}`, taskID), "P2", 0, nil, nil, []string{"bot"}, "")
		if err != nil {
			t.Fatalf("insert parent: %v", err)
		}
		reply, _, err := d.InsertMessageWithDeliveries("p1", "bot", "cto", "response", "re", "ok", "{}", "P2", 0, &parent.ID, nil, []string{"cto"}, "")
		if err != nil {
			t.Fatalf("insert reply: %v", err)
		}
		if got := storedTaskID(t, d, reply.ID); got != "" {
			t.Fatalf("cross-project reply task_id = %q, want NULL", got)
		}
	})

	t.Run("MetadataWinsOverInheritedWithConflictLog", func(t *testing.T) {
		d := testDB(t)
		inherited := linkageTask(t, d, "p1")
		declared := linkageTask(t, d, "p1")
		parent, _, err := d.InsertMessageWithDeliveries("p1", "cto", "bot", "task", "s", "c", fmt.Sprintf(`{"task_id":%q}`, inherited), "P2", 0, nil, nil, []string{"bot"}, "")
		if err != nil {
			t.Fatalf("insert parent: %v", err)
		}

		var buf bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(prev)

		reply, _, err := d.InsertMessageWithDeliveries("p1", "bot", "cto", "response", "re", "ok", fmt.Sprintf(`{"task_id":%q}`, declared), "P2", 0, &parent.ID, nil, []string{"cto"}, "")
		if err != nil {
			t.Fatalf("insert reply: %v", err)
		}
		if got := storedTaskID(t, d, reply.ID); got != declared {
			t.Fatalf("reply task_id = %q, want metadata %q", got, declared)
		}
		if n := strings.Count(buf.String(), "[linkage] task_id conflict"); n != 1 {
			t.Fatalf("want exactly 1 [linkage] conflict line, got %d: %q", n, buf.String())
		}
		if !strings.Contains(buf.String(), declared) || !strings.Contains(buf.String(), inherited) {
			t.Fatalf("conflict line must name both ids: %q", buf.String())
		}
	})

	t.Run("InboxUnchanged", func(t *testing.T) {
		d := testDB(t)
		taskID := linkageTask(t, d, "p1")
		linked, _, err := d.InsertMessageWithDeliveries("p1", "cto", "bot", "task", "s", "c", fmt.Sprintf(`{"task_id":%q}`, taskID), "P2", 0, nil, nil, []string{"bot"}, "")
		if err != nil {
			t.Fatalf("insert linked: %v", err)
		}
		plain, _, err := d.InsertMessageWithDeliveries("p1", "cto", "bot", "notification", "s", "c", "{}", "P2", 0, nil, nil, []string{"bot"}, "")
		if err != nil {
			t.Fatalf("insert plain: %v", err)
		}
		inbox, err := d.GetInbox("p1", "bot", true, 50)
		if err != nil {
			t.Fatalf("get inbox: %v", err)
		}
		seen := map[string]bool{}
		for _, m := range inbox {
			seen[m.ID] = true
		}
		if len(inbox) != 2 || !seen[linked.ID] || !seen[plain.ID] {
			t.Fatalf("inbox must surface linked and unlinked messages alike, got %d: %v", len(inbox), seen)
		}
	})

	t.Run("BackfillIdempotent", func(t *testing.T) {
		d := testDB(t)
		taskID := linkageTask(t, d, "p1")
		other := linkageTask(t, d, "p2")
		m, _, err := d.InsertMessageWithDeliveries("p1", "cto", "bot", "task", "s", "c", fmt.Sprintf(`{"task_id":%q}`, taskID), "P2", 0, nil, nil, []string{"bot"}, "")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		cross, _, err := d.InsertMessageWithDeliveries("p1", "cto", "bot", "task", "s", "c", fmt.Sprintf(`{"task_id":%q}`, other), "P2", 0, nil, nil, []string{"bot"}, "")
		if err != nil {
			t.Fatalf("insert cross: %v", err)
		}
		// Simulate pre-linkage history: rows written before deriveTaskID existed.
		if _, err := d.conn.Exec("UPDATE messages SET task_id = NULL"); err != nil {
			t.Fatalf("reset task_id: %v", err)
		}

		n1, err := backfillMessageTaskIDs(d.conn)
		if err != nil {
			t.Fatalf("first backfill: %v", err)
		}
		if n1 != 1 {
			t.Fatalf("first backfill updated %d rows, want 1", n1)
		}
		n2, err := backfillMessageTaskIDs(d.conn)
		if err != nil {
			t.Fatalf("second backfill: %v", err)
		}
		if n2 != 0 {
			t.Fatalf("second backfill updated %d rows, want 0", n2)
		}
		if got := storedTaskID(t, d, m.ID); got != taskID {
			t.Fatalf("backfilled task_id = %q, want %q", got, taskID)
		}
		if got := storedTaskID(t, d, cross.ID); got != "" {
			t.Fatalf("cross-project row backfilled to %q, want NULL", got)
		}
	})
}
