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
	observerSchemaV3 = 3
	observerSchemaV4 = 4
	observerSchemaV5 = 5
	observerSchemaV6 = 6
	observerSchemaV7 = 7
	observerSchemaV8 = 8
	// observerSchemaCurrent is the newest schema version; keep it in sync
	// when an index migration is appended.
	observerSchemaCurrent = observerSchemaV8

	// observerSchemaLockKey is the fixed advisory-lock key serializing
	// concurrent bootstrap attempts against the same database.
	observerSchemaLockKey = int64(84241911)

	// Short lock and statement timeouts bound the bootstrap session (SSOT:
	// "may only create the empty v1 schema under a short advisory-lock, lock
	// timeout, and statement timeout").
	bootstrapLockTimeout      = 2 * time.Second
	bootstrapStatementTimeout = 15 * time.Second

	// indexMigrationTimeout bounds a single asynchronous index build. Index
	// construction is O(table size): on a production database the v5
	// transcript index already spans hundreds of MB, far beyond the 15s
	// structure timeout. CONCURRENTLY builds never block concurrent writes,
	// so a generous budget is safe and future-proof against data growth.
	indexMigrationTimeout = 10 * time.Minute
)

// observerMigrations is the ordered structure-migration file list. Every file
// creates or alters tables/columns/constraints and runs inside the bootstrap
// transaction under the short lock/statement timeouts above. Index migrations
// are deliberately NOT in this list: CREATE INDEX is O(table size), so indexes
// are applied asynchronously via observerIndexMigrations after the structure
// is ready, keeping NewAPI startup bounded regardless of data volume.
//
// The list is frozen by convention: future structure migrations append a new
// file and a new schema version constant, never mutate existing entries.
var observerMigrations = []string{
	"migrations/001_v1.sql",
	"migrations/002_v2.sql",
	"migrations/003_v3.sql",
	"migrations/004_v4.sql",
}

// observerIndexMigration describes one asynchronously-applied index. Index
// creation is non-transactional (CREATE INDEX CONCURRENTLY) and idempotent via
// a valid-index existence check; the schema version row is recorded only after
// the index is actually built, so the version reflects the index's real
// presence rather than an intent.
type observerIndexMigration struct {
	name       string // index name, checked against pg_indexes (valid only)
	createStmt string // CREATE INDEX CONCURRENTLY statement
	version    int    // schema version row recorded once the index exists
}

// observerIndexMigrations is the ordered index-migration list. Each entry is
// applied asynchronously via CREATE INDEX CONCURRENTLY and records its version
// row only after the index actually exists. v5 adds the transcript access-
// path index (session_id, id); v6 adds the turn_id lookup index used by the
// append idempotency probe and the turn-context read; v7 adds the
// content_state index used by the overview gap count; v8 adds the
// (session_id, occurred_at DESC, id DESC) composite index that serves the turn
// list query (WHERE session_id ORDER BY occurred_at DESC) directly, avoiding
// an occurred_at-index reverse scan plus filter that was ~1.3s on the
// production table.
var observerIndexMigrations = []observerIndexMigration{
	{
		name:       "idx_observer_contexts_session_id_id",
		createStmt: "CREATE INDEX CONCURRENTLY idx_observer_contexts_session_id_id ON observer_contexts (session_id, id)",
		version:    observerSchemaV5,
	},
	{
		name:       "idx_observer_contexts_turn_id",
		createStmt: "CREATE INDEX CONCURRENTLY idx_observer_contexts_turn_id ON observer_contexts (turn_id)",
		version:    observerSchemaV6,
	},
	{
		name:       "idx_observer_turns_content_state",
		createStmt: "CREATE INDEX CONCURRENTLY idx_observer_turns_content_state ON observer_turns (content_state)",
		version:    observerSchemaV7,
	},
	{
		name:       "idx_observer_turns_session_id_occurred_at",
		createStmt: "CREATE INDEX CONCURRENTLY idx_observer_turns_session_id_occurred_at ON observer_turns (session_id, occurred_at DESC, id DESC)",
		version:    observerSchemaV8,
	},
}

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

	// previousHMACKey is the previous-generation content HMAC key, used by
	// content reconstruction to decode items written before a rotation.
	// It is a secret and stays inside the adapter; set once at open time.
	previousHMACKey string
}

// SetPreviousHMACKey stores the previous-generation content HMAC key for the
// reconstruction decode fallback. The runtime wires it from the init
// configuration after the store opens; an empty key means no fallback.
func (s *pgStore) SetPreviousHMACKey(key string) {
	s.previousHMACKey = key
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
	// Structure is ready. In bootstrap mode, apply any missing index
	// migrations asynchronously: CREATE INDEX CONCURRENTLY is O(table size)
	// and must not block NewAPI startup or the observer's first writes. The
	// goroutine carries its own long budget; a failure is logged and retried
	// on the next startup (the index build is idempotent), and it never
	// disables the observer.
	if mode == SchemaModeBootstrap {
		go applyIndexMigrationsAsync(db)
	}
	return store, nil
}

// applyIndexMigrationsAsync runs the index migrations on a background
// goroutine with the generous indexMigrationTimeout budget. It is fire-and-
// forget: the observer is already enabled, and a failed build is retried on
// the next startup. A closed pool (process shutdown) simply fails the build,
// which is logged and harmless.
func applyIndexMigrationsAsync(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), indexMigrationTimeout)
	defer cancel()
	if err := applyIndexMigrations(ctx, db); err != nil {
		common.SysError("relayobserver: index migration failed: " + err.Error())
	}
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
// must hold a complete known prefix ([1], [1,2], ..., [1..7]) awaiting
// bootstrap, or [1..8] (current), and every required observer table must
// exist. On the current version it also checks the v2 column and v4 alias
// identity index so a schema whose version row lies about its structure is
// rejected. It never runs DDL, scans data tables, or executes VACUUM.
func verifySchema(ctx context.Context, db dbtx) error {
	versions, err := readSchemaVersions(ctx, db)
	if err != nil {
		return fmt.Errorf("relayobserver: schema verify: %w", err)
	}
	current := isVersionListCurrent(versions)
	if !current && !isVersionListV1(versions) && !isVersionListV2(versions) && !isVersionListV3(versions) && !isVersionListV4(versions) && !isVersionListV5(versions) && !isVersionListV6(versions) && !isVersionListV7(versions) {
		return fmt.Errorf("relayobserver: schema verify: version mismatch: have %v, want a complete prefix of [1, 2, 3, 4, 5, 6, 7, 8]", versions)
	}
	missing, err := missingObserverTables(ctx, db)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("relayobserver: schema verify: missing required tables %v", missing)
	}
	if current {
		// The version row claims the current schema; the v2 column must
		// actually exist, so a dropped column is caught here instead of
		// failing the retention pass.
		has, err := observerV2ColumnExists(ctx, db)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("relayobserver: schema verify: v2 column observer_content_objects.created_at is missing")
		}
		has, err = observerV4AliasIndexExists(ctx, db)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("relayobserver: schema verify: v4 provider-scoped alias identity index is missing")
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

// isVersionListV2 reports whether versions is exactly the complete v2 state
// [1, 2], the schema that still awaits the v3 upgrade at bootstrap.
func isVersionListV2(versions []int) bool {
	return len(versions) == 2 && versions[0] == observerSchemaV1 && versions[1] == observerSchemaV2
}

// isVersionListV3 reports whether versions is exactly the complete v3 state
// [1, 2, 3], which still awaits the v4 alias identity upgrade.
func isVersionListV3(versions []int) bool {
	return len(versions) == 3 && versions[0] == observerSchemaV1 && versions[1] == observerSchemaV2 && versions[2] == observerSchemaV3
}

// isVersionListV4 reports whether versions is exactly the complete v4 state
// [1, 2, 3, 4], which still awaits the v5 transcript index upgrade.
func isVersionListV4(versions []int) bool {
	return len(versions) == 4 && versions[0] == observerSchemaV1 && versions[1] == observerSchemaV2 && versions[2] == observerSchemaV3 && versions[3] == observerSchemaV4
}

// isVersionListV5 reports whether versions is exactly the v5 state
// [1, 2, 3, 4, 5], which still awaits the v6/v7 index upgrades.
func isVersionListV5(versions []int) bool {
	return len(versions) == 5 && versions[0] == observerSchemaV1 && versions[1] == observerSchemaV2 && versions[2] == observerSchemaV3 && versions[3] == observerSchemaV4 && versions[4] == observerSchemaV5
}

// isVersionListV6 reports whether versions is exactly the v6 state
// [1, 2, 3, 4, 5, 6], which still awaits the v7 index upgrade.
func isVersionListV6(versions []int) bool {
	return len(versions) == 6 && versions[0] == observerSchemaV1 && versions[1] == observerSchemaV2 && versions[2] == observerSchemaV3 && versions[3] == observerSchemaV4 && versions[4] == observerSchemaV5 && versions[5] == observerSchemaV6
}

// isVersionListV7 reports whether versions is exactly the v7 state
// [1, 2, 3, 4, 5, 6, 7], which still awaits the v8 index upgrade.
func isVersionListV7(versions []int) bool {
	return len(versions) == 7 && versions[0] == observerSchemaV1 && versions[1] == observerSchemaV2 && versions[2] == observerSchemaV3 && versions[3] == observerSchemaV4 && versions[4] == observerSchemaV5 && versions[5] == observerSchemaV6 && versions[6] == observerSchemaV7
}

// isVersionListCurrent reports whether versions is exactly the current state
// [1, 2, 3, 4, 5, 6, 7, 8].
func isVersionListCurrent(versions []int) bool {
	return len(versions) == 8 && versions[0] == observerSchemaV1 && versions[1] == observerSchemaV2 && versions[2] == observerSchemaV3 && versions[3] == observerSchemaV4 && versions[4] == observerSchemaV5 && versions[5] == observerSchemaV6 && versions[6] == observerSchemaV7 && versions[7] == observerSchemaV8
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

// observerV4AliasIndexExists reports whether the provider-scoped identity
// index introduced by v4 exists. The provider is part of alias identity: the
// same raw value may legitimately identify independent Codex and Claude
// sessions for the same user and node scope.
func observerV4AliasIndexExists(ctx context.Context, db dbtx) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM pg_class idx
		JOIN pg_index i ON i.indexrelid = idx.oid
		JOIN pg_class tbl ON tbl.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = tbl.relnamespace
		WHERE n.nspname = current_schema()
		  AND tbl.relname = 'observer_session_aliases'
		  AND idx.relname = 'idx_observer_session_aliases_identity'
		  AND i.indisunique
		  AND i.indpred IS NULL
		  AND (
		      SELECT array_agg(a.attname ORDER BY k.ordinality)
		      FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, ordinality)
		      JOIN pg_attribute a
		        ON a.attrelid = i.indrelid
		       AND a.attnum = k.attnum
		      WHERE k.attnum > 0
		  ) = ARRAY['node_scope', 'user_id', 'key_version', 'alias_digest', 'provider']::name[])`)
	if err != nil {
		return false, fmt.Errorf("relayobserver: schema verify: check v4 alias identity index: %w", err)
	}
	defer closeRows(rows)
	if !rows.Next() {
		return false, fmt.Errorf("relayobserver: schema verify: check v4 alias identity index: no result row")
	}
	var exists bool
	if err := rows.Scan(&exists); err != nil {
		return false, fmt.Errorf("relayobserver: schema verify: check v4 alias identity index: %w", err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("relayobserver: schema verify: check v4 alias identity index: %w", err)
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

// bootstrapSchemaTx applies the structure upgrades the current state needs,
// inside the caller's transaction: an empty schema applies every structure
// migration in order, a complete v1-v4 schema applies its pending structure
// suffix, and a complete current schema is an idempotent no-op. A partial or
// mismatched schema fails with an error and is left untouched. Index
// migrations are not applied here — they run asynchronously via
// applyIndexMigrations after the structure is ready.
func bootstrapSchemaTx(ctx context.Context, tx dbtx, tables map[string]bool, versions []int) error {
	switch {
	case len(tables) == 0 && len(versions) == 0:
		// The observer schema is empty: apply every migration in order.
		for _, file := range observerMigrations {
			if err := applyMigrationTx(ctx, tx, file); err != nil {
				return err
			}
		}
	case (isVersionListV1(versions) || isVersionListV2(versions) || isVersionListV3(versions) || isVersionListV4(versions)) && allRequiredTablesPresent(tables):
		// Complete older schema awaiting the upgrade: apply the pending
		// migrations from the current version upward. Each migration is
		// idempotent, so a repeated bootstrap is a no-op. A partial schema
		// never reaches this branch — it must never be patched.
		for _, file := range pendingMigrations(versions) {
			if err := applyMigrationTx(ctx, tx, file); err != nil {
				return err
			}
		}
	default:
		// Existing observer tables that are not a complete older schema are
		// never patched; the error disables the observer. A complete current
		// schema passes verify and the bootstrap is an idempotent no-op.
		if err := verifySchema(ctx, tx); err != nil {
			return fmt.Errorf("relayobserver: schema bootstrap: existing schema is not complete: %w", err)
		}
	}
	return nil
}

// applyMigrationTx reads one embedded migration and executes it on the
// caller's transaction.
func applyMigrationTx(ctx context.Context, tx dbtx, file string) error {
	sqlText, err := migrationsFS.ReadFile(file)
	if err != nil {
		return fmt.Errorf("relayobserver: schema bootstrap: read migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, string(sqlText)); err != nil {
		return fmt.Errorf("relayobserver: schema bootstrap: apply %s: %w", file, err)
	}
	return nil
}

// pendingMigrations returns the structure migrations a complete schema with
// the given version rows still needs, in order: [1] yields 002, 003, 004;
// [1, 2] yields 003, 004. Version rows are 1-indexed against the structure
// migration file order, so the count of applied structure migrations equals
// the number of version rows. Index migrations are excluded — they are applied
// asynchronously, not from this list.
func pendingMigrations(versions []int) []string {
	var pending []string
	for i, file := range observerMigrations {
		if i+1 > len(versions) {
			pending = append(pending, file)
		}
	}
	return pending
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

// applyIndexMigrations builds every missing observer index in order, outside
// any transaction. Each index uses CREATE INDEX CONCURRENTLY so it never
// blocks the observer's concurrent writes, and it is idempotent: a valid index
// already present is skipped, while an invalid index left by a previous failed
// CONCURRENTLY attempt is dropped before retrying. The schema version row is
// recorded only after the index is actually built, so the version always
// reflects the index's real presence. Each index runs under the generous
// indexMigrationTimeout, not the structure bootstrap timeout.
func applyIndexMigrations(ctx context.Context, db *sql.DB) error {
	for _, m := range observerIndexMigrations {
		valid, err := validIndexExists(ctx, db, m.name)
		if err != nil {
			return fmt.Errorf("relayobserver: index migration: check %s: %w", m.name, err)
		}
		if valid {
			// The index already exists (for example it was created manually,
			// as with the production v7/v8 indexes), but the schema version row
			// may be missing. Record it idempotently so observer_schema_versions
			// reflects the index's real presence, not just the builds this
			// process happened to perform.
			if err := recordIndexVersion(ctx, db, m.version); err != nil {
				return fmt.Errorf("relayobserver: index migration: record existing %s version %d: %w", m.name, m.version, err)
			}
			continue
		}
		// A previous CONCURRENTLY attempt may have left an INVALID index;
		// drop it so the retry starts clean. The name is a compile-time
		// constant, never user input.
		if _, err := db.ExecContext(ctx, "DROP INDEX IF EXISTS "+m.name); err != nil {
			return fmt.Errorf("relayobserver: index migration: drop stale %s: %w", m.name, err)
		}
		if err := buildIndexConcurrently(ctx, db, m.createStmt); err != nil {
			return fmt.Errorf("relayobserver: index migration: build %s: %w", m.name, err)
		}
		if err := recordIndexVersion(ctx, db, m.version); err != nil {
			return fmt.Errorf("relayobserver: index migration: record version %d: %w", m.version, err)
		}
	}
	return nil
}

// validIndexExists reports whether the named index exists and is VALID in the
// current schema. An index left INVALID by a failed CREATE INDEX CONCURRENTLY
// must not count as present, so it is dropped and rebuilt on the next pass.
func validIndexExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM pg_class c
		JOIN pg_index i ON i.indexrelid = c.oid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema()
		  AND c.relname = $1
		  AND i.indisvalid)`, name)
	if err != nil {
		return false, err
	}
	defer closeRows(rows)
	if !rows.Next() {
		return false, errors.New("relayobserver: index migration: no result row")
	}
	var exists bool
	if err := rows.Scan(&exists); err != nil {
		return false, err
	}
	return exists, rows.Err()
}

// buildIndexConcurrently executes one CREATE INDEX CONCURRENTLY on a dedicated
// connection with a session-level statement timeout matching the generous
// indexMigrationTimeout. CONCURRENTLY cannot run inside a transaction, so the
// statement executes on a fresh connection outside the bootstrap transaction.
func buildIndexConcurrently(ctx context.Context, db *sql.DB, stmt string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Session-level timeout on this dedicated connection only; the pool's
	// other connections keep their normal limits. Milliseconds avoids the
	// float-second rounding of SET statement_timeout = '600000ms' vs '600s'.
	ms := int64(indexMigrationTimeout / time.Millisecond)
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = '%d'", ms)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, stmt)
	return err
}

// recordIndexVersion records the schema version row for a built index,
// idempotently. It is a separate statement from the index build so the version
// row is only present once the index actually exists.
func recordIndexVersion(ctx context.Context, db *sql.DB, version int) error {
	_, err := db.ExecContext(ctx, `INSERT INTO observer_schema_versions (version, applied_at)
		VALUES ($1, now())
		ON CONFLICT (version) DO NOTHING`, version)
	return err
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

// Retention work remains bounded per pass while allowing cleanup to outrun
// ordinary gateway traffic at the worker's one-minute cadence.
const (
	retentionMaxTurnsPerPass     = 1_000
	retentionMaxSessionsPerPass  = 100
	retentionMaxOrphansPerPass   = 1_000
	retentionBacklogInspectLimit = 10_000
)

// InspectRetentionBacklog implements ContentPersistence. Counts are sampled
// through indexed, payload-free queries capped at limit+1 so the worker can
// expose an honest lower bound and a truncation bit without a full-table
// count during an outage backlog.
func (s *pgStore) InspectRetentionBacklog(ctx context.Context, turnCutoff, contentCutoff time.Time, limit int) (RetentionBacklog, error) {
	return inspectRetentionBacklogQ(ctx, sqlDBAdapter{db: s.db}, turnCutoff, contentCutoff, limit)
}

func inspectRetentionBacklogQ(ctx context.Context, q contentQuerier, turnCutoff, contentCutoff time.Time, limit int) (RetentionBacklog, error) {
	if limit < 1 {
		return RetentionBacklog{}, nil
	}
	probeLimit := limit + 1

	turns, turnOldest, turnTruncated, err := scanRetentionBacklogClass(ctx, q, `SELECT COUNT(*), COALESCE(MIN(occurred_at), to_timestamp(0))
FROM (
    SELECT occurred_at FROM observer_turns
    WHERE occurred_at < $1
    ORDER BY occurred_at
    LIMIT $2
) retention_backlog`, turnCutoff, limit, probeLimit)
	if err != nil {
		return RetentionBacklog{}, fmt.Errorf("relayobserver: inspect retention turns: %w", err)
	}
	sessions, sessionOldest, sessionTruncated, err := scanRetentionBacklogClass(ctx, q, `SELECT COUNT(*), COALESCE(MIN(last_seen), to_timestamp(0))
FROM (
    SELECT last_seen FROM observer_sessions
    WHERE last_seen < $1
    ORDER BY last_seen
    LIMIT $2
) retention_backlog`, turnCutoff, limit, probeLimit)
	if err != nil {
		return RetentionBacklog{}, fmt.Errorf("relayobserver: inspect retention sessions: %w", err)
	}
	objects, objectOldest, objectTruncated, err := scanRetentionBacklogClass(ctx, q, `SELECT COUNT(*), COALESCE(MIN(created_at), to_timestamp(0))
FROM (
    SELECT o.created_at FROM observer_content_objects o
    WHERE o.created_at < $1
      AND NOT EXISTS (
        SELECT 1 FROM observer_contexts c
        WHERE c.session_id = o.session_id
          AND c.item_digests @> to_jsonb(encode(o.item_digest, 'hex'))
      )
    ORDER BY o.created_at
    LIMIT $2
) retention_backlog`, contentCutoff, limit, probeLimit)
	if err != nil {
		return RetentionBacklog{}, fmt.Errorf("relayobserver: inspect retention objects: %w", err)
	}

	backlog := RetentionBacklog{
		Turns:     turns,
		Sessions:  sessions,
		Objects:   objects,
		Truncated: turnTruncated || sessionTruncated || objectTruncated,
	}
	for _, candidate := range []struct {
		count  int64
		oldest time.Time
	}{
		{count: turns, oldest: turnOldest},
		{count: sessions, oldest: sessionOldest},
		{count: objects, oldest: objectOldest},
	} {
		if candidate.count > 0 && (backlog.Oldest.IsZero() || candidate.oldest.Before(backlog.Oldest)) {
			backlog.Oldest = candidate.oldest
		}
	}
	return backlog, nil
}

func scanRetentionBacklogClass(ctx context.Context, q contentQuerier, query string, cutoff time.Time, limit, probeLimit int) (int64, time.Time, bool, error) {
	var count int64
	var oldest time.Time
	if err := q.QueryRow(ctx, query, cutoff, probeLimit).Scan(&count, &oldest); err != nil {
		return 0, time.Time{}, false, err
	}
	if count > int64(limit) {
		return int64(limit), oldest, true, nil
	}
	return count, oldest, false, nil
}

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
func (s *pgStore) DeleteTurnRetention(ctx context.Context, turnID uuid.UUID) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("relayobserver: delete turn retention: begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit
	deleted, err := deleteTurnRetentionTx(ctx, sqlTxAdapter{tx: tx}, turnID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return deleted, nil
}

// deleteTurnRetentionTx deletes one expired turn, its context row, and a
// session head that points at that context, inside the caller's transaction.
// A context row that is still another retained context's full checkpoint is
// skipped (the turn is kept for the next pass): a retained delta of the same
// session references it as its base, and deleting it would leave the delta
// dangling with its own (not yet expired) turn unreconstructable. The check
// excludes the row's own checkpoint self-reference (SSOT: a full row stores
// checkpoint_id = id), so a full checkpoint that no retained delta references
// is deletable instead of blocking its own turn forever.
func deleteTurnRetentionTx(ctx context.Context, tx contentTx, turnID uuid.UUID) (bool, error) {
	var referenced bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM observer_contexts d WHERE d.checkpoint_id IN (SELECT id FROM observer_contexts WHERE turn_id = $1) AND d.id <> d.checkpoint_id)`, turnID.String()).Scan(&referenced); err != nil {
		return false, fmt.Errorf("relayobserver: delete turn retention: check checkpoint references: %w", err)
	}
	if referenced {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE observer_session_heads SET context_id = NULL, checkpoint_id = NULL, group_ordinal = NULL WHERE context_id IN (SELECT id FROM observer_contexts WHERE turn_id = $1)`, turnID.String()); err != nil {
		return false, fmt.Errorf("relayobserver: delete turn retention: clear head: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_contexts WHERE turn_id = $1`, turnID.String()); err != nil {
		return false, fmt.Errorf("relayobserver: delete turn retention: delete context: %w", err)
	}
	result, err := tx.Exec(ctx, `DELETE FROM observer_turns WHERE id = $1`, turnID.String())
	if err != nil {
		return false, fmt.Errorf("relayobserver: delete turn retention: delete turn: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("relayobserver: delete turn retention: rows affected: %w", err)
	}
	return rows > 0, nil
}

// DeleteSessionRetention implements ContentPersistence. See the interface
// comment for the full contract.
func (s *pgStore) DeleteSessionRetention(ctx context.Context, sessionID uuid.UUID, cutoff time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit
	deleted, err := deleteSessionRetentionTx(ctx, sqlTxAdapter{tx: tx}, sessionID, cutoff)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return deleted, nil
}

// deleteSessionRetentionTx deletes one expired session and everything that
// references it, inside the caller's transaction. The session row is locked
// and its last_seen re-checked against the cutoff before anything is deleted:
// the retention pass lists expired sessions and then deletes them one by one,
// and a session that received a new turn between the list query and this
// transaction must survive (its last_seen moved past the cutoff, so the
// delete is a no-op). Content objects go first (they are unreferenced once
// the contexts go), then contexts, the head, the alias bindings, the
// session's turns (every turn of a still-expired session is itself expired,
// because last_seen is never older than any of its turns' occurred_at), and
// the session row itself. After it returns, no row references the session.
func deleteSessionRetentionTx(ctx context.Context, tx contentTx, sessionID uuid.UUID, cutoff time.Time) (bool, error) {
	var lastSeen time.Time
	if err := tx.QueryRow(ctx, `SELECT last_seen FROM observer_sessions WHERE id = $1 FOR UPDATE`, sessionID.String()).Scan(&lastSeen); err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: lock session: %w", err)
	}
	if !lastSeen.Before(cutoff) {
		// The session became active again after the retention list query; the
		// delete is a no-op. The locked row is released by the commit/rollback.
		return false, nil
	}
	runRetentionHook()
	if _, err := tx.Exec(ctx, `DELETE FROM observer_content_objects WHERE session_id = $1`, sessionID.String()); err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: delete content: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_contexts WHERE session_id = $1`, sessionID.String()); err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: delete contexts: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_session_heads WHERE session_id = $1`, sessionID.String()); err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: delete head: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_session_aliases WHERE session_id = $1`, sessionID.String()); err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: delete aliases: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observer_turns WHERE session_id = $1`, sessionID.String()); err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: delete turns: %w", err)
	}
	result, err := tx.Exec(ctx, `DELETE FROM observer_sessions WHERE id = $1`, sessionID.String())
	if err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: delete session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("relayobserver: delete session retention: rows affected: %w", err)
	}
	return rows > 0, nil
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
