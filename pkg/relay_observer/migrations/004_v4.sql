-- v4: provider is part of session-alias identity.
--
-- v1 accidentally enforced uniqueness only on the HMAC digest. The same raw
-- session identifier therefore collided across independent providers/profiles
-- (for example Codex and Claude) even though lookup and in-memory tests already
-- treated provider as part of the key. Drop the legacy UNIQUE constraint by
-- its column shape, then replace it with a provider-scoped unique index.
DO $$
DECLARE
    legacy_constraint name;
BEGIN
    SELECT c.conname
      INTO legacy_constraint
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
      JOIN pg_namespace n ON n.oid = t.relnamespace
     WHERE n.nspname = current_schema()
       AND t.relname = 'observer_session_aliases'
       AND c.contype = 'u'
       AND (
           SELECT array_agg(a.attname ORDER BY k.ordinality)
             FROM unnest(c.conkey) WITH ORDINALITY AS k(attnum, ordinality)
             JOIN pg_attribute a
               ON a.attrelid = c.conrelid
              AND a.attnum = k.attnum
       ) = ARRAY['node_scope', 'user_id', 'key_version', 'alias_digest']::name[]
     LIMIT 1;

    IF legacy_constraint IS NOT NULL THEN
        EXECUTE format(
            'ALTER TABLE observer_session_aliases DROP CONSTRAINT %I',
            legacy_constraint
        );
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_observer_session_aliases_identity
    ON observer_session_aliases (
        node_scope,
        user_id,
        key_version,
        alias_digest,
        provider
    );

INSERT INTO observer_schema_versions (version, applied_at)
VALUES (4, now())
ON CONFLICT (version) DO NOTHING;
