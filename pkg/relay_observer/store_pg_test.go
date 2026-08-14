package relayobserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the PostgreSQL adapter without a database: DSN
// classification, pool tuning, attempt marshaling, IP/trust mapping, and the
// Close contract. Everything that needs a real PostgreSQL lives in
// store_pg_integration_test.go under the relay_observer_pg_integration build
// tag; the default suite never skips and never dials a database.

const validKeywordDSN = "host=127.0.0.1 port=55433 user=postgres password=observer_local_only dbname=relay_observer sslmode=disable"

// TestValidatePGDSN locks the DSN contract: only PostgreSQL URIs and keyword
// DSNs parseable by pgx are accepted; empty, SQLite, MySQL, and garbage DSNs
// are classified with ErrUnsupportedDSN and the error never echoes the DSN
// itself (secrets and connection details must not reach status or logs).
func TestValidatePGDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want error
	}{
		{name: "empty", dsn: "", want: ErrUnsupportedDSN},
		{name: "blank", dsn: "   \t ", want: ErrUnsupportedDSN},
		{name: "sqlite memory", dsn: ":memory:", want: ErrUnsupportedDSN},
		{name: "sqlite file", dsn: "file:observer.db", want: ErrUnsupportedDSN},
		{name: "sqlite uri", dsn: "sqlite://observer.db", want: ErrUnsupportedDSN},
		{name: "mysql uri", dsn: "mysql://root:secret@127.0.0.1:3306/observer", want: ErrUnsupportedDSN},
		{name: "mysql keyword", dsn: "root:secret@tcp(127.0.0.1:3306)/observer", want: ErrUnsupportedDSN},
		{name: "jdbc", dsn: "jdbc:postgresql://127.0.0.1:55433/relay_observer", want: ErrUnsupportedDSN},
		{name: "garbage", dsn: "this is not a dsn at all", want: ErrUnsupportedDSN},
		{name: "garbage word", dsn: "garbage", want: ErrUnsupportedDSN},
		{name: "pg uri", dsn: "postgres://observer:secret@127.0.0.1:55433/relay_observer?sslmode=disable", want: nil},
		{name: "pg uri alt scheme", dsn: "postgresql://observer@127.0.0.1:55433/relay_observer", want: nil},
		{name: "pg keyword", dsn: validKeywordDSN, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePGDSN(tt.dsn)
			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrUnsupportedDSN)
			if tt.dsn != "" {
				assert.NotContains(t, err.Error(), tt.dsn, "error must never echo the DSN")
			}
			assert.NotContains(t, err.Error(), "secret", "error must never echo DSN secrets")
			assert.NotContains(t, err.Error(), "observer_local_only", "error must never echo DSN secrets")
		})
	}
}

// TestOpenPGStoreRejectsUnsupportedDSN verifies the rejection happens before
// any connection attempt: OpenPGStore on a non-PostgreSQL DSN fails with the
// classification without dialing anything.
func TestOpenPGStoreRejectsUnsupportedDSN(t *testing.T) {
	store, err := OpenPGStore(context.Background(), "sqlite:///tmp/observer.db", SchemaModeVerify)
	require.ErrorIs(t, err, ErrUnsupportedDSN)
	assert.Nil(t, store)

	store, err = OpenPGStore(context.Background(), "garbage dsn", SchemaModeBootstrap)
	require.ErrorIs(t, err, ErrUnsupportedDSN)
	assert.Nil(t, store)
}

// TestPGStorePoolConfiguration locks the SSOT pool tuning: max open
// connections 2, max idle 1, connection lifetime 60 seconds, applied to the
// underlying database/sql pool. sql.Open is lazy, so no database is dialed.
func TestPGStorePoolConfiguration(t *testing.T) {
	db, err := sql.Open("pgx", validKeywordDSN)
	require.NoError(t, err)
	defer db.Close()

	store := newPGStore(db)
	assert.Equal(t, defaultPGPoolConfig, store.poolCfg)
	assert.Equal(t, 2, db.Stats().MaxOpenConnections)
}

// TestPGStoreCloseContract verifies Close: idempotent, and a canceled context
// returns immediately with context.Canceled without closing anything.
func TestPGStoreCloseContract(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := newPGStore(mustOpenTestDB(t))
	err := store.Close(ctx)
	require.ErrorIs(t, err, context.Canceled)

	require.NoError(t, store.Close(context.Background()))
	require.NoError(t, store.Close(context.Background()))
}

// TestPGStoreWriteBatchEmpty verifies an empty batch is a no-op that never
// touches the pool (the pool below is never dialed).
func TestPGStoreWriteBatchEmpty(t *testing.T) {
	store := newPGStore(mustOpenTestDB(t))
	require.NoError(t, store.WriteBatch(context.Background(), nil))
	require.NoError(t, store.WriteBatch(context.Background(), []Event{}))
}

// TestMarshalAttempts locks the attempts wire format: a nil summary becomes
// an empty JSON array (never SQL NULL), and entries keep every field through
// common.Marshal.
func TestMarshalAttempts(t *testing.T) {
	data, err := marshalAttempts(nil)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(data))

	want := []AttemptSummary{
		{ChannelID: 1, Group: "default", StatusCode: 429, ErrorCode: "rate_limit", ElapsedMS: 5},
		{ChannelID: 2, Group: "default", StatusCode: 200, ElapsedMS: 145},
	}
	data, err = marshalAttempts(want)
	require.NoError(t, err)

	var got []AttemptSummary
	require.NoError(t, common.Unmarshal(data, &got))
	assert.Equal(t, want, got)
}

// TestIPTrustColumn locks the typed IPTrust mapping: the three typed values
// map verbatim to the ip_trust column and the empty tier becomes SQL NULL.
func TestIPTrustColumn(t *testing.T) {
	assert.Nil(t, ipTrustColumn(IPTrust("")))
	assert.Equal(t, "direct", ipTrustColumn(IPTrustDirect))
	assert.Equal(t, "proxy", ipTrustColumn(IPTrustProxy))
	assert.Equal(t, "none", ipTrustColumn(IPTrustNone))
}

// TestNilOrIPAndUUID locks the NULL mapping for optional columns: missing
// session id and missing client IP become SQL NULL, present values travel as
// their canonical text forms (PostgreSQL parses them into uuid and inet).
func TestNilOrIPAndUUID(t *testing.T) {
	assert.Nil(t, nilOrIP(nil))
	ip := net.ParseIP("203.0.113.7")
	assert.Equal(t, "203.0.113.7", nilOrIP(ip))

	assert.Nil(t, nilOrUUID(nil))
	id := ptrUUID()
	assert.Equal(t, id.String(), nilOrUUID(id))
}

// TestErrUnsupportedDSNClassification locks the classification contract: the
// sentinel is stable and wraps (errors.Is) rather than a free-form string.
func TestErrUnsupportedDSNClassification(t *testing.T) {
	assert.True(t, errors.Is(fmt.Errorf("wrapped: %w", ErrUnsupportedDSN), ErrUnsupportedDSN))
	assert.Equal(t, "relayobserver: unsupported observer SQL DSN (expected a PostgreSQL URI or keyword DSN)", ErrUnsupportedDSN.Error())
}

func mustOpenTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", validKeywordDSN)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// TestVersionListPredicates locks the schema version predicates: exactly [1]
// is the complete v1 state awaiting upgrade, exactly [1,2] is the complete v2
// state awaiting upgrade, exactly [1,2,3] is the complete v3 state awaiting
// upgrade, exactly [1,2,3,4] is the complete v4 state awaiting upgrade, and
// exactly [1,2,3,4,5] is current. Every other list (empty, single foreign
// version, out-of-order, extra versions) matches none.
func TestVersionListPredicates(t *testing.T) {
	for _, tt := range []struct {
		name     string
		versions []int
		v1       bool
		v2       bool
		v3       bool
		v4       bool
		v5       bool
		v6       bool
		current  bool
	}{
		{name: "empty", versions: nil},
		{name: "v1 only", versions: []int{1}, v1: true},
		{name: "v2 only", versions: []int{2}},
		{name: "v1 and v2", versions: []int{1, 2}, v2: true},
		{name: "v1 through v3", versions: []int{1, 2, 3}, v3: true},
		{name: "v1 through v4", versions: []int{1, 2, 3, 4}, v4: true},
		{name: "v1 through v5", versions: []int{1, 2, 3, 4, 5}, v5: true},
		{name: "v1 through v6", versions: []int{1, 2, 3, 4, 5, 6}, v6: true},
		{name: "current v7", versions: []int{1, 2, 3, 4, 5, 6, 7}, current: true},
		{name: "unknown version", versions: []int{99}},
		{name: "out of order", versions: []int{2, 1}},
		{name: "duplicate", versions: []int{1, 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.v1, isVersionListV1(tt.versions))
			assert.Equal(t, tt.v2, isVersionListV2(tt.versions))
			assert.Equal(t, tt.v3, isVersionListV3(tt.versions))
			assert.Equal(t, tt.v4, isVersionListV4(tt.versions))
			assert.Equal(t, tt.v5, isVersionListV5(tt.versions))
			assert.Equal(t, tt.v6, isVersionListV6(tt.versions))
			assert.Equal(t, tt.current, isVersionListCurrent(tt.versions))
		})
	}
}

// fakeDbtx implements dbtx over scripted rows for the verify/bootstrap
// decision tests: it records every statement and routes by the table the SQL
// touches, so every branch of the schema checks is exercised without a
// database.
type fakeDbtx struct {
	versions        []*fakeRow // rows for "SELECT version FROM observer_schema_versions"
	tables          []*fakeRow // rows for the information_schema.tables listing
	hasV2Col        bool       // result of the v2 column EXISTS check
	hasV4AliasIndex bool       // result of the v4 catalog-shape EXISTS check
	queryErr        error
	execErr         error
	execs           []string
}

func (f *fakeDbtx) QueryContext(ctx context.Context, query string, args ...any) (rowIter, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	switch {
	case strings.Contains(query, "SELECT version FROM observer_schema_versions"):
		return &fakeRows{rows: f.versions}, nil
	case strings.Contains(query, "information_schema.tables"):
		return &fakeRows{rows: f.tables}, nil
	case strings.Contains(query, "information_schema.columns"):
		return &fakeRows{rows: []*fakeRow{{values: []any{f.hasV2Col}}}}, nil
	case strings.Contains(query, "JOIN pg_index i"):
		return &fakeRows{rows: []*fakeRow{{values: []any{f.hasV4AliasIndex}}}}, nil
	}
	return nil, fmt.Errorf("fake: unhandled query %q", query)
}

func (f *fakeDbtx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	f.execs = append(f.execs, query)
	if f.execErr != nil {
		return nil, f.execErr
	}
	return fakeResult{n: 1}, nil
}

// allTables returns the complete required observer table listing.
func allTables() []*fakeRow {
	var rows []*fakeRow
	for _, name := range requiredObserverTables {
		rows = append(rows, &fakeRow{values: []any{name}})
	}
	return rows
}

func versionRows(versions ...int) []*fakeRow {
	var rows []*fakeRow
	for _, v := range versions {
		rows = append(rows, &fakeRow{values: []any{v}})
	}
	return rows
}

// TestVerifySchemaBranches locks every verifySchema branch: complete v1-v6
// prefixes and current v7 pass; foreign/empty/extra version lists are
// rejected, and a current schema missing its v2 column or v4 alias identity
// index is rejected.
func TestVerifySchemaBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("complete v1 passes", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(1), tables: allTables()}
		require.NoError(t, verifySchema(ctx, fx))
	})
	t.Run("complete v2 passes", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(1, 2), tables: allTables()}
		require.NoError(t, verifySchema(ctx, fx))
	})
	t.Run("complete v3 passes", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(1, 2, 3), tables: allTables()}
		require.NoError(t, verifySchema(ctx, fx))
	})
	t.Run("complete v4 passes", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(1, 2, 3, 4), tables: allTables()}
		require.NoError(t, verifySchema(ctx, fx))
	})
	t.Run("complete v5 passes", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(1, 2, 3, 4, 5), tables: allTables()}
		require.NoError(t, verifySchema(ctx, fx))
	})
	t.Run("complete v6 passes", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(1, 2, 3, 4, 5, 6), tables: allTables()}
		require.NoError(t, verifySchema(ctx, fx))
	})
	t.Run("current v7 passes", func(t *testing.T) {
		fx := &fakeDbtx{
			versions:        versionRows(1, 2, 3, 4, 5, 6, 7),
			tables:          allTables(),
			hasV2Col:        true,
			hasV4AliasIndex: true,
		}
		require.NoError(t, verifySchema(ctx, fx))
	})
	t.Run("version mismatch rejected", func(t *testing.T) {
		for _, versions := range [][]int{nil, {2}, {99}, {1, 2, 3, 4, 5, 7}, {1, 2, 3, 4, 5, 6, 6}, {2, 1}} {
			fx := &fakeDbtx{versions: versionRows(versions...), tables: allTables()}
			err := verifySchema(ctx, fx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "version mismatch", "versions %v", versions)
		}
	})
	t.Run("missing table rejected", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(1), tables: allTables()[:5]}
		err := verifySchema(ctx, fx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing required tables")
		assert.Contains(t, err.Error(), requiredObserverTables[5])
	})
	t.Run("missing v2 column rejected", func(t *testing.T) {
		fx := &fakeDbtx{
			versions:        versionRows(1, 2, 3, 4, 5, 6, 7),
			tables:          allTables(),
			hasV2Col:        false,
			hasV4AliasIndex: true,
		}
		err := verifySchema(ctx, fx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "v2 column observer_content_objects.created_at is missing")
	})
	t.Run("missing v4 alias index rejected", func(t *testing.T) {
		fx := &fakeDbtx{
			versions:        versionRows(1, 2, 3, 4, 5, 6, 7),
			tables:          allTables(),
			hasV2Col:        true,
			hasV4AliasIndex: false,
		}
		err := verifySchema(ctx, fx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "v4 provider-scoped alias identity index is missing")
	})
	t.Run("query failure propagated", func(t *testing.T) {
		fx := &fakeDbtx{queryErr: errors.New("pg unavailable")}
		err := verifySchema(ctx, fx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "schema verify")
	})
}

// TestObserverV2ColumnExists locks the v2 column probe: it reads exactly one
// EXISTS row and surfaces the boolean.
func TestObserverV2ColumnExists(t *testing.T) {
	ctx := context.Background()
	for _, want := range []bool{true, false} {
		fx := &fakeDbtx{hasV2Col: want}
		got, err := observerV2ColumnExists(ctx, fx)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	fx := &fakeDbtx{queryErr: errors.New("pg unavailable")}
	_, err := observerV2ColumnExists(ctx, fx)
	require.Error(t, err)
}

func TestObserverV4AliasIndexExists(t *testing.T) {
	ctx := context.Background()
	for _, want := range []bool{true, false} {
		fx := &fakeDbtx{hasV4AliasIndex: want}
		got, err := observerV4AliasIndexExists(ctx, fx)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	fx := &fakeDbtx{queryErr: errors.New("pg unavailable")}
	_, err := observerV4AliasIndexExists(ctx, fx)
	require.Error(t, err)
}

// TestBootstrapSchemaTxBranches locks every bootstrapSchemaTx branch: an
// empty schema applies every migration in order, complete v1-v4 schemas apply
// their pending suffix, a current v5 schema is an idempotent no-op, and a
// partial/unknown schema is rejected and never patched.
func TestBootstrapSchemaTxBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("empty schema applies all migrations in order", func(t *testing.T) {
		fx := &fakeDbtx{}
		require.NoError(t, bootstrapSchemaTx(ctx, fx, map[string]bool{}, nil))
		require.Len(t, fx.execs, len(observerMigrations))
		for i, file := range observerMigrations {
			want, err := migrationsFS.ReadFile(file)
			require.NoError(t, err)
			assert.Equal(t, string(want), fx.execs[i], "migration %s", file)
		}
	})
	for _, tt := range []struct {
		name     string
		versions []int
	}{
		{name: "complete v1 applies v2 through v4", versions: []int{1}},
		{name: "complete v2 applies v3 through v4", versions: []int{1, 2}},
		{name: "complete v3 applies v4", versions: []int{1, 2, 3}},
		{name: "complete v4 applies no structure migration", versions: []int{1, 2, 3, 4}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx := &fakeDbtx{}
			tables := map[string]bool{}
			for _, name := range requiredObserverTables {
				tables[name] = true
			}
			require.NoError(t, bootstrapSchemaTx(ctx, fx, tables, tt.versions))
			wantFiles := observerMigrations[len(tt.versions):]
			require.Len(t, fx.execs, len(wantFiles))
			for i, file := range wantFiles {
				want, err := migrationsFS.ReadFile(file)
				require.NoError(t, err)
				assert.Equal(t, string(want), fx.execs[i], "migration %s", file)
			}
		})
	}
	t.Run("current schema is an idempotent no-op", func(t *testing.T) {
		fx := &fakeDbtx{
			versions:        versionRows(1, 2, 3, 4, 5),
			tables:          allTables(),
			hasV2Col:        true,
			hasV4AliasIndex: true,
		}
		tables := map[string]bool{}
		for _, name := range requiredObserverTables {
			tables[name] = true
		}
		require.NoError(t, bootstrapSchemaTx(ctx, fx, tables, []int{1, 2, 3, 4, 5}))
		assert.Empty(t, fx.execs, "a current schema runs no migration")
	})
	t.Run("partial schema rejected, never patched", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(1)}
		tables := map[string]bool{"observer_sessions": true}
		err := bootstrapSchemaTx(ctx, fx, tables, []int{1})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "existing schema is not complete")
		assert.Empty(t, fx.execs, "a partial schema must never be patched")
	})
	t.Run("unknown version rejected", func(t *testing.T) {
		fx := &fakeDbtx{versions: versionRows(99)}
		tables := map[string]bool{}
		for _, name := range requiredObserverTables {
			tables[name] = true
		}
		err := bootstrapSchemaTx(ctx, fx, tables, []int{99})
		require.Error(t, err)
		assert.Empty(t, fx.execs, "an unknown version must never be patched")
	})
	t.Run("migration exec failure propagated", func(t *testing.T) {
		fx := &fakeDbtx{execErr: errors.New("ddl failed")}
		err := bootstrapSchemaTx(ctx, fx, map[string]bool{}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "apply")
	})
}

// TestPendingMigrations locks the pending structure-migration mapping: the
// version row count selects the structure suffix that still needs to run.
// Index migrations are excluded — they are applied asynchronously.
func TestPendingMigrations(t *testing.T) {
	assert.Equal(t, []string{"migrations/002_v2.sql", "migrations/003_v3.sql", "migrations/004_v4.sql"}, pendingMigrations([]int{1}))
	assert.Equal(t, []string{"migrations/003_v3.sql", "migrations/004_v4.sql"}, pendingMigrations([]int{1, 2}))
	assert.Equal(t, []string{"migrations/004_v4.sql"}, pendingMigrations([]int{1, 2, 3}))
	assert.Empty(t, pendingMigrations([]int{1, 2, 3, 4}))
	assert.Empty(t, pendingMigrations([]int{1, 2, 3, 4, 5}))
}

// TestObserverIndexMigrations locks the asynchronous index-migration contract:
// every entry names its index, carries a CONCURRENTLY create statement that
// mentions that name, and maps to a positive schema version. The v5 transcript
// index specifically targets observer_contexts (session_id, id).
func TestObserverIndexMigrations(t *testing.T) {
	require.NotEmpty(t, observerIndexMigrations)
	for _, m := range observerIndexMigrations {
		require.NotEmpty(t, m.name, "index name must be set")
		require.NotEmpty(t, m.createStmt, "create statement must be set")
		assert.Contains(t, m.createStmt, "CONCURRENTLY", "index build must not block writes: %s", m.name)
		assert.Contains(t, m.createStmt, m.name, "create statement must name its index")
		assert.Greater(t, m.version, 0, "index must map to a positive schema version")
	}
	assert.Equal(t, observerSchemaV5, observerIndexMigrations[0].version)
	assert.Equal(t, "idx_observer_contexts_session_id_id", observerIndexMigrations[0].name)
	assert.Contains(t, observerIndexMigrations[0].createStmt, "(session_id, id)")
}

// TestReadSchemaVersions locks the version listing: rows come back in version
// order and scan errors propagate.
func TestReadSchemaVersions(t *testing.T) {
	ctx := context.Background()
	fx := &fakeDbtx{versions: versionRows(1, 2)}
	versions, err := readSchemaVersions(ctx, fx)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2}, versions)

	fx2 := &fakeDbtx{queryErr: errors.New("pg unavailable")}
	_, err = readSchemaVersions(ctx, fx2)
	require.Error(t, err)
}

// TestMissingObserverTables locks the table listing: observer_* tables are
// discovered from information_schema and the missing set names exactly the
// absent required tables.
func TestMissingObserverTables(t *testing.T) {
	ctx := context.Background()
	fx := &fakeDbtx{tables: allTables()}
	missing, err := missingObserverTables(ctx, fx)
	require.NoError(t, err)
	assert.Empty(t, missing)

	fx2 := &fakeDbtx{tables: allTables()[:3]}
	missing, err = missingObserverTables(ctx, fx2)
	require.NoError(t, err)
	assert.Equal(t, requiredObserverTables[3:], missing)
}

// TestMigrationsEmbedded locks the embedded structure-migration set: all files
// exist, 001 creates the v1 version row, 002 adds retention metadata, 003 adds
// query indexes, and 004 fixes provider-scoped session-alias identity. The v5
// transcript index is an asynchronous index migration (observerIndexMigrations),
// not an embedded structure file.
func TestMigrationsEmbedded(t *testing.T) {
	require.Len(t, observerMigrations, 4)
	for _, file := range observerMigrations {
		data, err := migrationsFS.ReadFile(file)
		require.NoError(t, err, "migration %s must be embedded", file)
		require.NotEmpty(t, data)
	}
	v1, err := migrationsFS.ReadFile(observerMigrations[0])
	require.NoError(t, err)
	v1Body := strings.ReplaceAll(string(v1), "\r\n", "\n")
	assert.Contains(t, v1Body, "INSERT INTO observer_schema_versions (version, applied_at)\nVALUES (1, now())\nON CONFLICT (version) DO NOTHING")

	v2, err := migrationsFS.ReadFile(observerMigrations[1])
	require.NoError(t, err)
	body := strings.ReplaceAll(string(v2), "\r\n", "\n")
	assert.Contains(t, body, "ALTER TABLE observer_content_objects ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now()")
	assert.Contains(t, body, "ON CONFLICT (version) DO NOTHING")
	assert.Contains(t, body, "VALUES (2, now())")
	// The retention pass relies on both indexes; their absence would force
	// sequential scans, which the SSOT forbids.
	assert.Contains(t, body, "idx_observer_content_objects_created_at")
	assert.Contains(t, body, "idx_observer_contexts_item_digests")

	v3, err := migrationsFS.ReadFile(observerMigrations[2])
	require.NoError(t, err)
	body3 := strings.ReplaceAll(string(v3), "\r\n", "\n")
	// The keyset page lists order by (last_seen, id) / (occurred_at, id) and
	// the session model filter is an EXISTS probe over (session_id, model);
	// the v3 composite indexes serve exactly those shapes.
	assert.Contains(t, body3, "CREATE INDEX IF NOT EXISTS idx_observer_sessions_last_seen_id ON observer_sessions (last_seen DESC, id DESC)")
	assert.Contains(t, body3, "CREATE INDEX IF NOT EXISTS idx_observer_turns_occurred_at_id ON observer_turns (occurred_at DESC, id DESC)")
	assert.Contains(t, body3, "CREATE INDEX IF NOT EXISTS idx_observer_turns_session_id_model ON observer_turns (session_id, model)")
	assert.Contains(t, body3, "VALUES (3, now())")

	v4, err := migrationsFS.ReadFile(observerMigrations[3])
	require.NoError(t, err)
	body4 := strings.ReplaceAll(string(v4), "\r\n", "\n")
	assert.Contains(t, body4, "idx_observer_session_aliases_identity")
	assert.Contains(t, body4, "alias_digest,\n        provider")
	assert.Contains(t, body4, "VALUES (4, now())")
}
