package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
}

const (
	repo         = "TsukumoHQ/WRAI.TH"
	releaseAPI   = "https://api.github.com/repos/" + repo + "/releases/latest"
	serviceLabel = "com.agent-relay"
	binaryName   = "agent-relay"
)

func runUpdate(args []string) {
	force := false
	// noRestart stages the new binary (atomic swap) WITHOUT restarting the
	// service. The new binary applies on the next launch. This is the safe
	// default for an auto-update fired mid-session by a release-watcher: it
	// must never kill a live process (see installBinary / TSU-74).
	noRestart := envFlag("AGENT_RELAY_NO_SELF_RESTART")
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
		}
		if a == "--no-restart" || a == "--stage" {
			noRestart = true
		}
		if a == "--help" || a == "-h" {
			fmt.Print(`usage: agent-relay update [--force] [--no-restart]

Check for updates and install the latest version. The binary is replaced
atomically (rename, never in-place truncate), so a process already running the
old binary — e.g. a live ` + "`agent-relay mcp`" + ` stdio pipe Claude Code spawned —
keeps running uninterrupted; the new binary applies on its next launch.

flags:
  -f, --force        Update even if already on latest version
      --no-restart   Stage only: swap the binary but do NOT restart the service
                     (alias: --stage). Also set via AGENT_RELAY_NO_SELF_RESTART.
                     Use this for auto-update fired while an interactive MCP is live.
  -h, --help         Show this help
`)
			return
		}
	}

	// 1. Get current version
	currentVersion := getCurrentVersion()
	fmt.Printf("  current: %s\n", currentVersion)

	// 1b. Refuse to update a dev/unknown build — would overwrite local work
	if !force && isDevBuild(currentVersion) {
		fmt.Println("\n  dev build detected — skipping auto-update to avoid overwriting local work")
		fmt.Println("  to rebuild from current source: make build")
		fmt.Println("  to force download of a release: agent-relay update --force")
		return
	}

	// 2. Get latest version from GitHub
	fmt.Print("  checking latest release... ")
	latestVersion, err := getLatestVersion()
	if err != nil {
		fmt.Printf("\nerror: could not check latest version: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s\n", latestVersion)

	// 3. Compare
	cmp := compareSemver(currentVersion, latestVersion)
	if !force && cmp == 0 {
		fmt.Println("\n  already up to date")
		return
	}
	if !force && cmp > 0 {
		fmt.Printf("\n  local version %s is ahead of latest release %s — nothing to update\n", currentVersion, latestVersion)
		fmt.Println("  use --force to install the release anyway (will downgrade)")
		return
	}

	if !force {
		fmt.Printf("\n  update available: %s → %s\n", currentVersion, latestVersion)
	}

	// 4. Find the binary path
	binPath, err := findBinaryPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  binary: %s\n", binPath)

	// 5. Try build from source, fallback to download
	if tryBuildUpdate(binPath, latestVersion) {
		fmt.Println("  built from source")
	} else if tryDownloadUpdate(binPath, latestVersion) {
		fmt.Println("  downloaded prebuilt binary")
	} else {
		fmt.Fprintln(os.Stderr, "error: update failed — could not build or download")
		os.Exit(1)
	}

	// 5b. Refresh the shipped skill + activity hooks (best-effort — never fails
	// the binary update). A binary-only swap would leave the /relay skill and the
	// ingest hooks stale on the user's machine.
	refreshSkillAndHooks(latestVersion)

	// 6. Restart service (skipped in staged mode — the swap already happened
	// atomically, so the new binary applies on the next launch with no live
	// process killed).
	restartService(noRestart)

	// 7. Verify
	fmt.Print("\n  verifying... ")
	newVersion := getCurrentVersion()
	fmt.Printf("%s\n", newVersion)
	fmt.Println("\n  update complete")
}

// isDevBuild returns true for versions produced from unclean source trees
// or binaries built without -ldflags. Auto-update would destroy local work.
func isDevBuild(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" || v == "unknown" {
		return true
	}
	return strings.Contains(v, "-dirty") || strings.Contains(v, "-g") && strings.Count(v, "-") >= 2
}

// compareSemver returns -1, 0, or 1 for a<b, a==b, a>b. Tolerates dev suffixes
// (treats "v0.5.0-5-g7eba408-dirty" as > "v0.5.0").
func compareSemver(a, b string) int {
	ai, adev := parseSemver(a)
	bi, bdev := parseSemver(b)
	for i := 0; i < 3; i++ {
		if ai[i] != bi[i] {
			if ai[i] < bi[i] {
				return -1
			}
			return 1
		}
	}
	// Equal base → dev suffix wins
	if adev && !bdev {
		return 1
	}
	if !adev && bdev {
		return -1
	}
	return 0
}

func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	dev := false
	if i := strings.Index(v, "-"); i >= 0 {
		dev = true
		v = v[:i]
	}
	parts := strings.SplitN(v, ".", 3)
	out := [3]int{0, 0, 0}
	for i := 0; i < len(parts) && i < 3; i++ {
		if n, err := strconv.Atoi(parts[i]); err == nil {
			out[i] = n
		}
	}
	return out, dev
}

func getCurrentVersion() string {
	out, err := exec.Command(binaryName, "--version").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	v := strings.TrimSpace(string(out))
	// Output is "agent-relay v0.3.1" → extract version
	if parts := strings.Fields(v); len(parts) >= 2 {
		return parts[len(parts)-1]
	}
	return v
}

func getLatestVersion() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(releaseAPI)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	if release.TagName == "" {
		return "", fmt.Errorf("no releases found")
	}
	return release.TagName, nil
}

func findBinaryPath() (string, error) {
	// Check if we're in the source repo
	if _, err := os.Stat("go.mod"); err == nil {
		if data, err := os.ReadFile("go.mod"); err == nil && strings.Contains(string(data), "agent-relay") {
			// We're in the source repo — build in place
			exe, _ := os.Executable()
			if exe != "" {
				return exe, nil
			}
		}
	}

	// Find installed binary
	path, err := exec.LookPath(binaryName)
	if err == nil {
		// Resolve symlinks
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return resolved, nil
		}
		return path, nil
	}

	// Common paths
	for _, p := range []string{
		"/usr/local/bin/" + binaryName,
		filepath.Join(os.Getenv("HOME"), ".local", "bin", binaryName),
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("cannot find %s binary", binaryName)
}

func tryBuildUpdate(binPath, version string) bool {
	// Need go and a C compiler
	if _, err := exec.LookPath("go"); err != nil {
		return false
	}
	hasCc := false
	for _, cc := range []string{"cc", "gcc", "clang"} {
		if _, err := exec.LookPath(cc); err == nil {
			hasCc = true
			break
		}
	}
	if !hasCc {
		return false
	}

	// Clone to temp dir
	tmpDir, err := os.MkdirTemp("", "agent-relay-update-*")
	if err != nil {
		return false
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	fmt.Print("  cloning... ")
	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", version,
		"https://github.com/"+repo+".git", filepath.Join(tmpDir, "src"))
	if err := cmd.Run(); err != nil {
		// Try without --branch (tag might not exist for dev)
		cmd = exec.Command("git", "clone", "--depth", "1",
			"https://github.com/"+repo+".git", filepath.Join(tmpDir, "src"))
		if err := cmd.Run(); err != nil {
			fmt.Println("failed")
			return false
		}
	}
	fmt.Println("ok")

	fmt.Print("  building... ")
	buildCmd := exec.Command("go", "build", "-tags", "fts5",
		"-ldflags", fmt.Sprintf("-s -w -X main.Version=%s", version),
		"-o", filepath.Join(tmpDir, binaryName), ".")
	buildCmd.Dir = filepath.Join(tmpDir, "src")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if err := buildCmd.Run(); err != nil {
		fmt.Println("failed")
		return false
	}
	fmt.Println("ok")

	// Replace binary
	return installBinary(filepath.Join(tmpDir, binaryName), binPath)
}

func tryDownloadUpdate(binPath, version string) bool {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	// Windows ships a .zip; every other platform a .tar.gz.
	ext := "tar.gz"
	if osName == "windows" {
		ext = "zip"
	}
	archiveName := fmt.Sprintf("agent-relay-%s-%s.%s", osName, arch, ext)
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, version)

	tmpDir, err := os.MkdirTemp("", "agent-relay-dl-*")
	if err != nil {
		return false
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	fmt.Print("  downloading... ")
	archivePath := filepath.Join(tmpDir, archiveName)
	if err := download(archivePath, base+"/"+archiveName); err != nil {
		fmt.Println("failed")
		return false
	}
	fmt.Println("ok")

	// Integrity: verify the archive against the release's SHA256SUMS before
	// extracting/installing. Fail closed on mismatch; only skip when the sums
	// file is absent (older releases that predate signing).
	sumsPath := filepath.Join(tmpDir, "SHA256SUMS")
	if err := download(sumsPath, base+"/SHA256SUMS"); err != nil {
		fmt.Println("  warning: SHA256SUMS not published for this release — skipping integrity check")
	} else {
		ok, err := verifyChecksum(archivePath, archiveName, sumsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error: checksum check failed: %v\n", err)
			return false
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "  error: checksum mismatch — refusing to install (possible tampering)")
			return false
		}
		fmt.Println("  checksum verified")
	}

	if err := extractArchive(archivePath, tmpDir); err != nil {
		fmt.Fprintf(os.Stderr, "  error: extract failed: %v\n", err)
		return false
	}

	extracted := filepath.Join(tmpDir, binaryName)
	if osName == "windows" {
		extracted += ".exe"
	}
	return installBinary(extracted, binPath)
}

// download fetches url to dst with curl (already a dependency of the updater).
func download(dst, url string) error {
	return exec.Command("curl", "-fsSL", "-o", dst, url).Run()
}

// refreshSkillAndHooks pulls the /relay skill docs and the activity-tracking
// hooks for the just-installed version and writes them to the user's Claude
// config (same layout as install.sh): skill → ~/.claude/commands/, hooks →
// ~/.claude/hooks/. Best-effort — a failure here never fails the binary update.
func refreshSkillAndHooks(version string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	base := "https://raw.githubusercontent.com/" + repo + "/" + version + "/skill/"
	fmt.Print("  refreshing skill + hooks... ")

	// Skill docs → ~/.claude/commands/
	cmdDir := filepath.Join(home, ".claude", "commands")
	_ = os.MkdirAll(cmdDir, 0o755)
	skillOK := 0
	for _, f := range []string{"relay.md", "tools-reference.md"} {
		if download(filepath.Join(cmdDir, f), base+f) == nil {
			skillOK++
		}
	}

	// Public end-user skill → ~/.claude/skills/agent-relay/SKILL.md. Baked into
	// the binary (const), so refresh it from there — no download, always current
	// with the just-installed binary.
	pubSkillOK := InstallPublicSkill(home) == nil

	// Activity hooks → ~/.claude/hooks/ (executable)
	hooksDir := filepath.Join(home, ".claude", "hooks")
	_ = os.MkdirAll(hooksDir, 0o755)
	hooks := []string{
		"ingest-pre-tool.sh", "ingest-post-tool.sh", "ingest-stop.sh",
		"ingest-subagent-start.sh", "ingest-subagent-stop.sh", "session-start.sh",
	}
	hookOK := 0
	for _, h := range hooks {
		dst := filepath.Join(hooksDir, h)
		if download(dst, base+"hooks/"+h) == nil {
			_ = os.Chmod(dst, 0o755)
			hookOK++
		}
	}

	// Re-wire settings.json so an update REPAIRS the hook wiring, not just the
	// scripts — a downloaded script that isn't referenced in settings.json never
	// fires (the partial-state bug that left last_seen/tokens dead). Idempotent;
	// skipped on Windows (bash hooks, .ps1 is a follow-up).
	wired := 0
	if runtime.GOOS != "windows" {
		settingsPath := filepath.Join(home, ".claude", "settings.json")
		wired, _ = mergeHookSettings(hooksDir, settingsPath)
	}
	pubStr := "ok"
	if !pubSkillOK {
		pubStr = "fail"
	}
	fmt.Printf("skill %d/2 (public %s), hooks %d/%d (wired %d new)\n", skillOK, pubStr, hookOK, len(hooks), wired)
}

// verifyChecksum compares the SHA-256 of file against the entry for name in a
// `sha256sum`-format SHA256SUMS file ("<hex>  <name>" per line).
func verifyChecksum(file, name, sumsPath string) (bool, error) {
	sums, err := os.Open(sumsPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = sums.Close() }()

	want := ""
	sc := bufio.NewScanner(sums)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && filepath.Base(fields[1]) == name {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return false, fmt.Errorf("no checksum entry for %s", name)
	}

	af, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer func() { _ = af.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, af); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// extractArchive extracts files from a .tar.gz or .zip into destDir using the
// Go stdlib — no external tar/unzip, which stock Windows lacks. Entry names are
// flattened to their base to prevent path traversal (zip-slip).
func extractArchive(archivePath, destDir string) error {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, destDir)
	}
	return extractTarGz(archivePath, destDir)
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := writeExtracted(filepath.Join(destDir, filepath.Base(hdr.Name)), tr); err != nil {
			return err
		}
	}
	return nil
}

func extractZip(archivePath, destDir string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for _, zf := range r.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeExtracted(filepath.Join(destDir, filepath.Base(zf.Name)), rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeExtracted(dst string, src io.Reader) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, src) //nolint:gosec // size-bounded by our own release archive
	return err
}

// installBinary atomically replaces dst with the new binary at src.
//
// It writes a sibling temp file then rename(2)s it over dst. Rename swaps the
// directory entry to a NEW inode; any process already executing the old binary
// (critically, a live `agent-relay mcp` stdio pipe Claude Code spawned) keeps
// running the old, now-unlinked inode and is never touched — the new binary
// applies only on its next launch. The previous implementation used
// `install -m 755` which truncates and rewrites the existing inode in place;
// that corrupts a running executable's text segment → SIGBUS → the live MCP
// pipe dies fleet-wide. That was the owner-reported TSU-74 footgun.
func installBinary(src, dst string) bool {
	if err := atomicReplace(src, dst); err == nil {
		return true
	}
	// Permission fallback (e.g. root-owned /usr/local/bin): stage a sibling temp
	// via sudo, then rename it over dst with `mv -f`. mv on the same filesystem
	// is a rename — same inode-swap guarantee, never an in-place truncate.
	staged := dst + ".new"
	if err := exec.Command("sudo", "install", "-m", "755", src, staged).Run(); err == nil {
		if err := exec.Command("sudo", "mv", "-f", staged, dst).Run(); err == nil {
			return true
		}
		_ = exec.Command("sudo", "rm", "-f", staged).Run()
	}
	fmt.Fprintf(os.Stderr, "  error: could not install binary to %s\n", dst)
	return false
}

// atomicReplace copies src to a sibling temp file in dst's directory, then
// renames it over dst. Both files must live on the same filesystem for rename(2)
// to be atomic — using dst's own directory guarantees that. Returns an error
// (rather than crashing) so installBinary can fall back to the sudo path.
func atomicReplace(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".agent-relay-stage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename; a no-op once renamed.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

func restartService(noRestart bool) {
	if noRestart {
		fmt.Println("  staged — not restarting (new binary applies on next launch)")
		return
	}
	fmt.Print("  restarting service... ")

	switch runtime.GOOS {
	case "darwin":
		plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", serviceLabel+".plist")
		if _, err := os.Stat(plist); err != nil {
			fmt.Println("no service found (manual start)")
			return
		}
		uid := os.Getuid()
		// Stop
		_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d", uid), plist).Run()
		// bootout is asynchronous: it returns before the old process has released
		// the listen socket. A fixed sleep races it — if the port is still held
		// when we bootstrap, the new process hits EADDRINUSE and log.Fatalf's,
		// leaving NO relay running (fleet-wide outage until a manual restart).
		// Wait for the port to actually free before starting the replacement.
		waitPortFree(servePort(), 5*time.Second)
		// Start
		if err := exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), plist).Run(); err != nil {
			_ = exec.Command("launchctl", "load", plist).Run()
		}
		fmt.Println("ok (launchd)")

	case "linux":
		// Check if systemd service exists
		if err := exec.Command("systemctl", "--user", "is-enabled", binaryName).Run(); err != nil {
			fmt.Println("no service found (manual start)")
			return
		}
		_ = exec.Command("systemctl", "--user", "restart", binaryName).Run()
		fmt.Println("ok (systemd)")

	default:
		fmt.Println("unsupported platform — restart manually")
	}
}

// envFlag reports whether an env var is set to a truthy value (anything but
// empty / "0" / "false").
func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "", "0", "false", "no":
		return false
	}
	return true
}

// servePort returns the port the relay serves on (PORT env, else the 8090
// default — same resolution as startServer in main.go).
func servePort() string {
	if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		return p
	}
	return "8090"
}

// waitPortFree blocks until nothing is listening on 127.0.0.1:port, or timeout
// elapses (best-effort — returns either way). A refused dial means the old
// listener has released the socket and a replacement can bind safely.
func waitPortFree(port string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return // refused/unreachable = port is free
		}
		_ = conn.Close()
		time.Sleep(150 * time.Millisecond)
	}
}
