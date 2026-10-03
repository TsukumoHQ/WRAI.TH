package relay

import (
	"os"
	"strings"
	"testing"
)

// W8 D5 (ruling wraith-deploying-ruling): both boards show a Deploying column
// between In Review and Done, and the relay skill documents deploy_task. No JS
// runtime harness exists in this repo, so — like console_v2_boards_test.go —
// the client-side mapping is pinned at the source level (a node DOM receipt
// rides in .niwa/receipts/d5-board-columns.txt).
func TestBoardsShowDeployingColumn(t *testing.T) {
	order := func(t *testing.T, src string, markers ...string) {
		t.Helper()
		last := -1
		for _, m := range markers {
			i := strings.Index(src, m)
			if i < 0 {
				t.Fatalf("missing %q", m)
			}
			if i < last {
				t.Fatalf("%q out of order (want %v)", m, markers)
			}
			last = i
		}
	}

	t.Run("v1 kanban", func(t *testing.T) {
		k := readAsset(t, "static/js/kanban.js")
		order(t, k, `const COLUMNS = ["backlog", "todo", "in-progress", "in-review", "deploying", "done"];`)
		for _, m := range []string{`deploying: "DEPLOYING"`, `deploying: "deploying",`, `if (s.includes("deploy")) return "deploying";`} {
			if !strings.Contains(k, m) {
				t.Errorf("kanban.js missing %q", m)
			}
		}
	})

	t.Run("v2 board", func(t *testing.T) {
		api := readAsset(t, "static/v2/api.js")
		order(t, api, `{ key: 'in_review', label: 'In Review'`, `{ key: 'deploying', label: 'Deploying'`, `{ key: 'done', label: 'Done'`)
		for _, m := range []string{`case 'deploying': return 'deploying';`, `'task.deploying': 'deploying'`} {
			if !strings.Contains(api, m) {
				t.Errorf("api.js missing %q", m)
			}
		}
		// A drop into Deploying has no status to write (deploy_task needs the
		// merge sha): the board must not fire a transition for it.
		board := readAsset(t, "static/v2/board.js")
		if !strings.Contains(board, "if (!status) return;") {
			t.Error("board.js dropTo must skip columns without a writable status (Deploying)")
		}
	})

	t.Run("skill documents deploy_task", func(t *testing.T) {
		b, err := os.ReadFile("../../skill/relay.md")
		if err != nil {
			t.Fatal(err)
		}
		skill := string(b)
		for _, m := range []string{"deploy_task", "in-review → deploying", "merge_sha"} {
			if !strings.Contains(skill, m) {
				t.Errorf("skill/relay.md missing %q", m)
			}
		}
	})
}
