#!/usr/bin/env bash
set -euo pipefail

DB_PATH="${COMMSYNC_DB_PATH:-$HOME/.local/state/commsync/commsync.db}"
STATE_DIR="${COMMSYNC_WATCH_STATE_DIR:-$HOME/.local/state/commsync}"
STATE_FILE="${COMMSYNC_WATCH_STATE_FILE:-$STATE_DIR/watch-last-id}"
LOG_FILE="${COMMSYNC_WATCH_LOG_FILE:-$STATE_DIR/watch.log}"
POLL_SECONDS="${COMMSYNC_WATCH_INTERVAL_SECONDS:-60}"
WATCH_AGENT="${COMMSYNC_WATCH_AGENT:-riker-watch}"

mkdir -p "$STATE_DIR"
touch "$LOG_FILE"

read_last_id() {
  if [[ -f "$STATE_FILE" ]]; then
    cat "$STATE_FILE"
  else
    echo "0"
  fi
}

write_last_id() {
  printf '%s\n' "$1" > "$STATE_FILE"
}

current_max_id() {
  sqlite3 "$DB_PATH" "select coalesce(max(id),0) from messages where archived_at is null;"
}

post_startup_notice() {
  python3 - <<'PY' | "${COMMSYNC_BIN:-commsync}" >/dev/null
import json, os
reqs = [
  {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"watch-loop","version":"0"}}},
  {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"post_message","arguments":{
    "room":"general",
    "from":os.environ.get("COMMSYNC_WATCH_AGENT","riker-watch"),
    "to":"all",
    "topic":"commsync-watch",
    "status":"info",
    "body":"Background corridor watch online. Poll cadence: 60s. Scope: detect new live messages, log them, and keep a standing watch marker alive for this Codex session."
  }}}
]
for req in reqs:
    print(json.dumps(req))
PY
}

log_line() {
  printf '%s %s\n' "$(date -u +%FT%TZ)" "$1" >> "$LOG_FILE"
}

last_id="$(read_last_id)"
max_id="$(current_max_id)"
if [[ "$last_id" =~ ^[0-9]+$ ]] && [[ "$max_id" =~ ^[0-9]+$ ]] && (( max_id > last_id )); then
  write_last_id "$max_id"
else
  write_last_id "${last_id:-0}"
fi

post_startup_notice
log_line "watcher-start poll=${POLL_SECONDS}s db=${DB_PATH}"

while true; do
  max_id="$(current_max_id)"
  last_id="$(read_last_id)"

  if [[ ! "$last_id" =~ ^[0-9]+$ ]]; then
    last_id=0
  fi

  if [[ "$max_id" =~ ^[0-9]+$ ]] && (( max_id > last_id )); then
    sqlite3 -separator '|' "$DB_PATH" "
      select id, created_at, room_name, from_agent, to_agent, topic, status
      from messages
      where archived_at is null and id > $last_id
      order by id asc;
    " | while IFS='|' read -r id created_at room_name from_agent to_agent topic status; do
      log_line "new-message id=${id} room=${room_name} from=${from_agent} to=${to_agent} topic=${topic} status=${status} created_at=${created_at}"
    done
    write_last_id "$max_id"
  fi

  sleep "$POLL_SECONDS"
done
