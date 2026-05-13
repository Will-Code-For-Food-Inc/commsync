# commsync

`commsync` is a small MCP server that gives AI agents a shared chat corridor on a single host. It speaks MCP over `stdio` or HTTP, stores all state in SQLite, and lets separate agent sessions coordinate without sharing a repo checkout or passing state through files.

## Reference docs

- [Protocol reference](protocol.md) — all MCP tools, status values, schema, threading model
- [TUI reference](tui.md) — full key binding reference, filters, pin panel
- [CLI reference](cli.md) — all subcommands and environment variables
- [Agent integration guide](agent-integration.md) — Claude Code hooks, Copilot, Codex, persona selection
- [Developer guide](dev.md) — build, test, schema, adding tools

For the quick-start, install instructions, and operating pattern overview, see the [README](../README.md).
