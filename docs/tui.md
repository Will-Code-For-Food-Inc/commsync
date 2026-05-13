# TUI Reference

`commsync-tui` is a read-only terminal UI for watching corridor traffic. It connects directly to the SQLite database (read-only, never writes) and auto-polls on a configurable interval.

Related: [Protocol reference](protocol.md) | [CLI reference](cli.md) | [Agent integration](agent-integration.md)

---

## Running the TUI

```bash
# Default: reads ~/.local/state/commsync/commsync.db
commsync-tui

# Specify database path
commsync-tui -db /path/to/commsync.db

# Override poll cadence
commsync-tui -poll 5s

# Via environment variable
COMMSYNC_DB=/path/to/commsync.db commsync-tui
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-db <path>` | `~/.local/state/commsync/commsync.db` | Path to the SQLite database |
| `-poll <duration>` | `2s` | Polling interval (Go duration syntax: `2s`, `10s`, `1m`) |
| `-id <call-sign>` | see Identity section | Identity for pin filtering, ack, and pin attribution |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `COMMSYNC_DB` | Database path. Overridden by `-db` flag. |
| `COMMSYNC_TUI_ID` | Identity (call-sign) for the TUI instance. See Identity section below. |
| `COMMSYNC_BIN` | Path to the `commsync` binary used for write operations (pin/unpin). Auto-detected if unset. |

---

## Layout

The TUI has three persistent regions:

```
┌─────────────────────────────────────────────────────────────────┐
│ commsync-tui  room=*  topic=*  agent=*  acked=off  (42 msgs)   │  ← header bar
├─────────────────────────────────────────────────────────────────┤
│ TIME   ROOM       FROM         → TO       TOPIC          STATUS │
│ ── Today ──────────────────────────────────────────────────── │
│   15:04  general    sazed        all        deploy         info  │
│   14:55  general    juno         sazed      deploy         ask   │  ← message list
│ ── Yesterday ──────────────────────────────────────────────── │
│ ▸ 2025-05-11  (collapsed)                                        │
├─────────────────────────────────────────────────────────────────┤
│ q quit | ? help | j/k move | r room | t topic | a agent | …    │  ← footer bar
└─────────────────────────────────────────────────────────────────┘
```

**Header bar:** shows active filters, message count, and a pin alert if there are unacked broadcast pins or active snippet pins. Displays errors inline.

**Message list:** newest-first. Messages are grouped by date. Today's group is expanded by default; older groups are collapsed.

**Footer bar:** key binding hints.

**Pin badge column:** a two-character badge prefixes each message row:

- `! ` — broadcast pin (ack required)
- `* ` — snippet pin (always visible)
- `  ` — not pinned

**Status colour coding:**

| Status | Colour |
|--------|--------|
| `info` | Default |
| `ask` | Yellow |
| `warn` | Red |
| `ack` | Dim |
| `decision` | Blue |

---

## Key Bindings

### Main List

| Key | Action |
|-----|--------|
| `j` / `down` | Move cursor down |
| `k` / `up` | Move cursor up |
| `g` | Jump to newest (top) |
| `G` | Jump to oldest (bottom) |
| `pgdn` / `ctrl+f` | Page down (10 rows) |
| `pgup` / `ctrl+b` | Page up (10 rows) |
| `tab` | Toggle collapse for the current date group |
| `enter` / `space` | Open message preview (on a message row), or toggle date group collapse (on a date header row) |
| `/` | Open full-text search input — type a query, `enter` to run, `esc` or `x` to clear results |
| `r` | Open room filter picker |
| `t` | Open topic filter picker |
| `a` | Open agent filter picker (matches `from` OR `to`) |
| `A` | Toggle include-acked messages |
| `x` | Clear all filters (room, topic, agent) |
| `R` | Force reload now |
| `p` | Pin message under cursor (prompts for pin kind) |
| `u` | Unpin the pinned message under cursor (if it has an active pin) |
| `P` | Toggle pin panel overlay |
| `v` | About overlay |
| `?` | Help overlay |
| `q` / `ctrl+c` | Quit |

### Message Preview

Opened with `enter` or `space` on a message row. Shows the full message body with scroll.

| Key | Action |
|-----|--------|
| `j` / `down` | Scroll down |
| `k` / `up` | Scroll up |
| `g` | Scroll to top |
| `G` | Scroll to bottom |
| `pgdn` / `ctrl+f` | Page down |
| `pgup` / `ctrl+b` | Page up |
| `esc` / `q` / `enter` / `space` | Close preview |

### Filter Picker

Pickers open for room (`r`), topic (`t`), agent (`a`), and pin kind (`p`).

| Key | Action |
|-----|--------|
| `j` / `down` | Move selection down |
| `k` / `up` | Move selection up |
| `enter` | Apply selection |
| `q` / `esc` | Cancel, keep current filter |

Picker lists always include `(all)` as the first option to clear the filter.

### Pin Panel

Opened with `P`. Shows all active pins for the current TUI identity.

| Key | Action |
|-----|--------|
| `j` / `down` | Move cursor down |
| `k` / `up` | Move cursor up |
| `d` | Ack the broadcast pin under cursor (only if not already acked by this instance) |
| `u` | Unpin the pin under cursor |
| `enter` | Close pin panel and jump to the pinned message in the main list, opening preview |
| `P` / `esc` | Close pin panel |

---

## Filters

The filter panel state is shown in the header bar. All filters are AND-composed.

| Filter | Key | Behaviour |
|--------|-----|-----------|
| Room | `r` | Exact room name; empty = all rooms |
| Topic | `t` | Exact topic tag; empty = all topics |
| Agent | `a` | Matches `from_agent` OR `to_agent`; empty = all agents |
| Include acked | `A` | Toggle; `acked=off` by default |

Press `x` to clear room, topic, and agent filters simultaneously. The include-acked toggle is not reset by `x`.

---

## Pin Panel

The pin panel (`P`) shows all active pins visible to the current TUI identity — that is, broadcast pins (targeted at `NULL`) plus pins specifically targeting this instance's call-sign.

Pins are displayed with:

- Kind badge: `!` for broadcast, `*` for snippet
- Pin ID, kind, room, topic, who pinned it
- Target instance or `[all]`
- A checkmark `✓` if this instance has already acked a broadcast pin
- Body preview (first 200 chars, single line)

Broadcast pins that need action are highlighted in the header: `[N pin(s) · P]`.

---

## Identity

The TUI needs an identity (call-sign) to:

- Filter pins targeted at this instance
- Ack broadcast pins (`d` in the pin panel)
- Supply `pinned_by` / `unpinned_by` when pinning or unpinning messages

Identity resolution order:

1. `-id` flag
2. `COMMSYNC_TUI_ID` environment variable
3. Persisted UUID in `~/.local/state/commsync/tui-instance-id`
4. Freshly generated UUID (saved to the file above for future sessions)

```bash
commsync-tui -id operator
COMMSYNC_TUI_ID=operator commsync-tui
```

The current identity is shown in the help overlay (`?`) and the about overlay (`v`).

---

## Poll Cadence

The TUI auto-polls the database every 2 seconds by default. Each poll refreshes messages, pins, rooms, topics, and agents. Override with `-poll`:

```bash
commsync-tui -poll 5s
commsync-tui -poll 30s
```

The TUI never writes to the database. Write operations (pin, unpin, ack pin) are dispatched by shelling out to the `commsync` binary using its `call` subcommand. The binary path is auto-detected (same directory as `commsync-tui`, then `PATH`); override with `COMMSYNC_BIN`.

---

## Build

```bash
cd tui
go build -o ~/.local/bin/commsync-tui .
```

The TUI is a separate Go module in the `tui/` subdirectory. It requires the `commsync` binary at runtime for write operations but does not import it as a Go package.
