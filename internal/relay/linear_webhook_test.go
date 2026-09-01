package relay

import (
	"testing"
	"time"

	"agent-relay/internal/connector"
	"agent-relay/internal/models"
)

// fakeTaskConn is a minimal connector.TaskConnector stub for exercising
// pushStatusAsync's gating without a live Linear connector.
type fakeTaskConn struct {
	onPushStatus func(linearIssueID, status, comment string) error
}

func (f *fakeTaskConn) Ingest(_ []byte, _ string) ([]connector.TaskEvent, error) { return nil, nil }
func (f *fakeTaskConn) PushInReview(_, _ string) error                           { return nil }
func (f *fakeTaskConn) PushStatus(linearIssueID, status, comment string) error {
	if f.onPushStatus != nil {
		return f.onPushStatus(linearIssueID, status, comment)
	}
	return nil
}
func (f *fakeTaskConn) Comment(_, _ string) error            { return nil }
func (f *fakeTaskConn) ReconcileCycle(_ string) (int, error) { return 0, nil }
func (f *fakeTaskConn) MapState(s string) string             { return connector.MapStateType(s) }
func (f *fakeTaskConn) Active() bool                         { return true }

// TestPushStatusAsyncSkipsSecondaryMirror proves the fan-out write-back gate:
// a SECONDARY mirror task (linear_project_map routed its issue to several
// relay projects; this row isn't the primary) must never push a Linear state
// change — only the primary mirror does (single-write-back-per-issue, AC5).
func TestPushStatusAsyncSkipsSecondaryMirror(t *testing.T) {
	pushed := 0
	fake := &fakeTaskConn{onPushStatus: func(string, string, string) error { pushed++; return nil }}

	issueID := "iss-x"
	secondary := &models.Task{Source: "linear", LinearIssueID: &issueID, LinearSecondary: true}
	primary := &models.Task{Source: "linear", LinearIssueID: &issueID, LinearSecondary: false}

	pushStatusAsync(fake, secondary, "in-review", nil)
	pushStatusAsync(fake, primary, "in-review", nil)

	// pushStatusAsync fires the push in a goroutine; give it a beat to land.
	deadline := time.Now().Add(time.Second)
	for pushed == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pushed != 1 {
		t.Errorf("PushStatus called %d times, want exactly 1 (secondary mirror must be skipped, primary must fire)", pushed)
	}
}

// TestPushStatusAsyncNativeAndNoLinearIDUnaffected guards that the new
// Secondary gate never touches a native task or a Linear task with no issue id
// — pre-existing no-op paths must stay byte-identical.
func TestPushStatusAsyncNativeAndNoLinearIDUnaffected(t *testing.T) {
	pushed := 0
	fake := &fakeTaskConn{onPushStatus: func(string, string, string) error { pushed++; return nil }}

	native := &models.Task{Source: "native"}
	pushStatusAsync(fake, native, "in-review", nil)

	linearNoID := &models.Task{Source: "linear"}
	pushStatusAsync(fake, linearNoID, "in-review", nil)

	time.Sleep(20 * time.Millisecond)
	if pushed != 0 {
		t.Errorf("PushStatus called %d times, want 0 for native/no-issue-id tasks", pushed)
	}
}
