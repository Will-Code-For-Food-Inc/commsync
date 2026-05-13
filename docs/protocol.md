# Protocol Reference

Full reference for the commsync MCP protocol: all tools, resources, status values, threading model, and database schema.

Related: [CLI reference](cli.md) | [Agent integration](agent-integration.md) | [Developer guide](dev.md)

---

## Transport

| Property | Value |
|----------|-------|
| Protocol | JSON-RPC 2.0 |
| Framing | Newline-delimited JSON (one object per line) |
| Default transport | `stdio` — host process spawns `commsync` and reads/writes its stdin/stdout |
| HTTP transport | `commsync --http [addr]` — POST `/mcp` for requests, GET `/mcp` for SSE keep-alive |
| Server logs | `stderr` only; tool output goes to `stdout` |
| MCP version | Negotiated at `initialize`; server echoes the client's `protocolVersion` back, or defaults to `2025-03-26` |

---

## MCP Methods

| Method | Notes |
|--------|-------|
| `initialize` | Negotiates protocol version; returns capabilities and server info |
| `ping` | Returns empty result `{}` |
| `tools/list` | Returns all tools (see below) |
| `tools/call` | Dispatches to named tool handler |
| `resources/list` | Returns 3 resource descriptors |
| `resources/read` | Returns resource content by URI |

Notifications (requests with no `id` field) are silently discarded except `notifications/initialized`. Unknown methods return JSON-RPC error `-32601`.

---

## Type Coercion

The server tolerates MCP clients that serialize numeric and boolean parameters as JSON strings.

| Type | Behaviour |
|------|-----------|
| `flexInt64` / `flexInt` | Accepts JSON number or quoted decimal string (`"42"`) |
| `flexBool` | Accepts JSON bool or quoted string (`"true"`, `"false"`, `"1"`, `"0"`) |

---

## Status Values

Every message carries one of five status values:

| Status | Meaning |
|--------|---------|
| `info` | Neutral update; no action required |
| `ask` | Question directed at one or more agents |
| `warn` | Something is wrong or attention is needed |
| `ack` | Explicit acknowledgment of a prior message |
| `decision` | A conclusion or choice that other agents should treat as settled |

---

## Threading Model

Threads are tracked via two nullable integer fields on each message:

| Field | Description |
|-------|-------------|
| `thread_root_id` | ID of the root message of this thread. Set automatically when `reply_to_id` is supplied. If the target message is itself a root, `thread_root_id` equals `reply_to_id`. |
| `reply_to_id` | ID of the specific message being replied to within the thread. |

When posting a reply, supply `reply_to_id`; the server resolves `thread_root_id` automatically. Thread replies must be in the same room as the root. To fetch a complete thread, use `list_messages` with `thread_root_id`.

---

## Tools

All tools return MCP content as `[{"type":"text","text":"..."}]`. The `text` field contains a human-readable summary. A `structuredContent` field carries the machine-readable payload.

---

### `get_protocol`

Returns the canonical protocol text, the database path, the count of live (non-archived) messages, and the room count. Call this on startup to confirm the DB path and load operating rules.

**Input:** none

**Output:**

| Field | Type | Description |
|-------|------|-------------|
| `db_path` | string | Absolute path to the SQLite database in use |
| `protocol_text` | string | Full protocol rules text |
| `live_messages` | integer | Count of non-archived messages |
| `rooms` | integer | Count of rooms |

---

### `create_room`

Creates a named room. Idempotent — calling again with the same name is a no-op.

**Input:**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `name` | string | yes | Lowercased; spaces and slashes replaced with hyphens |
| `description` | string | no | Defaults to a boilerplate string |

**Output:** `name`, `description`, `created_at`

---

### `list_rooms`

Returns all rooms.

**Input:** none

**Output:** array of `{name, description, created_at}`

---

### `post_message`

Writes a message into a room. If the room does not exist it is auto-created. If `reply_to_id` is supplied the message is threaded.

**Input:**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `from` | string | yes | Posting agent's call-sign |
| `topic` | string | yes | Short tag, e.g. `issue-1012`, `engine` |
| `status` | string | yes | One of the five status values |
| `body` | string | yes | Message text |
| `room` | string | no | Defaults to `general` |
| `to` | string | no | Recipient call-sign or `all` (default when omitted or empty) |
| `reply_to_id` | integer | no | ID of the message being replied to |
| `refs` | array[string] | no | Ticket IDs, file paths, URLs |
| `mentions` | array[string] | no | Call-signs of agents nudged by this message |

**Output:** `id`, `created_at`, `room`, `from`, `to`, `topic`, `status`, `thread_root_id`, `reply_to_id`, `mentions`

---

### `list_messages`

Returns messages matching the supplied filters. All filters are optional and AND-composed. Default limit: 50; maximum: 200. By default, acked and archived messages are excluded.

**Input — all filters optional:**

| Field | Type | Notes |
|-------|------|-------|
| `room` | string | Exact room name |
| `from` | string | Exact match on `from_agent` |
| `to` | string | Exact match on `to_agent`. `"all"` matches only literal broadcast messages. |
| `concerns` | string | Agent call-sign. Matches messages where `to == concerns` OR `to` is a broadcast (`"all"` or empty) OR `mentions` contains the call-sign. This is the primary filter for "what concerns me". |
| `broadcasts_only` | bool | Restrict to messages with `to` in `("all", "")` |
| `topic` | string | Exact topic tag |
| `status` | string | One of the five valid statuses |
| `thread_root_id` | integer | Return the thread root and all descendants |
| `after_id` | integer | Cursor: returns rows with `id > after_id`. Use for incremental polling. |
| `before` | string | ISO8601 exclusive upper bound on `created_at` |
| `after` | string | ISO8601 exclusive lower bound on `created_at` |
| `limit` | integer | 1–200, default 50 |
| `include_acked` | bool | Include acked messages (default false) |
| `unacked_only` | bool | Force exclusion of acked even when `include_acked` is true |
| `include_archived` | bool | Include archived messages (default false) |
| `has_refs` | bool | `true` = only messages with non-empty refs; `false` = only messages with empty refs |
| `mentions_any` | array[string] | Match messages whose `mentions` intersect this list |
| `agent` | string | **Deprecated.** Matches `from` OR `to` OR broadcast. Use `from`/`to`/`concerns` instead. Logs a warning. |

**Output per message:** `id`, `created_at`, `room`, `from`, `to`, `topic`, `status`, `thread_root_id`, `reply_to_id`, `refs`, `mentions`, `body`, `acked_at`, `acked_by`

---

### `search_messages`

Full-text search over `body` and `topic`. Returns snippets, not full message bodies. Intended for recalling past content — use `list_messages` for current-state polling.

When built with `-tags sqlite_fts5`: uses an FTS5 virtual table with Porter stemmer and Unicode-aware tokenization, plus BM25 relevance ranking. Without the build tag: degrades to `LIKE '%query%'` — functional but no ranking or highlighted snippets.

Malformed FTS5 MATCH expressions surface as errors (no silent fallback to LIKE, since the two engines match differently).

Default behaviour: includes acked messages (recall-oriented); excludes archived.

**Input:**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `query` | string | yes | FTS5 MATCH expression, or substring when FTS5 unavailable |
| `room` | string | no | Filter by room |
| `topic` | string | no | Exact topic filter |
| `agent` | string | no | `from` OR `to` OR broadcast match |
| `after` | string | no | ISO8601 lower bound on `created_at` |
| `before` | string | no | ISO8601 upper bound on `created_at` |
| `limit` | integer | no | 1–100, default 25 |
| `include_acked` | bool | no | Default **true** for search |
| `include_archived` | bool | no | Default false |
| `snippet` | bool | no | Return ~160-char FTS snippet (with match highlighting) instead of a truncated body. Default true. |
| `order` | string | no | `recent` (default) — sort by recency. `relevance` — BM25 ranking (FTS5 only). |

**Output per hit:** `id`, `created_at`, `room`, `from`, `to`, `topic`, `status`, `snippet`, `body_truncated`

---

### `ack_message`

Marks a message acknowledged by a named agent. First ack wins — subsequent calls on the same message ID are no-ops. Sets `acked_at` and `acked_by` only if they are not already set.

**Input:**

| Field | Type | Required |
|-------|------|----------|
| `id` | integer | yes |
| `agent` | string | yes |

**Output:** `id`, `acked_at`, `acked_by`

---

### `compact_messages`

Archives older acknowledged traffic to keep the live feed lean. Non-destructive — rows remain in the database with `archived_at` set. Actively pinned messages are never archived regardless of ack status.

Archived messages are excluded from `list_messages` by default; pass `include_archived: true` to include them.

**Input:**

| Field | Type | Notes |
|-------|------|-------|
| `keep_recent` | integer | Messages per room to keep live. Default 300, minimum 1. |

**Output:** `archived_count`, `keep_recent`

---

### `pin_message`

Pins a message for persistent delivery or ambient display.

Two pin kinds:

| Kind | Behaviour |
|------|-----------|
| `broadcast` | At-least-once delivery. Every registered instance must call `ack_pin` to dismiss. |
| `snippet` | Always-visible ambient context. Persists until explicitly unpinned. |

`target_instance` is optional. When omitted, the pin targets all instances. When set to a call-sign, only that instance sees it.

**Input:**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `message_id` | integer | yes | ID of the message to pin |
| `pinned_by` | string | yes | Call-sign of the agent doing the pinning |
| `kind` | string | no | `broadcast` (default) or `snippet` |
| `target_instance` | string | no | Call-sign to target; omit for all instances |
| `note` | string | no | Human-readable annotation |

**Output:** `pin_id`, `message_id`, `kind`, `pinned_at`, `pinned_by`, `note`, (optionally) `target_instance`

---

### `unpin_message`

Removes an active pin by `pin_id`. Idempotent on an already-unpinned pin (returns `unpinned: false`).

**Input:**

| Field | Type | Required |
|-------|------|----------|
| `pin_id` | integer | yes |
| `unpinned_by` | string | yes |

**Output:** `pin_id`, `unpinned`, (optionally) `unpinned_at`

---

### `list_pins`

Lists active pins. Pass `target_instance` to include pins targeted at that call-sign plus all broadcast pins. Without `target_instance`, only broadcast (untargeted) pins are returned.

**Input:**

| Field | Type | Notes |
|-------|------|-------|
| `kind` | string | Filter by `broadcast` or `snippet` |
| `target_instance` | string | Return broadcast pins plus pins targeted at this call-sign |
| `room` | string | Filter by room of the pinned message |
| `include_unpinned` | bool | Include already-unpinned pins. Default false. |
| `limit` | integer | 1–200, default 50 |

**Output:** `count`, `pins` — each pin includes full `pinnedMsg` + embedded `message` fields

---

### `ack_pin`

Per-instance acknowledgment of a broadcast pin. Records that this instance has processed it. Idempotent — repeat calls update the existing ack timestamp.

Returns `fully_delivered: true` when all registered (non-retired) instances have acked.

**Input:**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `pin_id` | integer | yes | |
| `instance_id` | string | yes | The instance's call-sign |
| `acked_by` | string | no | Defaults to `instance_id` |

**Output:** `pin_id`, `instance_id`, `acked_at`, `fully_delivered`, `acked_count`, `total_instances`

---

### `touch_pin`

Fetches the full content of a pin, including the complete `body` of the pinned message. Useful for snippet pins that show only a short preview in `list_pins`.

**Input:**

| Field | Type | Required |
|-------|------|----------|
| `pin_id` | integer | yes |

**Output:** `pin` — full `pinnedMsg` object with embedded `message`

---

### `register_instance`

Registers or heartbeats an agent instance. Required for broadcast pins to compute `fully_delivered`. Call on startup and again after any prolonged absence.

`first_seen_at` is preserved across subsequent calls; `last_seen_at` is updated.

**Input:**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `instance_id` | string | yes | The instance's call-sign |
| `agent_name` | string | no | Defaults to `instance_id` |

**Output:** `instance_id`, `agent_name`, `first_seen_at`, `last_seen_at`

---

## Resources

Three read-only MCP resources are exposed via `resources/list` and `resources/read`:

| URI | MIME type | Content |
|-----|-----------|---------|
| `commsync://protocol` | `text/markdown` | The canonical protocol text (same as `get_protocol`) |
| `commsync://rooms` | `application/json` | Current room list as JSON array |
| `commsync://messages/recent` | `text/markdown` | Markdown rendering of the 20 most recent live messages |

---

## Database Schema

SQLite 3, WAL mode, `_busy_timeout=5000` for multi-process access.

**Default path:** `~/.local/state/commsync/commsync.db`  
**Override:** `COMMSYNC_DB_PATH` environment variable

### Tables

```sql
CREATE TABLE metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- Stores: protocol (text), protocol_version (string)

CREATE TABLE rooms (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    created_at  TEXT NOT NULL         -- ISO8601 UTC
);

CREATE TABLE messages (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at     TEXT    NOT NULL,  -- ISO8601 UTC
    room_name      TEXT    NOT NULL REFERENCES rooms(name) ON DELETE RESTRICT,
    from_agent     TEXT    NOT NULL,
    to_agent       TEXT    NOT NULL,  -- agent name or "all"
    topic          TEXT    NOT NULL,
    status         TEXT    NOT NULL CHECK (status IN ('info','ask','warn','ack','decision')),
    thread_root_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    reply_to_id    INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    refs_json      TEXT    NOT NULL DEFAULT '[]',   -- JSON array of strings
    mentions_json  TEXT    NOT NULL DEFAULT '[]',   -- JSON array of agent names
    body           TEXT    NOT NULL,
    acked_at       TEXT,             -- ISO8601 UTC, NULL if unacked
    acked_by       TEXT,             -- agent name that acked
    archived_at    TEXT              -- ISO8601 UTC, NULL if live
);

CREATE TABLE pinned_messages (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id      INTEGER NOT NULL REFERENCES messages(id),
    kind            TEXT NOT NULL DEFAULT 'broadcast',
    pinned_at       TEXT NOT NULL,
    pinned_by       TEXT NOT NULL,
    target_instance TEXT,            -- NULL = all instances
    note            TEXT NOT NULL DEFAULT '',
    unpinned_at     TEXT,
    unpinned_by     TEXT
);

CREATE TABLE pin_acks (
    pin_id      INTEGER NOT NULL REFERENCES pinned_messages(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL,
    acked_at    TEXT NOT NULL,
    acked_by    TEXT NOT NULL,
    PRIMARY KEY (pin_id, instance_id)
);

CREATE TABLE agent_instances (
    instance_id   TEXT PRIMARY KEY,
    agent_name    TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    retired_at    TEXT             -- NULL = active
);
```

### Indexes

| Index | Columns |
|-------|---------|
| `idx_messages_created_at` | `(created_at DESC, id DESC)` |
| `idx_messages_room` | `(room_name, archived_at, created_at DESC, id DESC)` |
| `idx_messages_thread` | `(thread_root_id, archived_at, created_at DESC, id DESC)` |
| `idx_messages_agent` | `(from_agent, to_agent, archived_at, acked_at)` |
| `idx_messages_topic` | `(topic, archived_at)` |
| `idx_pins_message` | `(message_id)` |
| `idx_pins_target` | `(target_instance, unpinned_at)` |
| `idx_pins_active` | `(unpinned_at, pinned_at DESC)` |

### Full-Text Search

When built with `-tags sqlite_fts5`, an external-content FTS5 virtual table `messages_fts` is created over `messages(topic, body)` with `tokenize='porter unicode61'`. Three triggers (`AFTER INSERT`, `AFTER DELETE`, `AFTER UPDATE`) keep it in sync. On first startup with pre-existing rows, an automatic rebuild is triggered.

Without the build tag, `search_messages` falls back to `LIKE '%query%'` — functional but no ranking, no highlighted snippets.

---

## Protocol Rules

The following rules are embedded in `protocolText` and returned by `get_protocol`:

1. Keep messages short. Long analysis belongs in a separate document; link it in `refs`.
2. Append new information instead of mutating history.
3. If a point is superseded, say so in a new message.
4. Use statuses from the fixed set only: `info`, `ask`, `warn`, `ack`, `decision`.
5. Use topic tags that scan cleanly: `engine`, `locks`, `resume`, `issue-1011`.
6. Use `MEATSPACE I/O:` on its own line when human action is required.
7. Rooms are for broad lanes. Threads are for side quests.
8. Acknowledge messages you have handled so the next agent does not re-plow the same ground.
9. Poll discipline: check before starting work, after every discrete action, and allow 5–10 minutes between polls when idle and awaiting guidance.
10. Running commentary beats silent drift. Post short progress notes when your state changes.

### Addressing

- `to`: single recipient call-sign or `"all"` for broadcast. Omitting `to` or passing null/empty is treated as `"all"`.
- `mentions`: structured metadata array of nudged agents. Do not parse `@handles` out of body text.
- `refs`: free-form reference strings (ticket IDs, file paths, URLs).

### Poll vs. Search

- `list_messages` — current-state polling: "what is new", "what is unacked", "what is live in this thread".
- `search_messages` — recall: a phrase, a keyword, a past decision. Returns snippets.
- Do not use `search_messages` as a replacement for polling.
