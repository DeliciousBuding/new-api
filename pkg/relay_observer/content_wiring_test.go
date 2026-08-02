package relayobserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the T2.6 worker-side content capture pipeline: the
// dispatcher normalizes every admitted event that carries a parsed request,
// backfills its ContentState so the metadata write carries the true outcome,
// and appends the captured content after the metadata write succeeds. The
// scripted store records both write faces, so the two-phase order, the
// fail-open degradation, and the append retry are all deterministic.

// contentEventPtr returns a pointer to a fresh event carrying a parsed OpenAI
// chat request and Codex session identity material — the worker-side capture
// input of one turn.
func contentEventPtr() *Event {
	ev := sampleEvent()
	ev.RelayFormat = string(types.RelayFormatOpenAI)
	req := dto.Request(&dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{Role: "user", Content: "hello world"},
		},
	})
	ev.Request = &req
	ev.Identity = IdentityInput{
		Headers: http.Header{"X-Codex-Turn-Metadata": {`{"thread_id":"thr-1","session_id":"ses-1"}`}},
		Body:    []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello world"}]}`),
	}
	return &ev
}

// contentStore returns a scripted store whose write notifications are wired
// for worker-driven tests.
func contentStore() *scriptedStore {
	return &scriptedStore{writeNotify: make(chan struct{}, 16)}
}

// contentCfg wires the observer HMAC key into a dispatcher configuration so
// alias resolution and item digests work in tests.
func contentCfg(mutate func(*Config)) func(*Config) {
	return func(c *Config) {
		c.HMACKey = testHMACKey
		c.HMACKeyVersion = 1
		if mutate != nil {
			mutate(c)
		}
	}
}

// TestWorkerCaptureBackfillsContentStateAndAppends is the happy-path contract:
// one event with a parsed request is normalized in the worker, its
// ContentState is backfilled to full before the metadata write, and the
// content append carries the resolved session aliases, the deterministic turn
// row id, and the canonical items.
func TestWorkerCaptureBackfillsContentStateAndAppends(t *testing.T) {
	store := contentStore()
	d, _ := newTestDispatcher(t, store, contentCfg(func(c *Config) { c.BatchSize = 1 }))

	require.True(t, d.TryEnqueue(contentEventPtr(), 1<<20))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
	require.Len(t, store.batches[0], 1)
	assert.Equal(t, ContentStateFull, store.batches[0][0].ContentState)

	require.Len(t, store.appends, 1)
	require.Len(t, store.appends[0], 1)
	in := store.appends[0][0]
	assert.Equal(t, "node-a", in.NodeScope)
	assert.Equal(t, int64(7), in.UserID)
	assert.Equal(t, turnRowID("node-a", "req_123"), in.TurnID)
	require.Len(t, in.Aliases, 2) // primary thread alias plus the auxiliary session alias
	assert.Equal(t, ScopeCodexCLI, in.Aliases[0].Scope)
	assert.Equal(t, SourceTurnThread, in.Aliases[0].Source)
	assert.Equal(t, SourceTurnSession, in.Aliases[1].Source)
	require.NotEmpty(t, in.Items)
	assert.Equal(t, CanonicalKindMessage, in.Items[0].Kind)
	assert.Equal(t, "user", in.Items[0].Role)
	assert.Equal(t, "hello world", in.Items[0].Content[0].Text)
	assert.Equal(t, int64(0), d.contentGaps.Load())
}

// TestWorkerTruncationBackfillsGapAndAppendsMarker is the gap contract: an
// admission reservation below the canonical byte total truncates the tail and
// the backfilled ContentState is gap; the append carries the truncated items
// closed by an explicit gap marker (data, not silent loss).
func TestWorkerTruncationBackfillsGapAndAppendsMarker(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{Role: "user", Content: "alpha alpha alpha"},
			{Role: "user", Content: "bravo bravo bravo"},
		},
	}
	full := NormalizeRequest(string(types.RelayFormatOpenAI), req, NormalizeOptions{
		Reservation: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey,
	})
	require.Equal(t, ContentStateFull, full.ContentState)
	var total int64
	for _, it := range full.Items {
		p, err := common.Marshal(it)
		require.NoError(t, err)
		total += int64(len(p))
	}

	store := contentStore()
	d, _ := newTestDispatcher(t, store, contentCfg(func(c *Config) { c.BatchSize = 1 }))

	ev := sampleEventPtr()
	ev.RelayFormat = string(types.RelayFormatOpenAI)
	reqIface := dto.Request(req)
	ev.Request = &reqIface
	ev.Identity = IdentityInput{
		Headers: http.Header{"X-Codex-Turn-Metadata": {`{"thread_id":"thr-2"}`}},
	}
	require.True(t, d.TryEnqueue(ev, total-1)) // one canonical byte short of the full total
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
	assert.Equal(t, ContentStateGap, store.batches[0][0].ContentState)
	require.Len(t, store.appends, 1)
	require.Len(t, store.appends[0], 1)
	items := store.appends[0][0].Items
	require.NotEmpty(t, items)
	last := items[len(items)-1]
	assert.Equal(t, CanonicalKindGap, last.Kind)
	assert.True(t, last.Truncated)
	assert.Greater(t, last.LogicalBytes, int64(0))
	assert.NotEmpty(t, last.Hmac)
	assert.Equal(t, int64(1), d.contentGaps.Load())
}

// TestWorkerNormalizePanicDegradesToMetadataOnly is the fail-open contract for
// a panicking normalizer: a typed-nil request DTO reaches the openai branch
// and dereferences nil inside normalization. The event degrades to
// metadata-only, is counted as a content gap, produces no append, and the
// worker keeps running for the next event.
func TestWorkerNormalizePanicDegradesToMetadataOnly(t *testing.T) {
	store := contentStore()
	d, _ := newTestDispatcher(t, store, contentCfg(func(c *Config) { c.BatchSize = 1 }))

	ev := sampleEventPtr()
	ev.RelayFormat = string(types.RelayFormatOpenAI)
	var nilReq dto.Request = (*dto.GeneralOpenAIRequest)(nil) // non-nil interface, nil pointer
	ev.Request = &nilReq
	require.True(t, d.TryEnqueue(ev, 1<<20))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)

	store.mu.Lock()
	require.Len(t, store.batches, 1)
	require.Len(t, store.batches[0], 1)
	assert.Equal(t, ContentStateMetadataOnly, store.batches[0][0].ContentState)
	assert.Empty(t, store.appends)
	store.mu.Unlock()
	assert.Equal(t, int64(1), d.contentGaps.Load())

	// The worker survived the degraded event: the next admission is written
	// and captured normally.
	require.True(t, d.TryEnqueue(contentEventPtr(), 1<<20))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 2 }, 2*time.Second, time.Millisecond)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 2)
	assert.Equal(t, ContentStateFull, store.batches[1][0].ContentState)
}

// TestWorkerNilRequestStaysMetadataOnly is the nil-Request contract: an event
// without a parsed request is never normalized, keeps the request-path
// metadata-only default, and is not counted as a worker-side content gap.
func TestWorkerNilRequestStaysMetadataOnly(t *testing.T) {
	store := contentStore()
	d, _ := newTestDispatcher(t, store, contentCfg(func(c *Config) { c.BatchSize = 1 }))

	ev := sampleEventPtr()
	ev.ContentState = ContentStateMetadataOnly // the request-path default
	require.True(t, d.TryEnqueue(ev, 100))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
	assert.Equal(t, ContentStateMetadataOnly, store.batches[0][0].ContentState)
	assert.Empty(t, store.appends)
	assert.Equal(t, int64(0), d.contentGaps.Load())
}

// TestWorkerMixedBatchStates locks the per-event independence inside one
// batch: a captured event, a nil-Request event, and an unknown-format event
// flush together, each with its own state, and only the captured event
// produces an append. The unknown format is counted as a worker-side content
// gap; the nil-Request event is not.
func TestWorkerMixedBatchStates(t *testing.T) {
	store := contentStore()
	d, _ := newTestDispatcher(t, store, contentCfg(func(c *Config) { c.BatchSize = 3 }))

	unknown := sampleEventPtr()
	unknown.RelayFormat = "gemini" // outside the normalizer whitelist
	unknown.ContentState = ContentStateMetadataOnly
	unknownReq := dto.Request(&dto.GeneralOpenAIRequest{Model: "gemini-2", Messages: []dto.Message{{Role: "user", Content: "hi"}}})
	unknown.Request = &unknownReq

	nilReq := sampleEventPtr()
	nilReq.ContentState = ContentStateMetadataOnly // the request-path default

	require.True(t, d.TryEnqueue(contentEventPtr(), 1<<20))
	require.True(t, d.TryEnqueue(nilReq, 100))
	require.True(t, d.TryEnqueue(unknown, 100))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 3 }, 2*time.Second, time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
	require.Len(t, store.batches[0], 3)
	assert.Equal(t, ContentStateFull, store.batches[0][0].ContentState)
	assert.Equal(t, ContentStateMetadataOnly, store.batches[0][1].ContentState)
	assert.Equal(t, ContentStateMetadataOnly, store.batches[0][2].ContentState)
	require.Len(t, store.appends, 1)
	require.Len(t, store.appends[0], 1)
	assert.Equal(t, int64(1), d.contentGaps.Load())
}

// TestWorkerAppendFailureOpensCircuitAndRetries is the two-phase failure
// contract: an AppendTurns failure after a successful metadata write opens the
// circuit without dropping or re-counting the already-written event, and the
// next pass retries the retained appends (metadata rows are idempotent, so
// the retry never duplicates them).
func TestWorkerAppendFailureOpensCircuitAndRetries(t *testing.T) {
	store := contentStore()
	store.setAppendErr(errBoom)
	d, clk := newTestDispatcher(t, store, contentCfg(func(c *Config) { c.BatchSize = 1 }))

	require.True(t, d.TryEnqueue(contentEventPtr(), 1<<20))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(0), d.droppedTotal.Load())
	require.Len(t, d.pendingAppends, 1)
	assert.Equal(t, turnRowID("node-a", "req_123"), d.pendingAppends[0].TurnID)

	// Recover: clear the scripted failure, let the cooldown expire, and let
	// the half-open probe write a fresh event; the probe flush merges the
	// retained appends and retries them.
	store.setAppendErr(nil)
	clk.advance(initialCooldown)
	lastCooldownTimer(t, clk, d.cfg.FlushInterval).fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)

	require.True(t, d.TryEnqueue(sampleEventPtr(), 100)) // probe, no request
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	// The first AppendTurns call failed and recorded nothing; the retried call
	// carries the retained append of the first event.
	require.Len(t, store.appends, 1)
	require.Len(t, store.appends[0], 1)
	assert.Equal(t, turnRowID("node-a", "req_123"), store.appends[0][0].TurnID)
	assert.Len(t, d.pendingAppends, 0)
}

// TestWorkerAppendFailureDropsOldestBeyondCap locks the pending-appends bound:
// a persistently failing content store keeps at most maxPendingAppendEvents
// appends; beyond the cap the oldest are dropped and counted as content gaps
// while the metadata rows stay written.
func TestWorkerAppendFailureDropsOldestBeyondCap(t *testing.T) {
	d := &Dispatcher{cfg: DefaultConfig()}
	appends := make([]ContentInput, maxPendingAppendEvents+3)
	for i := range appends {
		appends[i].TurnID = turnRowID("node-a", "req")
	}
	d.retainPendingAppends(appends)
	require.Len(t, d.pendingAppends, maxPendingAppendEvents)
	assert.Equal(t, int64(3), d.contentGaps.Load())
}

// TestWorkerMetadataFailureKeepsRetainedAppends locks the retained-appends
// boundary: a metadata write failure after a retained append must not clear
// it — the retained append's metadata rows were already written, so its
// content still needs the retry. The next recovery merges and retries it.
func TestWorkerMetadataFailureKeepsRetainedAppends(t *testing.T) {
	store := contentStore()
	store.setAppendErr(errBoom)
	d, clk := newTestDispatcher(t, store, contentCfg(func(c *Config) { c.BatchSize = 1 }))

	require.True(t, d.TryEnqueue(contentEventPtr(), 1<<20))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Len(t, d.pendingAppends, 1)

	// Recover to half-open, then fail the metadata write: the retained append
	// must survive the failed metadata batch.
	store.setAppendErr(nil)
	clk.advance(initialCooldown)
	lastCooldownTimer(t, clk, d.cfg.FlushInterval).fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)
	store.setErr(errBoom)
	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Len(t, d.pendingAppends, 1, "a failed metadata write must not drop retained appends")

	// Full recovery: the probe flush retries the retained append.
	store.setErr(nil)
	clk.advance(initialCooldown)
	lastCooldownTimer(t, clk, d.cfg.FlushInterval).fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)
	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.appends, 1)
	require.Len(t, store.appends[0], 1)
	assert.Equal(t, turnRowID("node-a", "req_123"), store.appends[0][0].TurnID)
	assert.Len(t, d.pendingAppends, 0)
}

// TestPlanContentSkipsWithoutHMACKey is the identity fail-open contract: with
// an unconfigured HMAC key, alias resolution fails and no append is planned —
// the event keeps its normalized ContentState and nothing touches the store.
func TestPlanContentSkipsWithoutHMACKey(t *testing.T) {
	cfg := DefaultConfig() // HMACKey empty: ResolveIdentity errors out
	d := &Dispatcher{cfg: cfg}

	ev := sampleEvent()
	ev.RelayFormat = string(types.RelayFormatOpenAI)
	req := dto.Request(&dto.GeneralOpenAIRequest{Model: "gpt-5", Messages: []dto.Message{{Role: "user", Content: "hi"}}})
	ev.Request = &req
	ev.Identity = IdentityInput{
		Headers: http.Header{"X-Codex-Turn-Metadata": {`{"thread_id":"thr-3"}`}},
	}
	batch := []queuedEvent{{ev: &ev, reservation: 1 << 20}}

	appends := d.planContent(batch)
	assert.Equal(t, ContentStateFull, ev.ContentState)
	assert.Empty(t, appends)
	assert.Equal(t, int64(0), d.contentGaps.Load())
}
