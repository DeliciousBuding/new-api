-- 005_v5.sql — observer v5: transcript index coverage.
--
-- The transcript endpoint (GET /api/relay-observer/sessions/:id/transcript)
-- filters observer_contexts by session_id and orders by id. v1/v3 never
-- added an index for that access path, so the read falls back to a full
-- table scan or a PK scan across every session's rows. v5 adds the
-- composite index that bounds the scan to the session's own context rows
-- and serves the ORDER BY id directly.
--
-- The script is idempotent (IF NOT EXISTS) and is executed inside the
-- bootstrap transaction, after the short advisory lock, lock_timeout, and
-- statement_timeout are set. It must never be executed outside that
-- bootstrap path. It runs both on an empty schema (right after 001_v1.sql
-- through 004_v4.sql) and as the v1..v4 -> v5 upgrade of a complete schema.

CREATE INDEX IF NOT EXISTS idx_observer_contexts_session_id_id ON observer_contexts (session_id, id);

-- The schema is complete only with its version row. ON CONFLICT keeps the
-- whole script idempotent under the bootstrap transaction.
INSERT INTO observer_schema_versions (version, applied_at)
VALUES (5, now())
ON CONFLICT (version) DO NOTHING;
