# [ingest/usage] the Stop hook counts each assistant message's tokens once, for Claude and Codex transcripts

## Team : wraith-engine (tsukumo)
## Branch : wraith/c627b758-usage-dedupe (from main)
## Relay task : c627b758-5487-43e6-976b-00570bcd858a
## Trace : trace=3d13b813611730e30347a9546b0bad54
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: a Claude transcript where one message spans N content-block lines contributes that message's usage exactly once
- [ ] 2. test: a Codex rollout transcript ingests the sum of its token_count events' last_token_usage (non-zero)
- [ ] 3. test: an unknown transcript shape ingests 0 and logs one warning line, never errors the hook
- [ ] 4. submit note: before/after totals on one real local session of each engine

## 2. Root cause & decisions

# c627b758 — the Stop hook counts each assistant message's tokens once, for Claude and Codex

ROOT_CAUSE: skill/hooks/ingest-stop.sh summed .message.usage on EVERY new transcript line. Claude Code writes one line per content block (thinking / text / tool_use), each repeating the same message.usage, so a 3-block message was counted 3x (real session 0b4b2dc8: 171 assistant lines / 93 messages). Codex rollouts never carry .message.usage (usage lives in event_msg token_count .payload.info.last_token_usage), so every Codex session ingested 0 silently.

FIX: one jq pass detects the chunk format. Codex (any line with an object payload and type session_meta/turn_context/event_msg/response_item): sum token_count last_token_usage; input = input_tokens - cached_input_tokens, cache_read = cached_input_tokens, cache_creation = cache_write_input_tokens, output = output_tokens (reasoning NOT added: total_tokens = input + output on real rollouts, so output already holds it); model = last turn_context.payload.model. token_usage_record lines (Codex 0.155, same usage again) and token_count with info:null are skipped. Claude: usage keyed by message.id (line index fallback), last wins. Anything else (or unparsable JSON): no tokens POST, one stderr line "unrecognised transcript format", exit 0. POST body shape unchanged; no relay/server change, no schema change.

BEFORE/AFTER (real local sessions, full transcript):
  Claude 0b4b2dc8 (769 lines): before in 340 / out 95533 / cache_read 21859812 / cache_creation 441220
                               after  in 184 / out 40750 / cache_read 12110850 / cache_creation 295257
                               (Claude's own cost-state for the session: out 41633, cache_read 14252164; it also counts subagent calls not in this file)
  Codex 01a10241 (431 lines):  before all 0, model ""
                               after  in 109301 / out 4976 / cache_read 1773440 / model gpt-6.1-sol
                               == Codex's own final total_token_usage (input 1882741 = 109301 + 1773440, output 4976)

NOT DONE: RELAY_ENGINE override, mapper/hooks install --engine (T1 AC2-5 of the G5 research) — not in this ticket's ACs. UNCERTAIN: not run live under a Codex Stop hook (stdin session_id/transcript_path shape assumed same as Claude's; ~/.codex/hooks.json already calls this script).

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/ingest -run TestStopHook -count=1
  test_sha: d828b07
  output: --- FAIL: TestStopHook_ClaudeCountsEachMessageOnce: tokens = {Input:38 Output:6279 CacheRead:1146348 CacheCreation:103218}, want {Input:18 Output:2425 CacheRead:535654 CacheCreation:48054}
          --- FAIL: TestStopHook_CodexSumsTokenCountEvents: tokens = {Input:0 Output:0 CacheRead:0 Model:}, want {Input:19128 Output:390 CacheRead:103040 Model:gpt-6.1-sol}
          --- FAIL: TestStopHook_UnknownShapeIngestsZeroAndWarnsOnce: unknown transcript must ingest nothing, got [{Input:0 Output:0 ...}]

## review-wraith verdict: SHIP
Scope: skill/hooks/ingest-stop.sh (embedded via embed_hooks.go, shipped by hooks install / update), internal/ingest/stop_hook_test.go, internal/ingest/testdata/{claude-multiblock,codex-rollout,unknown-shape}.jsonl
Gate: vet -tags fts5 OK / gofmt OK / bash -n OK / test -tags fts5 ./... OK (on 16a0a4c)
BLOCKERS: none
NITS: a chunk with one malformed JSON line (e.g. a partial line mid-write) still drops the whole chunk as before; it now warns instead of failing silently. install.sh's inline fallback ingest-stop.sh (activity only) and install.ps1 have no token logic, untouched. Fixtures: content and session ids stripped, Codex token_usage_record thread/response ids dropped.
Migration note: none.

## 3. Files changed

```
internal/ingest/stop_hook_test.go                | 135 +++++++++++++++++++++++
 internal/ingest/testdata/claude-multiblock.jsonl |  30 +++++
 internal/ingest/testdata/codex-rollout.jsonl     |  41 +++++++
 internal/ingest/testdata/unknown-shape.jsonl     |   2 +
 skill/hooks/ingest-stop.sh                       |  50 +++++++--
 5 files changed, 248 insertions(+), 10 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c627b758-5487-43e6-976b-00570bcd858a`._
