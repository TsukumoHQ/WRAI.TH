package linear

import "testing"

func TestResolveStateID(t *testing.T) {
	states := []stateInfo{
		{ID: "bk", Name: "Backlog", Type: "backlog"},
		{ID: "td", Name: "Todo", Type: "unstarted"},
		{ID: "ip", Name: "In Progress", Type: "started"},
		{ID: "rev", Name: "In Review", Type: "started"},
		{ID: "blk", Name: "Blocked", Type: "started"},
		{ID: "dn", Name: "Done", Type: "completed"},
		{ID: "cx", Name: "Canceled", Type: "canceled"},
	}
	cases := map[string]string{
		"in-review":   "rev",
		"blocked":     "blk",
		"in-progress": "ip",
		"accepted":    "ip",
		"done":        "dn",
		"cancelled":   "cx",
		"pending":     "td",
		"bogus":       "",
	}
	for status, want := range cases {
		if got := resolveStateID(status, states); got != want {
			t.Errorf("resolveStateID(%q) = %q, want %q", status, got, want)
		}
	}
}

// A team without a Blocked state → blocked maps to nothing (caller falls back to
// a comment), and in-progress still resolves to the lone started state.
func TestResolveStateID_NoBlockedState(t *testing.T) {
	states := []stateInfo{
		{ID: "td", Name: "Todo", Type: "unstarted"},
		{ID: "ip", Name: "Doing", Type: "started"},
		{ID: "dn", Name: "Done", Type: "completed"},
	}
	if got := resolveStateID("blocked", states); got != "" {
		t.Errorf("blocked without a Blocked state = %q, want empty", got)
	}
	if got := resolveStateID("in-progress", states); got != "ip" {
		t.Errorf("in-progress = %q, want ip", got)
	}
}
