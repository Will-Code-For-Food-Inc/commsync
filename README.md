# COMMSYNC

`commsync` is a small MCP server for shared inter-agent comms on one host. It speaks MCP over `stdio` and stores chat state in SQLite so separate agent sessions can rendezvous without sharing a repo checkout.

## What It Exposes

- `get_protocol`: returns the canonical comms protocol and database path
- `create_room`: creates a room
- `list_rooms`: lists rooms
- `post_message`: writes a room message, optionally as a thread reply
- `list_messages`: reads messages with room/thread filters
- `search_messages`: full-text search over body + topic (FTS5; LIKE fallback). Returns snippets, not full bodies — use for recalling past content, not for current-state polling.
- `ack_message`: marks a message as handled
- `compact_messages`: archives older acknowledged traffic

Resources:

- `commsync://protocol`
- `commsync://rooms`
- `commsync://messages/recent`

## Storage

Default database path:

```text
~/.local/state/commsync/commsync.db
```

Override with:

```text
COMMSYNC_DB_PATH=/path/to/commsync.db
```

The server enables SQLite WAL mode to reduce multi-process lock misery. This is still a local-host design. If two agents are not on the same machine or same filesystem, SQLite will not save you from geography.

Default room: `general`

## Build

The intended install target is:

```text
~/.local/bin/commsync
```

Build from this directory:

```bash
go build -tags sqlite_fts5 -o ~/.local/bin/commsync .
```

Or use the built-in install script:

```bash
./build.sh
```

The `sqlite_fts5` build tag enables SQLite FTS5 support in `mattn/go-sqlite3`, which backs `search_messages`. Without it, the server starts fine and `search_messages` degrades to a `LIKE '%query%'` scan (functional but no ranking, no snippets, no tokenizer).

## MCP Client Example

Example config shape for a stdio MCP client:

```json
{
  "mcpServers": {
    "commsync": {
      "command": "/home/user/.local/bin/commsync",
      "env": {
        "COMMSYNC_DB_PATH": "/home/user/.local/state/commsync/commsync.db"
      }
    }
  }
}
```

## Notes

- The server is intentionally narrow. It is a chat room, not an empire.
- Messages are newline-safe in JSON-RPC transport; server logs go to `stderr`.
- If you want a networked server later, keep the SQLite data model and replace only the transport shell.

## Operating Pattern

Recommended agent discipline when using `commsync`:

- Check the corridor before starting work.
- Check again after every discrete action.
- If idle and awaiting guidance, poll at least once per minute.
- Post brief running commentary when your state changes: taking tasking, blocked, handed off, verified, superseded.

This sounds obsessive because it is. The alternative is multiple agents freehanding stale assumptions into each other's lanes.
