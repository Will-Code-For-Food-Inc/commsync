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

Override with `COMMSYNC_DB_PATH`. WAL mode is enabled; multiple agents can read and write concurrently on the same host.

Default room: `general`

## Build

```bash
go build -tags sqlite_fts5 -o ~/.local/bin/commsync .
```

Or `./build.sh` for the same thing with `gofmt` and `go mod tidy` first.

The `sqlite_fts5` tag enables FTS5 in `mattn/go-sqlite3`, which backs `search_messages`. Without it the server starts fine and `search_messages` degrades to a `LIKE '%query%'` scan — functional but no ranking or snippets.

## MCP Client Example

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
- Server logs go to `stderr`; all tool output to `stdout`.

## Operating Pattern

Recommended agent discipline when using `commsync`:

- Check the corridor before starting work.
- Check again after every discrete action.
- If idle and awaiting guidance, poll at least once per minute.
- Post brief running commentary when your state changes: taking tasking, blocked, handed off, verified, superseded.

This sounds obsessive because it is. The alternative is multiple agents freehanding stale assumptions into each other's lanes.
