#!/bin/bash
# PostToolUse → relay activity (tool finished). Fire-and-forget, never blocks.
RELAY_URL="${RELAY_URL:-http://localhost:8090}"
INPUT=$(cat)
command -v jq >/dev/null 2>&1 || exit 0
SID=$(printf '%s' "$INPUT" | jq -r '.session_id // ""')
TOOL=$(printf '%s' "$INPUT" | jq -r '.tool_name // ""')
TS=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
[ -z "$SID" ] && exit 0
PAYLOAD=$(jq -nc --arg s "$SID" --arg t "$TOOL" --arg ts "$TS" \
  '{session_id:$s, type:"tool_end", tool:$t, ts:$ts}')
curl -fsS -m 2 -X POST "$RELAY_URL/api/ingest/activity" \
  ${RELAY_API_KEY:+-H "Authorization: Bearer $RELAY_API_KEY"} \
  -H "Content-Type: application/json" -d "$PAYLOAD" >/dev/null 2>&1 &
exit 0
