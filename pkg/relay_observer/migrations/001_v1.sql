-- 001_v1.sql — observer v1 schema.
--
-- Strictly per RELAY_OBSERVABILITY.md "PostgreSQL Schema Contract": tables,
-- columns, constraints, and the required index coverage (session recency,
-- turn time, user/profile/model filters, content digest lookup) match the
-- contract. No extensions are required.
--
-- The script is idempotent (IF NOT EXISTS / ON CONFLICT DO NOTHING) and is
-- executed inside the bootstrap transaction, after the short advisory lock,
-- lock_timeout, and statement_timeout are set. It must never be executed
-- outside that bootstrap path.

CREATE TABLE IF NOT EXISTS observer_schema_versions (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS observer_sessions (
    id            UUID PRIMARY KEY,
    node_scope    TEXT,
    user_id       BIGINT,
    client_family TEXT,
    first_seen    TIMESTAMPTZ,
    last_seen     TIMESTAMPTZ,
    turn_count    BIGINT,
    gap_count     BIGINT
);

CREATE TABLE IF NOT EXISTS observer_session_aliases (
    node_scope   TEXT,
    user_id      BIGINT,
    key_version  SMALLINT,
    provider     TEXT,
    source       TEXT,
    alias_digest BYTEA,
    session_id   UUID,
    first_seen   TIMESTAMPTZ,
    last_seen    TIMESTAMPTZ,
    UNIQUE (node_scope, user_id, key_version, alias_digest)
);

CREATE TABLE IF NOT EXISTS observer_turns (
    id                UUID PRIMARY KEY,
    node_scope        TEXT,
    event_id          TEXT,
    session_id        UUID NULL,
    occurred_at       TIMESTAMPTZ,
    user_id           BIGINT,
    token_id          BIGINT,
    client_profile    TEXT,
    model             TEXT,
    upstream_model    TEXT,
    relay_format      TEXT,
    success           BOOLEAN,
    status_code       INT,
    error_type        TEXT,
    error_code        TEXT,
    latency_ms        BIGINT,
    first_response_ms BIGINT,
    stream            BOOLEAN,
    prompt_tokens     BIGINT,
    completion_tokens BIGINT,
    cached_tokens     BIGINT,
    quota             BIGINT,
    attempts          JSONB,
    attempts_omitted  INT,
    client_ip         INET NULL,
    ip_trust          TEXT NULL,
    country_code      TEXT,
    country           TEXT,
    city              TEXT,
    asn               BIGINT,
    asn_org           TEXT,
    content_state     TEXT,
    UNIQUE (node_scope, event_id)
);

CREATE TABLE IF NOT EXISTS observer_content_objects (
    id            BIGSERIAL PRIMARY KEY,
    session_id    UUID,
    item_digest   BYTEA,
    kind          TEXT,
    role          TEXT,
    codec         TEXT,
    payload       BYTEA,
    logical_bytes INT,
    stored_bytes  INT,
    truncated     BOOLEAN,
    UNIQUE (session_id, item_digest)
);

CREATE TABLE IF NOT EXISTS observer_contexts (
    id                  BIGSERIAL PRIMARY KEY,
    session_id          UUID,
    turn_id             UUID UNIQUE,
    checkpoint_id       BIGINT,
    group_ordinal       SMALLINT,
    common_prefix_count INT,
    item_count          INT,
    item_digests        JSONB,
    logical_bytes       BIGINT,
    CHECK (group_ordinal BETWEEN 0 AND 8)
);

CREATE TABLE IF NOT EXISTS observer_session_heads (
    session_id    UUID PRIMARY KEY,
    context_id    BIGINT,
    checkpoint_id BIGINT,
    group_ordinal SMALLINT
);

-- Index coverage per the schema contract: session recency, turn time,
-- user/profile/model filters, and content digest lookup (the content digest
-- lookup is covered by UNIQUE(session_id, item_digest) above).
CREATE INDEX IF NOT EXISTS idx_observer_sessions_last_seen ON observer_sessions (last_seen);
CREATE INDEX IF NOT EXISTS idx_observer_turns_occurred_at ON observer_turns (occurred_at);
CREATE INDEX IF NOT EXISTS idx_observer_turns_user_id ON observer_turns (user_id);
CREATE INDEX IF NOT EXISTS idx_observer_turns_client_profile ON observer_turns (client_profile);
CREATE INDEX IF NOT EXISTS idx_observer_turns_model ON observer_turns (model);
CREATE INDEX IF NOT EXISTS idx_observer_turns_session_id ON observer_turns (session_id);

-- The schema is complete only with its version row. ON CONFLICT keeps the
-- whole script idempotent under the bootstrap transaction.
INSERT INTO observer_schema_versions (version, applied_at)
VALUES (1, now())
ON CONFLICT (version) DO NOTHING;
