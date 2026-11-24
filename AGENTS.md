# SessionMan (`agentx`) Agent Rules

Repository state starts at `.agent/state/INDEX.md`.

## Mandatory Execution-Layer Rules for Adding or Modifying Agents (User Ruling 2026-11-22)

Before adding a new Agent adapter or changing any headless execution path (`sm session continue` or `ExternalSubagent`), obey these three invariants:

1. **Single execution path per agent (no dual tracks)**: Never build a separate OneShot/print-mode transport for `session continue` alongside a persistent protocol for `ExternalSubagent`. Both run one client that implements `headless.Session` (`internal/headless`): Claude on native stream-json (`internal/claudestream`), Codex on `codex app-server` (`internal/codexrpc`), AGY on its native ACP v1 (`internal/acp`), Pi on RPC (`internal/pirpc`). `internal/execution` opens the client; callers do not branch per agent.
2. **Native protocol first, no third-party wrappers**: Use the agent's own headless protocol. Reuse `internal/acp` only for agents whose native CLI speaks ACP. Never depend on a community wrapper binary (it would not use the user's installed CLI and may not be installed at all).
3. **Session ownership stays in SM; the protocol only loads native sessions**: SM owns unified-history conversion, native session preparation (`internal/sessions`), S/C directory isolation (`internal/nativedirs`), hook mounting, and reading back the native store for complete canonical deltas. The protocol client only resumes the SM-prepared native session and executes turns. Never invent a second path because a protocol has no history-injection API.
