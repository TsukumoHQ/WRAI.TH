---
name: review-installation
description: Domain Q&A gate for the wrai.th installers — the curl-to-bash / iwr-to-PowerShell path that runs with a user's privileges on a machine that is not the author's, plus the Makefile, launchd/systemd service, and the install-smoke clean-room contract. Routes on a diff touching install.sh, install.ps1, install-smoke/, Makefile, com.agent-relay.plist, or scripts/. The checklist leads with supply-chain integrity and non-destructive idempotency, because the two ways an installer betrays a user are running tampered code and clobbering config they already had.
paths: install.sh, install.ps1, install-smoke/, Makefile, com.agent-relay.plist, scripts/
---

# review-installation — the runs-on-someone-else's-machine gate

`install.sh` is piped straight from `curl` into `bash` with the user's privileges. It downloads a binary, writes to `~/.claude/`, and installs an auto-start service. Every one of those is a way to do harm if it's wrong. Review integrity first, then whether it's safe to re-run and safe on config that already exists, then privilege and cross-platform parity.

Shell-quoting and lint nits (`shellcheck`, `PSScriptAnalyzer`) are the CI job. The install-smoke clean-room already asserts "CLI on PATH / `--version` exits 0 / skill file landed / hook registered." Don't re-check those here. Everything below is judgment the smoke test and linter can't make.

## 1. Supply-chain integrity — you are shipping code that runs as the user
- [ ] **A downloaded artifact is checksum-verified before it executes, and verification fails closed.** `install.sh` fetches `SHA256SUMS` and `die`s on mismatch — good. But it only *warns* when the sums file is absent ("older releases"): confirm any new release path still publishes sums, because a silent skip is a silent hole. **`install.ps1` currently downloads and `Expand-Archive`s the zip with no `Get-FileHash` check at all** — a diff that adds or moves the Windows download path must not widen that gap; an unverified binary from a hijacked release or MITM'd redirect runs with the user's rights.
- [ ] **The checksum is bound to the right filename, not just present somewhere in the sums file.** `grep -q "$got" SHA256SUMS` matches the hash anywhere in the file; prefer matching the `hash  filename` pair so a valid hash for a *different* artifact can't wave a swapped archive through.
- [ ] **`REPO` and every hard-coded URL point at the canonical source** (`TsukumoHQ/WRAI.TH`). A "fix" that repoints raw.githubusercontent or the releases host to a fork or a redirect that breaks silently is how the whole fleet installs the wrong binary. Downloads use `curl -fsSL` / `Invoke-WebRequest` over HTTPS — never plain HTTP, never `-k`/`--insecure`.

## 2. Idempotency & non-destructive to existing config
- [ ] **Re-running the installer is safe and converges — it never appends a duplicate.** The `settings.json` hook merge dedups by command string before appending (`already = any(... command == cmd ...)`); a change to how hooks are registered must preserve that check, or every re-run and every auto-update stacks another copy of six hooks and the user's Claude Code fires each event N times.
- [ ] **User-owned files are backed up before mutation and merged, never overwritten.** `settings.json` is `cp`'d to `.bak` first and merged key-by-key; `.mcp.json` is merged, not replaced. A diff that starts writing a whole file where it used to merge silently deletes the user's other hooks, other MCP servers, or their formatting. Confirm the merge still preserves unrelated keys.
- [ ] **The service definition is reinstalled cleanly, not double-loaded.** launchd path `bootout`s the old label before `bootstrap`; systemd `daemon-reload`s then `restart`s. Two live services on one DB is the single-writer-corruption bug from the runtime side — the installer must guarantee exactly one.

## 3. Fail-open vs fail-closed — the right choice per step
- [ ] **Optional enrichment fails open; the core install fails closed.** Hook download, skill copy, and project scan are best-effort (`|| true`, inline fallbacks) — correct, because a missing `jq` shouldn't abort the whole install. But the binary landing on PATH and its integrity check must be hard failures (`die`). A diff that makes a security or core step best-effort (or a cosmetic step fatal) has the polarity backwards.
- [ ] **A missing optional dependency degrades loudly, not silently-wrong.** Hooks need `jq`+`curl`; the settings merge needs `python3`. When absent, the installer must *tell the user the feature won't work and how to fix it* (it does today via `missing_optional`), not install a hook that exits 0 doing nothing while the dashboard shows a dead agent.

## 4. Privilege & footprint
- [ ] **`sudo` is used only for the one path that needs it, and only after trying without.** The Makefile and installer write to `/usr/local/bin` with `sudo` only when the dir isn't user-writable, falling back from a non-sudo `install` first. A blanket `sudo` on the whole script, or a `sudo` around a `curl | bash`, escalates the whole install — least privilege means the elevation wraps the single file copy, nothing more.
- [ ] **Temp dirs are `mktemp -d` and cleaned on every exit path**, including the failure `die`s. A left-behind world-readable tmpdir with a half-downloaded binary is both a leak and a confusing re-run state.

## 5. Cross-platform parity & reversibility
- [ ] **A behavior added to one installer is added to its sibling, or the divergence is deliberate and noted.** `install.sh` (macOS/Linux) and `install.ps1` (Windows) are two implementations of one contract; the smoke test runs the same asserts against each. A new flag, a new hook event, or a new safety check landing in only one leaves the other platform quietly weaker (the checksum gap in §1 is the standing example).
- [ ] **`--uninstall` reverses everything the install created** — binary, symlink, service (`bootout`+remove / `disable`+remove), skill, and the hook wiring. An install step with no matching uninstall step orphans state on the user's machine. Adding an install artifact means adding its removal.
- [ ] **PATH and service edits are idempotent too.** The Windows path append checks membership before adding; a service is stopped before it's redefined. Re-running must not produce a doubled PATH entry or a second scheduled task.

## Verdict (paste into the review)
```
review-installation: PASS | PASS-WITH-NITS | BLOCK
Scope: <files/areas>
BLOCKERS: <none, or file:line — integrity/idempotency/privilege risk + the concrete harm on a real user's machine + the fix>
NITS: <non-blocking>
```
