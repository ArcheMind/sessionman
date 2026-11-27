# SessionMan

[![CI](https://github.com/ArcheMind/sessionMan/actions/workflows/ci.yml/badge.svg)](https://github.com/ArcheMind/sessionMan/actions/workflows/ci.yml)
[![CI platforms](https://img.shields.io/badge/CI%20platforms-Linux%20%7C%20macOS%20%7C%20Windows-2ea44f)](https://github.com/ArcheMind/sessionMan/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ArcheMind/sessionMan?display_name=tag)](https://github.com/ArcheMind/sessionMan/releases)
[![License](https://img.shields.io/github/license/ArcheMind/sessionMan)](LICENSE)

[Website](https://sessionman.dev/)

**SessionMan (`sm`) helps you keep working across coding agents.**

### **Load** your Claude Code session in Codex when your usage runs out, and keep working.

![Select a Claude Code session and choose Codex in the interactive home](docs/assets/readme/load.gif)

### **Offload** session history and continue seamlessly.

![Choose automatic offload and an agent interactively](docs/assets/readme/trim.gif)

Select a session and press `m` for manual loading, or choose `m` at the load prompt. Each user prompt represents a full turn and starts as Keep. Use `r` / `o` / `d` to set Keep / Offload / Delete, `V` with `j` / `k` to select a range, and Enter to load. Only offloaded turns are written to the referenced history file.

### **Find** an old session across all your agents and get back to work.

![Browse sessions across agents and workspaces](docs/assets/readme/find.gif)

### **Ship** your session to someone else so they can pick up where you left off.

![Press e on a session to export it from the interactive home](docs/assets/readme/ship.gif)

Minimal footprint, near-zero dependencies — easy to integrate.

```console
$ sm                # every recent session, every agent
$ sm codex          # start a new Codex session
```

## One list of every agent's work

`sm` with no arguments opens one screen with your presets and the recent sessions of every agent, grouped by project.

Press `/` to filter the displayed sessions by their full user prompts, including earlier messages. Space-separated terms must all match; matching ignores case and supports Chinese substrings. Use arrow keys to select, Enter to open, and Esc to clear the search. Backspace edits the query. Search runs in memory without a persistent index.

Start from a preset or resume recent work:

```text
New session · Presets
› codex-review  Codex · gpt-5.6-luna · low     Read-only research and code review using native …
  pi-review     Pi · openai-codex/gpt-5.6-luna  Read-only independent research and code review u…
  + Create preset…

Recent sessions · Current workspace
  claude    Sep 11 05:08 (18m)      Design the interactive session-resume experience for the…
  codex     Sep 11 04:42 (47m)      Refactor the OAuth callback and tighten token refresh ha…
  pi        Sep 11 03:24 (44m)      Reduce duplicate state in the keyboard navigation handler

Recent sessions · Global
  codex     Sep 10 12:20 (1h 15m)   ../data-pipeline      Fix the streaming response parser …
  claude    Sep 10 10:48 (33m)      ../api-gateway        Review the production timeout inci…
  pi        Sep 10 06:15 (55m)      ../infrastructure     Add a preflight check before the p…

↑/↓ or j/k select · Enter open · e export session · m manage presets · q/Esc cancel
```

Non-interactive and filtered:

```console
sm session list --all --limit 20
sm session list --source codex --all --include-subagents
sm session show <session-id> --source claude --peek
```

Child Agent sessions stay hidden unless `--include-subagents` is set.

## Continue it in any agent

Press Enter on a session, then choose who continues it:

```text
Choose an agent:
> agy
  claude
  codex
  pi
```

![Interactive session resume](docs/assets/interactive-resume.gif)

The same hand-off without the screen:

```console
sm session resume codex <claude-session-id> --source claude
```

SessionMan converts the session into the target agent's native transcript format and starts its native resume (`agy --conversation`, `claude --resume`, `codex resume`, `pi --session`, …). Source sessions are only read, never modified.

## Move it to another machine

Press e on a session in the interactive home to export it as a unified transcript file (`<agent>-<title>-<id>.json`), or stream it:

```console
sm session export <session-id> --source claude > work.json
sm session import codex work.json      # write into codex's native store, print the new session id
sm --resume work.json                  # import and resume interactively in one step
```

## Call another agent's preset from inside yours

A preset saves agent, model, effort, and when to use it. The same collection feeds `sm` and the MCP tools, so an agent launched through `sm` (for example `sm claude` or `sm codex`) gets the ExternalSubagent tools automatically and can run your other presets directly:

```console
$ sm preset list
claude-review        claude     static requirements satisfied; login and model access verified per execution
pi-review            pi         static requirements satisfied; login and model access verified per execution

$ sm codex
> Ask my claude-review preset to review this change.
```

Each call runs the preset's own native agent in its own session. No relay inference call, no per-task registration. SessionMan passes no permission flags by default (`mode: "normal"`), so each agent's own configuration decides what it may do; `mode: "yolo"` bypasses the agent's permission checks and sandbox. `isolation: "worktree"` runs the subagent in a fresh git worktree; it only changes where the agent works.

```text
StartExternalSubagent · SendExternalSubagentMessage · StopExternalSubagent · ListExternalSubagentSessions
```

Exact inputs, limits, and acceptance evidence: [docs/subagents.md](docs/subagents.md).

## Configure once, every agent follows

```text
~/.agents/skills/<name>/SKILL.md        shared user skills (projected on takeover)
<C>/skills/<name>/SKILL.md              unified skills under --config-dir / SM_CONFIG_DIR
<C>/mcp_config.json                     unified MCP servers (claude · codex · agy)
<C>/hooks.json                          portable hooks (claude · codex · pi · agy)
~/.sessionman/presets/<name>/PRESET.md  sm · the ExternalSubagent tools above
```

When `--config-dir <C>` (`SM_CONFIG_DIR`) is set — or when a subagent runs with `unified_config` enabled (the default, using `~/.sessionman/config` when `SM_CONFIG_DIR` is unset) — SessionMan composes an isolated shadow root per agent without polluting your native `~/.claude`, `~/.codex`, `~/.pi`, or `~/.gemini` directories:

- **Skills**: `~/.agents/skills/*` and `<C>/skills/*` are uniformly symlinked into each agent's managed `skills/` directory (`claude`, `codex`, `pi`, `agy`), with agent-specific `<C>/<agent>/skills/*` taking precedence on name collisions.
- **MCP**: `<C>/mcp_config.json` (`mcpServers`) is mounted into `claude` (via a generated `--mcp-config`), `codex` (merged into shadow `config.toml`), and `agy` (merged into shadow `mcp_config.json`), preserving agent-specific overrides.
- **Hooks**: `<C>/hooks.json` is compiled into each agent's isolated/shadow root.

```console
$ sm skill install https://github.com/example/agent-skills.git --path skills/review --scope user
$ sm hook list
SCOPE    EVENT             COMMAND
user     SessionStart      echo session started
user     UserPromptSubmit  echo prompt submitted
user     Stop              echo stopped
```

A preset is one small file:

```markdown
---
agent: codex
model: gpt-5.6-luna
effort: low
---

Read-only research and code review. Not for large refactors.
```

Launch, resume, and headless continuation share parameter handling. Use
`--model`, `--effort`, `--mode normal|yolo`, `--user-config true|false`,
`--append-system-prompt`, and `--mcp-config` where the target agent and transport
support them; native arguments follow `--`. Preset launches and resumes also
forward the saved `flags` and `user_config`. Subagents default to `unified_config: true`
(taking over `SM_CONFIG_DIR` or `~/.sessionman/config`); set `unified_config: false`
in `PRESET.md` or the interactive preset editor to keep native directories directly.
Unsupported combinations and known conflicts between unified fields and native flags
are reported explicitly.
Codex CLI YOLO uses `--dangerously-bypass-approvals-and-sandbox`; its app-server
uses the corresponding thread policy fields. `approval_policy="never"` alone
only disables approval prompts and does not remove a sandbox.

`sm` shows `+ Create preset…` on the home screen and `m` edits the highlighted one. Native config stays native: SessionMan never merges into your Claude, Codex, or Pi configuration; hook and skill bridges remain visible to native discovery and trust review. For agy, SessionMan owns a marked plugin under `~/.gemini/config/plugins/sessionman`; it preserves unrelated native configuration.

## Install

### macOS and Linux

```bash
curl -fsSL https://sessionman.dev/install | sh
```

Downloads the release artifact for the current platform, verifies its SHA-256 checksum, and installs `sm` to `$HOME/.local/bin`. Set `SM_INSTALL_DIR` for another directory.

### Go

```bash
go install github.com/ArcheMind/sessionman/cmd/sm@latest
```

The installed executable is `sm`.

Prebuilt archives for macOS, Linux, and Windows are on the [releases page](https://github.com/ArcheMind/sessionMan/releases). See [installation](docs/installation.md) for version pinning, verification, Windows setup, and source builds.

Run `sm update` to immediately check for and install the latest stable release, bypassing the daily cache. Interactive bare `sm` checks for a stable update at most once a day and asks before installing it. A successful update exits without starting a session. Set `SM_NO_UPDATE_CHECK=1` to disable the startup check and prompt. Agent commands, MCP, and other non-interactive invocations never check for updates.

## Supported agents

| Agent | Sessions | Continue in it | Reads `~/.agents/skills` | Portable hooks |
| --- | --- | --- | --- | --- |
| Antigravity CLI (`agy`) | yes | yes | project roots; user install projection; unified `SM_CONFIG_DIR` | `SessionStart`, `Stop` |
| Claude Code | yes | yes | yes, via `.claude/skills` | yes |
| Codex CLI | yes | yes | yes | yes |
| DeepSeek Harness | — | — | yes | — |
| Pi | yes | yes | yes | yes |

Antigravity CLI 1.5.8 supports native SQLite sessions, model discovery, presets and ACP ExternalSubagents. `sm session continue agy` places `--append-system-prompt` in a `<system-prompt>` text block before the original user text in the same message. `sm agent exec agy` rejects `--append-system-prompt`. Both commands reject temporary `--mcp-config`. ACP supports session-scoped MCP. Provider effort is unverified. Its owned plugin is also discovered by plain `agy`, whose MCP bridge requires launching through `sm` to bind the workspace. See [agy acceptance and boundaries](docs/agy-acceptance.md).

DeepSeek Harness has no verified session source, so it is not a session or resume target. SessionMan can still install, authenticate, and launch it. DeepSeek Harness is not part of the actively developed skill and hook integrations.
Codex transcript import resolves its provider through app-server configuration
inspection; imports using `--profile` or `user_config: false` are rejected because
that inspection cannot honor those selections. Existing native sessions do not
require this import step. Exact native versions and evidence: [lifecycle and protocol audit](docs/lifecycle-and-protocol-audit.md).

## Command reference

```text
sm
sm --resume <transcript-file>
sm <agent> [--model <model>] [--cwd <path>] [--dry-run] [-- <native args...>]
sm [--json|--yaml] status [--usage]

sm [--json|--yaml] agent <list|show|install|updates|update|run|exec> [args...]
sm [--json|--yaml] agent updates <agent> [--install-root <dir>]
sm [--json|--yaml] agent update <agent> --install-root <dir>
sm [--json|--yaml] auth <list|show|usage|login|logout> [args...]
sm [--json|--yaml] session <list|show|resume|export|import|continue> [args...]
sm [--json|--yaml] skill <list|show|install> [args...]
sm [--json|--yaml] hook list
sm [--json|--yaml] mcp <list|show|serve> [args...]
sm [--json|--yaml] preset <list|show> [args...]

sm update
sm version
```

`--json` and `--yaml` apply to SessionMan-owned data and dry-run plans; native install, login, launch, and resume keep their own interactive output.

On macOS and Linux, `agent updates` checks stable releases for Claude, Codex, Pi and AGY; `agent update` stages and verifies only the selected installed CLI before activating it. Set `SM_CLI_INSTALL_ROOT` for subsequent SessionMan commands to use the same managed directory. With this directory set, Codex selects the highest verified stable version in that directory or `PATH`; without it, SessionMan keeps normal `PATH` selection. Windows reports managed updates as unsupported.

```console
sm status                            # readiness, account, and available models per agent
sm status --usage                    # include subscription quota/rate-limit windows
sm auth usage                        # query remaining quota across supported agents
sm agent install codex --dry-run     # then without --dry-run to install
sm auth login codex                  # native authentication
sm agent show codex                  # binary path, capabilities, models
sm skill list                        # skills by filesystem source and reader
sm mcp list                          # MCP servers by filesystem source and reader
sm agent exec codex --model <id> < prompt.txt
sm --json session resume pi <session-id> --source claude --dry-run
sm --json session continue claude --mode yolo --mcp-config mcp.json < transcript.json
sm session continue claude --stream < transcript.json
```

`session continue --stream` writes NDJSON: completed canonical Protocol 2.0 transcript entries
as `{"type":"entry","entry":...}`, then exactly one
`{"type":"result","session":...}` (a Protocol 2.0 session) after success. The result is the authoritative
delta; entry events form an identical ordered prefix, with the same IDs,
parents, tool IDs, content, and timestamps. Imported history and the echoed prompt
are excluded. Errors and cancellation produce no successful result. Without
`--stream`, the final structured output is the same Protocol 2.0 delta session.

Claude, Codex, and Pi emit messages during execution. Codex's trailing assistant record is
held until a following message closes it (its parser merges consecutive reasoning
and tool calls); the final record is flushed after execution. Output is based on
completed native store records, so visibility depends on the agent flushing its
store. Agy accepts `--stream` with deferred delivery: after execution succeeds,
it emits the final transcript's messages followed by the result. No messages
are emitted while Agy is running.

## Boundaries

SessionMan owns the workflow around agents, not the agents:

- no universal agent settings file beyond the documented skills, hooks, unified `mcp_config.json` (in isolated `SM_CONFIG_DIR` roots), and presets;
- no credential storage or proxying;
- no replacement package managers or session databases;
- no invented capabilities or fallbacks when a native source is missing.

Read [architecture](docs/architecture.md) and [product principles](docs/product-principles.md) for the reasoning.

## Development

Go 1.26.0 or later.

```bash
make verify
./bin/sm version
```

`make verify` is the repository contract: formatting, static checks, tests, protocol audits, build, and smoke checks. Read [CONTRIBUTING.md](CONTRIBUTING.md) before proposing a change. Regenerate the terminal demo with `vhs docs/demo.tape`.

## Security and support

Report vulnerabilities privately per [SECURITY.md](SECURITY.md). Usage questions and confirmed bugs go through [SUPPORT.md](SUPPORT.md).

SessionMan is an independent open-source project, not affiliated with Anthropic, OpenAI, Google, or Pi's maintainers. Product names belong to their respective owners.

## License

[MIT](LICENSE) © 2026 ArcheMind
