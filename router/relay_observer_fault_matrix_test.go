package router

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relayobserver "github.com/QuantumNous/new-api/pkg/relay_observer"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the T3.3 store fault regression matrix over the T3.2 Root
// observer routes. It drives the same real middleware chain (SetApiRouter)
// and the same controller.SetRelayObserverRuntime injection point as the
// production wiring, and proves the SSOT red line "the observer never affects
// the main service" at the HTTP layer:
//
//   - unavailable: a store connection failure whose raw error carries DSN/HMAC
//     material degrades every query route (200 + data.degraded=true +
//     reason=unavailable) with zero secret leakage in the response;
//   - timeout: a typed QueryError{Kind:Timeout} or a raw context deadline
//     degrades every query route with reason=timeout;
//   - locked: a real PostgreSQL table lock blocks the query until the injected
//     millisecond timeout (dual mode: real restricted PG when
//     TEST_RELAY_OBSERVER_POSTGRES_DSN points at the disposable local
//     instance, fake injection otherwise);
//   - probes: GET /api/status and GET /api/notice return byte-identical
//     responses while the observer is faulted — the audit line cannot affect
//     the normal service;
//   - P2-1: the overview window clamps (window_seconds>3600, windows>48) are
//     asserted on the value the store actually receives.
//
// All fault injection is deterministic (fake runtime or a controlled table
// lock); no test sleeps to win a race.

// recordingQueryStore wraps fakeObserverQueryStore and records the Overview
// query the controller actually issued, so the P2-1 clamp branches can be
// asserted on the store-side input rather than on the response echo.
type recordingQueryStore struct {
	*fakeObserverQueryStore
	overviewQuery relayobserver.OverviewQuery
}

func (r *recordingQueryStore) Overview(ctx context.Context, query relayobserver.OverviewQuery) (relayobserver.OverviewResult, error) {
	r.overviewQuery = query
	return r.fakeObserverQueryStore.Overview(ctx, query)
}

// faultMatrixRoutes maps each concrete request path to the fake store field
// that fails on that route, so one table drives the whole matrix. The session
// turns route reaches ListTurns only after its GetSession preflight succeeds
// (the fake's zero-value row returns no error).
var faultMatrixRoutes = []struct {
	name  string
	path  string
	fault func(f *fakeObserverQueryStore, err error)
}{
	{name: "overview", path: "/api/relay-observer/overview", fault: func(f *fakeObserverQueryStore, err error) { f.overviewErr = err }},
	{name: "sessions", path: "/api/relay-observer/sessions", fault: func(f *fakeObserverQueryStore, err error) { f.sessionsErr = err }},
	{name: "session", path: "/api/relay-observer/sessions/00000000-0000-0000-0000-000000000001", fault: func(f *fakeObserverQueryStore, err error) { f.sessionErr = err }},
	{name: "session turns", path: "/api/relay-observer/sessions/00000000-0000-0000-0000-000000000001/turns", fault: func(f *fakeObserverQueryStore, err error) { f.turnsErr = err }},
	{name: "turn context", path: "/api/relay-observer/turns/00000000-0000-0000-0000-000000000001/context?session_id=00000000-0000-0000-0000-0000000000bb", fault: func(f *fakeObserverQueryStore, err error) { f.contextErr = err }},
}

// TestRelayObserverFaultMatrixDegraded runs the two fake-injected fault
// dimensions over all five query routes. Every row must land on the degraded
// envelope with the stable reason, and the unavailable dimension must leak
// none of the raw error, DSN, HMAC, or endpoint material.
func TestRelayObserverFaultMatrixDegraded(t *testing.T) {
	raw := "dial tcp 127.0.0.1:55433: connect: connection refused while opening postgres://obs:topsecretpw@127.0.0.1:55433/relay_observer (hmac=hushhush)"
	dimensions := []struct {
		name   string
		err    error
		reason string
		leaks  []string
	}{
		{name: "unavailable", err: errors.New(raw), reason: "unavailable",
			leaks: []string{raw, "topsecretpw", "hushhush", "postgres://", "55433", "127.0.0.1"}},
		{name: "timeout typed", err: &relayobserver.QueryError{Kind: relayobserver.QueryErrTimeout, Msg: "query slot busy"}, reason: "timeout"},
		{name: "timeout deadline", err: context.DeadlineExceeded, reason: "timeout"},
	}
	for _, dim := range dimensions {
		for _, route := range faultMatrixRoutes {
			t.Run(dim.name+"/"+route.name, func(t *testing.T) {
				engine, rootToken, _, _ := relayObserverTestEnv(t)
				store := &fakeObserverQueryStore{}
				route.fault(store, dim.err)
				injectObserverRuntime(t, &fakeObserverRuntime{qs: store, timeout: time.Second, ok: true})
				rec := rootObserverRequest(t, engine, rootToken, route.path)
				assert.Equal(t, http.StatusOK, rec.Code)
				var body map[string]any
				require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
				assert.Equal(t, true, body["success"])
				data, ok := body["data"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, true, data["degraded"])
				assert.Equal(t, dim.reason, data["reason"])
				bodyText := rec.Body.String()
				for _, secret := range dim.leaks {
					assert.NotContains(t, bodyText, secret, "raw fault material must never reach the response")
				}
			})
		}
	}
}

// TestRelayObserverFaultMatrixLockedDegraded covers the locked dimension: a
// query blocked behind a database lock must still answer the degraded
// envelope with reason=timeout, never hang and never return internal text.
//
// When TEST_RELAY_OBSERVER_POSTGRES_DSN is set and points at the disposable
// restricted instance (loopback, port 55433, database relay_observer), the
// test runs the real path: a control connection holds an ACCESS EXCLUSIVE lock
// on observer_turns, the injected millisecond query timeout expires while the
// store query waits, and the HTTP layer reports degraded. Otherwise it
// degrades to the fake injection (the same envelope shape with a lock-flavored
// timeout), so the default suite stays deterministic and never dials a
// database — and never touches a container it does not own.
func TestRelayObserverFaultMatrixLockedDegraded(t *testing.T) {
	dsn := os.Getenv("TEST_RELAY_OBSERVER_POSTGRES_DSN")
	if dsn == "" || validateDisposableDSN(dsn) != nil {
		t.Log("locked dimension degraded to fake injection: no TEST_RELAY_OBSERVER_POSTGRES_DSN pointing at the disposable local PG")
		engine, rootToken, _, _ := relayObserverTestEnv(t)
		injectObserverRuntime(t, &fakeObserverRuntime{
			qs: &fakeObserverQueryStore{
				overviewErr: &relayobserver.QueryError{Kind: relayobserver.QueryErrTimeout, Msg: "query blocked by table lock"},
			},
			timeout: time.Second,
			ok:      true,
		})
		rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/overview")
		assert.Equal(t, http.StatusOK, rec.Code)
		var body map[string]any
		require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
		data := body["data"].(map[string]any)
		assert.Equal(t, true, data["degraded"])
		assert.Equal(t, "timeout", data["reason"])
		assert.NotContains(t, rec.Body.String(), "blocked by table lock", "the raw fault text must never reach the response")
		return
	}
	realLockedDegraded(t, dsn)
}

// validateDisposableDSN refuses any DSN that is not the disposable local
// PostgreSQL: only loopback, only port 55433, and only the relay_observer
// database. The real locked path never runs against anything else.
func validateDisposableDSN(dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("dsn must be parseable by pgx: %v", err)
	}
	if cfg.Host != "127.0.0.1" && cfg.Host != "localhost" {
		return fmt.Errorf("dsn must target loopback only, got host %q", cfg.Host)
	}
	if cfg.Port != 55433 {
		return fmt.Errorf("dsn must target port 55433, got %d", cfg.Port)
	}
	if cfg.Database != "relay_observer" {
		return fmt.Errorf("dsn must target the relay_observer database, got %q", cfg.Database)
	}
	return nil
}

// realLockedDegraded runs the locked dimension against the real restricted
// PostgreSQL: an ACCESS EXCLUSIVE lock on observer_turns held by a control
// connection blocks the store query until the injected millisecond timeout.
// The lock is released in t.Cleanup (rollback closes the transaction), so the
// container is left exactly as the test found it.
func realLockedDegraded(t *testing.T, dsn string) {
	ctx := context.Background()
	control, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	require.NoError(t, control.Ping())
	t.Cleanup(func() { _ = control.Close() })

	// Ensure the observer_turns table exists (bootstrap only when missing; the
	// disposable instance owns the relay_observer database). Connection-level
	// failures here mean the container is not usable -> fail the test loudly,
	// because the operator explicitly pointed the DSN at the disposable PG.
	var exists bool
	require.NoError(t, control.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'observer_turns')`).Scan(&exists))
	if !exists {
		store, err := relayobserver.OpenPGStore(ctx, dsn, relayobserver.SchemaModeBootstrap)
		require.NoError(t, err)
		require.NoError(t, store.Close(ctx))
	}

	// Hold the table lock: an ACCESS EXCLUSIVE lock blocks the observer's
	// SELECT until the injected query timeout expires. The transaction stays
	// open for the whole test and is rolled back by Close.
	_, err = control.ExecContext(ctx, "BEGIN")
	require.NoError(t, err)
	_, err = control.ExecContext(ctx, "LOCK TABLE observer_turns IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = control.ExecContext(context.Background(), "ROLLBACK") })

	store, err := relayobserver.OpenPGStore(ctx, dsn, relayobserver.SchemaModeVerify)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	qs, err := relayobserver.NewQueryStore(store)
	require.NoError(t, err)

	engine, rootToken, _, _ := relayObserverTestEnv(t)
	// The injected timeout is the lock-release driver: milliseconds, no sleeps.
	injectObserverRuntime(t, &fakeObserverRuntime{qs: qs, timeout: 400 * time.Millisecond, ok: true})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/overview")
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	data := body["data"].(map[string]any)
	assert.Equal(t, true, data["degraded"])
	assert.Equal(t, "timeout", data["reason"])
}

// TestRelayObserverFaultMatrixProbesUnaffected proves the audit line cannot
// affect the normal service at the HTTP layer: the public /api/status and
// /api/notice probes return byte-identical responses while the observer is
// faulted (store connection failure) as they do on the healthy baseline, and
// never carry the degraded marker.
func TestRelayObserverFaultMatrixProbesUnaffected(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	probePaths := []string{"/api/status", "/api/notice"}

	// Baseline: no observer runtime injected at all.
	baseline := map[string]string{}
	for _, path := range probePaths {
		rec := rootObserverRequest(t, engine, "", path)
		assert.Equal(t, http.StatusOK, rec.Code, path)
		baseline[path] = rec.Body.String()
	}

	// Faulted observer: every query route degrades, the probes must not move.
	raw := "dial tcp 127.0.0.1:55433: connect: connection refused while opening postgres://obs:topsecretpw@127.0.0.1:55433/relay_observer (hmac=hushhush)"
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs:      &fakeObserverQueryStore{sessionsErr: errors.New(raw)},
		timeout: time.Second,
		ok:      true,
	})
	// Prove the fault is actually live on the observer surface before probing.
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/sessions")
	assert.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"degraded":true`)

	for _, path := range probePaths {
		rec := rootObserverRequest(t, engine, "", path)
		assert.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, baseline[path], rec.Body.String(), "%s must be byte-identical while the observer is faulted", path)
		assert.NotContains(t, rec.Body.String(), "degraded", "%s must never carry the observer degraded marker", path)
	}
}

// TestRelayObserverOverviewWindowClamp is the P2-1 regression: the overview
// window clamps (window_seconds>3600, windows>48) must reach the store, not
// just the response echo. The recording store captures the OverviewQuery the
// controller issued after clamping.
func TestRelayObserverOverviewWindowClamp(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	store := &recordingQueryStore{fakeObserverQueryStore: &fakeObserverQueryStore{}}
	injectObserverRuntime(t, &fakeObserverRuntime{qs: store, timeout: time.Second, ok: true})

	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/overview?window_seconds=7200&windows=96")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 3600, store.overviewQuery.WindowSeconds, "window_seconds must clamp at 3600 before reaching the store")
	assert.Equal(t, 48, store.overviewQuery.Windows, "windows must clamp at 48 before reaching the store")

	// In-range values pass through untouched.
	rec = rootObserverRequest(t, engine, rootToken, "/api/relay-observer/overview?window_seconds=3600&windows=48")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 3600, store.overviewQuery.WindowSeconds, "window_seconds at the cap must pass through")
	assert.Equal(t, 48, store.overviewQuery.Windows, "windows at the cap must pass through")
}
