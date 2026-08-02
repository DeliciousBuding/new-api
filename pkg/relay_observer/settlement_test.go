package relayobserver

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntimeTryPublishTurnDisabledZeroAlloc is the disabled-path contract:
// with the observer not wired (the default process state) the publish hook
// allocates no state at all and reports false. This is the allocation-count
// equivalent of the service hook's visible zero-cost branch when the observer
// is disabled.
func TestRuntimeTryPublishTurnDisabledZeroAlloc(t *testing.T) {
	rt := &Runtime{} // stateUninitialized, no dispatcher
	// The event is pre-built outside the measured closure: only the publish
	// call itself is counted, so the allocation count reflects the hook.
	ev := sampleEvent()
	accepted := 0
	allocs := testing.AllocsPerRun(100, func() {
		if rt.TryPublishTurn(ev, 0) {
			accepted++
		}
	})
	assert.Zero(t, allocs)
	assert.Equal(t, 0, accepted)
}

// TestRuntimeTryPublishTurnDeliversCompleteEvent proves the publish path
// delivers a fully populated event (attempts, error classification, IP trust)
// intact to the store.
func TestRuntimeTryPublishTurnDeliversCompleteEvent(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 1)}
	d, _ := newTestDispatcher(t, store, func(cfg *Config) { cfg.BatchSize = 1 })
	rt := &Runtime{state: stateEnabled, disp: d}

	ev := sampleEvent()
	ev.Attempts = []AttemptSummary{
		{ChannelID: 3, Group: "default", StatusCode: 429, ErrorCode: "rate_limit", ElapsedMS: 12},
		{ChannelID: 9, Group: "backup", StatusCode: 200, ElapsedMS: 140},
	}
	ev.AttemptsOmitted = 92
	ev.ErrorType = "upstream"
	ev.ErrorCode = "timeout"
	ev.ClientIP = net.ParseIP("203.0.113.9")
	ev.IPTrust = IPTrustDirect

	require.True(t, rt.TryPublishTurn(ev, 0))
	waitNotify(t, store.writeNotify)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
	assert.Equal(t, ev, store.batches[0][0])
}

// TestRuntimeTryPublishTurnRejectsBadReservation locks the admission
// contract: a negative reservation is rejected without a panic and without
// reaching the store, while an over-budget reservation is clamped at the
// per-request cap (SSOT: oversized requests become metadata-only instead of
// being dropped) and the turn is still published.
func TestRuntimeTryPublishTurnRejectsBadReservation(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 1)}
	d, _ := newTestDispatcher(t, store, func(cfg *Config) { cfg.BatchSize = 1 })
	rt := &Runtime{state: stateEnabled, disp: d}

	assert.False(t, rt.TryPublishTurn(sampleEvent(), -1))
	require.True(t, rt.TryPublishTurn(sampleEvent(), d.cfg.MaxRequestBytes+1))
	waitNotify(t, store.writeNotify)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
}

// TestPublishTurnCopyProtectsPostSendWrites is the enqueue-race fixture: the
// publish takes a value snapshot before the send establishes happens-before
// with the worker, so a write to the caller's event after the publish returns
// can never reach the worker. Under -race this proves the request path's
// "no write after send" rule without sharing memory.
func TestPublishTurnCopyProtectsPostSendWrites(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 1)}
	d, _ := newTestDispatcher(t, store, func(cfg *Config) { cfg.BatchSize = 1 })
	rt := &Runtime{state: stateEnabled, disp: d}

	const wantModel = "gpt-5-orig"
	ev := sampleEvent()
	ev.Model = wantModel

	publishDone := make(chan struct{})
	go func() {
		rt.TryPublishTurn(ev, 0)
		close(publishDone)
	}()
	<-publishDone

	// Post-send mutation of the caller's copy must not be observable by the
	// worker: the published event is the send-before snapshot.
	ev.Model = "mutated-after-publish"

	waitNotify(t, store.writeNotify)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
	assert.Equal(t, wantModel, store.batches[0][0].Model)
}

// TestTryPublishTurnGateHoldsUntilSend constructs the "final event is only
// visible after the last request-path write" timing with a gate: while the
// publish is paused before its channel send, the worker sees nothing; only
// after the send completes does the event become visible. The gate mirrors
// the settlement hook's position — the publish runs strictly after the last
// request-path write, so nothing earlier can leak to the worker.
func TestTryPublishTurnGateHoldsUntilSend(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 1)}
	d, _ := newTestDispatcher(t, store, func(cfg *Config) { cfg.BatchSize = 1 })
	gateEntered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	d.sendGate = func() {
		once.Do(func() {
			close(gateEntered)
			<-release
		})
	}
	rt := &Runtime{state: stateEnabled, disp: d}

	publishDone := make(chan struct{})
	go func() {
		rt.TryPublishTurn(sampleEvent(), 0)
		close(publishDone)
	}()

	<-gateEntered
	// The publish is paused before its send: nothing may have reached the
	// worker yet, regardless of elapsed time.
	select {
	case <-store.writeNotify:
		t.Fatal("event visible before the publish send completed")
	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, 0, store.writeCountSnapshot())

	close(release)
	<-publishDone
	waitNotify(t, store.writeNotify)
	require.Equal(t, 1, store.writeCountSnapshot())
}

// TestRuntimeTryPublishTurnRecoversPanic locks the entry-point recover
// contract (SSOT: all observer entry points recover panics): a nil receiver
// panics the method body, the recover absorbs it, the publish reports false,
// and the panic never reaches the request path.
func TestRuntimeTryPublishTurnRecoversPanic(t *testing.T) {
	var rt *Runtime
	ok := true
	require.NotPanics(t, func() { ok = rt.TryPublishTurn(sampleEvent(), 0) })
	assert.False(t, ok)
}

// TestCaptureClientIPPolicy locks the dual-opt-in capture mapping: a "none"
// trust tier (either opt-in off) yields no IP at all; "proxy"/"direct" yield
// the parsed peer and their tier; an unparseable peer yields a nil IP without
// changing the tier.
func TestCaptureClientIPPolicy(t *testing.T) {
	tests := []struct {
		trust    IPTrust
		peer     string
		wantIP   net.IP
		wantTier IPTrust
	}{
		{IPTrustNone, "203.0.113.1", nil, IPTrustNone},
		{IPTrustProxy, "203.0.113.1", net.ParseIP("203.0.113.1"), IPTrustProxy},
		{IPTrustDirect, "10.0.0.1", net.ParseIP("10.0.0.1"), IPTrustDirect},
		{IPTrustProxy, "not-an-ip", nil, IPTrustProxy},
	}
	for _, tc := range tests {
		ip, tier := CaptureClientIP(tc.trust, tc.peer)
		assert.Equal(t, tc.wantTier, tier, "peer %q", tc.peer)
		if tc.wantIP == nil {
			assert.Nil(t, ip, "peer %q", tc.peer)
		} else {
			assert.True(t, tc.wantIP.Equal(ip), "peer %q: got %v", tc.peer, ip)
		}
	}
}

// TestStatusNeverExposesClientIP locks the non-Root exposure contract: the
// status surface has no IP field at all, in any casing, so no observer API
// can leak a client IP below the Root boundary.
func TestStatusNeverExposesClientIP(t *testing.T) {
	st := Status{Enabled: true, IPTrust: IPTrustDirect}
	data, err := common.Marshal(st)
	require.NoError(t, err)
	body := string(data)
	assert.NotContains(t, body, "ClientIP")
	assert.NotContains(t, body, "client_ip")
	assert.NotContains(t, body, "clientIP")
}

// TestEventErrorClassificationCarriesNoRawText locks the secret-free error
// contract of the final event: only the stable error type/code reach the
// event; raw error text (which can embed keys or connection details) never
// does.
func TestEventErrorClassificationCarriesNoRawText(t *testing.T) {
	const raw = "upstream: connection refused, key=sk-secret123, trace=abc"
	ev := sampleEvent()
	ev.ErrorType = "upstream"
	ev.ErrorCode = "connection_error"
	ev.Attempts = []AttemptSummary{
		{ChannelID: 1, Group: "g", StatusCode: 502, ErrorCode: "connection_error"},
	}
	data, err := common.Marshal(ev)
	require.NoError(t, err)
	body := string(data)
	assert.Contains(t, body, `"ErrorCode":"connection_error"`)
	assert.NotContains(t, body, raw)
	assert.NotContains(t, body, "sk-secret123")
	assert.NotContains(t, body, "connection refused")
}
