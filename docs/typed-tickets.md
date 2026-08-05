# Typed tickets at dispatch

The V-lifecycle's left branch: a task is born with the artifact its verification
phase will check. A dispatch may carry a **typed ticket** — `goal`,
`acceptance_criteria`, `dod` — and on projects that enforce it, an incomplete
ticket is refused at the relay.

## The three fields

| field | type | meaning |
|-------|------|---------|
| `goal` | string | one-line intent — *why* this task exists |
| `acceptance_criteria` | JSON array of strings | the individually testable items the review gate verdicts against (the right branch of the V) |
| `dod` | string | definition of done — the merge bar |

All three persist on the task and are returned by `get_task` / `list_tasks`
(additive fields; clients that ignore them are unaffected). A dispatch that
omits them stores safe defaults (`""`, `[]`, `""`) — never NULL.

## Wire format

`dispatch_task` (MCP) — each field is a top-level argument; `acceptance_criteria`
is a **JSON-array string**:

```
dispatch_task({
  project: "niwa", profile: "backend-lead-3", title: "…",
  goal: "determinise task birth",
  acceptance_criteria: "[\"refuses without goal\",\"get_task renders the fields\"]",
  dod: "cargo test -p agentd green"
})
```

`batch_dispatch_tasks` — inside the `tasks` JSON array, `acceptance_criteria` is
a **real JSON array** (not a string):

```
batch_dispatch_tasks({ project: "niwa", tasks: "[
  {\"profile\":\"dev\",\"title\":\"…\",\"goal\":\"…\",
   \"acceptance_criteria\":[\"a\",\"b\"],\"dod\":\"…\"}
]" })
```

The HTTP `POST /tasks` path mirrors the batch shape: `acceptance_criteria` is a
JSON array in the request body.

## Enforcement — per project, default off

The relay serves many projects; a global hard refusal would break every
free-form dispatcher. Enforcement is a per-project flag,
`projects.require_typed_ticket` (default `0`). `niwa` is seeded **on**; flip
others with `SetProjectRequiresTypedTicket(project, true)`.

When a project enforces tickets:

- **`dispatch_task`** with a missing `goal`, `acceptance_criteria`, or `dod` is
  refused; the error names the missing fields, e.g.
  `typed ticket required for project 'niwa': missing [goal, dod]. …`
- **`batch_dispatch_tasks`** applies the same rule **per item** — an incomplete
  item lands in the response `errors` array while the complete items dispatch
  (not all-or-nothing).

A field counts as *present* only when non-blank; `acceptance_criteria` counts
only when it is a JSON array with at least one non-blank item (`[]` or
`["  "]` is treated as missing — an empty checklist verifies nothing).

Projects **without** the flag dispatch exactly as before — the three fields are
optional and default to empty.

## Linear-origin tasks

Enforcement is uniform: a task **born in Linear** (mirrored from an issue) is
held to the same bar. There are no Linear custom fields — the ticket lives in
the **issue description** as markdown sections, so it is portable, human-visible
and needs zero per-workspace config:

```
## Goal
<one-line intent>

## Acceptance Criteria
- <testable item>
- <testable item>

## DoD
<definition of done>
```

Headers are case-insensitive and any depth works (`#`, `##`, `### `); `Definition
of Done` is accepted for `DoD`. Acceptance Criteria must carry at least one
bullet (`-`, `*`, `+`, or `1.`).

On a project with `require_typed_ticket` on:

- **Conforming issue** → mirrored and dispatched as normal, and the parsed
  goal / acceptance_criteria / dod are written onto the mirror row (so the
  review gate verdicts a Linear task per requirement, same as a relay dispatch).
- **Non-conforming issue** → **refused**: the mirror is persisted as a
  **`refused` row** — never dispatched, never surfaced as a pending/active task —
  and a **loud comment is posted back on the Linear issue** naming the missing
  sections and pointing here. It is never a silent relay log — otherwise the
  executive believes the task dispatched and the work dies in the void. An
  already-mirrored task **in flight** (accepted / in-progress / in-review /
  blocked / terminal) is never retro-refused.
- **Flag off** → Linear sync is unchanged (a non-conforming issue mirrors as a
  plain row, no comment).

The webhook→issue comment is the one Linear write allowed outside an executive
(agents never touch Linear; they go through the relay).

### Refused rows — states and the anti-spam marker

An issue can arrive by **two** paths: the webhook (real-time) and the reconcile
**poll** (heals missed webhooks, and is the only path on a webhook-less localhost
relay). Both run refusal through one mechanism so no issue dies silently and
neither path spams:

- The refused mirror carries status **`refused`** and a **`refusal_notified_at`**
  marker. The loud comment fires **exactly once** — stamped by the marker — no
  matter how many webhook re-deliveries or poll cycles see the same
  non-conforming issue. (A non-agent issue was never dispatchable, so it is not
  refused on either path.)
- **Becomes conforming** → the row flips out of `refused` to the normal Linear
  status, the marker is **reset**, and it dispatches on its next started state —
  exactly like any conforming issue.
- **Regresses to non-conforming** while still refusable (a fresh, unstarted
  backlog issue) → because the marker was reset, it **re-notifies once**. Work
  already in flight is left untouched.

State flow: `refused ⇄ (normal mirror)` — the marker is set on entering
`refused`, cleared on leaving it.
