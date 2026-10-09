package ingest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stopTokens is the body skill/hooks/ingest-stop.sh POSTs to /api/ingest/tokens.
type stopTokens struct {
	SessionID     string `json:"session_id"`
	Input         int64  `json:"input"`
	Output        int64  `json:"output"`
	CacheRead     int64  `json:"cache_read"`
	CacheCreation int64  `json:"cache_creation"`
	Model         string `json:"model"`
}

type stopRun struct {
	stderr   string
	tokens   []stopTokens
	activity int
}

// runStopHook runs the real Stop hook against a fixture transcript with a fake
// relay behind RELAY_URL. The hook POSTs in the background, so it waits for the
// activity POST (always sent) plus a grace period for the tokens POST.
func runStopHook(t *testing.T, fixture string) stopRun {
	t.Helper()
	for _, bin := range []string{"bash", "jq", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	got := make(chan [2]string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- [2]string{r.URL.Path, string(b)}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	transcript, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	stdin, _ := json.Marshal(map[string]string{"session_id": "sess-" + fixture, "transcript_path": transcript})
	tmp := t.TempDir()
	cmd := exec.Command("bash", filepath.Join("..", "..", "skill", "hooks", "ingest-stop.sh"))
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = append(os.Environ(), "RELAY_URL="+srv.URL, "RELAY_API_KEY=", "HOME="+tmp, "XDG_CACHE_HOME="+tmp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook must never fail: %v (stderr %q)", err, stderr.String())
	}

	run := stopRun{stderr: stderr.String()}
	deadline := time.After(5 * time.Second)
	grace := (<-chan time.Time)(nil)
	for {
		select {
		case req := <-got:
			switch req[0] {
			case "/api/ingest/activity":
				run.activity++
				grace = time.After(time.Second)
			case "/api/ingest/tokens":
				var tok stopTokens
				if err := json.Unmarshal([]byte(req[1]), &tok); err != nil {
					t.Fatalf("tokens body %q: %v", req[1], err)
				}
				run.tokens = append(run.tokens, tok)
			}
		case <-grace:
			return run
		case <-deadline:
			t.Fatalf("no activity POST within 5s (got %+v)", run)
		}
	}
}

// One Claude message is written as one transcript line per content block, each
// repeating the same usage. The fixture holds 19 usage lines for 9 messages;
// summing every line inflated output 6279 vs 2425 real.
func TestStopHook_ClaudeCountsEachMessageOnce(t *testing.T) {
	run := runStopHook(t, "claude-multiblock.jsonl")
	if len(run.tokens) != 1 {
		t.Fatalf("want 1 tokens POST, got %d", len(run.tokens))
	}
	want := stopTokens{SessionID: "sess-claude-multiblock.jsonl", Input: 18, Output: 2425,
		CacheRead: 535654, CacheCreation: 48054, Model: "claude-opus-5-5"}
	if run.tokens[0] != want {
		t.Fatalf("tokens = %+v, want %+v", run.tokens[0], want)
	}
}

// Codex has no .message.usage: usage lives in event_msg token_count events
// (info.last_token_usage, per turn). input_tokens includes the cached part and
// output_tokens already includes reasoning (total_tokens = input + output), so
// input = input - cached and reasoning is not added. token_usage_record lines
// repeat the same usage and info:null token_count lines carry none.
func TestStopHook_CodexSumsTokenCountEvents(t *testing.T) {
	run := runStopHook(t, "codex-rollout.jsonl")
	if len(run.tokens) != 1 {
		t.Fatalf("want 1 tokens POST, got %d", len(run.tokens))
	}
	want := stopTokens{SessionID: "sess-codex-rollout.jsonl", Input: 19128, Output: 390,
		CacheRead: 103040, CacheCreation: 0, Model: "gpt-6.1-sol"}
	if run.tokens[0] != want {
		t.Fatalf("tokens = %+v, want %+v", run.tokens[0], want)
	}
}

func TestStopHook_UnknownShapeIngestsZeroAndWarnsOnce(t *testing.T) {
	run := runStopHook(t, "unknown-shape.jsonl")
	if run.activity != 1 {
		t.Fatalf("activity POSTs = %d, want 1", run.activity)
	}
	if len(run.tokens) != 0 {
		t.Fatalf("unknown transcript must ingest nothing, got %+v", run.tokens)
	}
	lines := strings.Split(strings.TrimRight(run.stderr, "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "unrecognised transcript") {
		t.Fatalf("want exactly one warning line, got %q", run.stderr)
	}
}
