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
//
// pushStatusAsync fires PushStatus from a goroutine, so the call count must be
// observed through a channel (synchronized by Go's memory model), never a bare
// shared counter — a plain `pushed++`/read pair here is a real data race under
// `go test -race` even though it "worked" unraced locally.
func TestPushStatusAsyncSkipsSecondaryMirror(t *testing.T) {
	calls := make(chan struct{}, 2)
	fake := &fakeTaskConn{onPushStatus: func(string, string, string) error {
		calls <- struct{}{}
		return nil
	}}

	issueID := "iss-x"
	secondary := &models.Task{Source: "linear", LinearIssueID: &issueID, LinearSecondary: true}
	primary := &models.Task{Source: "linear", LinearIssueID: &issueID, LinearSecondary: false}

	pushStatusAsync(fake, secondary, "in-review", nil)
	pushStatusAsync(fake, primary, "in-review", nil)

	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("PushStatus never called — the primary mirror must push")
	}
	select {
	case <-calls:
		t.Fatal("PushStatus called a second time — the secondary mirror must be skipped")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPushStatusAsyncNativeAndNoLinearIDUnaffected guards that the new
// Secondary gate never touches a native task or a Linear task with no issue id
// — pre-existing no-op paths must stay byte-identical.
func TestPushStatusAsyncNativeAndNoLinearIDUnaffected(t *testing.T) {
	calls := make(chan struct{}, 2)
	fake := &fakeTaskConn{onPushStatus: func(string, string, string) error {
		calls <- struct{}{}
		return nil
	}}

	native := &models.Task{Source: "native"}
	pushStatusAsync(fake, native, "in-review", nil)

	linearNoID := &models.Task{Source: "linear"}
	pushStatusAsync(fake, linearNoID, "in-review", nil)

	select {
	case <-calls:
		t.Fatal("PushStatus called for a native/no-issue-id task, want no-op")
	case <-time.After(50 * time.Millisecond):
	}
}
