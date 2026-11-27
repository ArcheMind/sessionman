# Unified Transcript Format

The unified transcript format is owned by the shared protocol repository:

https://github.com/ArcheMind/sessionman-protocol

- Spec: `docs/unified-transcript-format.md`
- Normative schema: `schema/unified-transcript.schema.json`
- Go implementation (used by sessionman): `github.com/ArcheMind/sessionman-protocol/go`

sessionman imports the protocol types and defines no transcript message shapes of
its own. Format changes land in sessionman-protocol first, then sessionman upgrades
its pinned module version.

## Antigravity CLI native storage

Antigravity CLI (`agy`) 1.5.8 stores conversations as SQLite databases under
`~/.gemini/antigravity-cli/conversations/<id>.db`. SessionMan reads the native
Step protobuf payloads directly, including committed WAL records. The standalone
native fixture and descriptor field inventory live in
`internal/sessions/testdata/agy/`.

Export uses the same unified transcript schema as every other agent. Images
are embedded as base64; local image and complete tool-result resource files
are resolved during reading, so the exported transcript does not depend on
the original resource paths. Missing resources and unsupported native payloads
produce explicit errors. If native readable thinking field 3 and raw thinking
field 16 both contain different nonempty values, parsing reports unsupported
data rather than discarding one: their semantic mapping and signature binding
have not been established.

Import creates a new conversation UUID, a new native SQLite database and an
entry in `conversation_summaries.db`. It does not clone a source database or
retain a raw database backup in the unified transcript. Source files are only
read. Native conversion output for `agy` is SQLite bytes, not JSON text.

## Thinking input and output policy

Thinking text and its signature are distinct. Native input parsing and the
unified transcript preserve readable thinking, native signatures, and opaque
ciphertext, including thoughts that contain only ciphertext. This input
representation does not authorize replay of those signatures.

Every target agent (Claude, Codex, Pi, and Antigravity CLI) follows the same
output policy for session import and native conversion, including conversion
back to the source agent:

- Never restore thinking signatures or encrypted thinking payloads.
- Emit readable thinking as ordinary text. The Claude Code target writes a text
  block `<text>\n※` (the raw text, a newline, then U+203B), because leading
  markers and `<think>`-style wrappers in Claude transcripts have triggered
  Anthropic `reasoning_extraction` refusals on resume. Other targets wrap the
  text in `<think>` and `</think>`. The importer accepts both forms.
- Drop thoughts containing only ciphertext; do not emit empty thinking blocks
  or an assistant message left empty by this removal.
- Preserve other supported text, tool calls, linked tool results, and images
  according to the destination's native format.

For example, a unified thinking block with `text: "Inspect the files."` and a
native signature becomes this ordinary native text, without its signature
(shown in the `<think>` form used by non-Claude targets):

```text
<think>
Inspect the files.
</think>
```

Reparsing that standalone text block lifts it into unsigned unified thinking;
it does not recover the original signature or ciphertext. Native source files
and resource files remain read-only. Tool call identifiers remain linked to
their results, including complete results recovered from local native resource
files. The shared protocol schema is unchanged; this is an sessionman output
policy and does not require an `originAgent` field.

See [Antigravity acceptance](agy-acceptance.md) for the conversion matrix and
live evidence boundaries.
