package relay

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"agent-relay/internal/db"
	"agent-relay/internal/web"
)

// readAsset returns an embedded v2/v1 static asset or fails the test.
func readAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(web.StaticFiles, name)
	if err != nil {
		t.Fatalf("read embedded asset %s: %v", name, err)
	}
	return string(b)
}

// TestV2DeleteTask is the S2 (task 654d358d) contract: the v2 console exposes
// the existing REST DELETE /api/tasks/:id behind a confirmation. The five
// subtests map 1:1 to the five acceptance criteria.
func TestV2DeleteTask(t *testing.T) {
	// AC1 — api.js exposes deleteTask(id) → DELETE /api/tasks/:id, same error style.
	t.Run("WrapperDeleteTaskPresent", func(t *testing.T) {
		api := readAsset(t, "static/v2/api.js")
		if !strings.Contains(api, "deleteTask:") {
			t.Error("api.js does not expose a deleteTask wrapper")
		}
		// The wrapper must issue a DELETE to the task endpoint...
		if !strings.Contains(api, "sendJSON('DELETE', `/api/tasks/") {
			t.Error("deleteTask must sendJSON('DELETE', `/api/tasks/…`)")
		}
		// ...and go through sendJSON, which carries the shared error handling
		// (res.ok → j.detail/j.error) that the other wrappers use.
		if !strings.Contains(api, "deleteMemory:") || !strings.Contains(api, "deleteRule:") {
			t.Error("expected sibling delete wrappers (deleteMemory/deleteRule) that share the error style")
		}
	})

	// AC2 — board.js shows a delete button per task, gated by an explicit confirm.
	t.Run("ButtonGatedByConfirm", func(t *testing.T) {
		board := readAsset(t, "static/v2/board.js")
		if !strings.Contains(board, "delete-btn") {
			t.Fatal("board.js has no delete button")
		}
		if !strings.Contains(board, "window.confirm(") {
			t.Fatal("delete is not gated by window.confirm")
		}
		guard := strings.Index(board, "window.confirm(")
		call := strings.Index(board, "ctx.api.deleteTask(")
		if guard < 0 || call < 0 {
			t.Fatal("expected both a confirm guard and a deleteTask call in board.js")
		}
		if guard >= call {
			t.Errorf("confirm guard (@%d) must precede the deleteTask call (@%d)", guard, call)
		}
	})

	// AC3 — cancelling the confirm makes NO network call. Enforced statically:
	// there is exactly ONE deleteTask call site and it is unreachable unless the
	// confirm passed (the guard `if (!window.confirm(...)) return;` sits above it).
	// Limitation: this is a source-level guarantee — a true DOM/runtime click
	// test needs a JS harness (none exists in this repo), stated in the report.
	t.Run("CancelNoNetwork", func(t *testing.T) {
		board := readAsset(t, "static/v2/board.js")
		if n := strings.Count(board, "ctx.api.deleteTask("); n != 1 {
			t.Fatalf("expected exactly 1 deleteTask call site, found %d", n)
		}
		if !strings.Contains(board, "if (!window.confirm(") {
			t.Error("the confirm guard must be a negated early-return (if (!window.confirm(…)) return;)")
		}
		if !strings.Contains(board, ")) return;") {
			t.Error("the confirm guard must early-return before any fetch when cancelled")
		}
	})

	// AC4 — a successful delete removes the task; the server DELETE round-trips
	// and the board no longer lists it (the UI drops the card via reconcile).
	t.Run("DeleteRemovesTaskLive", func(t *testing.T) {
		r := testRelay(t)
		_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
		task, _ := r.DB.DispatchTask("p1", "dev", "bot-a", "to delete", "", "P2", nil, nil, db.TypedTicket{}, false, nil)

		before := doAPI(r, "GET", "/tasks/board?project=p1", "")
		if got := len(decodeJSONArray(t, before)); got != 1 {
			t.Fatalf("board before delete: want 1 task, got %d", got)
		}

		del := doAPI(r, "DELETE", "/tasks/"+task.ID+"?project=p1", "")
		if del.Code != http.StatusOK {
			t.Fatalf("DELETE: want 200, got %d: %s", del.Code, del.Body.String())
		}
		if data := decodeJSON(t, del); data["deleted"] != true {
			t.Errorf("DELETE: want deleted=true, got %v", data["deleted"])
		}

		after := doAPI(r, "GET", "/tasks/board?project=p1", "")
		if got := len(decodeJSONArray(t, after)); got != 0 {
			t.Errorf("board after delete: want 0 tasks, got %d", got)
		}
	})

	// AC5 — v1 (static/index.html) is untouched. The v2 delete affordance must
	// not have leaked into the v1 showroom. Byte-identity to main is verified
	// out-of-band (git diff --stat main…HEAD lists only the three v2 files);
	// here we assert the v1 entrypoint carries none of the v2 delete markers.
	t.Run("BuildGreenV1ZeroDiff", func(t *testing.T) {
		v1 := readAsset(t, "static/index.html")
		for _, marker := range []string{"deleteTask", "delete-btn"} {
			if strings.Contains(v1, marker) {
				t.Errorf("v1 index.html unexpectedly contains %q — v2 delete leaked into v1", marker)
			}
		}
		// The danger styling shipped in v2, not v1.
		if css := readAsset(t, "static/v2/v2.css"); !strings.Contains(css, ".cmd-btn.danger") {
			t.Error("v2.css is missing the .cmd-btn.danger delete-button style")
		}
	})
}
