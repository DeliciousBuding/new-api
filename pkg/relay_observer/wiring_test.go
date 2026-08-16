package relayobserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the Runtime composition (wiring.go): the single fail-open
// runtime face that combines Config, Store, and Dispatcher. All tests use a
// scripted store and an injected opener seam — no real PostgreSQL is ever
// contacted — and never sleep: the scripted store signals writes
// deterministically. clearObserverEnv comes from config_test.go.

// openerRecorder is the injected store-opener seam: it counts calls and can
// script a store or an error. Thread-safe because Init may be driven
// concurrently.
type openerRecorder struct {
	mu    sync.Mutex
	calls int
	store Store
	err   error
}

func (o *openerRecorder) open(ctx context.Context, cfg Config) (Store, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if o.err != nil {
		return nil, o.err
	}
	return o.store, nil
}

func (o *openerRecorder) callCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

// enableObserverEnv clears the observer environment and sets the minimal valid
// enabled configuration — including the content HMAC key, which Init now
// requires. Tests that exercise a specific failure past the HMAC-key check
// layer their own variables on top.
func enableObserverEnv(t *testing.T) {
	t.Helper()
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
	t.Setenv("RELAY_OBSERVER_HMAC_KEY", "test-hmac-key")
}

// TestRuntimeUninitializedStatusDisabled proves the runtime is
// disabled-by-default and its status is stable before any Init.
func TestRuntimeUninitializedStatusDisabled(t *testing.T) {
	rt := NewRuntime()
	for i := 0; i < 3; i++ {
		st := rt.Status()
		assert.False(t, st.Enabled)
		assert.Equal(t, ReasonDisabled, st.ReasonCode)
		assert.Equal(t, IPTrustNone, st.IPTrust)
	}
}

// TestRuntimeDisabledInitOpensNoStore proves that a disabled configuration
// never opens a store: zero database connections, zero opener calls.
func TestRuntimeDisabledInitOpensNoStore(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "false")
	rec := &openerRecorder{store: &scriptedStore{}}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, 0, rec.callCount())
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonDisabled, st.ReasonCode)
}

// TestRuntimeMissingHMACKeyFailsClosed proves an enabled observer without a
// content HMAC key self-disables with ReasonHMACKeyMissing and never opens a
// store, instead of running in a misleading metadata-only-but-healthy state.
func TestRuntimeMissingHMACKeyFailsClosed(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
	rec := &openerRecorder{store: &scriptedStore{}}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, 0, rec.callCount())
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonHMACKeyMissing, st.ReasonCode)
	assert.Equal(t, IPTrustNone, st.IPTrust)
	assert.False(t, rt.CanPublish())
}

// TestRuntimeConfigInvalidFailsOpen proves that an unparsable configuration
// disables the observer with ReasonConfigInvalid and never opens a store.
func TestRuntimeConfigInvalidFailsOpen(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_QUEUE_SIZE", "not-a-number")
	rec := &openerRecorder{store: &scriptedStore{}}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, 0, rec.callCount())
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonConfigInvalid, st.ReasonCode)
}

// TestRuntimeNonPGDSNFailsOpen proves the real opener path: a non-PostgreSQL
// DSN is rejected before any connection is attempted and the observer is
// disabled with ReasonStoreInitFailed.
func TestRuntimeNonPGDSNFailsOpen(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_SQL_DSN", "mysql://user:pass@127.0.0.1:3306/obs")
	rt := NewRuntime()
	rt.Init()
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonStoreInitFailed, st.ReasonCode)
	assert.Equal(t, IPTrustNone, st.IPTrust)
}

// TestRuntimeStoreOpenErrorFailsOpen proves a generic store open failure
// (unreachable database, pool error) disables the observer fail-open.
func TestRuntimeStoreOpenErrorFailsOpen(t *testing.T) {
	enableObserverEnv(t)
	rec := &openerRecorder{
		err: errors.New("relayobserver: ping PostgreSQL pool: dial tcp 127.0.0.1:5432: connect: connection refused"),
	}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, 1, rec.callCount())
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonStoreInitFailed, st.ReasonCode)
}

// TestRuntimeSchemaErrorFailsOpen proves a schema verify/bootstrap failure
// disables the observer with the dedicated ReasonSchemaMismatch code.
func TestRuntimeSchemaErrorFailsOpen(t *testing.T) {
	enableObserverEnv(t)
	rec := &openerRecorder{
		err: errors.New("relayobserver: schema verify: version mismatch: have [2], want [1]"),
	}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, 1, rec.callCount())
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonSchemaMismatch, st.ReasonCode)
}

// TestRuntimeEnabledStatus proves a healthy init leaves the runtime enabled,
// exposes the effective IP trust tier, and that Status never touches the
// store (no SQL on the status path). TRUSTED_PROXIES=none pins the strict
// direct-connect tier so the assertion is deterministic.
func TestRuntimeEnabledStatus(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_RECORD_IP", "true")
	t.Setenv("TRUSTED_PROXIES", "none")
	store := &scriptedStore{}
	rec := &openerRecorder{store: store}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, 1, rec.callCount())

	st := rt.Status()
	require.True(t, st.Enabled)
	assert.Empty(t, st.ReasonCode)
	assert.Equal(t, IPTrustDirect, st.IPTrust)

	// The dispatcher is wired: an event is admitted.
	ev := sampleEvent()
	require.True(t, rt.disp.TryEnqueue(&ev, 1))

	// Repeated status reads are stable and never touch the store.
	writesBefore := store.writeCountSnapshot()
	for i := 0; i < 3; i++ {
		st := rt.Status()
		assert.True(t, st.Enabled)
	}
	assert.Equal(t, writesBefore, store.writeCountSnapshot())
}

// TestRuntimeCanPublishTracksEnabledState locks the lock-free fast path of
// the request-path hooks: only an enabled runtime with a started dispatcher
// reports publishable, and disabling or closing the runtime flips it off, so
// the fail-open disabled path allocates nothing in the hooks.
func TestRuntimeCanPublishTracksEnabledState(t *testing.T) {
	enableObserverEnv(t)
	store := &scriptedStore{}
	rec := &openerRecorder{store: store}
	rt := NewRuntime()
	rt.openStore = rec.open

	assert.False(t, rt.CanPublish(), "uninitialized runtime must not publish")
	rt.Init()
	assert.True(t, rt.CanPublish(), "enabled runtime must publish")
	require.True(t, rt.Status().Enabled)

	rt.Close(context.Background())
	assert.False(t, rt.CanPublish(), "closed runtime must not publish")
}

// TestRuntimeCanPublishDisabledConfig proves a disabled configuration never
// opens a store and never becomes publishable: the hooks stay zero-cost.
func TestRuntimeCanPublishDisabledConfig(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "false")
	rec := &openerRecorder{store: &scriptedStore{}}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, 0, rec.callCount())
	assert.False(t, rt.CanPublish())
}

// TestRuntimeEnabledStoreFailureFailsOpen proves a store that fails after a
// successful init only degrades the observer (circuit open) — the runtime
// stays alive and readable, never panicking or disabling the process.
func TestRuntimeEnabledStoreFailureFailsOpen(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_BATCH_SIZE", "1")
	store := &scriptedStore{err: errBoom}
	store.writeNotify = make(chan struct{}, 16)
	rec := &openerRecorder{store: store}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()

	ev := sampleEvent()
	require.True(t, rt.disp.TryEnqueue(&ev, 1))
	<-store.writeNotify // the worker attempted the write and failed

	require.Eventually(t, func() bool {
		return rt.Status().CircuitOpen
	}, 2*time.Second, 5*time.Millisecond)
	st := rt.Status()
	assert.True(t, st.Enabled) // runtime remains enabled; the circuit reports the degradation
	assert.Equal(t, ReasonCircuitOpen, st.ReasonCode)
}

// TestRuntimeInitIdempotent proves repeated Init calls have no effect: the
// opener runs exactly once.
func TestRuntimeInitIdempotent(t *testing.T) {
	enableObserverEnv(t)
	store := &scriptedStore{}
	rec := &openerRecorder{store: store}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	rt.Init()
	rt.Init()
	assert.Equal(t, 1, rec.callCount())
	assert.True(t, rt.Status().Enabled)
}

// TestRuntimeCloseIdempotent proves Close is idempotent and that a closed
// runtime reports disabled.
func TestRuntimeCloseIdempotent(t *testing.T) {
	enableObserverEnv(t)
	store := &scriptedStore{}
	rec := &openerRecorder{store: store}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	rt.Close(context.Background())
	rt.Close(context.Background())
	rt.Close(context.Background())
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonDisabled, st.ReasonCode)
}

// TestRuntimeCloseBeforeInit proves Close is a safe no-op on a never-initialized
// runtime.
func TestRuntimeCloseBeforeInit(t *testing.T) {
	rt := NewRuntime()
	rt.Close(context.Background())
	rt.Close(context.Background())
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonDisabled, st.ReasonCode)
}

// TestRuntimeCloseBoundedByContext proves shutdown respects the hard budget:
// a store whose Close blocks until the context expires never holds shutdown
// past the budget.
func TestRuntimeCloseBoundedByContext(t *testing.T) {
	enableObserverEnv(t)
	store := &blockingCloseStore{scriptedStore: scriptedStore{}}
	rec := &openerRecorder{store: store}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	rt.Close(ctx)
	elapsed := time.Since(start)
	// The observer never waits longer than the budget; the main shutdown
	// outcome is never changed by observer Close.
	assert.Less(t, elapsed, 2*time.Second)
}

// blockingCloseStore is a scripted store whose Close blocks until the budget
// expires, proving the runtime returns at the deadline instead of waiting
// indefinitely for the store.
type blockingCloseStore struct {
	scriptedStore
}

func (s *blockingCloseStore) Close(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestRuntimeStatusHidesSecrets proves every disabled path serializes a
// status that contains neither the DSN nor the HMAC keys, even when the raw
// store error echoed the DSN.
func TestRuntimeStatusHidesSecrets(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_SQL_DSN", "postgres://alice:topsecretpw@127.0.0.1:5432/obs")
	t.Setenv("RELAY_OBSERVER_HMAC_KEY", "hushhush")
	rec := &openerRecorder{
		err: errors.New("dial tcp 127.0.0.1:5432: connect: connection refused while opening postgres://alice:topsecretpw@127.0.0.1:5432/obs"),
	}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, ReasonStoreInitFailed, rt.Status().ReasonCode)

	out, err := common.Marshal(rt.Status())
	require.NoError(t, err)
	assert.NotContains(t, string(out), "topsecretpw")
	assert.NotContains(t, string(out), "hushhush")
	assert.NotContains(t, string(out), "5432")

	// The redacted Config view also hides the secrets on the enabled path.
	cfg := DefaultConfig()
	cfg.SQLDSN = "postgres://alice:topsecretpw@127.0.0.1:5432/obs"
	cfg.HMACKey = "hushhush"
	cfgText := cfg.String()
	assert.NotContains(t, cfgText, "topsecretpw")
	assert.NotContains(t, cfgText, "hushhush")
}

// TestRuntimeConcurrentInitCloseStatus proves Init/Close/Status are
// concurrency-safe: concurrent Init calls open the store exactly once and
// concurrent status reads stay stable, including under -race.
func TestRuntimeConcurrentInitCloseStatus(t *testing.T) {
	enableObserverEnv(t)
	store := &scriptedStore{}
	rec := &openerRecorder{store: store}
	rt := NewRuntime()
	rt.openStore = rec.open

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.Init()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.Status()
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, rec.callCount())
	assert.True(t, rt.Status().Enabled)

	rt.Close(context.Background())
	for i := 0; i < 8; i++ {
		assert.False(t, rt.Status().Enabled)
	}
}

// TestRuntimeSchemaClassificationBoundary proves the schema classification
// only matches schema-class errors; other store errors stay store_init_failed
// even when their text mentions schema-adjacent words.
func TestRuntimeSchemaClassificationBoundary(t *testing.T) {
	enableObserverEnv(t)
	rec := &openerRecorder{err: errors.New("relayobserver: open PostgreSQL pool: schema is unreachable")}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, ReasonStoreInitFailed, rt.Status().ReasonCode)
}

// TestRuntimeStatusJSONShape proves the status serializes with the frozen
// field names and contains no secret-bearing extras.
func TestRuntimeStatusJSONShape(t *testing.T) {
	rt := NewRuntime()
	out, err := common.Marshal(rt.Status())
	require.NoError(t, err)
	s := string(out)
	assert.True(t, strings.Contains(s, `"Enabled"`), s)
	assert.True(t, strings.Contains(s, `"ReasonCode"`), s)
	assert.True(t, strings.Contains(s, `"CircuitOpen"`), s)
	assert.NotContains(t, s, "dsn")
	assert.NotContains(t, s, "hmac")
}

// TestRuntimeInitPanicFailsOpen proves a store opener that panics disables the
// observer instead of crashing the process (SSOT: all observer entry points
// recover panics; the observer never panics).
func TestRuntimeInitPanicFailsOpen(t *testing.T) {
	enableObserverEnv(t)
	rt := NewRuntime()
	rt.openStore = func(ctx context.Context, cfg Config) (Store, error) {
		panic("scripted opener panic")
	}
	require.NotPanics(t, rt.Init)
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonStoreInitFailed, st.ReasonCode)
}

// TestRuntimeNilStoreFailsOpen proves an opener that succeeds without a store
// disables the observer: a nil store would panic the shutdown path (a nil
// interface method call on Store.Close inside Dispatcher.Stop).
func TestRuntimeNilStoreFailsOpen(t *testing.T) {
	enableObserverEnv(t)
	rt := NewRuntime()
	rt.openStore = func(ctx context.Context, cfg Config) (Store, error) {
		return nil, nil
	}
	rt.Init()
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonStoreInitFailed, st.ReasonCode)
}

// panicCloseStore is a scripted store whose Close panics, proving the runtime
// absorbs a store release failure during shutdown instead of crashing the main
// shutdown path.
type panicCloseStore struct {
	scriptedStore
}

func (s *panicCloseStore) Close(ctx context.Context) error {
	panic("scripted store close panic")
}

// TestRuntimeClosePanicAbsorbed proves a store Close panic during shutdown is
// absorbed by the runtime: the main shutdown outcome never changes (SSOT:
// "an observer Stop failure never changes the main shutdown outcome").
func TestRuntimeClosePanicAbsorbed(t *testing.T) {
	enableObserverEnv(t)
	rt := NewRuntime()
	rt.openStore = func(ctx context.Context, cfg Config) (Store, error) {
		return &panicCloseStore{}, nil
	}
	rt.Init()
	require.True(t, rt.Status().Enabled)
	require.NotPanics(t, func() { rt.Close(context.Background()) })
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonDisabled, st.ReasonCode)
}

// TestRuntimeIPTrustRequiresDualOptIn proves the effective tier is "none"
// unless both opt-ins are on: the NewAPI system setting (LogRecordIpEnabled)
// and RELAY_OBSERVER_RECORD_IP (SSOT IP section).
func TestRuntimeIPTrustRequiresDualOptIn(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_RECORD_IP", "true")
	orig := common.LogRecordIpEnabled
	common.LogRecordIpEnabled = false
	t.Cleanup(func() { common.LogRecordIpEnabled = orig })
	rt := NewRuntime()
	rt.openStore = (&openerRecorder{store: &scriptedStore{}}).open
	rt.Init()
	st := rt.Status()
	require.True(t, st.Enabled)
	assert.Equal(t, IPTrustNone, st.IPTrust)
}

// TestRuntimeIPTrustProxyUnderDefaultTrust proves the tier is "proxy" under
// the default trusted-CIDR configuration (TRUSTED_PROXIES unset), because
// gin.ClientIP() may then derive the peer from X-Forwarded-For (SSOT: peers on
// trusted-proxy networks may supply X-Forwarded-For).
func TestRuntimeIPTrustProxyUnderDefaultTrust(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_RECORD_IP", "true")
	t.Setenv("TRUSTED_PROXIES", "")
	rt := NewRuntime()
	rt.openStore = (&openerRecorder{store: &scriptedStore{}}).open
	rt.Init()
	st := rt.Status()
	require.True(t, st.Enabled)
	assert.Equal(t, IPTrustProxy, st.IPTrust)
}

// TestRuntimeIPTrustDirectStrictConnect proves the tier is "direct" only under
// TRUSTED_PROXIES=none, where ClientIP() is the RemoteAddr origin.
func TestRuntimeIPTrustDirectStrictConnect(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_RECORD_IP", "true")
	t.Setenv("TRUSTED_PROXIES", "none")
	rt := NewRuntime()
	rt.openStore = (&openerRecorder{store: &scriptedStore{}}).open
	rt.Init()
	st := rt.Status()
	require.True(t, st.Enabled)
	assert.Equal(t, IPTrustDirect, st.IPTrust)
}

// TestRuntimeSchemaSubpathClassifiedMismatch proves the schema classification
// also covers the verify sub-path errors (information_schema listing) that
// store_pg.go returns without the "schema verify:" wrapper: they are schema
// check failures, not store init failures.
func TestRuntimeSchemaSubpathClassifiedMismatch(t *testing.T) {
	enableObserverEnv(t)
	rec := &openerRecorder{err: errors.New("relayobserver: list observer tables: permission denied for table information_schema")}
	rt := NewRuntime()
	rt.openStore = rec.open
	rt.Init()
	assert.Equal(t, ReasonSchemaMismatch, rt.Status().ReasonCode)
}

// TestRuntimeInitStoreOpenBounded proves the store open is bounded: a store
// opener that blocks until its context expires cannot stall Init (and with it
// NewAPI startup, which calls Init synchronously before listening) past the
// open budget. The opener sees a cancellable context and the runtime ends up
// disabled fail-open.
func TestRuntimeInitStoreOpenBounded(t *testing.T) {
	enableObserverEnv(t)
	rt := NewRuntime()
	rt.openTimeout = 50 * time.Millisecond
	rt.openStore = func(ctx context.Context, cfg Config) (Store, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	rt.Init()
	elapsed := time.Since(start)
	// The runtime never stalls Init past the budget; the opener's own wait is
	// cut short by the runtime-provided context, not by the test.
	assert.Less(t, elapsed, time.Second)
	st := rt.Status()
	assert.False(t, st.Enabled)
	assert.Equal(t, ReasonStoreInitFailed, st.ReasonCode)
}

// ---------------------------------------------------------------------------
// T3.2 query surface and HMAC key wiring

// fakeQueryStore is a minimal QueryStore stub for the querySurface seam: the
// wiring tests only assert that the seam result passes through unchanged, so
// every method returns zero values.
type fakeQueryStore struct {
	store Store
}

func (fakeQueryStore) Overview(ctx context.Context, query OverviewQuery) (OverviewResult, error) {
	return OverviewResult{}, nil
}

func (fakeQueryStore) ListSessions(ctx context.Context, query SessionQuery) (SessionPage, error) {
	return SessionPage{}, nil
}

func (fakeQueryStore) ListTurns(ctx context.Context, query TurnQuery) (TurnPage, error) {
	return TurnPage{}, nil
}

func (fakeQueryStore) TurnContext(ctx context.Context, query ContextQuery) (TurnContextResult, error) {
	return TurnContextResult{}, nil
}

func (fakeQueryStore) Transcript(ctx context.Context, query TranscriptQuery) (TranscriptPage, error) {
	return TranscriptPage{}, nil
}

func (fakeQueryStore) GetSession(ctx context.Context, id uuid.UUID) (SessionSummary, error) {
	return SessionSummary{}, nil
}

// TestRuntimeQuerySurfaceUninitialized proves an uninitialized runtime has no
// query surface: (nil, 0, false) so the Root controllers emit the degraded
// envelope instead of touching a query that cannot run.
func TestRuntimeQuerySurfaceUninitialized(t *testing.T) {
	rt := NewRuntime()
	qs, timeout, ok := rt.QuerySurface()
	assert.Nil(t, qs)
	assert.Zero(t, timeout)
	assert.False(t, ok)
}

// TestRuntimeQuerySurfaceDisabled proves a runtime disabled by configuration
// has no query surface either, exactly like the uninitialized state.
func TestRuntimeQuerySurfaceDisabled(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "false")
	rt := NewRuntime()
	rt.Init()
	qs, timeout, ok := rt.QuerySurface()
	assert.Nil(t, qs)
	assert.Zero(t, timeout)
	assert.False(t, ok)
}

// TestRuntimeQuerySurfaceNonPGStore proves a runtime whose store cannot be
// wrapped by the query port (any non-PostgreSQL adapter) reports the query
// surface unavailable: the wrapper is created lazily and a wrap failure is
// fail-open, never a panic.
func TestRuntimeQuerySurfaceNonPGStore(t *testing.T) {
	enableObserverEnv(t)
	rt := NewRuntime()
	rt.openStore = (&openerRecorder{store: &scriptedStore{}}).open
	rt.Init()
	require.True(t, rt.Status().Enabled)
	qs, timeout, ok := rt.QuerySurface()
	assert.Nil(t, qs)
	assert.Zero(t, timeout)
	assert.False(t, ok)
}

// TestRuntimeQuerySurfaceWrapsPGStore proves the default path wraps the
// enabled PostgreSQL store lazily: the first call creates the bounded query
// surface and caches it (repeated calls return the same instance), and the
// stored query timeout from the configuration is passed through. The pool is
// never dialed: sql.Open is lazy and the wrapper never queries.
func TestRuntimeQuerySurfaceWrapsPGStore(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_QUERY_TIMEOUT_MS", "250")
	db, err := sql.Open("pgx", "postgres://observer:observer@127.0.0.1:55433/relay_observer")
	require.NoError(t, err)
	defer db.Close()
	rt := NewRuntime()
	rt.openStore = (&openerRecorder{store: newPGStore(db)}).open
	rt.Init()
	require.True(t, rt.Status().Enabled)

	qs, timeout, ok := rt.QuerySurface()
	require.True(t, ok)
	require.NotNil(t, qs)
	assert.Equal(t, 250*time.Millisecond, timeout)
	qs2, timeout2, ok2 := rt.QuerySurface()
	require.True(t, ok2)
	assert.Same(t, qs, qs2, "the query surface wrapper must be cached")
	assert.Equal(t, timeout, timeout2)
}

// TestRuntimeQuerySurfaceSeamInjected proves the private querySurface seam
// replaces the whole default behavior: an injected surface is returned
// verbatim, so tests can drive every controller failure path without a real
// store (same pattern as the openStore seam).
func TestRuntimeQuerySurfaceSeamInjected(t *testing.T) {
	rt := NewRuntime()
	scripted := &scriptedStore{}
	rt.querySurface = func() (QueryStore, time.Duration, bool) {
		return fakeQueryStore{store: scripted}, 7 * time.Second, true
	}
	qs, timeout, ok := rt.QuerySurface()
	assert.True(t, ok)
	assert.Equal(t, 7*time.Second, timeout)
	_, isFake := qs.(fakeQueryStore)
	assert.True(t, isFake, "the seam result must pass through unchanged")

	// The seam may also report an unavailable surface.
	rt.querySurface = func() (QueryStore, time.Duration, bool) {
		return nil, 0, false
	}
	qs, timeout, ok = rt.QuerySurface()
	assert.Nil(t, qs)
	assert.Zero(t, timeout)
	assert.False(t, ok)
}

// TestRuntimeHMACKeyEmptyByDefault proves the HMAC key of an uninitialized or
// key-less runtime is empty, which the turn context reconstruction treats as
// "skip the digest re-verification" (T3.1 semantics).
func TestRuntimeHMACKeyEmptyByDefault(t *testing.T) {
	rt := NewRuntime()
	assert.Empty(t, rt.HMACKey())
}

// TestRuntimeHMACKeyStoredFromConfig proves Init stores the configured HMAC
// key and the runtime hands it back verbatim (the Root turn-context handler
// uses it for the T2.3 digest re-verification).
func TestRuntimeHMACKeyStoredFromConfig(t *testing.T) {
	enableObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_HMAC_KEY", "test-hmac-key-material")
	rt := NewRuntime()
	rt.openStore = (&openerRecorder{store: &scriptedStore{}}).open
	rt.Init()
	assert.Equal(t, "test-hmac-key-material", rt.HMACKey())
}

// TestQuerySurfaceSeamCalledWithoutLock is the P2-2 regression: an injected
// query-surface seam must be called outside the runtime lock. The old code
// invoked the seam while holding r.mu, so a seam that re-enters the runtime
// (QuerySurface, HMACKey, Status) deadlocked on the same lock. The seam here
// re-enters via HMACKey; the call must complete, never deadlock.
func TestQuerySurfaceSeamCalledWithoutLock(t *testing.T) {
	rt := NewRuntime()
	rt.mu.Lock()
	rt.state = stateEnabled
	rt.cfg.QueryTimeout = time.Second
	rt.queryStore = fakeQueryStore{}
	rt.mu.Unlock()

	rt.querySurface = func() (QueryStore, time.Duration, bool) {
		_ = rt.HMACKey() // re-enters the runtime lock; deadlocks if the seam runs under r.mu
		return fakeQueryStore{}, 2 * time.Second, true
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		qs, timeout, ok := rt.QuerySurface()
		require.True(t, ok)
		assert.Equal(t, 2*time.Second, timeout)
		assert.NotNil(t, qs)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("QuerySurface seam deadlocked on the runtime lock")
	}
}
