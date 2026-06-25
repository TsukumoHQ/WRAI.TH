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
	repo         = "Synergix-lab/WRAI.TH"
	releaseAPI   = "https://api.github.com/repos/" + repo + "/releases/latest"
	serviceLabel = "com.agent-relay"
	binaryName   = "agent-relay"
)

func runUpdate(args []string) {
	force := false
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
		}
		if a == "--help" || a == "-h" {
			fmt.Print(`usage: agent-relay update [--force]

Check for updates and install the latest version.

flags:
  -f, --force   Update even if already on latest version
  -h, --help    Show this help
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

	// 6. Restart service
	restartService()

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
	fmt.Printf("skill %d/2, hooks %d/%d\n", skillOK, hookOK, len(hooks))
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

func installBinary(src, dst string) bool {
	// Try direct copy first
	cmd := exec.Command("install", "-m", "755", src, dst)
	if err := cmd.Run(); err != nil {
		// Try with sudo
		cmd = exec.Command("sudo", "install", "-m", "755", src, dst)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  error: could not install binary to %s: %v\n", dst, err)
			return false
		}
	}
	return true
}

func restartService() {
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
		// Small delay for clean shutdown
		time.Sleep(500 * time.Millisecond)
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
