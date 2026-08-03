-- 003_v3.sql — observer v3: keyset and filter composite indexes.
--
-- Strictly per RELAY_OBSERVABILITY.md "PostgreSQL Schema Contract" and the
-- T5.2 performance findings: the keyset page lists order by
-- (last_seen DESC, id DESC) / (occurred_at DESC, id DESC), which the v1
-- single-column indexes cannot serve without a sort node; and the session
-- list's model filter is an EXISTS probe over (session_id, model), which the
-- separate single-column indexes cannot serve either. v3 adds the three
-- composite indexes:
--   * idx_observer_sessions_last_seen_id — keyset session paging
--   * idx_observer_turns_occurred_at_id — keyset turn paging
--   * idx_observer_turns_session_id_model — model-filtered EXISTS probes
--
-- The script is idempotent (IF NOT EXISTS) and is executed inside the
-- bootstrap transaction, after the short advisory lock, lock_timeout, and
-- statement_timeout are set. It must never be executed outside that
-- bootstrap path. It runs both on an empty schema (right after 001_v1.sql
-- and 002_v2.sql) and as the v1/v2 -> v3 upgrade of a complete schema.

CREATE INDEX IF NOT EXISTS idx_observer_sessions_last_seen_id ON observer_sessions (last_seen DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_observer_turns_occurred_at_id ON observer_turns (occurred_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_observer_turns_session_id_model ON observer_turns (session_id, model);

-- The schema is complete only with its version row. ON CONFLICT keeps the
-- whole script idempotent under the bootstrap transaction.
INSERT INTO observer_schema_versions (version, applied_at)
VALUES (3, now())
ON CONFLICT (version) DO NOTHING;
