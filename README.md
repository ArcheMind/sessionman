# SessionMan

## Install

macOS / Linux:

```bash
curl -fsSL https://sessionman.dev/install | sh
```

[Windows & other installation options](docs/installation.md)

## What you can do

### **Load** your Claude Code session in Codex when your usage runs out, and keep working.

![Load a Claude Code session in Codex](docs/assets/load.png)

### **Trim** a bloated session and continue seamlessly.

![Trim a long session before resuming](docs/assets/trim.png)

### **Find** an old session across all your agents and get back to work.

![Browse sessions across agents and workspaces](docs/assets/find.png)

### **Ship** your session to someone else so they can pick up where you left off.

![Export a session and resume the shared transcript](docs/assets/ship.png)

## Supported agents

| Agent | Sessions | Continue in it | Shared skills | Portable hooks |
| --- | --- | --- | --- | --- |
| Antigravity CLI (`agy`) | ✓ | ✓ | Project roots; user install projection | `SessionStart`, `Stop` |
| Claude Code | ✓ | ✓ | Via `.claude/skills` | ✓ |
| Codex CLI | ✓ | ✓ | ✓ | ✓ |
| DeepSeek Harness | — | — | ✓ | — |
| OpenCode | ✓ | Native sessions only | ✓ | — |
| Pi | ✓ | ✓ | ✓ | ✓ |

Shared skills are read from `~/.agents/skills`. OpenCode can resume its own sessions, but cannot resume imported sessions. DeepSeek Harness has no verified session source.

[Agent support details](docs/lifecycle-and-protocol-audit.md)

[Subagents](docs/subagents.md) · [Architecture](docs/architecture.md) · [Contributing](CONTRIBUTING.md) · [Support](SUPPORT.md) · [MIT License](LICENSE)
