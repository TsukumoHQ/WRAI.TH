---
name: review-agent-runtime
description: Domain Q&A gate for the wrai.th relay runtime — the Go/MCP/SQLite core that is the shared coordination backbone for the whole agent fleet. Routes on a diff touching main.go, serve_lock_*.go, embed_hooks.go, or internal/ (db, relay, ingest, cli, config, models). A regression here does not fail one caller — it silently blinds every agent at once, so the checklist leads with data-integrity and concurrency, not style.
paths: main.go, main_test.go, embed_hooks.go, serve_lock_unix.go, serve_lock_windows.go, internal/, go.mod, go.sum
---

# review-agent-runtime — the fleet-backbone gate

This runtime is a **single-writer SQLite relay serving many concurrent agents over MCP**. The failure mode that matters is not a crash — a crash is loud. It is a change that *silently* drops a message, empties a result set, double-claims a task, or opens the API to the network. Review in that order: integrity first, availability second, boundary third, surface last. Only assert an item the diff actually touches.

Mechanical checks (build with `-tags fts5`, `go vet`, `gofmt`, `golangci-lint`, the test suite) are the CI gate's job, not this document's. Every item below is a judgment call CI cannot make.

## 1. Schema & scan lockstep — the single most destructive silent bug
- [ ] **`agentColumns` and `scanAgent` change together or not at all.** `agents.go` builds every agent SELECT from the `agentColumns` const (16 columns today) and unpacks it in `scanAgent`. Add a column to one string without the other and the scan arity mismatches — `Query` errors are often swallowed and the call returns **zero rows**, so a live fleet reads as empty. If the diff edits either, re-count both by hand; a passing build does not catch a column that scans into the wrong field.
- [ ] **Wide-table additions go through `ensureColumns` + a dedicated query, never by widening `agentColumns`.** Widening the shared column list re-arities every existing query at once; an isolated column + its own reader can't drop unrelated result sets.
- [ ] **Migrations are additive and idempotent** (`ensureColumns`, `CREATE … IF NOT EXISTS`), never a destructive `ALTER`/`DROP`. A rolling deploy runs an old binary against a new DB and a new binary against an old DB in the same hour; both must work, because the fleet's members update at their own pace.
- [ ] **A read of a new column tolerates its absence** on an older DB — don't `SELECT new_col` on a path that can run before its `ensureColumns` did, or the whole query fails for legacy databases.

## 2. Concurrency — TOCTOU, double-claim, races
- [ ] **State transitions use a guarded conditional UPDATE, not SELECT-then-UPDATE.** Task claim/start/review/complete must be `UPDATE … WHERE status = <expected>` with a `RowsAffected() == 0` check (the pattern already in `tasks.go`). Two agents polling the same pending task will both pass a prior `SELECT`; only the row-count guard lets exactly one win. A SELECT-then-UPDATE here is a double-dispatch waiting to happen.
- [ ] **Re-emit / retry paths are idempotent.** A dispatch or notification that can fire twice (restart, reconfigure, webhook redelivery) must not produce two `task.dispatched` events — a duplicated P0 wakes the whole fleet twice. Guard on a dedup key or a state precondition, because at-least-once delivery is the default and exactly-once is something you build.
- [ ] **Shared in-memory state (registry, ingest Detector, SSE bus) is mutex-guarded and lock scopes stay tiny.** These are read/written from HTTP handlers and background goroutines concurrently; an unguarded map write is a `fatal error: concurrent map` that takes the whole relay down — and with it every agent's coordination.

## 3. Single-writer & availability — the SSOT must not go silent
- [ ] **No second writer, ever.** The writer pool is `SetMaxOpenConns(1)` and the process guards on an flock serve-lock (`dbPath+.lock`, `serve_lock_unix.go`); reads go through `d.ro()` (the RO pool). A change that opens a second writable handle, or drops the lock guard, reintroduces the exact bug that once wiped agents+teams when a stray launchd relay came up on the same file. WAL + `_txlock=immediate` are load-bearing — don't alter the DSN casually.
- [ ] **No new per-request SQLite write on a hot path.** Contention here is transaction *frequency*, not row locks: activity stays in-memory (ingest → Detector, never persisted), token usage batches, ingest handlers are fire-and-forget. A synchronous write added to a per-tool-call or per-message path serializes the whole fleet behind one connection.
- [ ] **Fallible DB reads handle both error and empty without panicking.** A `nil`-deref on a missing row, or a helper that returns `nil, nil` and gets dereferenced upstream, turns a benign miss into a handler crash. The inbox invariant specifically: a fetch is a **non-destructive peek** (`unread = not acknowledged`) — never gate unread on `state='queued'` and never let a read consume, or messages vanish before they're seen.

## 4. Network & auth boundary
- [ ] **A non-loopback bind without a key refuses to start** (`startServer` `isLoopbackHost` check) — the whole API/MCP surface is otherwise open to the LAN. If the diff adds a bind path or a config source for the address, that refusal must still fire.
- [ ] **The loopback auth exemption keys on the real TCP peer (`r.RemoteAddr`), never `X-Forwarded-For`.** A forwarded header is attacker-controlled; trusting it lets a remote caller spoof loopback and skip auth entirely. Keys are compared with `crypto/subtle.ConstantTimeCompare` — a plain `==` on the token is a timing oracle.
- [ ] **New routes sit behind the existing middleware order** (CORS → rate-limit → body-limit → auth) and respect the permission model (broadcast admin-gated, team/conversation membership checked, cross-project DM exec-gated). A handler registered outside the chain is an unauthenticated hole.

## 5. MCP / API surface consistency
- [ ] **Every tool comes from the one `toolRegistry()` in `toolset.go`** (64 entries today) so stdio `mcp`, HTTP `/mcp`, and `discover_tools`/`call_tool` expose an identical set. A tool wired directly into a handler but not the registry is invisible in discovery mode and drifts the two transports apart.
- [ ] **No tool or route bypasses identity resolution** (`resolveProject`/`resolveAgent`). A handler that trusts a caller-supplied `as`/`project` without resolving it lets one agent act as another across the shared board.
- [ ] **New contract fields are additive with safe non-NULL defaults.** The typed-ticket work (`goal`, `acceptance_criteria`, `dod`) stores `""`/`[]`/`""` when omitted and is returned by `get_task`/`list_tasks` — a client that ignores the fields is unaffected. Follow that shape: never make an existing client's omitted field a hard error or a NULL that a scan chokes on.

## 6. Process lifecycle & restart safety
- [ ] **Self-update replaces the binary by atomic rename** (`<dst>.new` → chmod → `os.Rename`), never `cp`/`install`/`O_TRUNC` in place — truncating a running binary SIGBUSes a live `agent-relay mcp` stdio pipe and kills the client's connection mid-session. Auto-fired updates stage rather than hot-restart a live stdio MCP.
- [ ] **Shutdown drains SSE.** `Shutdown` must cancel long-lived streams (the `shutdownCtx` path) or `http.Server.Shutdown` hangs on an open subscriber until the 10s cap — and a bind that fails on `EADDRINUSE` must exit non-zero, not sit idle while a stale process serves stale UI.

## Verdict (paste into the review)
```
review-agent-runtime: PASS | PASS-WITH-NITS | BLOCK
Scope: <files/areas>
BLOCKERS: <none, or file:line — invariant broken + why it silently harms the fleet + the fix>
NITS: <non-blocking>
```
