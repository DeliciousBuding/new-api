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
	observerSchemaV2 = 2

	// observerSchemaLockKey is the fixed advisory-lock key serializing
	// concurrent bootstrap attempts against the same database.
	observerSchemaLockKey = int64(84241911)

	// Short lock and statement timeouts bound the bootstrap session (SSOT:
	// "may only create the empty v1 schema under a short advisory-lock, lock
	// timeout, and statement timeout").
	bootstrapLockTimeout      = 2 * time.Second
	bootstrapStatementTimeout = 15 * time.Second
)

// observerMigrations is the ordered migration file list. An empty schema
// applies every file in order; a complete v1 schema applies only the v2
// upgrade. Each file is idempotent and runs inside the bootstrap transaction.
// The list is frozen by convention: future migrations append a new file and a
// new schema version constant, never mutate this list.
var observerMigrations = []string{"migrations/001_v1.sql", "migrations/002_v2.sql"}

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
		err = verifySchema(ctx, dbtxAdapter{row: db})
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

// dbtxRow is the concrete database/sql surface verify and bootstrap need;
// *sql.DB and *sql.Tx both satisfy it.
type dbtxRow interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// dbtx is the query surface verify and bootstrap consume; rows come back as
// rowIter so tests can exercise the decision logic against a fake data layer,
// exactly like the other seams.
type dbtx interface {
	QueryContext(ctx context.Context, query string, args ...any) (rowIter, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// dbtxAdapter adapts dbtxRow to dbtx.
type dbtxAdapter struct{ row dbtxRow }

func (a dbtxAdapter) QueryContext(ctx context.Context, query string, args ...any) (rowIter, error) {
	rows, err := a.row.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (a dbtxAdapter) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return a.row.ExecContext(ctx, query, args...)
}

// verifySchema performs the bounded startup schema check: the version table
// must hold exactly [1] (complete v1, upgrade pending at bootstrap) or
// [1, 2] (current), and every required observer table must exist. On the
// current version it also checks the v2 column so a schema whose version row
// lies about its structure is rejected. It never runs DDL, scans data
// tables, or executes VACUUM.
func verifySchema(ctx context.Context, db dbtx) error {
	versions, err := readSchemaVersions(ctx, db)
	if err != nil {
		return fmt.Errorf("relayobserver: schema verify: %w", err)
	}
	current := isVersionListCurrent(versions)
	if !current && !isVersionListV1(versions) {
		return fmt.Errorf("relayobserver: schema verify: version mismatch: have %v, want [%d] or [%d, %d]", versions, observerSchemaV1, observerSchemaV1, observerSchemaV2)
	}
	missing, err := missingObserverTables(ctx, db)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("relayobserver: schema verify: missing required tables %v", missing)
	}
	if current {
		// The version row claims v2; the v2 column must actually exist, so a
		// dropped column is caught here instead of failing the retention pass.
		has, err := observerV2ColumnExists(ctx, db)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("relayobserver: schema verify: v2 column observer_content_objects.created_at is missing")
		}
	}
	return nil
}

// readSchemaVersions returns the observer_schema_versions rows in version
// order. Callers wrap the error with their own prefix.
func readSchemaVersions(ctx context.Context, db dbtx) ([]int, error) {
	rows, err := db.QueryContext(ctx, "SELECT version FROM observer_schema_versions ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read observer_schema_versions: %w", err)
	}
	defer closeRows(rows)
	versions := []int{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read versions: %w", err)
	}
	return versions, nil
}

// isVersionListV1 reports whether versions is exactly the complete v1 state
// [1], the schema that still awaits the v2 upgrade at bootstrap.
func isVersionListV1(versions []int) bool {
	return len(versions) == 1 && versions[0] == observerSchemaV1
}

// isVersionListCurrent reports whether versions is exactly the current state
// [1, 2].
func isVersionListCurrent(versions []int) bool {
	return len(versions) == 2 && versions[0] == observerSchemaV1 && versions[1] == observerSchemaV2
}

// observerV2ColumnExists reports whether the v2 created_at column exists on
// observer_content_objects.
func observerV2ColumnExists(ctx context.Context, db dbtx) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'observer_content_objects'
		  AND column_name = 'created_at')`)
	if err != nil {
		return false, fmt.Errorf("relayobserver: schema verify: check v2 column: %w", err)
	}
	defer closeRows(rows)
	if !rows.Next() {
		return false, fmt.Errorf("relayobserver: schema verify: check v2 column: no result row")
	}
	var exists bool
	if err := rows.Scan(&exists); err != nil {
		return false, fmt.Errorf("relayobserver: schema verify: check v2 column: %w", err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("relayobserver: schema verify: check v2 column: %w", err)
	}
	return exists, nil
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
	defer closeRows(rows)
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

// bootstrapSchema creates the empty v1 observer schema (then upgrades it to
// the current version), upgrades a complete v1 schema to the current version,
// or confirms an already-current schema, all inside one transaction guarded
// by a short advisory lock, lock timeout, and statement timeout. An empty
// schema applies every migration in order; a complete v1 schema applies only
// the v2 upgrade. A partial or mismatched existing schema fails with an
// error and is left untouched; a lock that cannot be acquired fails fast
// without queuing.
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

	tables, err := observerTablesIn(ctx, dbtxAdapter{row: tx})
	if err != nil {
		return fmt.Errorf("relayobserver: schema bootstrap: %w", err)
	}
	var versions []int
	if len(tables) > 0 {
		// An empty observer schema has no version table yet; reading it only
		// when tables exist keeps the bootstrap path usable on a fresh
		// database.
		versions, err = readSchemaVersions(ctx, dbtxAdapter{row: tx})
		if err != nil {
			return fmt.Errorf("relayobserver: schema bootstrap: %w", err)
		}
	}
	if err := bootstrapSchemaTx(ctx, dbtxAdapter{row: tx}, tables, versions); err != nil {
		return err
	}
	return tx.Commit()
}

// bootstrapSchemaTx applies the schema upgrades the current state needs,
// inside the caller's transaction: an empty schema applies every migration in
// order, a complete v1 schema applies only the v2 upgrade, and a complete
// current schema is an idempotent no-op. A partial or mismatched schema fails
// with an error and is left untouched.
func bootstrapSchemaTx(ctx context.Context, tx dbtx, tables map[string]bool, versions []int) error {
	switch {
	case len(tables) == 0 && len(versions) == 0:
		// The observer schema is empty: apply every migration in order.
		for _, file := range observerMigrations {
			sqlText, err := migrationsFS.ReadFile(file)
			if err != nil {
				return fmt.Errorf("relayobserver: schema bootstrap: read migration: %w", err)
			}
			if _, err := tx.ExecContext(ctx, string(sqlText)); err != nil {
				return fmt.Errorf("relayobserver: schema bootstrap: apply %s: %w", file, err)
			}
		}
	case isVersionListV1(versions) && allRequiredTablesPresent(tables):
		// Complete v1 awaiting the upgrade: apply only the v2 migration. The
		// migration is idempotent, so a repeated bootstrap is a no-op. A
		// partial schema never reaches this branch — it must never be
		// patched.
		file := observerMigrations[len(observerMigrations)-1]
		sqlText, err := migrationsFS.ReadFile(file)
		if err != nil {
			return fmt.Errorf("relayobserver: schema bootstrap: read migration: %w", err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlText)); err != nil {
			return fmt.Errorf("relayobserver: schema bootstrap: apply %s: %w", file, err)
		}
	default:
		// Existing observer tables that are not the complete v1 schema are
		// never patched; the error disables the observer. A complete current
		// schema passes verify and the bootstrap is an idempotent no-op.
		if err := verifySchema(ctx, tx); err != nil {
			return fmt.Errorf("relayobserver: schema bootstrap: existing schema is not complete v1 or v2: %w", err)
		}
	}
	return nil
}

// allRequiredTablesPresent reports whether every required observer table is
// present in the observed table set.
func allRequiredTablesPresent(tables map[string]bool) bool {
	for _, name := range requiredObserverTables {
		if !tables[name] {
			return false
		}
	}
	return true
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

// turnRowID deterministically derives the observer_turns row id of an event
// from its idempotency key (node_scope, event_id). WriteBatch and the worker's
// content append share it, so an observer_contexts row always references the
// metadata row of the same event on every write and every idempotent replay.
// Existing rows written with random ids keep them: the UNIQUE key is
// (node_scope, event_id), so a replay never collides on the id.
func turnRowID(nodeScope, eventID string) uuid.UUID {
	return uuid.NewSHA1(uuid.Nil, []byte(nodeScope+"\x00"+eventID))
}

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
	// Session recency statement: every turn bound to a session — including
	// metadata-only turns that persist no content — must keep last_seen at
	// least as fresh as the turn's occurred_at, so the session retention
	// invariant (an expired session never carries a not-yet-expired turn)
	// holds for all turns, not only the content-bearing ones.
	bumpStmt, err := tx.PrepareContext(ctx, `UPDATE observer_sessions SET last_seen = GREATEST(last_seen, $1) WHERE id = $2`)
	if err != nil {
		return fmt.Errorf("relayobserver: write batch: prepare session bump: %w", err)
	}
	defer bumpStmt.Close()

	for i := range events {
		ev := &events[i]
		attempts, err := marshalAttempts(ev.Attempts)
		if err != nil {
			return fmt.Errorf("relayobserver: write batch: marshal attempts: %w", err)
		}
		args := []any{
			turnRowID(ev.NodeScope, ev.EventID), ev.NodeScope, ev.EventID, nilOrUUID(ev.SessionID), ev.OccurredAt,
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
		if ev.SessionID != nil {
			if _, err := bumpStmt.ExecContext(ctx, ev.OccurredAt, ev.SessionID.String()); err != nil {
				return fmt.Errorf("relayobserver: write batch: bump session last_seen: %w", err)
			}
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

// Retention bounds per the SSOT: at most one pass every six hours, at most
// 1000 expired turns, 100 expired sessions, and 1000 orphan objects per pass.
const (
	retentionMaxTurnsPerPass    = 1000
	retentionMaxSessionsPerPass = 100
	retentionMaxOrphansPerPass  = 1000
)

// ListExpiredTurns implements ContentPersistence. See the interface comment
// for the full contract. The limit bounds the pass; the predicate is the
// indexed occurred_at column.
func (s *pgStore) ListExpiredTurns(ctx context.Context, cutoff time.Time, limit int) ([]TurnRetentionRef, error) {
	return listExpiredTurnsQ(ctx, sqlDBAdapter{db: s.db}, cutoff, limit)
}

// listExpiredTurnsQ lists expired turns through the query seam. It selects
// only the retention columns (id, session_id) — never attempts, geo, or
// content payload columns.
func listExpiredTurnsQ(ctx context.Context, q contentQuerier, cutoff time.Time, limit int) ([]TurnRetentionRef, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT id::text, session_id::text FROM observer_turns WHERE occurred_at < $1 ORDER BY occurred_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("relayobserver: list expired turns: %w", err)
	}
	defer closeRows(rows)
	var refs []TurnRetentionRef
	for rows.Next() {
		var idText string
		var sidText sql.NullString
		if err := rows.Scan(&idText, &sidText); err != nil {
			return nil, fmt.Errorf("relayobserver: list expired turns: scan: %w", err)
		}
		turnID, err := uuid.Parse(idText)
		if err != nil {
			return nil, fmt.Errorf("relayobserver: list expired turns: invalid turn id %q: %w", idText, err)
		}
		ref := TurnRetentionRef{TurnID: turnID}
		if sidText.Valid {
			sid, err := uuid.Parse(sidText.String)
			if err != nil {
				return nil, fmt.Errorf("relayobserver: list expired turns: invalid session id %q: %w", sidText.String, err)
			}
			ref.SessionID = &sid
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relayobserver: list expired turns: %w", err)
	}
	return refs, nil
}

// ListExpiredSessions implements ContentPersistence. See the interface
// comment for the full contract.
func (s *pgStore) ListExpiredSessions(ctx context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	return listExpiredSessionsQ(ctx, sqlDBAdapter{db: s.db}, cutoff, limit)
}

// listExpiredSessionsQ lists expired session ids through the query seam.
func listExpiredSessionsQ(ctx context.Context, q contentQuerier, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT id::text FROM observer_sessions WHERE last_seen < $1 ORDER BY last_seen LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("relayobserver: list expired sessions: %w", err)
	}
	defer closeRows(rows)
	var ids []uuid.UUID
	for rows.Next() {
		var idText string
		if err := rows.Scan(&idText); err != nil {
			return nil, fmt.Errorf("relayobserver: list expired sessions: scan: %w", err)
		}
		id, err := uuid.Parse(idText)
		if err != nil {
			return nil, fmt.Errorf("relayobserver: list expired sessions: invalid session id %q: %w", idText, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relayobserver: list expired sessions: %w", err)
	}
	return ids, nil
}

// DeleteTurnRetention implements ContentPersistence. See the interface
// comment for the full contract.
func (s *pgStore) DeleteTurnRetention(ctx context.Context, turnID uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("relayobserver: delete turn retention: begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit
	if err := deleteTurnRetentionTx(ctx, sqlTxAdapter{tx: tx}, turnID); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteTurnRetentionTx deletes one expired turn, its context row, and a
// session head that points at that context, inside the caller's transaction.
// A context row that is still a group's full checkpoint is skipped (the turn
// is kept for the next pass): a retained delta of the same session references
// it as its base, and deleting it would leave the delta dangling with its own
// (not yet expired) turn unreconstructable.
func deleteTurnRetentionTx(ctx context.Context, tx contentTx, turnID uuid.UUID) error {
	var referenced bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM observer_contexts d WHERE d.checkpoint_id IN (SELECT id FROM observer_contexts WHERE turn_id = $1))`, turnID.String()).Scan(&referenced); err != nil {
		return fmt.Errorf("relayobserver: delete turn retention: check checkpoint references: %w", err)
	}
	if referenced {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE observer_session_heads SET context_id = NULL, checkpoint_id = NULL, group_ordinal = NULL WHERE context_id IN (SELECT id FROM observer_contexts WHERE turn_id = $1)`, turnID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete turn retention: clear head: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_contexts WHERE turn_id = $1`, turnID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete turn retention: delete context: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_turns WHERE id = $1`, turnID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete turn retention: delete turn: %w", err)
	}
	return nil
}

// DeleteSessionRetention implements ContentPersistence. See the interface
// comment for the full contract.
func (s *pgStore) DeleteSessionRetention(ctx context.Context, sessionID uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("relayobserver: delete session retention: begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit
	if err := deleteSessionRetentionTx(ctx, sqlTxAdapter{tx: tx}, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteSessionRetentionTx deletes one expired session and everything that
// references it, inside the caller's transaction. Content objects go first
// (they are unreferenced once the contexts go), then contexts, the head, the
// alias bindings, the session's turns (all expired by definition: last_seen
// is never older than any of its turns' occurred_at), and the session row
// itself. After it returns, no row references the session.
func deleteSessionRetentionTx(ctx context.Context, tx contentTx, sessionID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM observer_content_objects WHERE session_id = $1`, sessionID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete session retention: delete content: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_contexts WHERE session_id = $1`, sessionID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete session retention: delete contexts: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_session_heads WHERE session_id = $1`, sessionID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete session retention: delete head: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_session_aliases WHERE session_id = $1`, sessionID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete session retention: delete aliases: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_turns WHERE session_id = $1`, sessionID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete session retention: delete turns: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_sessions WHERE id = $1`, sessionID.String()); err != nil {
		return fmt.Errorf("relayobserver: delete session retention: delete session: %w", err)
	}
	return nil
}

// DeleteOrphanContent implements ContentPersistence. See the interface
// comment for the full contract. The delete is bounded by a LIMIT on the
// candidate subquery; the candidate predicate (created_at < cutoff) is
// indexed and the reference check (JSONB containment against the session's
// context digest lists) probes the GIN index, so the pass never reads the
// content payload.
func (s *pgStore) DeleteOrphanContent(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return deleteOrphanContentQ(ctx, sqlDBAdapter{db: s.db}, cutoff, limit)
}

// deleteOrphanContentQ deletes bounded orphan objects through the query
// seam. It returns the number of rows deleted.
func deleteOrphanContentQ(ctx context.Context, q contentQuerier, cutoff time.Time, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	res, err := q.Exec(ctx, `DELETE FROM observer_content_objects o
WHERE o.id IN (
    SELECT o2.id FROM observer_content_objects o2
    WHERE o2.created_at < $1
      AND NOT EXISTS (
        SELECT 1 FROM observer_contexts c
        WHERE c.session_id = o2.session_id
          AND c.item_digests @> to_jsonb(encode(o2.item_digest, 'hex'))
      )
    ORDER BY o2.id
    LIMIT $2
)`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("relayobserver: delete orphan content: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("relayobserver: delete orphan content: rows affected: %w", err)
	}
	return int(n), nil
}
