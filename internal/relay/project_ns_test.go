package relay

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizeProject(t *testing.T) {
	cases := map[string]string{
		"synergix_prod":           "synergix-prod",
		"Synergix-Prod":           "synergix-prod",
		"  aitime-calc  ":         "aitime-calc",
		"synergix_prod@synx-prod": "synergix-prod@synx-prod",
		"":                        "",
	}
	for in, want := range cases {
		if got := NormalizeProject(in); got != want {
			t.Errorf("NormalizeProject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	if d := editDistance("aitime-calc", "aitime-calc"); d != 0 {
		t.Errorf("identical strings: %d", d)
	}
	if d := editDistance("aitime-calk", "aitime-calc"); d != 1 {
		t.Errorf("one substitution: %d", d)
	}
	if d := editDistance("", "abc"); d != 3 {
		t.Errorf("empty vs abc: %d", d)
	}
}

// TestResolveProjectBindsToRegistration reproduces the live black hole: an
// agent whose MCP connection carries no ?project= sent its founder report
// into the "default" namespace nobody reads. With exactly one registration,
// a project-less call now resolves to the agent's own project.
func TestResolveProjectBindsToRegistration(t *testing.T) {
	h := testHandlers(t)
	if _, err := h.HandleRegisterAgent(context.Background(), call(map[string]any{
		"project": "aitime-calc", "name": "cto-56",
	})); err != nil {
		t.Fatal(err)
	}

	got := h.resolveProject(context.Background(), call(map[string]any{"as": "cto-56"}))
	if got != "aitime-calc" {
		t.Errorf("bound project = %q, want aitime-calc", got)
	}

	// An explicit project parameter still wins over the registration.
	got = h.resolveProject(context.Background(), call(map[string]any{
		"as": "cto-56", "project": "other-place",
	}))
	if got != "other-place" {
		t.Errorf("explicit project = %q, want other-place", got)
	}
}

// TestResolveProjectAmbiguousStaysDefault: a name registered in two projects
// must not be guessed — better the visible default than a silent misroute.
func TestResolveProjectAmbiguousStaysDefault(t *testing.T) {
	h := testHandlers(t)
	for _, p := range []string{"proj-a", "proj-b"} {
		if _, err := h.HandleRegisterAgent(context.Background(), call(map[string]any{
			"project": p, "name": "cto",
		})); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.resolveProject(context.Background(), call(map[string]any{"as": "cto"})); got != "default" {
		t.Errorf("ambiguous binding = %q, want default", got)
	}
}

// TestSendMessageUnknownProjectFailsLoud: writing into a namespace nobody
// created must error (with the nearest real name), not vanish.
func TestSendMessageUnknownProjectFailsLoud(t *testing.T) {
	h := testHandlers(t)
	if _, err := h.HandleRegisterAgent(context.Background(), call(map[string]any{
		"project": "trovex-growth", "name": "cmo",
	})); err != nil {
		t.Fatal(err)
	}

	res, err := h.HandleSendMessage(context.Background(), call(map[string]any{
		"project": "trovex-growht", // transposition typo
		"as":      "cmo",
		"to":      "user",
		"content": "hello",
	}))
	if err != nil {
		t.Fatal(err)
	}
	msg := expectError(t, res)
	if !strings.Contains(msg, "unknown project") || !strings.Contains(msg, "trovex-growth") {
		t.Errorf("expected unknown-project error suggesting trovex-growth, got %q", msg)
	}
}

// TestSendMessageNormalizesSpelling: the underscore spelling of a hyphen
// project is the SAME project after normalization — the send goes through
// and lands in the canonical namespace.
func TestSendMessageNormalizesSpelling(t *testing.T) {
	h := testHandlers(t)
	if _, err := h.HandleRegisterAgent(context.Background(), call(map[string]any{
		"project": "synergix-prod", "name": "cto",
	})); err != nil {
		t.Fatal(err)
	}

	res, err := h.HandleSendMessage(context.Background(), call(map[string]any{
		"project": "synergix_prod",
		"as":      "cto",
		"to":      "user",
		"content": "deploy done",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("normalized send should succeed, got %v", res.Content)
	}
	msgs, err := h.db.GetInbox("synergix-prod", "user", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected 1 message in canonical project, got %d", len(msgs))
	}
}
