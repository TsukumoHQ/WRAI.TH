# `wake_agent`

> Wake a sleeping or inactive seed agent — and optionally launch a claude
> process under its canonical identity. Counterpart to `sleep_agent`.

Two modes, selected by the presence of `prompt` or `cycle` :

1. **DB-only wake** (no `prompt`, no `cycle`) — transition status to `active`, refresh `last_seen`, clear `deactivated_at`. Returns `{status, agent, rows_affected}`.
2. **Wake + execute** (`prompt` or `cycle` set) — same DB transition, then launch a claude process *under the seed identity* (e.g. `endurance`, NOT a generated child name). No row inserted in the `agents` table. Memories/messages produced during the cycle are signed under the canonical seed name. When the cycle ends (success / failure / TTL expiration), the agent is put back to sleep automatically. Returns `{status:"woken-and-executing", agent, mode}`.

Together with `sleep_agent` forms the wake/work/sleep cadence for cycle-driven agents — no ephemeral child rows accumulating in `agents`.

## Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `agent` | `string` | ✓ | Name of the agent to wake (the target seed, e.g. `endurance`). Distinct from `as`, which identifies the caller. |
| `as` | `string` |  | Act as this agent (caller identity, overrides connection default). Used for tracking — `wake_agent` never targets `as`, since a sleeping agent cannot call MCP tools. |
| `project` | `string` |  | Project namespace (overrides connection default). |
| `prompt` | `string` |  | **Mode 2 (legacy)** — raw prompt to run under the seed identity. Mutually exclusive with `cycle`. |
| `cycle` | `string` |  | **Mode 2 (Agent OS)** — cycle name from the `cycles` table ; the relay assembles identity + context from DB. Mutually exclusive with `prompt`. |
| `ttl` | `string` |  | Max execution time in Go duration format (`5m`, `1h`). Default from cycle TTL or `10m`. |
| `allowed_tools` | `string` |  | Comma-separated tool allowlist for the claude process. |

## Example calls

**Mode 1 — DB-only wake:**

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "wake_agent",
    "arguments": { "agent": "endurance", "as": "user" }
  }
}
```

Returns:
```json
{ "status": "awake", "agent": "endurance", "rows_affected": 1 }
```

**Mode 2 — Wake + execute (Agent OS):**

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "wake_agent",
    "arguments": {
      "agent": "endurance",
      "cycle": "endurance-weekly-analysis",
      "ttl": "5m",
      "as": "planificateur"
    }
  }
}
```

Returns immediately :
```json
{ "status": "woken-and-executing", "agent": "endurance", "mode": "agent-os" }
```

The cycle runs asynchronously ; memories and messages produced during it are signed `endurance`. When it ends, the agent transitions back to `sleeping`.

## State machine

```
                                wake_agent (mode 1, DB-only)
sleeping ────────────────────────────────────────────────────► active
inactive ────────────────────────────────────────────────────► active   (deactivated_at cleared)
active   ────────────────────────────────────────────────────► active   (no-op, rows_affected=0)
deleted  ────────────────────────────────────────────────────► deleted  (no-op — wake does not resurrect deleted agents)

                                wake_agent (mode 2, with prompt or cycle)
sleeping/inactive ──► active ──► [claude runs as <agent>] ──► sleeping
                                  (no row created in agents)
```

## Key behavioural guarantees

- **No new row** in `agents` is ever inserted by `wake_agent`. It either updates an existing row or no-ops.
- **Identity propagation** : the claude process is instructed to `Pass as: "<agent>"` on every MCP call, exactly as scheduled cycles already do (see `executeCycle` in `internal/spawn/manager.go`). Memories and messages produced during the cycle are attributed to the seed, not to a child hash.
- **Automatic re-sleep** in mode 2 : after the cycle ends, the agent is set back to `sleeping`. The seed remains the persistent identity ; only its `status` and `last_seen` oscillate.
- **Idempotent in mode 1** : `rows_affected = 0` on already-active or missing target is not an error.
