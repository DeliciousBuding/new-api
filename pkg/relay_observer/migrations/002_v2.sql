-- 002_v2.sql — observer v2: content object creation timestamps.
--
-- Strictly per RELAY_OBSERVABILITY.md "Retention": content is deleted only
-- after no retained context references it, and the orphan grace period
-- (RETENTION_CONTENT_DAYS) needs a timestamp on observer_content_objects,
-- which v1 did not have. v2 adds created_at, plus the two indexes the
-- bounded retention pass relies on:
--   * idx_observer_content_objects_created_at — the orphan candidate scan
--     predicate (created_at < cutoff) is indexed;
--   * idx_observer_contexts_item_digests (GIN) — the reference-safety check
--     (JSONB containment, item_digests @> ...) is an indexed probe, never a
--     full context scan.
--
-- The script is idempotent (IF NOT EXISTS / ON CONFLICT DO NOTHING) and is
-- executed inside the bootstrap transaction, after the short advisory lock,
-- lock_timeout, and statement_timeout are set. It must never be executed
-- outside that bootstrap path. It runs both on an empty schema (right after
-- 001_v1.sql) and as the v1 -> v2 upgrade of a complete v1 schema.

ALTER TABLE observer_content_objects ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS idx_observer_content_objects_created_at ON observer_content_objects (created_at);
CREATE INDEX IF NOT EXISTS idx_observer_contexts_item_digests ON observer_contexts USING GIN (item_digests);

-- The schema is complete only with its version row. ON CONFLICT keeps the
-- whole script idempotent under the bootstrap transaction.
INSERT INTO observer_schema_versions (version, applied_at)
VALUES (2, now())
ON CONFLICT (version) DO NOTHING;
