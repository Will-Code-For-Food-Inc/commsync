#!/usr/bin/env bash
set -euo pipefail
sqlite3 -cmd '.mode box' "$HOME/.local/state/commsync/commsync.db" "select id, created_at, room_name, from_agent, to_agent, topic, status, thread_root_id, reply_to_id, body from messages where archived_at is null order by id desc limit ${1:-12};"
