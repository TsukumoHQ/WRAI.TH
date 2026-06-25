#!/bin/bash
# SessionStart → relay identity bind. Fires on startup|resume|clear|compact.
# Claude Code rotates session_id on /clear; the worktree cwd is stable, so the
# relay re-binds session_id→agent by cwd and returns additionalContext that
# re-injects the agent's identity into the fresh session. Synchronous (we need
# the response) but short-timeout and fail-silent: relay down → no context, no
# disruption.
RELAY_URL="${RELAY_URL:-http://localhost:8090}"
INPUT=$(cat)
command -v jq >/dev/null 2>&1 || exit 0
SID=$(printf '%s' "$INPUT" | jq -r '.session_id // ""')
CWD=$(printf '%s' "$INPUT" | jq -r '.cwd // ""')
SRC=$(printf '%s' "$INPUT" | jq -r '.source // ""')
TP=$(printf '%s' "$INPUT" | jq -r '.transcript_path // ""')
[ -z "$SID" ] && exit 0
PAYLOAD=$(jq -nc --arg s "$SID" --arg c "$CWD" --arg src "$SRC" --arg tp "$TP" \
  '{session_id:$s, cwd:$c, source:$src, transcript_path:$tp}')
RESP=$(curl -fsS -m 3 -X POST "$RELAY_URL/api/ingest/session-start" \
  ${RELAY_API_KEY:+-H "Authorization: Bearer $RELAY_API_KEY"} \
  -H "Content-Type: application/json" -d "$PAYLOAD" 2>/dev/null)
CTX=$(printf '%s' "$RESP" | jq -r '.additionalContext // ""' 2>/dev/null)
[ -z "$CTX" ] && exit 0
jq -nc --arg c "$CTX" \
  '{hookSpecificOutput:{hookEventName:"SessionStart", additionalContext:$c}}'
exit 0
