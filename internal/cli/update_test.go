package cli

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTarGz(t *testing.T, path, entry string, content []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gz.Close()
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "agent-relay-linux-amd64.tar.gz")
	writeTarGz(t, arc, "agent-relay", []byte("#!/bin/true\n"))

	if err := extractArchive(arc, dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "agent-relay"))
	if err != nil {
		t.Fatalf("extracted binary missing: %v", err)
	}
	if string(got) != "#!/bin/true\n" {
		t.Fatalf("bad content: %q", got)
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "agent-relay-linux-amd64.tar.gz")
	if err := os.WriteFile(arc, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("payload"))
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	line := hex.EncodeToString(sum[:]) + "  agent-relay-linux-amd64.tar.gz\n"
	if err := os.WriteFile(sumsPath, []byte("deadbeef  other.tar.gz\n"+line), 0o644); err != nil {
		t.Fatal(err)
	}

	ok, err := verifyChecksum(arc, "agent-relay-linux-amd64.tar.gz", sumsPath)
	if err != nil || !ok {
		t.Fatalf("valid checksum rejected: ok=%v err=%v", ok, err)
	}

	// Tamper the archive → must fail.
	if err := os.WriteFile(arc, []byte("payload-TAMPERED"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = verifyChecksum(arc, "agent-relay-linux-amd64.tar.gz", sumsPath)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatal("tampered archive passed checksum verification")
	}

	// Missing entry → error.
	if _, err := verifyChecksum(arc, "nonexistent.tar.gz", sumsPath); err == nil {
		t.Fatal("expected error for missing checksum entry")
	}
}

// TestAtomicReplaceKeepsOldInode is the core TSU-74 guarantee: replacing the
// binary must NOT disturb a process already running the old one (a live
// `agent-relay mcp` stdio pipe). An in-place truncate-write would corrupt that
// process's text segment (SIGBUS); an atomic rename swaps the directory entry to
// a new inode and leaves the old, now-unlinked inode intact for the running
// process. We simulate "a process holding the binary open" with a kept file
// descriptor and assert it still reads the OLD bytes after the swap.
func TestAtomicReplaceKeepsOldInode(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "agent-relay")
	if err := os.WriteFile(dst, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A live process (e.g. the stdio MCP Claude spawned) holds the binary open.
	held, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	src := filepath.Join(dir, "new")
	if err := os.WriteFile(src, []byte("NEW-BINARY"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := atomicReplace(src, dst); err != nil {
		t.Fatalf("atomicReplace: %v", err)
	}

	// The held fd must still see the OLD inode — the running MCP survives untouched.
	got, err := io.ReadAll(held)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "OLD-BINARY" {
		t.Errorf("held fd should still read the old inode, got %q", got)
	}

	// The path now resolves to the NEW binary, with exec perms restored.
	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != "NEW-BINARY" {
		t.Errorf("dst should be the new binary, got %q", onDisk)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("new binary perms = %v, want 0755", st.Mode().Perm())
	}

	// No staging temp left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == "" && len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Errorf("leftover staging temp: %s", e.Name())
		}
	}
}

func TestWaitPortFreeReturnsWhenFree(t *testing.T) {
	// Nothing listening on this port → waitPortFree must return well within the
	// timeout (not block for the full duration).
	start := time.Now()
	waitPortFree("1", 2*time.Second) // port 1 is privileged/unused → dial refused
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitPortFree blocked %v on a free port, expected near-instant", elapsed)
	}
}

func TestEnvFlag(t *testing.T) {
	const k = "AGENT_RELAY_TEST_FLAG"
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false}, {"0", false}, {"false", false}, {"no", false},
		{"1", true}, {"true", true}, {"yes", true}, {"on", true},
	} {
		t.Setenv(k, tc.val)
		if got := envFlag(k); got != tc.want {
			t.Errorf("envFlag(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}
