-- 005_v9.sql — observer v9: transient session marker.
--
-- Full-content capture binds stateless traffic (requests without a resolvable
-- session identity) to a per-turn transient session. The is_transient marker
-- separates those synthetic sessions from real identity sessions: the session
-- list excludes them by default, while retention cleans them up like any
-- other session (30-day expiry deletes the session, its content, and its
-- turns together).
--
-- The script is idempotent (IF NOT EXISTS) and is executed inside the
-- bootstrap transaction, after the short advisory lock, lock_timeout, and
-- statement_timeout are set. It must never be executed outside that
-- bootstrap path. It runs both on an empty schema (right after 004_v4.sql)
-- and as the v1-v8 -> v9 upgrade of a complete schema.

ALTER TABLE observer_sessions ADD COLUMN IF NOT EXISTS is_transient BOOLEAN NOT NULL DEFAULT false;

-- The schema is complete only with its version row. ON CONFLICT keeps the
-- whole script idempotent under the bootstrap transaction.
INSERT INTO observer_schema_versions (version, applied_at)
VALUES (9, now())
ON CONFLICT (version) DO NOTHING;
