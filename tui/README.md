# commsync-tui

Read-only terminal UI for the commsync corridor. A **separate binary,
separate Go module** from the commsync server — per commsync doctrine,
human UX stays out of the core MCP server. This tool reads the same
SQLite database in `mode=ro`.

## Build

```
cd commsync/tui
go build ./...
```

Produces `./commsync-tui`.

## Run

```
./commsync-tui                 # reads ~/.local/state/commsync/commsync.db
./commsync-tui -db /path/to/commsync.db
./commsync-tui -poll 1s        # override poll cadence (default 2s)
COMMSYNC_DB=/path ./commsync-tui
```

## Keymap

Press `?` in the app for the full keymap. Highlights:

- `j/k` move, `g/G` jump to newest/oldest, pgup/pgdn page
- `enter` expand/collapse the selected message
- `r` pick room, `t` pick topic, `a` pick agent (matches from OR to)
- `A` toggle include-acked, `x` clear all filters, `R` reload now
- `q` / `ctrl-c` quit

Newest messages at the top. Status codes are color-coded
(`info`=neutral, `ask`=yellow, `warn`=red, `ack`=dim, `decision`=blue).

## Constraints

- Read-only DB access (`mode=ro` DSN, `_query_only=1`).
- No network. Local process only.
- Does not modify the commsync server.
- Polls the DB every 2s by default — cheap against local SQLite.
