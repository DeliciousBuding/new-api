# Agent corpus fixtures

Fully synthetic, de-identified request samples for the Relay Observer capture
chain. No API keys, no tokens, no real session/thread ids, no user content —
every id is a `fx-*` fixture id and every message is placeholder text.

## Files

| file | format | scope | shape |
|------|--------|-------|-------|
| `codex-small.json` | responses | codex_cli | single user input + turn metadata header |
| `codex-multiturn.json` | responses | codex_cli | tool invocation loop, latest activity at the tail |
| `claude-small.json` | messages | claude_cli | single user message + session header |
| `claude-tool-chain.json` | messages | claude_cli | tool_use / tool_result adjacency chain |
| `boundary-full-fit.json` | responses | codex_cli | canonical capture fits the configured budget without truncation |
| `boundary-exact-fit.json` | chat | codex_cli | capture total exactly equal to the limit (full-fit bypass) |
| `boundary-below-gap-marker.json` | messages | claude_cli | capture limit below the gap marker itself |

Boundary fixtures carry an explicit `format` field in `_meta`; the corpus
harness infers the format of the other samples from the body shape.

## Per-sample record (the `_meta` block)

Every fixture carries the capture metrics needed to verify admission and canonical capture budgets:

- `raw_body_bytes` — the request body size as admitted (reservation input)
- `capture_limit` — the canonical capture budget derived from it (for the
  boundary fixtures, deliberately tuned to pin the capture boundary)
- `scope_expect` / `sources_expect` — the identity resolution contract

The normalize/capture outcome matrix (normalized_item_count, unbounded
canonical bytes, persisted counts, omitted counts, content_state,
session_created) is produced by the corpus harness that consumes these
fixtures; the samples themselves stay static so the corpus is comparable
across runs and machines (same policy as the golden normalizer corpus in
`../normalizer/`).

## Regeneration policy

The `_meta.raw_body_bytes` values must stay in sync with the actual serialized
body. To regenerate after editing a fixture, serialize `body` back to JSON
(compact) and update `raw_body_bytes` and `capture_limit` accordingly.
