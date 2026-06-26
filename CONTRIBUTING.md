# Contributing to wrai.th

Thanks for your interest in contributing! Here's how to get started.

## Development setup

```bash
# Clone
git clone https://github.com/TsukumoHQ/WRAI.TH.git
cd WRAI.TH

# Build (requires Go 1.23+ and a C compiler for CGO/SQLite)
CGO_ENABLED=1 go build -tags fts5 -o agent-relay .

# Run
./agent-relay
# Open http://localhost:8090
```

## Project structure

```
internal/
  db/         SQLite models (agents, messages, memories, tasks, goals, vault)
  relay/      MCP handlers, REST API, SSE streams
  ingest/     Activity hook ingestion (fsnotify on ~/.pixel-office/events/)
  vault/      Obsidian vault watcher (fsnotify + FTS5 indexing)
  web/        Embedded static assets (canvas UI)
  models/     Shared structs
docs/         Embedded relay documentation (go:embed, indexed as _relay project)
skill/        /relay skill for Claude Code
install.sh    macOS/Linux installer
install.ps1   Windows installer
```

## How to contribute a new feature

### 1. Start with an issue

Before writing code, open a [Feature Request](https://github.com/TsukumoHQ/WRAI.TH/issues/new?template=feature.yml). Describe:
- The problem or friction you're hitting
- Your proposed solution
- Which component is affected (MCP tools, UI, memory, tasks, etc.)

This avoids duplicate work and lets us discuss the approach before you invest time.

### 2. Get the green light

Wait for a maintainer to respond. Small fixes and docs can skip this step — just open a PR directly. For anything that adds or changes MCP tools, API endpoints, or DB schema, we want to discuss first.

### 3. Fork and branch

```bash
# Fork on GitHub, then:
git clone https://github.com/YOUR-USERNAME/WRAI.TH.git
cd WRAI.TH
git checkout -b feat/your-feature-name
```

Branch naming:
- `feat/short-description` — new features
- `fix/short-description` — bug fixes
- `docs/short-description` — documentation

### 4. Develop

- Build often: `go build -tags fts5 .`
- Test with the relay running: `./agent-relay` then use MCP tools or the web UI
- Frontend changes are instant (embedded static files, just refresh the browser)
- Backend changes require a rebuild

### 5. Commit

Use [conventional commits](https://www.conventionalcommits.org/):

```
feat(memory): add TTL expiration for context-layer memories
fix(ui): planet click detection off by 10px on retina displays
docs: add vault auto-injection example to README
ci: add arm64 Linux test to installer workflow
```

Format: `type(scope): description` — scope is optional but helpful.

### 6. Open a PR

Push your branch and open a PR. The PR template asks for:
- **Summary** — what and why (1-3 bullet points)
- **Changes** — key files modified
- **Test plan** — how you verified it works

All PRs are **squash merged** into `main` for a clean history.

### 7. Review and merge

A maintainer will review. We aim for fast turnaround. Once approved, the PR is squash-merged and your branch is automatically deleted.

## What to work on

- Check [open issues](https://github.com/TsukumoHQ/WRAI.TH/issues) — look for `good first issue` labels
- Join the [Discord](https://discord.gg/QPq7qfbEk8) to discuss ideas
- The MCP tools were designed by agents themselves — if you use the relay and hit friction, that's a valid feature request

### Good first contributions

- Add tests (MCP handlers and REST API have coverage — extend it)
- Improve error messages in MCP tool handlers
- Add new pixel art planet biomes or robot variants
- Documentation improvements

## Code style

- Go standard formatting (`gofmt`)
- Keep it simple — the relay is a single binary with zero external dependencies beyond SQLite
- Frontend is vanilla JS (no framework, no build step) with canvas rendering
- Don't over-engineer — three similar lines beat a premature abstraction

## Branching strategy

```
main (protected)
  └── feat/your-feature   ← PR → squash merge into main
  └── fix/some-bug         ← PR → squash merge into main
```

- **`main`** is the only long-lived branch. It's always deployable.
- All work happens in short-lived feature branches forked from `main`.
- PRs are squash-merged — one commit per feature in `main`.
- No develop/staging branches. No release branches. Ship from `main`.

## Releases

Releases are tag-driven:

1. Maintainer tags `main` with `vX.Y.Z` (semver)
2. **Release workflow** runs: cross-compiles 5 binaries (darwin/arm64, darwin/amd64, linux/amd64, linux/arm64, windows/amd64) with `-tags fts5`
3. GitHub Release is created with all binaries + install scripts
4. **Test Installers workflow** triggers automatically: tests the one-liner on 7 environments (3 Linux distros, 2 macOS, Windows, source build)
5. Users get the update via `install.sh` / `install.ps1` (always pulls latest release)
