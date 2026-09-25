# [release] v1.22.0 release notes in CHANGELOG.md (v1.21.1..main)

## Team : wraith-engine (tsukumo)
## Branch : wraith/engine-283b6f16-release-1.22 (from main)
## Relay task : 283b6f16-833c-4047-8c59-5f2a3e797e24
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 CHANGELOG.md starts (after the header) with '## [1.22.0] — 2026-09-25', one headline sentence, then Added / Changed / Fixed lines; every one of the 17 commits in git log v1.21.1..main (or 18 with 044a4876) is cited by sha7 exactly once; no commit outside that range is cited.
- [ ] 2. AC2 Upgrade notes list each schema change with its real table/column name as found in internal/db/db.go (grep-verifiable) and say they are additive and applied on first start; the three MCP tool names match internal/relay/tools.go exactly.
- [ ] 3. AC3 section ends with the Full diff compare URL v1.21.1...v1.22.0; the 0.7.0 and older sections are byte-identical to main.

## 2. Root cause & decisions

ROOT_CAUSE: v1.21.1..main had no release notes. The previous r2 draft was rejected because the reviewer read the relay task ids in the mandated "(ticket8, sha7)" format as commit shas; that draft also predated 038d089 and 5e48a6e.
DECISION: fresh branch off main (cto-tsukumo 94fc21bd: the old branch wraith-backend/release-1.22 is not to be reused). Content: the r2 text from 79420f8, with every specific claim re-checked against the code, plus answer obligations slice A (044a4876, 038d089) in wraith-cto's wording (f956af6e), and slice B (a01d0b87, 5e48a6e), which merged while this was in the gate (rebased; 19 commits now). Also the 7 seeded norms and the answer_reply_age / answer_role_age settings. CHANGELOG.md only; the 0.7.0 and older sections are byte-identical.
Legend: in "(xxxxxxxx, yyyyyyy)" the first id is the relay TASK id (8 hex), the second is the commit sha7 from `git log v1.21.1..main --no-merges`.

## Section as it will appear in the GitHub release
## [1.22.0] — 2026-09-25

Obligations become first-class relay data: the ACK checker now runs on a norms/obligations engine with an escalation chain agents can see, discharge or decline, questions sent to agents become tracked obligations that escalate when unanswered, while messages keep their task links and reply chains after retention, and memory writes stop losing concurrent updates.

### Added
- Obligation tools for agents: `obligations_mine` lists what you owe (your profile pool plus tasks assigned to you, with deadline and escalation depth), `obligation_discharge` marks one fulfilled after the relay re-checks it, and `obligation_decline` hands it up the chain immediately with a reason class (`not_mine`, `cannot`, `blocked_by`, `duplicate`) (79f48b9e, d1361bf)
- `send_message` takes an optional `task_id`, stored as the message's task link; an unknown id or one that contradicts `metadata.task_id` is refused before anything is written, and so is a malformed `reply_to`. A well-formed `reply_to` whose parent is missing is still accepted, and the result now reports `reply_to_resolved` (fde40c25, b8f3d88)
- Messages are linked to their task at insert: `messages.task_id` is filled from `metadata.task_id` or inherited from the `reply_to` parent in the same project, and older rows are backfilled on start (da1945d5, c2051b3)
- Message tombstones: the retention purge leaves one content-free row per deleted message (ids, routing, priority, no subject or body) in the same transaction, so a purged id can still be told apart from a mistyped one (f53b2160, c6add48)
- New `policy` message type for standing doctrine: it never expires, is never dead-lettered or purged, and is shown in its own `policies` block at every boot until the agent acks it (adefc1f4, 0571352)
- `set_memory` takes an optional `based_on` (the memory id you read, auto-filled from your last `get_memory`). If someone wrote a newer version in between, both stay live as siblings, the result names the conflict, and the other author is told (1a031d43, da972fc)
- Answer obligations: a direct or team message sent with `action_required` `ask` or `decide` now opens one obligation per recipient, fulfilled automatically when that recipient replies (the reply chain is followed through purged messages too). Recipients see them in `obligations_mine` and can discharge or decline them. Broadcast asks and conversation messages are excluded, and only messages sent after the upgrade are covered (044a4876, 038d089)
- Unanswered questions escalate: an answer obligation still open after `answer_reply_age` (1 h) is marked missed and passes to the recipient's manager (their `reports_to`, else an active executive), and after a further `answer_role_age` (2 h) to the human, who is only ever the last step. Each step sends one P1 message quoting the question, as a reply to it, so answering that message counts; a late answer from the original recipient closes the steps opened for it. The original message is never touched (a01d0b87, 5e48a6e)
- Boot selection journal: `get_session_context` logs one `[budget]` line per boot with the candidate, selected and omitted message ids, so the inbox budget can be replayed from logs. The boot payload itself is unchanged (409fb2e8, 6b3441c)

### Changed
- The ACK checker runs on a new norms/obligations engine instead of hard-coded checks. Behaviour is identical to v1.21, proven by an equivalence test against the old checker (f77efe18, 266a026)
- ACK escalation is now a chain: a task left unclaimed notifies its dispatcher at 15 min, escalates at 45 min, goes to the dispatcher's manager (else an executive, else the founder) at 90 min, and reaches the human operator only at 4 h. Notices are stored messages instead of push-only, so an offline dispatcher still gets them, and a notify can no longer arrive after an escalation. Tasks already escalated before the upgrade send nothing new (6b4369f0, d215a0c)
- Integrity scan: a slow scan log line now names the slowest phase and check, `orphan_agent_profile` only flags active agents, and a task on a missing board with no re-home target is recorded once instead of being logged on every sweep (27a77033, 52a9abf)

### Fixed
- A task promoted from backlog, unblocked or reset no longer fires the whole ACK chain at once: the ACK clock restarts each time a task becomes pending (new `tasks.pending_since`) instead of counting from its original dispatch (c933b2f1, c5288ca)
- The ACK clock also restarts when a task goes back to pending through a watchdog requeue, an expired lease, or the deactivation of the agent holding it (58ece5e2, cef0f87)
- The ACK sanction log line again ends with the task age in minutes, lost when the sanction code was shared with `obligation_decline` (904e024d, 81ba213)
- A memory writer working from an old version no longer silently archives a newer concurrent write; a superseded version now gets its `valid_until` closed, and re-saving the same value with a new layer is no longer dropped (df33d619, c4956f8)
- Replies to a purged message keep their parent's trace and action instead of waking the recipient as a new task, and `reply_to` checks treat a tombstoned parent as resolved (93b6f1cf, 5845c2f)
- Inbox budget scoring: an unknown priority no longer scores as P0, an unparseable or future `created_at` no longer counts as the freshest, and agents without tags can reach a full score. `apply_budget` is still off by default, so this was latent (1f190795, 8978751)
- Test suite: the token-usage flusher is stopped before a test closes its database, which removes a data race on captured logs. No change in production (d230b752, 4cbaa4c)

### Upgrade notes
All schema changes are additive and applied automatically on first start. Older binaries ignore the new tables and column, so rolling back is safe.
- New table `message_tombstones` with index `idx_message_tombstones_reply`.
- New tables `norms` and `obligations` with indexes `idx_obligations_active`, `idx_obligations_bearer`, `idx_obligations_subject`. Seven norms are seeded if absent: `ack.notify`, `ack.escalate`, `ack.manager`, `ack.human`, `answer.reply`, `answer.role`, `answer.human`.
- New column `tasks.pending_since`, backfilled to `dispatched_at` for existing tasks.
- `messages.task_id` already existed; it is now written on every insert, and rows whose `metadata.task_id` names a task in the same project are backfilled on each start (a no-op after the first).
- New settings `ack_manager_age` (default 90 min) and `ack_human_age` (default 4 h), each clamped to 1 min to 48 h. The existing `ack_notify_age` and `ack_escalate_age` are unchanged.
- New settings `answer_reply_age` (default 60 min, clamped to 1 min to 24 h), the deadline recorded on each answer obligation, and `answer_role_age` (default 2 h, clamped to 1 min to 48 h), the time the manager gets before the question reaches the human.
- Only questions sent after the upgrade get answer obligations, so the first sweep sends nothing to the human for older messages.
- The first integrity scan after the upgrade closes open `orphan_agent_profile` rows that belong to inactive or deleted agents.

Full diff: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.21.1...v1.22.0

## Verification script (run from the repo root)
```bash
#!/bin/bash
# Release-notes check: every cited (task8, sha7) pair is real and the range is covered exactly once.
set -u
sec=$(sed -n '/^## \[1.22.0\]/,/^Full diff:/p' CHANGELOG.md)
pairs=$(printf '%s\n' "$sec" | grep -oE '\(([0-9a-f]{8}), ([0-9a-f]{7})\)' | tr -d '(),' )
cited=$(printf '%s\n' "$pairs" | awk '{print $2}' | sort)
range=$(git log v1.21.1..main --no-merges --format=%h --abbrev=7 | sort)
fail=0
while read -r task sha; do
  if ! git cat-file -e "$sha^{commit}" 2>/dev/null; then echo "FAIL $sha: not a commit"; fail=1; continue; fi
  trailer=$(git log -1 --format=%B "$sha" | sed -n 's/^Task: \([0-9a-f]\{8\}\).*/\1/p')
  if [ "$trailer" != "$task" ]; then echo "FAIL $sha: cited task $task, commit trailer Task: $trailer"; fail=1; else echo "ok   $sha exists, Task: $task matches"; fi
done <<< "$pairs"
dups=$(printf '%s\n' "$cited" | uniq -d)
[ -n "$dups" ] && { echo "FAIL cited more than once: $dups"; fail=1; }
missing=$(comm -13 <(printf '%s\n' "$cited" | uniq) <(printf '%s\n' "$range"))
extra=$(comm -23 <(printf '%s\n' "$cited" | uniq) <(printf '%s\n' "$range"))
[ -n "$missing" ] && { echo "FAIL in range but not cited: $missing"; fail=1; }
[ -n "$extra" ] && { echo "FAIL cited but not in v1.21.1..main: $extra"; fail=1; }
echo "cited=$(printf '%s\n' "$cited" | grep -c .) range=$(printf '%s\n' "$range" | grep -c .) dups=$(printf '%s' "$dups" | grep -c .) missing=$(printf '%s' "$missing" | grep -c .) extra=$(printf '%s' "$extra" | grep -c .)"
if diff -q <(sed -n '/^## \[0.7.0\]/,$p' CHANGELOG.md) <(git show main:CHANGELOG.md | sed -n '/^## \[0.7.0\]/,$p') >/dev/null; then echo "ok   0.7.0 and older sections byte-identical to main"; else echo "FAIL older sections differ"; fail=1; fi
for n in obligations_mine obligation_discharge obligation_decline; do grep -q "\"$n\"" internal/relay/tools.go && echo "ok   tool $n in internal/relay/tools.go" || { echo "FAIL tool $n"; fail=1; }; done
for n in message_tombstones idx_message_tombstones_reply norms obligations idx_obligations_active idx_obligations_bearer idx_obligations_subject pending_since ack_manager_age ack_human_age answer_reply_age answer_role_age answer.reply answer.role answer.human; do grep -q -- "$n" internal/db/db.go && echo "ok   $n in internal/db/db.go" || { echo "FAIL $n not in db.go"; fail=1; }; done
exit $fail
```

Parenthesised 8-hex tokens are relay task ids, not commits; 7-hex tokens are commit SHAs, verified below.

Output:
```
ok   d1361bf exists, Task: 79f48b9e matches
ok   b8f3d88 exists, Task: fde40c25 matches
ok   c2051b3 exists, Task: da1945d5 matches
ok   c6add48 exists, Task: f53b2160 matches
ok   0571352 exists, Task: adefc1f4 matches
ok   da972fc exists, Task: 1a031d43 matches
ok   038d089 exists, Task: 044a4876 matches
ok   5e48a6e exists, Task: a01d0b87 matches
ok   6b3441c exists, Task: 409fb2e8 matches
ok   266a026 exists, Task: f77efe18 matches
ok   d215a0c exists, Task: 6b4369f0 matches
ok   52a9abf exists, Task: 27a77033 matches
ok   c5288ca exists, Task: c933b2f1 matches
ok   cef0f87 exists, Task: 58ece5e2 matches
ok   81ba213 exists, Task: 904e024d matches
ok   c4956f8 exists, Task: df33d619 matches
ok   5845c2f exists, Task: 93b6f1cf matches
ok   8978751 exists, Task: 1f190795 matches
ok   4cbaa4c exists, Task: d230b752 matches
cited=19 range=19 dups=0 missing=0 extra=0
ok   0.7.0 and older sections byte-identical to main
ok   tool obligations_mine in internal/relay/tools.go
ok   tool obligation_discharge in internal/relay/tools.go
ok   tool obligation_decline in internal/relay/tools.go
ok   message_tombstones in internal/db/db.go
ok   idx_message_tombstones_reply in internal/db/db.go
ok   norms in internal/db/db.go
ok   obligations in internal/db/db.go
ok   idx_obligations_active in internal/db/db.go
ok   idx_obligations_bearer in internal/db/db.go
ok   idx_obligations_subject in internal/db/db.go
ok   pending_since in internal/db/db.go
ok   ack_manager_age in internal/db/db.go
ok   ack_human_age in internal/db/db.go
ok   answer_reply_age in internal/db/db.go
ok   answer_role_age in internal/db/db.go
ok   answer.reply in internal/db/db.go
ok   answer.role in internal/db/db.go
ok   answer.human in internal/db/db.go
```

## review-wraith verdict: SHIP
Scope: CHANGELOG.md only (0 lines removed vs main).
Gate: go build -tags fts5 ./... OK (verify_cmd). No code change.
Checks: every cited sha7 is a commit in v1.21.1..main, cited once, and its Task: trailer matches the cited task id (19/19). Tool names match internal/relay/tools.go; every table, index, column, setting and norm name matches internal/db/db.go. messages.task_id is described as existing (it was in v1.21.1), not new. The install/upgrade line is the README's. No tag, no release, no deploy by me.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- Any commit that lands on main before the tag needs a line here; the script above reports it as "in range but not cited".

## 3. Files changed

```
CHANGELOG.md                                       |  42 ++++++
 ...0-release-notes-in-changelog-md-v1-21-1-main.md | 165 +++++++++++++++++++++
 2 files changed, 207 insertions(+)
```

## 4. QA Log

### Round 2 — ❌ REJECTED by review-283b6f16-833c-4047-8c59-5f2a3e797e24
- 🔴 AC1: fabricated sha7s + missing real commit — evidence: git log v1.21.1..main = 18 commits; CHANGELOG cites 17 of 18 (missing 038d089 slice A create obligation) AND cites 17 sha7s that do not exist anywhere in the repo (79f48b9e,fde40c25,da1945d5,f53b2160,adefc1f4,1a031d43,409fb2e8,f77efe18,6b4369f0,27a77033,c933b2f1,58ece5e2,904e024d,df33d619,93b6f1cf,1f190795,d230b752) — fabrication violates no-commit-outside-range
- 🟢 AC2: schema + tool names match — evidence: grep db.go:727,741,747,792-794,983,990,801,815,818 confirm message_tombstones/norms/obligations tables + idx_* + tasks.pending_since + ack_manager_age/ack_human_age defaults+clamps; tools.go:492,501,512 confirm obligations_mine/obligation_discharge/obligation_decline — test: N/A — docs/schema-name verification, no runtime behavior
- 🟢 AC3: URL present, older sections byte-identical — evidence: CHANGELOG.md:42 has Full diff URL; diff <(sed -n /^## [0.7.0]/,$p CHANGELOG.md) vs origin/main = exit 0 — test: N/A — docs/structure verification

### Round 2 — ❌ REJECTED by human:cto-tsukumo

### Round 3 — ❌ REJECTED by review-283b6f16-833c-4047-8c59-5f2a3e797e24
- 🔴 AC1: 18 phantom SHAs fabricated/hallucinated; 1 real commit (5e48a6e slice B deadline breach) not cited. AC1 fails. — evidence: git rev-list --all | grep returns no match for 18 cited SHAs (79f48b9e, fde40c25, da1945d5, f53b2160, adefc1f4, 1a031d43, 044a4876, 409fb2e8, f77efe18, 6b4369f0, 27a77033, c933b2f1, 58ece5e2, 904e024d, df33d619, 93b6f1cf, 1f190795, d230b752); git log v1.21.1..origin/main | wc -l = 19 but CHANGELOG only cites 18 of those (5e48a6e slice B missing) — test: N/A — pure docs criterion; verification = sha7 resolves via git rev-list
- 🟢 AC2: All schema names + 3 MCP tool names match exactly; Upgrade notes correctly state additive + first-start auto-apply. — evidence: internal/db/db.go:727 message_tombstones, 747 norms, 770 obligations, 741 idx_message_tombstones_reply, 792-794 idx_obligations_active/bearer/subject, 1008 pending_since, 800/803/814/817/829/832/835 seven norms (ack.notify/escalate/manager/human, answer.reply/role/human). internal/relay/tools.go:492/501/512 obligations_mine/obligation_discharge/obligation_decline. Upgrade notes declare additive + auto-applied on first start. — test: N/A — pure docs criterion; verification = grep against db.go and tools.go
- 🟢 AC3: Section ends with compare URL; 0.7.0 and older sections byte-identical to main. — evidence: CHANGELOG.md:44 Full diff: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.21.1...v1.22.0; diff <(branch 0.7.0+ section) <(origin/main 0.7.0+ section) is empty (byte-identical). — test: N/A — pure docs criterion; verification = byte diff of 0.7.0+ sections

### Round 3 — ❌ REJECTED by human:cto-tsukumo

## 5. Timeline

- round 2 → **reject** (review-283b6f16-833c-4047-8c59-5f2a3e797e24)
- round 2 → **reject** (human:cto-tsukumo)
- round 3 → **reject** (review-283b6f16-833c-4047-8c59-5f2a3e797e24)
- round 3 → **reject** (human:cto-tsukumo)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `283b6f16-833c-4047-8c59-5f2a3e797e24`._
