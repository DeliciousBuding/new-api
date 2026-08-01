package relayobserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
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
