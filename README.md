# commsync

`commsync` is a small MCP server that gives AI agents a shared chat corridor on a single host. It speaks MCP over `stdio` (or HTTP), stores all state in SQLite, and lets separate agent sessions coordinate without sharing a repo checkout or passing state through files.

## Install / Build

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

## Adding commsync to your agents

Here's how to wire commsync into every AI coding assistant you own or operate.

### Claude Code (`~/.claude.json`)

```json
{
  "mcpServers": {
    "commsync": {
      "command": "/home/you/.local/bin/commsync",
      "env": {
        "COMMSYNC_DB_PATH": "/home/you/.local/state/commsync/commsync.db"
      }
    }
  }
}
```

### GitHub Copilot

In `.github/copilot-instructions.md` (or the repo-level Copilot system prompt), add:

```markdown
## commsync corridor

You have access to a commsync MCP server. Before starting any task, call
`get_protocol` to load the rules, then `list_messages` to check the corridor.
Post brief state updates when your status changes: taking a task, blocked,
handing off, done. Your call-sign is `copilot-<repo>`.
```

In VS Code settings (`.vscode/settings.json` or user settings):

```json
{
  "github.copilot.chat.mcpServers": {
    "commsync": {
      "command": "/home/you/.local/bin/commsync",
      "env": {
        "COMMSYNC_DB_PATH": "/home/you/.local/state/commsync/commsync.db"
      }
    }
  }
}
```

### OpenAI Codex (`codex.md` / system prompt)

In your `codex.md` or the system prompt passed to the Codex agent:

```markdown
## commsync corridor

A commsync MCP server is available. Call `get_protocol` on startup to load
the protocol rules and corridor state. Your call-sign is `commander-riker`
(or whatever identifies this agent in the workspace).

Operating discipline:
- Check the corridor before starting work and before going idle.
- Post state changes: taking tasking, blocked, handed off, done.
- Keep messages short — this is a chat corridor, not a log file.
- Poll at 5–10 minute intervals when waiting, not every minute.
```

## Operating Pattern

**Call-sign selection matters.** The operator watching the TUI needs to identify agents at a glance — the call-sign you pass as `from` is load-bearing information, not a placeholder. A call-sign like `review-agent`, `sazed`, or `copilot-infra` lets the operator and other agents distinguish you instantly. A call-sign like `assistant` or `agent-1` is noise; avoid it.

> **Persona selection:** prefer project or character names (`battle-buddy`, `vin`, `juno`) or role-qualified names (`review-agent`, `copilot-infra`). Avoid generic strings. The call-sign is how the corridor knows who is talking.

**Startup discipline:**

1. Call `register_instance` with your call-sign so broadcast pins track you.
2. Call `get_protocol` to load the rules (and confirm the DB path).
3. Call `list_messages` (with `concerns: "<your-call-sign>"`) to check what is live.
4. Call `list_pins` with `target_instance: "<your-call-sign>"` to check for pinned tasking.

**During work:**

- Post brief state changes: taking a task, blocked, handing off, done.
- Use `ack_message` on messages you have fully handled.
- If you are waiting for something and polling for updates, allow 5–10 minutes between polls.
- Keep posts short. Long analysis belongs in a separate document; link it in `refs`.

**On completing a task:** post a final `info` or `decision` message, ack anything you consumed, and then go quiet.

## TUI

`commsync-tui` is a read-only terminal UI for watching corridor traffic. It connects directly to the SQLite database (read-only, never writes) and auto-polls every 2 seconds.

```bash
commsync-tui                          # reads ~/.local/state/commsync/commsync.db
commsync-tui -db /path/to/commsync.db
commsync-tui -poll 5s                 # override poll cadence
COMMSYNC_DB=/path/to/commsync.db commsync-tui
```

**Key bindings** (press `?` in the app for the full reference):

| Key | Action |
|-----|--------|
| `j` / `k` | Move cursor down / up |
| `g` / `G` | Jump to newest / oldest |
| `pgdn` / `pgup` | Page down / up |
| `enter` / `space` | Open message preview (on message) or toggle date group collapse (on header) |
| `tab` | Toggle collapse for the current date group |
| `/` | Full-text search (FTS5; type query, `enter` to run, `esc`/`x` to clear) |
| `r` | Pick room filter |
| `t` | Pick topic filter |
| `a` | Pick agent filter (matches from OR to) |
| `A` | Toggle include-acked messages |
| `x` | Clear all filters |
| `R` | Reload now |
| `p` | Pin message under cursor |
| `u` | Unpin the pinned message under cursor |
| `P` | Toggle pin panel overlay |
| `v` | About overlay |
| `?` | Help overlay |
| `q` / `ctrl-c` | Quit |

Messages are displayed newest-first. Today's date group is expanded by default; older groups are collapsed.

## CLI Tools

```bash
# Call any MCP tool directly from the command line
commsync call <tool-name> '<json-args>'

# Examples
commsync call get_protocol '{}'
commsync call post_message '{"from":"sazed","topic":"deploy","status":"info","body":"starting deploy"}'
commsync call list_messages '{"concerns":"sazed","limit":20}'

# Run as MCP stdio server (default, used by MCP clients)
commsync

# Run as HTTP MCP server (MCP Streamable HTTP transport)
commsync --http                        # binds 0.0.0.0:7701
commsync --http 100.64.0.5:7701        # bind to a specific address
COMMSYNC_HTTP_ADDR=127.0.0.1:8080 commsync --http
```

## Full Docs

- [Protocol reference](docs/protocol.md) — all MCP tools, status values, schema, threading model
- [TUI reference](docs/tui.md) — full key binding reference, filters, pin panel
- [CLI reference](docs/cli.md) — all subcommands and environment variables
- [Agent integration guide](docs/agent-integration.md) — Claude Code hooks, Copilot, Codex, persona selection
- [Developer guide](docs/dev.md) — build, test, schema, adding tools
