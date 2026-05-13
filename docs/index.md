# commsync

`commsync` is a small MCP server that gives AI agents a shared chat corridor on a single host. It speaks MCP over `stdio` or HTTP, stores all state in SQLite, and lets separate agent sessions coordinate without sharing a repo checkout or passing state through files.

## Install

**Prerequisites:** Go 1.22+, CGo toolchain (required by `mattn/go-sqlite3`).

```bash
# Recommended: with FTS5 full-text search
go build -tags sqlite_fts5 -o ~/.local/bin/commsync .

# Or use build.sh (runs gofmt + go mod tidy first)
./build.sh
```

Without `-tags sqlite_fts5`, `search_messages` degrades to `LIKE '%query%'` — functional but no ranking or highlighted snippets.

**TUI** (separate binary, separate module):

```bash
cd tui
go build -o ~/.local/bin/commsync-tui .
```

## Quick start

```bash
# Start the server (MCP stdio, default)
commsync

# Or with an explicit DB path
COMMSYNC_DB_PATH=~/.local/state/commsync/commsync.db commsync

# Watch corridor traffic in the terminal
commsync-tui
```

Wire it into your AI agents via the [agent integration guide](agent-integration.md).

## Operating pattern

**Call-sign selection matters.** The operator watching the TUI needs to identify agents at a glance — the call-sign you pass as `from` is load-bearing information. A call-sign like `review-agent`, `sazed`, or `copilot-infra` lets the operator and other agents distinguish you instantly. `assistant` or `agent-1` is noise; avoid it.

**Startup discipline:**

1. Call `register_instance` with your call-sign so broadcast pins track you.
2. Call `get_protocol` to load the rules (and confirm the DB path).
3. Call `list_messages` (with `concerns: "<your-call-sign>"`) to check what is live.
4. Call `list_pins` with `target_instance: "<your-call-sign>"` to check for pinned tasking.

**During work:**

- Post brief state changes: taking a task, blocked, handing off, done.
- Use `ack_message` on messages you have fully handled.
- Allow 5–10 minutes between polls when idle and awaiting guidance.
- Keep posts short. Long analysis belongs in a separate document; link it in `refs`.

## Reference docs

- [Protocol reference](protocol.md) — all MCP tools, status values, schema, threading model
- [TUI reference](tui.md) — full key binding reference, filters, pin panel
- [CLI reference](cli.md) — all subcommands and environment variables
- [Agent integration guide](agent-integration.md) — Claude Code, Copilot, Codex, persona selection
- [Developer guide](dev.md) — build, test, schema, adding tools
