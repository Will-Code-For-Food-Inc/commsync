# Agent Integration Guide

How to wire commsync into every AI coding assistant you own or operate, and how to run well inside the corridor once it's connected.

Related: [Protocol reference](protocol.md) | [CLI reference](cli.md) | [TUI reference](tui.md)

---

## Call-Sign Selection

Your call-sign is the string you pass as `from` in every message. It is load-bearing: the operator watching the TUI and any other agents in the corridor use it to distinguish you at a glance.

**Good call-signs:**
- Project or workspace names: `battle-buddy`, `ops-central`
- Character names: `sazed`, `vin`, `juno`, `riker`
- Role-qualified names: `review-agent`, `copilot-infra`, `codex-pr-123`

**Bad call-signs:** `assistant`, `agent`, `agent-1`, `claude`, `model`. These are noise — they identify the model family, not the agent instance.

A good call-sign is unique within the workspace, memorable in a chat log, and gives the operator a mental handle for which process is talking.

---

## Operating Discipline

### Startup Checklist

Every agent, on every session start, should do this before touching any files or code:

1. `register_instance` — with your call-sign, so broadcast pins can compute `fully_delivered`.
2. `get_protocol` — loads the rules and confirms the DB path.
3. `list_messages` with `concerns: "<your-call-sign>"` — checks what is live and concerns you.
4. `list_pins` with `target_instance: "<your-call-sign>"` — checks for pinned tasking.

### During Work

- Post brief state changes: taking a task, blocked on something, handing off, done.
- Use `ack_message` on messages you have fully handled, so the next agent does not re-plow the same ground.
- When waiting for guidance and polling for updates, allow 5–10 minutes between polls — not every minute.
- Keep messages short. Long analysis belongs in a separate document; link it in `refs`.
- If human action is required, put `MEATSPACE I/O:` on its own line in the message body.

### Completing a Task

Post a final `info` or `decision` message, ack any messages you consumed, and then go quiet.

---

## Claude Code

### MCP server config

Add to `~/.claude.json` (user-level, all projects) or `.claude/settings.json` (project-level):

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

### Operating discipline hooks

Add to your project's `CLAUDE.md` or as a custom system prompt instruction:

```markdown
## commsync corridor

You have access to a commsync MCP server. Follow this discipline on every session:

**Startup (before touching any files):**
1. Call `mcp__commsync__register_instance` with your call-sign.
2. Call `mcp__commsync__get_protocol` to load the rules.
3. Call `mcp__commsync__list_messages` with `concerns: "<your-call-sign>"`.
4. Call `mcp__commsync__list_pins` with `target_instance: "<your-call-sign>"`.

**During work:**
- Post brief state updates when your status changes.
- Ack messages you have handled.
- Poll at 5–10 minute intervals when idle, not every minute.
- Keep messages short; link long analysis in `refs`.

Your call-sign for this session is: `<your-call-sign>`
```

Replace `<your-call-sign>` with the actual call-sign for this agent instance.

---

## GitHub Copilot

### VS Code MCP config

In `.vscode/settings.json` (project) or user settings:

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

### Copilot instructions

In `.github/copilot-instructions.md` (repo-level system prompt):

```markdown
## commsync corridor

You have access to a commsync MCP server. Before starting any task:
1. Call `get_protocol` to load the rules.
2. Call `list_messages` with `concerns: "copilot-<repo>"` to check the corridor.

Post brief state updates when your status changes: taking a task, blocked, handing off, done. Ack messages you have handled. Poll at 5–10 minute intervals when waiting, not every minute. Keep messages short.

Your call-sign is `copilot-<repo>` (replace `<repo>` with the repository name).
```

---

## OpenAI Codex

### `codex.md` or system prompt

```markdown
## commsync corridor

A commsync MCP server is available. Call `get_protocol` on startup to load the protocol rules and confirm corridor state.

**Startup:**
1. `register_instance` with your call-sign.
2. `get_protocol` to load the rules.
3. `list_messages` with `concerns: "<your-call-sign>"`.
4. `list_pins` with `target_instance: "<your-call-sign>"`.

**Operating discipline:**
- Post state changes: taking tasking, blocked, handed off, done.
- Ack messages you have handled.
- Poll at 5–10 minute intervals when waiting, not every minute.
- Keep messages short — this is a chat corridor, not a log file.

Your call-sign is `commander-riker` (or whatever identifies this agent in the workspace).
```

---

## Custom Operating Discipline

The canonical protocol text is stored in the database and returned by `get_protocol`. You can inject custom operating instructions for your workspace by prepending them to your system prompt, or by including them in a `CLAUDE.md` / `copilot-instructions.md` / `codex.md` file.

A useful pattern: treat the corridor's `get_protocol` response as the floor, and your agent-specific prompt as the ceiling. The protocol text tells agents *how* to use the corridor; your custom instructions tell them *what* the current workspace looks like, who else is in the room, and any workspace-specific conventions.

Example workspace preamble:

```markdown
## Workspace agents

| Call-sign | Role |
|-----------|------|
| `battle-buddy` | Lead coordinator (Claude Code) |
| `juno` | Dev execution (Claude Code on ~/operations) |
| `commander-riker` | Review agent (Codex) |
| `copilot-infra` | In-editor assist (VS Code Copilot) |

Default room: `general`. Use room `review` for PR review threads.
```

Include this in each agent's system prompt so every agent knows who else is in the corridor.
