# Developer Guide

Building, testing, and extending commsync.

Related: [Protocol reference](protocol.md) | [CLI reference](cli.md)

---

## Prerequisites

| Requirement | Notes |
|-------------|-------|
| Go 1.22+ | Main server. TUI module requires Go 1.24+. |
| CGo toolchain | Required by `mattn/go-sqlite3` (C compiler, `pkg-config`) |
| `libsqlite3` headers | Usually `libsqlite3-dev` (Debian/Ubuntu) or `sqlite-devel` (Fedora) |
| `pkg-config` | Used by the sqlite3 driver to locate library flags |

Verify CGo is enabled:

```bash
go env CGO_ENABLED   # should print 1
```

---

## Build

### Server binary

```bash
# With FTS5 full-text search (recommended)
go build -tags sqlite_fts5 -o ~/.local/bin/commsync .

# Without FTS5 (search falls back to LIKE)
go build -o ~/.local/bin/commsync .
```

### build.sh

`build.sh` at the repo root runs `gofmt`, `go mod tidy`, and the FTS5 build in one step:

```bash
./build.sh
```

Output is installed to `~/.local/bin/commsync`.

### TUI binary

The TUI is a separate Go module in the `tui/` subdirectory:

```bash
cd tui
go build -o ~/.local/bin/commsync-tui .
```

The TUI depends on `mattn/go-sqlite3` and the Charmbracelet stack (`bubbletea`, `lipgloss`). It does not import the server module.

---

## Testing

There is no automated test suite at this time. Smoke-test with the `call` subcommand:

```bash
# Start fresh: post a message and read it back
commsync call post_message '{"from":"dev","topic":"test","status":"info","body":"hello"}'
commsync call list_messages '{"limit":5}'

# Exercise threading
commsync call post_message '{"from":"dev","topic":"test","status":"ask","body":"thread root"}'
# note the returned id, e.g. 2
commsync call post_message '{"from":"dev","topic":"test","status":"info","body":"reply","reply_to_id":2}'
commsync call list_messages '{"thread_root_id":2}'

# Ack and compact
commsync call ack_message '{"id":1,"agent":"dev"}'
commsync call compact_messages '{"keep_recent":10}'

# Pins
commsync call pin_message '{"message_id":2,"pinned_by":"dev","kind":"broadcast"}'
commsync call list_pins '{}'
commsync call register_instance '{"instance_id":"dev"}'
commsync call ack_pin '{"pin_id":1,"instance_id":"dev"}'

# Search (requires FTS5 build)
commsync call search_messages '{"query":"hello"}'
```

Set `COMMSYNC_DB_PATH` to use a throwaway database during development:

```bash
COMMSYNC_DB_PATH=/tmp/test.db commsync call get_protocol '{}'
```

---

## Adding a New Tool

1. **Define the handler function** in `main.go`. Follow the existing pattern:

   ```go
   func (s *server) toolMyNewTool(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
       var args struct {
           Field string `json:"field"`
       }
       if err := json.Unmarshal(raw, &args); err != nil {
           return nil, err
       }
       // ... logic ...
       return toolResult("Summary text.", map[string]interface{}{
           "key": "value",
       }), nil
   }
   ```

2. **Register the handler** in `handleToolCall`:

   ```go
   case "my_new_tool":
       return s.toolMyNewTool(ctx, params.Arguments)
   ```

3. **Add to `toolset()`** — the function that returns the tools list served by `tools/list`:

   ```go
   {
       Name:        "my_new_tool",
       Description: "One sentence description.",
       InputSchema: map[string]interface{}{
           "type": "object",
           "properties": map[string]interface{}{
               "field": map[string]interface{}{"type": "string"},
           },
           "required": []string{"field"},
       },
   },
   ```

4. Update `docs/protocol.md` with the new tool's input/output schema and behaviour notes.

### Type helpers

Use `flexInt64`, `flexInt`, and `flexBool` for numeric and boolean fields that MCP clients may serialize as strings. See the [Protocol reference](protocol.md#type-coercion) for details.

### Return shape

All tools return via `toolResult(text, payload)`:

```go
func toolResult(text string, structured map[string]interface{}) map[string]interface{} {
    return map[string]interface{}{
        "content": []map[string]interface{}{{"type": "text", "text": text}},
        "structuredContent": structured,
    }
}
```

`text` is the human-readable summary included in the MCP content block. `structured` is the machine-readable payload in `structuredContent`.

---

## Schema Changes

Schema is initialized by `initDB` in `main.go`. The function is idempotent — it uses `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` throughout, so re-running on an existing database is safe.

**Adding a column to `messages`:** add an entry to the `needed` map in `ensureMessageColumns`. This function uses `ALTER TABLE ... ADD COLUMN` and checks `PRAGMA table_info` first, so it only runs the DDL when the column is missing. This handles upgrades of existing deployments.

**Adding a new table:** add `CREATE TABLE IF NOT EXISTS` DDL to `initDB` directly, or to a new `ensure*` helper called from `initDB`. Follow the pattern of `ensurePinTables`.

**Dropping or renaming columns:** SQLite has limited `ALTER TABLE` support. Use a new column + data migration, or recreate the table. For now, commsync has never needed a destructive migration.

---

## TUI Development

The TUI lives in `tui/` as a separate Go module (`commsync-tui`). It imports `mattn/go-sqlite3` and the Charmbracelet TUI stack.

Key architecture:

- `store` — thin wrapper around `*sql.DB` with read-only queries
- `model` — Bubbletea model; holds all display state
- `Update` / `onKey` — event dispatch
- `View` / render* — pure rendering functions

The TUI never writes to the database directly. Write operations (pin, unpin, ack pin) are performed by shelling out to `commsync call <tool> '<json>'` via `callCommsync`. This keeps the TUI dependency-free of the server's internal logic.

To run the TUI against a development database:

```bash
cd tui
go run . -db /tmp/test.db
```

---

## Project Layout

```
main.go             # server, all MCP tools, DB layer
go.mod / go.sum     # server module (commsync)
build.sh            # gofmt + go mod tidy + build with FTS5
tui/
  main.go           # TUI binary
  go.mod / go.sum   # TUI module (commsync-tui)
docs/               # reference documentation
SPEC.md             # protocol and implementation spec
README.md           # quick-start and overview
```
