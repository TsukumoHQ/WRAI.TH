#!/bin/bash
# PreToolUse → relay activity. POSTs to the relay instead of dropping a file, so
# it works when the relay is on another host. Fire-and-forget: backgrounded curl
# with a short timeout, always exit 0 so it never blocks or fails a tool call.
RELAY_URL="${RELAY_URL:-http://localhost:8090}"
INPUT=$(cat)
command -v jq >/dev/null 2>&1 || exit 0
SID=$(printf '%s' "$INPUT" | jq -r '.session_id // ""')
TOOL=$(printf '%s' "$INPUT" | jq -r '.tool_name // ""')
FILE=$(printf '%s' "$INPUT" | jq -r '.tool_input.file_path // .tool_input.path // ""')
TS=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
[ -z "$SID" ] && exit 0
PAYLOAD=$(jq -nc --arg s "$SID" --arg t "$TOOL" --arg f "$FILE" --arg ts "$TS" \
  '{session_id:$s, type:"tool_start", tool:$t, file:$f, ts:$ts}')
curl -fsS -m 2 -X POST "$RELAY_URL/api/ingest/activity" \
  ${RELAY_API_KEY:+-H "Authorization: Bearer $RELAY_API_KEY"} \
  -H "Content-Type: application/json" -d "$PAYLOAD" >/dev/null 2>&1 &
exit 0
