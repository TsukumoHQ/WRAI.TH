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
