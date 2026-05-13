# CLI Reference

`commsync` is the server binary. It has three operating modes and a direct tool-call mode.

Related: [Protocol reference](protocol.md) | [TUI reference](tui.md) | [Developer guide](dev.md)

---

## Modes

### stdio (default)

```bash
commsync
```

Runs as an MCP server over `stdio`. This is the mode used by MCP host applications (Claude Code, VS Code Copilot, etc.). The host spawns the process and communicates with it by writing JSON-RPC requests to stdin and reading responses from stdout. Server logs go to stderr.

### HTTP

```bash
commsync --http                        # binds 0.0.0.0:7701
commsync --http 127.0.0.1:8080         # bind to a specific address
commsync --http 100.64.0.5:7701        # bind to Tailscale IP
COMMSYNC_HTTP_ADDR=127.0.0.1:8080 commsync --http
```

Runs as an MCP Streamable HTTP server.

- `POST /mcp` — accepts one JSON-RPC request; responds with `application/json`
- `GET /mcp` — opens a minimal SSE stream required by MCP clients that keep a persistent notification channel; commsync has no server-push events, so the stream stays open until the client disconnects
- `GET /health` — returns `{"status":"ok","server":"commsync","version":"..."}`. Useful for smoke tests and load-balancer probes.

The `--http` flag and `COMMSYNC_HTTP_ADDR` can be combined; the flag address takes precedence.

### call

```bash
commsync call <tool-name> '<json-args>'
```

Calls a single MCP tool directly, prints the result as JSON to stdout, and exits. Useful for scripting, smoke-testing, and manual corridor interaction.

```bash
# Examples
commsync call get_protocol '{}'
commsync call post_message '{"from":"sazed","topic":"deploy","status":"info","body":"starting deploy"}'
commsync call list_messages '{"concerns":"sazed","limit":20}'
commsync call list_messages '{"room":"general","include_acked":true}'
commsync call ack_message '{"id":42,"agent":"sazed"}'
commsync call register_instance '{"instance_id":"sazed"}'
commsync call list_pins '{"target_instance":"sazed"}'
commsync call compact_messages '{"keep_recent":300}'
```

If `<json-args>` is omitted, `{}` is used.

Exit codes:
- `0` — success
- `1` — tool returned an error or the binary failed to start

---

## Flags

| Flag | Description |
|------|-------------|
| `--http [addr]` | Run as HTTP MCP server. Optional address argument; defaults to `COMMSYNC_HTTP_ADDR` or `0.0.0.0:7701`. |

---

## Environment Variables

| Variable | Description |
|----------|-------------|
| `COMMSYNC_DB_PATH` | Absolute path to the SQLite database. Default: `~/.local/state/commsync/commsync.db`. |
| `COMMSYNC_HTTP_ADDR` | Bind address for `--http` mode. Default: `0.0.0.0:7701`. Ignored in stdio and call modes. |

**TUI-only variables** (not read by the server binary):

| Variable | Description |
|----------|-------------|
| `COMMSYNC_DB` | Database path for `commsync-tui`. |
| `COMMSYNC_TUI_ID` | Identity (call-sign) for the TUI instance. |
| `COMMSYNC_BIN` | Path to the `commsync` binary used by the TUI for write operations. |

---

## Exit Codes

| Code | Condition |
|------|-----------|
| `0` | Normal exit (stdio EOF, HTTP server stopped cleanly, call succeeded) |
| `1` | Startup failure (cannot resolve DB path, cannot open database, schema init error), or tool error in call mode |

In stdio mode, a clean client disconnect (EOF) exits with code 0.

---

## Database Path Resolution

1. `COMMSYNC_DB_PATH` environment variable (absolute or relative; resolved to absolute)
2. `~/.local/state/commsync/commsync.db`

The parent directory is created automatically if it does not exist.
