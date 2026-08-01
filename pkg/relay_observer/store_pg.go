package relayobserver

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// This file is the PostgreSQL-only Store adapter. It owns the dedicated
// database pool (independent of model.DB and LOG_DB), the schema verify and
// bootstrap paths, and the idempotent batch write. It never calls FatalLog or
// panics: every failure is returned to the runtime, which disables or
// degrades the observer and never touches NewAPI startup, relay responses, or
// billing.

// Pool limits per the architecture SSOT: "The audit database pool is
// independent from DB and LOG_DB: max open connections 2, max idle 1,
// connection lifetime 60 seconds."
const (
	pgMaxOpenConns    = 2
	pgMaxIdleConns    = 1
	pgConnMaxLifetime = 60 * time.Second
)

// Schema and bootstrap constants.
const (
	observerSchemaV1 = 1

	// observerSchemaLockKey is the fixed advisory-lock key serializing
	// concurrent bootstrap attempts against the same database.
	observerSchemaLockKey = int64(84241911)

	// Short lock and statement timeouts bound the bootstrap session (SSOT:
	// "may only create the empty v1 schema under a short advisory-lock, lock
	// timeout, and statement timeout").
	bootstrapLockTimeout      = 2 * time.Second
	bootstrapStatementTimeout = 15 * time.Second

	// v1MigrationFile is the single versioned SQL file embedded below.
	v1MigrationFile = "migrations/001_v1.sql"
)

// ErrUnsupportedDSN classifies a SQL DSN the observer cannot use: it is empty,
// a SQLite/MySQL DSN, or not parseable by pgx as a PostgreSQL URI or keyword
// DSN. The classification is secret-free: it never echoes the DSN content.
var ErrUnsupportedDSN = errors.New("relayobserver: unsupported observer SQL DSN (expected a PostgreSQL URI or keyword DSN)")

// validatePGDSN accepts only PostgreSQL DSNs parseable by pgx (URI or keyword
// form). Empty, SQLite, MySQL, and garbage DSNs return ErrUnsupportedDSN. The
// returned error never contains the DSN itself: pgx parse errors can quote
// the input, so they are classified, never echoed.
func validatePGDSN(dsn string) error {
	if strings.TrimSpace(dsn) == "" {
		return fmt.Errorf("%w: DSN is empty", ErrUnsupportedDSN)
	}
	lower := strings.ToLower(strings.TrimSpace(dsn))
	switch {
	case strings.HasPrefix(lower, "mysql://"),
		strings.HasPrefix(lower, "sqlite:"),
		strings.HasPrefix(lower, "file:"),
		strings.HasPrefix(lower, "jdbc:"):
		return fmt.Errorf("%w: non-PostgreSQL DSN scheme", ErrUnsupportedDSN)
	}
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return fmt.Errorf("%w: DSN is not a PostgreSQL URI or keyword DSN", ErrUnsupportedDSN)
	}
	return nil
}

// pgPoolConfig is the frozen pool tuning of the observer. It is kept as a
// value so tests can assert the exact SSOT limits without driver getters for
// idle connections or connection lifetime.
type pgPoolConfig struct {
	maxOpenConns    int
	maxIdleConns    int
	connMaxLifetime time.Duration
}

func (c pgPoolConfig) apply(db *sql.DB) {
	db.SetMaxOpenConns(c.maxOpenConns)
	db.SetMaxIdleConns(c.maxIdleConns)
	db.SetConnMaxLifetime(c.connMaxLifetime)
}

var defaultPGPoolConfig = pgPoolConfig{
	maxOpenConns:    pgMaxOpenConns,
	maxIdleConns:    pgMaxIdleConns,
	connMaxLifetime: pgConnMaxLifetime,
}

// pgStore implements Store on the dedicated PostgreSQL pool.
type pgStore struct {
	db        *sql.DB
	poolCfg   pgPoolConfig
	closeOnce sync.Once
	closeErr  error
}

var _ Store = (*pgStore)(nil)

// newPGStore wraps a pool with the SSOT tuning. It is separate from
// OpenPGStore so tests can exercise pool configuration and Close without a
// reachable database.
func newPGStore(db *sql.DB) *pgStore {
	cfg := defaultPGPoolConfig
	cfg.apply(db)
	return &pgStore{db: db, poolCfg: cfg}
}

// OpenPGStore opens the dedicated observer pool for dsn, pings it under ctx,
// and runs the schema check for mode. It returns a Store only when the
// database is reachable and the schema is acceptable; every failure is
// returned as an error for the caller to classify (the observer is disabled,
// NewAPI starts normally). The pool never depends on model.DB or LOG_DB.
func OpenPGStore(ctx context.Context, dsn string, mode SchemaMode) (Store, error) {
	if err := validatePGDSN(dsn); err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("relayobserver: open PostgreSQL pool: %w", err)
	}
	store := newPGStore(db)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("relayobserver: ping PostgreSQL pool: %w", err)
	}
	switch mode {
	case SchemaModeVerify:
		err = verifySchema(ctx, db)
	case SchemaModeBootstrap:
		err = bootstrapSchema(ctx, db)
	default:
		err = fmt.Errorf("relayobserver: unknown schema mode %q", mode)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// dbtx is the query surface verify and bootstrap need; *sql.DB and *sql.Tx
// both satisfy it.
type dbtx interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// verifySchema performs the bounded startup schema check: the version table
// must hold exactly version 1 and every required observer table must exist.
// It never runs DDL, scans data tables, or executes VACUUM.
func verifySchema(ctx context.Context, db dbtx) error {
	rows, err := db.QueryContext(ctx, "SELECT version FROM observer_schema_versions ORDER BY version")
	if err != nil {
		return fmt.Errorf("relayobserver: schema verify: read observer_schema_versions: %w", err)
	}
	defer rows.Close()
	versions := []int{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("relayobserver: schema verify: scan version: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("relayobserver: schema verify: read versions: %w", err)
	}
	if len(versions) != 1 || versions[0] != observerSchemaV1 {
		return fmt.Errorf("relayobserver: schema verify: version mismatch: have %v, want [%d]", versions, observerSchemaV1)
	}
	missing, err := missingObserverTables(ctx, db)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("relayobserver: schema verify: missing required tables %v", missing)
	}
	return nil
}

// requiredObserverTables is the v1 table set besides observer_schema_versions.
var requiredObserverTables = []string{
	"observer_sessions",
	"observer_session_aliases",
	"observer_turns",
	"observer_content_objects",
	"observer_contexts",
	"observer_session_heads",
}

// observerTablesIn lists the observer_* tables of the current schema.
func observerTablesIn(ctx context.Context, db dbtx) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name LIKE 'observer\_%' ESCAPE '\'`)
	if err != nil {
		return nil, fmt.Errorf("relayobserver: list observer tables: %w", err)
	}
	defer rows.Close()
	found := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("relayobserver: scan observer table: %w", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relayobserver: list observer tables: %w", err)
	}
	return found, nil
}

// missingObserverTables returns the required v1 tables absent from the
// current schema.
func missingObserverTables(ctx context.Context, db dbtx) ([]string, error) {
	found, err := observerTablesIn(ctx, db)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range requiredObserverTables {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// bootstrapSchema creates the empty v1 observer schema, or confirms an
// already-complete v1 schema, all inside one transaction guarded by a short
// advisory lock, lock timeout, and statement timeout. A partial or mismatched
// existing schema fails with an error and is left untouched; a lock that
// cannot be acquired fails fast without queuing.
func bootstrapSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("relayobserver: schema bootstrap: begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", bootstrapLockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("relayobserver: schema bootstrap: set lock_timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", bootstrapStatementTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("relayobserver: schema bootstrap: set statement_timeout: %w", err)
	}

	// One short advisory-lock attempt: a concurrent bootstrap fails fast
	// instead of queuing. The xact lock dies with this transaction.
	var locked bool
	if err := tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", observerSchemaLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("relayobserver: schema bootstrap: advisory lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("relayobserver: schema bootstrap: advisory lock busy (another bootstrap is running)")
	}

	tables, err := observerTablesIn(ctx, tx)
	if err != nil {
		return fmt.Errorf("relayobserver: schema bootstrap: %w", err)
	}
	if len(tables) == 0 {
		// The observer schema is empty: apply the versioned v1 migration.
		sqlText, err := migrationsFS.ReadFile(v1MigrationFile)
		if err != nil {
			return fmt.Errorf("relayobserver: schema bootstrap: read migration: %w", err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlText)); err != nil {
			return fmt.Errorf("relayobserver: schema bootstrap: apply %s: %w", v1MigrationFile, err)
		}
	} else if err := verifySchema(ctx, tx); err != nil {
		// Existing observer tables that are not the complete v1 schema are
		// never patched; the error disables the observer.
		return fmt.Errorf("relayobserver: schema bootstrap: existing schema is not complete v1: %w", err)
	}
	return tx.Commit()
}

// insertTurnSQL inserts one turn row with a store-generated row UUID and
// skips duplicate (node_scope, event_id) rows.
const insertTurnSQL = `
INSERT INTO observer_turns (
	id, node_scope, event_id, session_id, occurred_at,
	user_id, token_id, client_profile, model, upstream_model, relay_format,
	success, status_code, error_type, error_code,
	latency_ms, first_response_ms, stream,
	prompt_tokens, completion_tokens, cached_tokens, quota,
	attempts, attempts_omitted,
	client_ip, ip_trust, country_code, country, city, asn, asn_org,
	content_state
) VALUES (
	$1, $2, $3, $4, $5,
	$6, $7, $8, $9, $10, $11,
	$12, $13, $14, $15,
	$16, $17, $18,
	$19, $20, $21, $22,
	$23, $24,
	$25, $26, $27, $28, $29, $30, $31,
	$32
) ON CONFLICT (node_scope, event_id) DO NOTHING`

// WriteBatch persists events in one transaction without retries. Duplicate
// (node_scope, event_id) rows are skipped by ON CONFLICT DO NOTHING; no
// table-wide lock is taken. Any error aborts the transaction and is returned
// to the runtime, which drops the batch and opens the circuit; the adapter
// never retries or loops.
func (s *pgStore) WriteBatch(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("relayobserver: write batch: begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit

	stmt, err := tx.PrepareContext(ctx, insertTurnSQL)
	if err != nil {
		return fmt.Errorf("relayobserver: write batch: prepare: %w", err)
	}
	defer stmt.Close()

	for i := range events {
		ev := &events[i]
		attempts, err := marshalAttempts(ev.Attempts)
		if err != nil {
			return fmt.Errorf("relayobserver: write batch: marshal attempts: %w", err)
		}
		args := []any{
			uuid.New(), ev.NodeScope, ev.EventID, nilOrUUID(ev.SessionID), ev.OccurredAt,
			ev.UserID, ev.TokenID, ev.ClientProfile, ev.Model, ev.UpstreamModel, ev.RelayFormat,
			ev.Success, ev.StatusCode, ev.ErrorType, ev.ErrorCode,
			ev.LatencyMS, ev.FirstResponseMS, ev.Stream,
			ev.PromptTokens, ev.CompletionTokens, ev.CachedTokens, ev.Quota,
			string(attempts), ev.AttemptsOmitted,
			nilOrIP(ev.ClientIP), ipTrustColumn(ev.IPTrust),
			ev.CountryCode, ev.Country, ev.City, ev.ASN, ev.ASNOrg,
			ev.ContentState,
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("relayobserver: write batch: insert: %w", err)
		}
	}
	return tx.Commit()
}

// marshalAttempts renders the bounded attempt summary as JSONB content. A nil
// slice becomes an empty JSON array, never SQL NULL, so readers can always
// decode the column.
func marshalAttempts(attempts []AttemptSummary) ([]byte, error) {
	if attempts == nil {
		attempts = []AttemptSummary{}
	}
	return common.Marshal(attempts)
}

// nilOrUUID returns SQL NULL for a missing session id. The id travels as its
// canonical text form: database/sql flattens the raw [16]byte array and would
// refuse or mangle it, while PostgreSQL parses the text into uuid.
func nilOrUUID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// nilOrIP returns SQL NULL when IP capture is off. The IP travels as its
// canonical text form: database/sql flattens net.IP into a plain byte slice
// and PostgreSQL would store the IPv4-mapped IPv6 form; the text form keeps
// the exact address on both protocols.
func nilOrIP(ip net.IP) any {
	if ip == nil {
		return nil
	}
	return ip.String()
}

// ipTrustColumn maps the typed trust tier verbatim to the ip_trust column;
// the empty tier (capture not wired) becomes SQL NULL. The three typed values
// ("direct", "proxy", "none") are defined once in types.go and consumed here,
// never redefined or translated.
func ipTrustColumn(tier IPTrust) any {
	if tier == "" {
		return nil
	}
	return string(tier)
}

// Close releases the pool, is idempotent, and returns before ctx expires.
// The first call performs the close; later calls return the same result. The
// runtime passes the remaining shutdown budget and treats any error as
// fail-open; the adapter does not loop or retry.
func (s *pgStore) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}
