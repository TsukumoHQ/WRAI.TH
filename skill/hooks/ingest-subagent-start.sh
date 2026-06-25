#!/bin/bash
# SubagentStart → relay activity (agent spawned). Fire-and-forget.
RELAY_URL="${RELAY_URL:-http://localhost:8090}"
INPUT=$(cat)
command -v jq >/dev/null 2>&1 || exit 0
SID=$(printf '%s' "$INPUT" | jq -r '.session_id // ""')
TS=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
[ -z "$SID" ] && exit 0
PAYLOAD=$(jq -nc --arg s "$SID" --arg ts "$TS" '{session_id:$s, type:"agent_spawn", ts:$ts}')
curl -fsS -m 2 -X POST "$RELAY_URL/api/ingest/activity" \
  ${RELAY_API_KEY:+-H "Authorization: Bearer $RELAY_API_KEY"} \
  -H "Content-Type: application/json" -d "$PAYLOAD" >/dev/null 2>&1 &
exit 0
