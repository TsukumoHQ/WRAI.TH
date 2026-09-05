package relay

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"agent-relay/internal/db"
)

// captureRelayLog redirects the stdlib logger into a buffer for fn's duration.
func captureRelayLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()
	fn()
	return buf.String()
}

// digestLimboBlocks sends ONE grouped digest per dispatcher, but only to a
// dispatcher that is still active; a gone/inactive dispatcher gets a suppressed
// journal line instead of a message into the void.
func TestDigestLimboBlocksGroupsAndSuppresses(t *testing.T) {
	r := testRelay(t)
	// boss-live is registered + active; boss-dead is never registered → gone.
	if _, _, err := r.DB.RegisterAgent("p1", "boss-live", "", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register boss-live: %v", err)
	}

	blocks := []db.LimboDisposition{
		{TaskID: "t1", Project: "p1", DispatchedBy: "boss-dead"},
		{TaskID: "t2", Project: "p1", DispatchedBy: "boss-dead"},
		{TaskID: "t3", Project: "p1", DispatchedBy: "boss-live"},
		{TaskID: "t4", Project: "p1", DispatchedBy: ""}, // no dispatcher → ignored
	}

	out := captureRelayLog(t, func() { r.digestLimboBlocks(blocks) })

	// gone dispatcher: one suppressed line, its two task ids grouped in order.
	if !strings.Contains(out, "limbo digest suppressed") || !strings.Contains(out, "boss-dead") {
		t.Fatalf("expected a suppressed digest for boss-dead, log:\n%s", out)
	}
	if !strings.Contains(out, "t1, t2") {
		t.Fatalf("expected boss-dead's tasks grouped as 't1, t2', log:\n%s", out)
	}
	// active dispatcher: NOT suppressed (its digest is sent, silent with no session).
	if strings.Contains(out, "boss-live") {
		t.Fatalf("boss-live is active — its digest must not be suppressed/logged, log:\n%s", out)
	}
	// empty-dispatcher disposition must not produce a suppressed line for "".
	if strings.Count(out, "limbo digest suppressed") != 1 {
		t.Fatalf("want exactly one suppressed line (boss-dead only), log:\n%s", out)
	}
}

// The dry-run shadow line is EXACTLY `integrity: limbo would-block <task> <age>d
// <assignee> <dispatcher>` — the format cto-tsukumo greps + counts after
// redeploy (`grep -c 'limbo would-block'`).
func TestLimboShadowLineFormat(t *testing.T) {
	got := limboShadowLine(db.LimboDisposition{
		TaskID: "abc123", AgeDays: 42, AssignedTo: "dead", DispatchedBy: "ghostboss",
	})
	want := "integrity: limbo would-block abc123 42d dead ghostboss"
	if got != want {
		t.Fatalf("shadow line = %q, want %q", got, want)
	}
}

// limboSweepApply defaults to dry-run; the DB setting enables writes, and the env
// override wins over the setting in both directions.
func TestLimboSweepApplyGating(t *testing.T) {
	r := testRelay(t)

	if r.limboSweepApply() {
		t.Fatal("default must be dry-run (apply=false)")
	}

	r.DB.SetSetting("limbo_sweep_apply", "1")
	if !r.limboSweepApply() {
		t.Fatal("setting limbo_sweep_apply=1 must enable apply")
	}

	// env override wins over the setting, both ways.
	t.Setenv("RELAY_LIMBO_SWEEP_APPLY", "0")
	if r.limboSweepApply() {
		t.Fatal("env=0 must override setting=1 (force dry-run)")
	}
	t.Setenv("RELAY_LIMBO_SWEEP_APPLY", "true")
	r.DB.SetSetting("limbo_sweep_apply", "0")
	if !r.limboSweepApply() {
		t.Fatal("env=true must override setting=0 (force apply)")
	}
}
