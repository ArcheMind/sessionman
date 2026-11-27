# SessionMan Agent Rules

Repository state starts at `.agent/state/INDEX.md`.

## Mandatory Execution-Layer Rules for Adding or Modifying Agents (User Ruling 2026-11-22)

Before adding a new Agent adapter or changing any headless execution path (`sm session continue` or `ExternalSubagent`), obey these three invariants:

1. **Single execution path per agent (no dual tracks)**: Never build a separate OneShot/print-mode transport for `session continue` alongside a persistent protocol for `ExternalSubagent`. Both run one client that implements `headless.Session` (`internal/headless`): Claude on native stream-json (`internal/claudestream`), Codex on `codex app-server` (`internal/codexrpc`), AGY on its native ACP v1 (`internal/acp`), Pi on RPC (`internal/pirpc`). `internal/execution` opens the client; callers do not branch per agent.
2. **No extra user dependencies first, ACP second**: An execution path may require only what the user already installed (`sm` and the agent's CLI); never depend on a wrapper binary such as `claude-agent-acp` or `codex-acp`. Among dependency-free protocols, prefer ACP (`internal/acp`) when the agent's own CLI speaks it; otherwise use the CLI's own headless protocol.
3. **Session ownership stays in SM; the protocol only loads native sessions**: SM owns unified-history conversion, native session preparation (`internal/sessions`), S/C directory isolation (`internal/nativedirs`), hook mounting, and reading back the native store for complete canonical deltas. The protocol client only resumes the SM-prepared native session and executes turns. Never invent a second path because a protocol has no history-injection API.

## Step Completion: Commit and Merge to `main`

Every step ends only when its changes are committed, merged into `main`, and pushed to `origin/main`. Uncommitted working-tree edits or unmerged branch commits do not count as complete.
