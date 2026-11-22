# SessionMan (`agentx`) Agent Rules

Repository state starts at `.agent/state/INDEX.md`.

## Mandatory Execution-Layer Rules for Adding or Modifying Agents (User Ruling 2026-11-22)

Before adding a new Agent adapter or changing any headless execution path (`sm session continue` or `ExternalSubagent`), read `.agent/state/coding-agent-runtime/design/acp-execution-unification.md` and obey these three invariants:

1. **Single execution path per agent (no dual tracks)**: Never build a separate OneShot/print-mode transport for `session continue` alongside a persistent protocol for `ExternalSubagent`. Both must share one execution client.
2. **ACP first**: When integrating a new agent (or converging an existing one), check native or official ACP support first and reuse `internal/acp` (`acp.Client`). Only fall back to a thin native RPC (like `internal/pirpc` for Pi) when no usable native/official ACP exists and the community wrapper lacks required capabilities or breaks isolation.
3. **Session ownership stays in SM; ACP only loads native sessions**: SM owns unified-history conversion, native session preparation (`internal/sessions`), S/C directory isolation (`internal/nativedirs`), hook mounting, and reading back the native store for complete canonical deltas. ACP only needs `session/load` or `session/resume` to open the SM-prepared native session and execute turns. Never reject ACP or invent a second path because "ACP has no history-injection API".
