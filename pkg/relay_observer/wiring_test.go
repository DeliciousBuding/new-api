package relayobserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
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

// TestRuntimeConfigInvalidFailsOpen proves that an unparsable configuration
// disables the observer with ReasonConfigInvalid and never opens a store.
func TestRuntimeConfigInvalidFailsOpen(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
// store (no SQL on the status path).
func TestRuntimeEnabledStatus(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
	t.Setenv("RELAY_OBSERVER_RECORD_IP", "true")
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

// TestRuntimeEnabledStoreFailureFailsOpen proves a store that fails after a
// successful init only degrades the observer (circuit open) — the runtime
// stays alive and readable, never panicking or disabling the process.
func TestRuntimeEnabledStoreFailureFailsOpen(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
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
