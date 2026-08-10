//go:build relay_observer_pg_integration

package relayobserver

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file runs against a real disposable PostgreSQL. It is compiled only
// under the relay_observer_pg_integration build tag; the default suite never
// skips and never dials a database. TEST_RELAY_OBSERVER_POSTGRES_DSN is
// mandatory and is refused unless it targets the disposable local instance:
// loopback host (127.0.0.1 or localhost), port 55433, database relay_observer.

// validateIntegrationDSN refuses any DSN that is not the disposable local
// PostgreSQL: only loopback, only port 55433, and only the relay_observer
// database. A non-empty unknown database is rejected.
func validateIntegrationDSN(dsn string) error {
	if strings.TrimSpace(dsn) == "" {
		return fmt.Errorf("TEST_RELAY_OBSERVER_POSTGRES_DSN must be set for relay_observer_pg_integration tests")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("TEST_RELAY_OBSERVER_POSTGRES_DSN must be parseable by pgx: %v", err)
	}
	if cfg.Host != "127.0.0.1" && cfg.Host != "localhost" {
		return fmt.Errorf("TEST_RELAY_OBSERVER_POSTGRES_DSN must target loopback only, got host %q", cfg.Host)
	}
	if cfg.Port != 55433 {
		return fmt.Errorf("TEST_RELAY_OBSERVER_POSTGRES_DSN must target port 55433, got %d", cfg.Port)
	}
	if cfg.Database != "relay_observer" {
		return fmt.Errorf("TEST_RELAY_OBSERVER_POSTGRES_DSN must target the relay_observer database, got %q", cfg.Database)
	}
	return nil
}

// integrationDSN returns the validated test DSN, failing the test when the
// environment does not point at the disposable local instance.
func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_RELAY_OBSERVER_POSTGRES_DSN")
	require.NoError(t, validateIntegrationDSN(dsn), "refusing to run integration tests against a non-disposable database")
	return dsn
}

// openFixturePool opens a test-only pool (independent of the adapter) for
// fixture setup: schema cleanup and direct assertions.
func openFixturePool(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { db.Close() })
	return db
}

// cleanupObserverSchema drops every observer_* table so tests start from a
// known-empty observer schema. Table names come from the database's own
// information_schema and are fixed-prefix, never user input.
func cleanupObserverSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name LIKE 'observer\_%' ESCAPE '\'`)
	require.NoError(t, err)
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for _, name := range names {
		_, err := db.Exec("DROP TABLE IF EXISTS " + name)
		require.NoError(t, err, "drop %s", name)
	}
}

// ensureV1 brings the schema to the complete v1 state for tests that need a
// working database: an absent or partial observer schema is dropped and
// bootstrapped from scratch.
func ensureV1(t *testing.T, dsn string) {
	t.Helper()
	db := openFixturePool(t, dsn)
	store, err := OpenPGStore(context.Background(), dsn, SchemaModeVerify)
	if err == nil {
		require.NoError(t, store.Close(context.Background()))
		return
	}
	cleanupObserverSchema(t, db)
	store, err = OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err, "bootstrap must succeed on an empty observer schema")
	require.NoError(t, store.Close(context.Background()))
}

// TestIntegrationDSNGuard locks the mandatory target: the tests refuse any
// database that is not the disposable local relay_observer instance.
func TestIntegrationDSNGuard(t *testing.T) {
	base := validKeywordDSN
	require.NoError(t, validateIntegrationDSN(base))

	bad := []string{
		"",
		"   ",
		"host=db.internal.example port=55433 user=postgres dbname=relay_observer",
		"host=127.0.0.1 port=5432 user=postgres dbname=relay_observer",
		"host=127.0.0.1 port=55433 user=postgres dbname=some_other_db",
		"postgres://postgres@127.0.0.1:55433/some_other_db?sslmode=disable",
	}
	for _, dsn := range bad {
		require.Error(t, validateIntegrationDSN(dsn), "DSN %q must be rejected", dsn)
	}
}

// TestIntegrationSchemaLifecycle covers bootstrap on an empty observer schema,
// verify right after, repeated bootstrap (idempotent success on complete v1),
// and verify again.
func TestIntegrationSchemaLifecycle(t *testing.T) {
	dsn := integrationDSN(t)
	db := openFixturePool(t, dsn)
	cleanupObserverSchema(t, db)

	store, err := OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err, "bootstrap must create v1 on an empty observer schema")
	require.NoError(t, store.Close(context.Background()))

	verifyStore, err := OpenPGStore(context.Background(), dsn, SchemaModeVerify)
	require.NoError(t, err, "verify must pass right after bootstrap")
	require.NoError(t, verifyStore.Close(context.Background()))

	againStore, err := OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err, "repeated bootstrap must be idempotent success on complete v1")
	require.NoError(t, againStore.Close(context.Background()))

	finalVerify, err := OpenPGStore(context.Background(), dsn, SchemaModeVerify)
	require.NoError(t, err, "verify must still pass after the repeated bootstrap")
	require.NoError(t, finalVerify.Close(context.Background()))
}

// TestIntegrationVerifyRejectsMissingSchema covers verify on an absent
// observer schema: the observer must be disabled, never created implicitly.
func TestIntegrationVerifyRejectsMissingSchema(t *testing.T) {
	dsn := integrationDSN(t)
	db := openFixturePool(t, dsn)
	cleanupObserverSchema(t, db)

	store, err := OpenPGStore(context.Background(), dsn, SchemaModeVerify)
	require.Error(t, err)
	assert.Nil(t, store)

	// Restore v1 for later tests.
	store, err = OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err)
	require.NoError(t, store.Close(context.Background()))
}

// TestIntegrationSchemaMismatchRejected covers partial and unknown-version
// schemas: verify and bootstrap both fail, and bootstrap never patches the
// broken schema.
func TestIntegrationSchemaMismatchRejected(t *testing.T) {
	dsn := integrationDSN(t)
	db := openFixturePool(t, dsn)
	ensureV1(t, dsn)

	// Partial schema: drop one required table.
	_, err := db.Exec("DROP TABLE observer_turns")
	require.NoError(t, err)

	store, err := OpenPGStore(context.Background(), dsn, SchemaModeVerify)
	require.Error(t, err, "verify must reject a partial schema")
	assert.Nil(t, store)

	store, err = OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.Error(t, err, "bootstrap must reject a partial schema")
	assert.Nil(t, store)

	var exists bool
	require.NoError(t, db.QueryRow("SELECT to_regclass('observer_turns') IS NOT NULL").Scan(&exists))
	assert.False(t, exists, "bootstrap must never patch a partial schema")

	// Unknown version: a foreign version row makes verify fail.
	_, err = db.Exec("INSERT INTO observer_schema_versions (version, applied_at) VALUES (99, now())")
	require.NoError(t, err)
	store, err = OpenPGStore(context.Background(), dsn, SchemaModeVerify)
	require.Error(t, err, "verify must reject an unknown schema version")
	assert.Nil(t, store)

	// Restore complete v1 for later tests.
	cleanupObserverSchema(t, db)
	store, err = OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err)
	require.NoError(t, store.Close(context.Background()))
}

// TestIntegrationBootstrapAdvisoryLock covers the short advisory lock:
// bootstrap fails fast while another transaction holds the lock and succeeds
// once the lock is released.
func TestIntegrationBootstrapAdvisoryLock(t *testing.T) {
	dsn := integrationDSN(t)
	db := openFixturePool(t, dsn)
	cleanupObserverSchema(t, db)

	lockTx, err := db.Begin()
	require.NoError(t, err)
	defer lockTx.Rollback()
	var locked bool
	require.NoError(t, lockTx.QueryRow("SELECT pg_try_advisory_xact_lock($1)", observerSchemaLockKey).Scan(&locked))
	require.True(t, locked, "fixture must acquire the bootstrap advisory lock")

	store, err := OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.Error(t, err, "bootstrap must fail while the advisory lock is held")
	assert.Nil(t, store)
	assert.Contains(t, err.Error(), "advisory lock")

	// Release the lock; bootstrap succeeds on the still-empty schema.
	require.NoError(t, lockTx.Rollback())
	store, err = OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err, "bootstrap must succeed after the advisory lock is released")
	require.NoError(t, store.Close(context.Background()))
}

// TestIntegrationWriteBatchIdempotent covers the batch write: every event
// gets a fresh row UUID, and duplicate (node_scope, event_id) rows are
// skipped by ON CONFLICT DO NOTHING.
func TestIntegrationWriteBatchIdempotent(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	scope := "t13-idem"
	events := []Event{
		sampleEvent(),
		sampleEvent(),
		sampleEvent(),
	}
	for i := range events {
		events[i].NodeScope = scope
		events[i].EventID = fmt.Sprintf("req-%d", i+1)
		events[i].OccurredAt = epoch.Add(time.Duration(i) * time.Second)
	}
	require.NoError(t, store.WriteBatch(context.Background(), events))

	// The same batch again: the three rows must not duplicate.
	require.NoError(t, store.WriteBatch(context.Background(), events))

	db := openFixturePool(t, dsn)
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_turns WHERE node_scope = $1", scope).Scan(&count))
	assert.Equal(t, 3, count, "duplicate (node_scope, event_id) rows must be skipped")

	// Every row carries a distinct UUID generated by the store.
	rows, err := db.Query("SELECT id FROM observer_turns WHERE node_scope = $1 ORDER BY event_id", scope)
	require.NoError(t, err)
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		parsed, err := uuid.Parse(id)
		require.NoError(t, err, "row id %q must be a UUID", id)
		assert.NotEqual(t, uuid.Nil, parsed)
		seen[id] = true
	}
	require.NoError(t, rows.Err())
	assert.Len(t, seen, 3, "each event must get its own row UUID")
}

// TestIntegrationWriteBatchAttemptsAndIPTrust covers the full event mapping:
// attempts as JSONB via common.Marshal, attempts_omitted, typed IPTrust
// mapped verbatim to ip_trust, client_ip as INET, and the geo fields.
func TestIntegrationWriteBatchAttemptsAndIPTrust(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	ev := sampleEvent()
	ev.NodeScope = "t13-attempts"
	ev.EventID = "req-attempts"
	ev.SessionID = ptrUUID()
	ev.Attempts = []AttemptSummary{
		{ChannelID: 7, Group: "default", StatusCode: 429, ErrorCode: "rate_limit", ElapsedMS: 5},
		{ChannelID: 9, Group: "default", StatusCode: 200, ErrorCode: "", ElapsedMS: 145},
	}
	ev.AttemptsOmitted = 3
	ev.ClientIP = net.ParseIP("203.0.113.7")
	ev.IPTrust = IPTrustDirect
	ev.CountryCode = "US"
	ev.Country = "United States"
	ev.City = "Ashburn"
	ev.ASN = 64512
	ev.ASNOrg = "Example"
	require.NoError(t, store.WriteBatch(context.Background(), []Event{ev}))

	db := openFixturePool(t, dsn)
	var (
		attempts    []byte
		omitted     int
		ipTrust     *string
		clientIP    *string
		sessionID   *string
		countryCode string
		country     string
		city        string
		asn         int64
		asnOrg      string
	)
	err := db.QueryRow(`SELECT attempts, attempts_omitted, ip_trust, host(client_ip), session_id, country_code, country, city, asn, asn_org
		FROM observer_turns WHERE node_scope = $1 AND event_id = $2`, ev.NodeScope, ev.EventID).
		Scan(&attempts, &omitted, &ipTrust, &clientIP, &sessionID, &countryCode, &country, &city, &asn, &asnOrg)
	require.NoError(t, err)

	// attempts must decode back to the exact summary (JSONB key ordering may
	// differ from the input, so compare after unmarshal).
	var got []AttemptSummary
	require.NoError(t, common.Unmarshal(attempts, &got))
	assert.Equal(t, ev.Attempts, got)
	assert.Equal(t, 3, omitted)
	require.NotNil(t, ipTrust)
	assert.Equal(t, "direct", *ipTrust, "typed IPTrust must map verbatim to ip_trust")
	require.NotNil(t, clientIP)
	assert.Equal(t, "203.0.113.7", *clientIP)
	require.NotNil(t, sessionID)
	assert.Equal(t, ev.SessionID.String(), *sessionID)
	assert.Equal(t, "US", countryCode)
	assert.Equal(t, "United States", country)
	assert.Equal(t, "Ashburn", city)
	assert.Equal(t, int64(64512), asn)
	assert.Equal(t, "Example", asnOrg)
}

// TestIntegrationIPTrustNoneAndNulls covers the capture-off mapping: no IP
// capture means NULL client_ip and NULL ip_trust, and a nil attempts summary
// becomes an empty JSON array.
func TestIntegrationIPTrustNoneAndNulls(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	ev := sampleEvent()
	ev.NodeScope = "t13-nulls"
	ev.EventID = "req-nulls"
	ev.ClientIP = nil
	ev.IPTrust = ""
	ev.Attempts = nil
	require.NoError(t, store.WriteBatch(context.Background(), []Event{ev}))

	db := openFixturePool(t, dsn)
	var (
		clientIP *string
		ipTrust  *string
		attempts []byte
	)
	require.NoError(t, db.QueryRow(`SELECT host(client_ip), ip_trust, attempts FROM observer_turns WHERE node_scope = $1 AND event_id = $2`,
		ev.NodeScope, ev.EventID).Scan(&clientIP, &ipTrust, &attempts))
	assert.Nil(t, clientIP)
	assert.Nil(t, ipTrust)
	assert.Equal(t, "[]", string(attempts), "nil attempts must become an empty JSON array, never SQL NULL")
}

// TestIntegrationContextCancellation covers the context contract on the live
// database: a canceled WriteBatch aborts without persisting, and a canceled
// Close returns immediately without blocking the pool.
func TestIntegrationContextCancellation(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	ev := sampleEvent()
	ev.NodeScope = "t13-cancel"
	ev.EventID = "req-cancel"
	err := store.WriteBatch(canceled, []Event{ev})
	require.ErrorIs(t, err, context.Canceled)

	db := openFixturePool(t, dsn)
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_turns WHERE node_scope = $1", ev.NodeScope).Scan(&count))
	assert.Zero(t, count, "a canceled batch must not persist anything")

	err = store.Close(canceled)
	require.ErrorIs(t, err, context.Canceled, "Close must return before a canceled context expires")
	require.NoError(t, store.Close(context.Background()))
}

// TestIntegrationCloseReleasesPool covers the Close contract on the live
// database: idempotent, and the pool rejects writes afterwards.
func TestIntegrationCloseReleasesPool(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)

	require.NoError(t, store.WriteBatch(context.Background(), []Event{sampleEvent()}))
	require.NoError(t, store.Close(context.Background()))
	require.NoError(t, store.Close(context.Background()), "Close must be idempotent")

	err := store.WriteBatch(context.Background(), []Event{sampleEvent()})
	require.Error(t, err, "writes after Close must fail")
}

// TestIntegrationPoolBounds covers the SSOT pool limits on the live database:
// the configured caps, and concurrent batch writes never using more than two
// connections.
func TestIntegrationPoolBounds(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	pg := store.(*pgStore)
	assert.Equal(t, defaultPGPoolConfig, pg.poolCfg)
	assert.Equal(t, 2, pg.db.Stats().MaxOpenConnections)

	// Eight concurrent writers must share the two-connection pool. The peak
	// sampler polls Stats; the assert is on the bounded pool, never on timing.
	const writers = 8
	const perWriter = 50
	start := make(chan struct{})
	errCh := make(chan error, writers)
	var peak int32
	stopSampling := make(chan struct{})
	var samplingDone sync.WaitGroup
	samplingDone.Add(1)
	go func() {
		defer samplingDone.Done()
		for {
			select {
			case <-stopSampling:
				return
			default:
				inUse := int32(pg.db.Stats().InUse)
				for {
					prev := atomic.LoadInt32(&peak)
					if inUse <= prev || atomic.CompareAndSwapInt32(&peak, prev, inUse) {
						break
					}
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	for i := 0; i < writers; i++ {
		go func(i int) {
			<-start
			events := make([]Event, perWriter)
			for j := range events {
				events[j] = sampleEvent()
				events[j].NodeScope = "t13-pool"
				events[j].EventID = fmt.Sprintf("req-%d-%d", i, j)
			}
			errCh <- store.WriteBatch(context.Background(), events)
		}(i)
	}
	close(start)
	for i := 0; i < writers; i++ {
		require.NoError(t, <-errCh)
	}
	close(stopSampling)
	samplingDone.Wait()
	assert.LessOrEqual(t, atomic.LoadInt32(&peak), int32(2), "concurrent writes must never exceed the two-connection pool")
	assert.LessOrEqual(t, pg.db.Stats().OpenConnections, 2)

	db := openFixturePool(t, dsn)
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_turns WHERE node_scope = 't13-pool'").Scan(&count))
	assert.Equal(t, writers*perWriter, count)
}

// openVerifyStore opens a Store against the complete v1 schema.
func openVerifyStore(t *testing.T, dsn string) Store {
	t.Helper()
	store, err := OpenPGStore(context.Background(), dsn, SchemaModeVerify)
	require.NoError(t, err)
	return store
}
