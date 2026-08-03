# Relay Observer

Relay Observer is NewAPI's bounded, fail-open request observability subsystem. It records relay metadata and a budgeted canonical view of request context without making the relay request depend on PostgreSQL, the observer worker, or the Root UI.

This document is the architecture and operations SSOT for `pkg/relay_observer`, the `/api/relay-observer/*` Root endpoints, and `web/src/features/observability`.

## 1. Non-negotiable boundaries

- **Relay remains authoritative.** Observer admission, normalization, persistence, query, or retention failure must never fail or delay the model request.
- **The request path never touches PostgreSQL.** It performs bounded extraction, one atomic byte reservation, and a non-blocking enqueue only.
- **Memory is bounded twice.** Queue count and reserved bytes are independent limits; one request is additionally capped by `MaxRequestBytes` and `CaptureLimitBytes`.
- **No raw session identifier is stored.** Session aliases are full-width HMAC-SHA-256 digests with an explicit key version.
- **Queries are Root-only.** Every `/api/relay-observer/*` route is behind `middleware.RootAuth`.
- **Status contains no secrets.** DSNs, HMAC material, request bodies, and canonical content never appear in `/status`.
- **No disk spool or retry storm.** A failed metadata batch is dropped and opens the bounded circuit. Content append retry is memory-bounded and idempotent.

## 2. Request capture and formats

The relay may convert a client request before sending it upstream. Observer keeps those concepts separate:

- `RelayFormat` is the final upstream format persisted with the turn.
- `CaptureRelayFormat` is worker-only and describes the original DTO supplied to the normalizer.

This distinction is required for conversions such as Claude client input routed through an OpenAI-compatible upstream. Persisted relay analytics still report the final upstream format, while canonical content is decoded using the actual in-memory DTO shape.

Supported canonical input families are OpenAI Chat Completions, OpenAI Responses, and Claude Messages. Unsupported or over-budget input degrades to `metadata_only` or a canonical `gap` marker; it does not affect relay success.

## 3. Admission, worker, and circuit

`Dispatcher.TryEnqueue` is the sole request-path entry point.

1. Reject invalid or oversized reservations.
2. Reject when stopped, queue/byte budgets are exhausted, or the write circuit is open/probing.
3. Reserve bytes atomically.
4. Register queue count before the non-blocking channel send.
5. Roll back both counters on every failed or panicking path.

One worker owns batching and PostgreSQL writes. A write failure opens an exponential circuit starting at 5 seconds and capped at 5 minutes. After cooldown, exactly one half-open probe is admitted; success closes the circuit and failure reopens it.

`RecentVolume` is the number of accepted events in the current wall-clock second. It is maintained as a timestamped atomic bucket and is independent of the configurable flush interval.

## 4. Identity and schema

An alias identity is:

```text
(node_scope, user_id, key_version, alias_digest, provider)
```

`provider` is the detected client profile (`codex_cli`, `codex_desktop`, `claude_cli`, and so on). It is part of identity because the same raw value may legitimately occur in independent profiles.

Schema v4 enforces this contract with the unique index:

```text
idx_observer_session_aliases_identity
```

on all five columns. Migration `004_v4.sql` removes the legacy four-column uniqueness constraint and creates the provider-scoped index. This preserves separate Codex and Claude sessions while retaining continuity for repeated turns inside each profile.

Schema modes:

- `bootstrap`: create an empty schema or upgrade a complete known prefix (`v1`, `v2`, or `v3`) transactionally to current.
- `verify`: perform bounded structural checks only; no DDL or data scan. Historical complete prefixes are accepted as upgrade-pending, while current v4 additionally requires the v2 `created_at` column and v4 alias index.

Unknown, partial, or lying schemas fail observer startup closed; the main relay process remains fail-open with observer disabled and a stable reason code.

## 5. Content persistence and exactly-once behavior

Metadata is inserted idempotently by event identity. Content append then resolves the provider-scoped primary alias and claims the metadata turn with:

```sql
UPDATE observer_turns
SET session_id = $1
WHERE id = $2 AND session_id IS NULL
```

The affected-row count is the durable exactly-once gate. Claim, session counters, canonical objects, context, and head update share one transaction. Replays and concurrent duplicates therefore cannot double-count a turn.

Canonical objects are compressed and deduplicated per session by digest. Context storage uses checkpoint/suffix groups; reconstruction verifies HMACs and emits explicit gaps for unavailable content rather than inventing data.

## 6. Queries and UI contract

Routes:

```text
GET /api/relay-observer/status
GET /api/relay-observer/overview
GET /api/relay-observer/sessions
GET /api/relay-observer/sessions/:id
GET /api/relay-observer/sessions/:id/turns
GET /api/relay-observer/turns/:id/context?session_id=...
```

Database queries have bounded timeouts and keyset pagination. A timeout or unavailable store returns HTTP 200 with a degraded envelope:

```json
{
  "success": true,
  "data": {
    "degraded": true,
    "reason": "timeout",
    "message": "..."
  }
}
```

The frontend validates every response with Zod at the network boundary. Contract drift becomes a normal React Query error state instead of propagating malformed data into components. Session and turn rows support mouse, Enter, and Space selection.

## 7. Retention and lock order

Retention runs at most once every six hours and deletes in bounded segments. Turn and content retention windows are configured separately. Supported values are clamped to 1–3650 days so cutoff arithmetic cannot overflow.

The shared serialization boundary is the session row:

```text
append:    session -> content -> head
retention: session -> head
```

No path takes head/content before the session boundary. PostgreSQL integration tests lock this order and the concurrent exactly-once behavior.

## 8. Configuration

Observer is opt-in. Required production inputs include an independent PostgreSQL DSN and HMAC key. Keep them in host-local secret storage; never put them in Git, image labels, status responses, or the main application DSN.

Important bounds:

- HMAC key versions: `0..32767`, matching PostgreSQL `SMALLINT`.
- Retention: `1..3650` days.
- Query timeout: clamped to the code maximum.
- Capture budget: use a conservative production value (normally 64–256 KiB), not the largest code default merely because memory is available.
- IP capture: disabled unless the deployment has an explicit, documented trust model.

Key rotation uses current and optional previous HMAC key/version slots. New aliases use the current version; previous aliases remain resolvable during the rotation window.

## 9. Required release evidence

A candidate is not complete until all applicable evidence is green:

```text
root + relaykit go vet/build/test
pkg/relay_observer race tests
service conversion-wiring race tests
full relay_observer PostgreSQL integration suite
v1/v2/v3 -> v4 migration lifecycle
Codex/Claude equal raw alias continuity on real PostgreSQL
OpenAI Chat / Responses / Claude request E2E
frontend frozen install, typecheck, tests, production build
Root auth matrix and malformed-response UI behavior
queue/circuit/fault-injection regression
benchmark comparison against the retained baseline
```

A source merge does not imply a public release. Tagging, immutable image digest creation, deploy-pin updates, canary, bake, and rollback are separate controlled steps.
