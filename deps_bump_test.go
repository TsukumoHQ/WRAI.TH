package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// External-dependency bump (dependabot #147 go-deps group + #146 actions/setup-go
// 6->7), landed as one commit series. One test per acceptance criterion.

// AC1: the go-deps bump is present in go.mod (the build/test green + `go mod tidy`
// no-diff are enforced by verify_cmd; this pins the exact versions that landed).
func TestDepsBumpBuildGreen(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, want := range []string{"github.com/mark3labs/mcp-go v1.0.0", "github.com/mattn/go-sqlite3 v1.14.50"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("go.mod missing bumped dependency %q", want)
		}
	}
}

// AC2: every workflow that uses actions/setup-go pins @v7 and none still
// references @v6.
func TestWorkflowsSetupGoV7(t *testing.T) {
	files, _ := filepath.Glob(".github/workflows/*.yml")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(b)
		if strings.Contains(s, "actions/setup-go@v6") {
			t.Fatalf("%s still pins actions/setup-go@v6", f)
		}
		if strings.Contains(s, "actions/setup-go@") && !strings.Contains(s, "actions/setup-go@v7") {
			t.Fatalf("%s uses actions/setup-go but not @v7", f)
		}
	}
}
