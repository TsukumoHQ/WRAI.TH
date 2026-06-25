#!/bin/bash
# Stop → relay. Two things, both fire-and-forget and fail-silent:
#   1. activity event (agent turn ended → waiting for user)
#   2. real token usage: read the NEW transcript lines since last Stop and sum
#      .message.usage, so the relay records true input/output/cache tokens
#      instead of a bytes/4 estimate. Line-offset per session avoids re-counting.
RELAY_URL="${RELAY_URL:-http://localhost:8090}"
INPUT=$(cat)
command -v jq >/dev/null 2>&1 || exit 0
SID=$(printf '%s' "$INPUT" | jq -r '.session_id // ""')
TP=$(printf '%s' "$INPUT" | jq -r '.transcript_path // ""')
TS=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
[ -z "$SID" ] && exit 0

AUTH=(${RELAY_API_KEY:+-H "Authorization: Bearer $RELAY_API_KEY"})

# 1. activity
ACT=$(jq -nc --arg s "$SID" --arg ts "$TS" '{session_id:$s, type:"stop", ts:$ts}')
curl -fsS -m 2 -X POST "$RELAY_URL/api/ingest/activity" "${AUTH[@]}" \
  -H "Content-Type: application/json" -d "$ACT" >/dev/null 2>&1 &

# 2. token usage from new transcript lines
if [ -n "$TP" ] && [ -f "$TP" ]; then
  OFF_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/agent-relay/offsets"
  mkdir -p "$OFF_DIR" 2>/dev/null
  OFF_FILE="$OFF_DIR/$SID"
  OFFSET=$(cat "$OFF_FILE" 2>/dev/null || echo 0)
  case "$OFFSET" in ''|*[!0-9]*) OFFSET=0 ;; esac
  TOTAL=$(wc -l < "$TP" 2>/dev/null | tr -d ' ')
  case "$TOTAL" in ''|*[!0-9]*) TOTAL=0 ;; esac
  if [ "$TOTAL" -gt "$OFFSET" ]; then
    BODY=$(tail -n +$((OFFSET + 1)) "$TP" 2>/dev/null | jq -s --arg s "$SID" --arg ts "$TS" '
      [ .[] | select(.message.usage != null) | .message.usage ] as $u
      | { session_id: $s, ts: $ts,
          input:          ([ $u[].input_tokens              // 0 ] | add // 0),
          output:         ([ $u[].output_tokens             // 0 ] | add // 0),
          cache_read:     ([ $u[].cache_read_input_tokens    // 0 ] | add // 0),
          cache_creation: ([ $u[].cache_creation_input_tokens // 0 ] | add // 0) }' 2>/dev/null)
    if [ -n "$BODY" ]; then
      curl -fsS -m 2 -X POST "$RELAY_URL/api/ingest/tokens" "${AUTH[@]}" \
        -H "Content-Type: application/json" -d "$BODY" >/dev/null 2>&1 &
    fi
    echo "$TOTAL" > "$OFF_FILE" 2>/dev/null
  fi
fi
exit 0
