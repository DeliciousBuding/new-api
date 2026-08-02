package relayobserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the dispatcher deterministically: a scripted store plus a
// fake clock drive every fault and concurrency scenario without sleeps or
// timing comparisons. The fake store differs from the port-level fakeStore in
// contracts_test.go: it can fail, panic, and block writes on demand.

var errBoom = errors.New("boom")

// sampleEventPtr returns a pointer to a fresh sample event (Go does not allow
// taking the address of a function call).
func sampleEventPtr() *Event {
	e := sampleEvent()
	return &e
}

// scriptedStore implements Store with scripted failure, panic, and blocking
// behavior. Every WriteBatch entry sends a non-blocking writeNotify so tests
// can synchronize on writes deterministically; AppendTurns has its own
// scripted error and appends recording so content failures are scriptable
// independently of metadata writes.
type scriptedStore struct {
	mu           sync.Mutex
	batches      [][]Event
	appends      [][]ContentInput
	writeCount   int
	err          error
	appendErr    error
	panicOnWrite bool
	blockWrites  chan struct{}
	writeNotify  chan struct{}
	closed       bool
}

var _ Store = (*scriptedStore)(nil)

func (s *scriptedStore) WriteBatch(ctx context.Context, events []Event) error {
	s.mu.Lock()
	s.writeCount++
	notify := s.writeNotify
	block := s.blockWrites
	pan := s.panicOnWrite
	err := s.err
	s.mu.Unlock()

	if notify != nil {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	if pan {
		panic("scripted store panic")
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
		// Re-read the error: a store that was healthy when the write started
		// may be scripted to fail while blocked.
		s.mu.Lock()
		err = s.err
		s.mu.Unlock()
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Copy the batch: the caller's slice shares its backing array with the
	// worker's in-progress batch, which the caller reuses ([:0] + append)
	// after this write returns.
	s.batches = append(s.batches, append([]Event(nil), events...))
	return nil
}

func (s *scriptedStore) AppendTurns(ctx context.Context, turns []ContentInput) error {
	s.mu.Lock()
	appendErr := s.appendErr
	s.mu.Unlock()
	if appendErr != nil {
		return appendErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends = append(s.appends, append([]ContentInput(nil), turns...))
	return nil
}

func (s *scriptedStore) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *scriptedStore) setErr(err error)       { s.mu.Lock(); s.err = err; s.mu.Unlock() }
func (s *scriptedStore) setAppendErr(err error) { s.mu.Lock(); s.appendErr = err; s.mu.Unlock() }
func (s *scriptedStore) setPanicOnWrite(v bool) { s.mu.Lock(); s.panicOnWrite = v; s.mu.Unlock() }
func (s *scriptedStore) setBlockWrites(ch chan struct{}) {
	s.mu.Lock()
	s.blockWrites = ch
	s.mu.Unlock()
}

func (s *scriptedStore) writeCountSnapshot() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeCount
}

// fakeTimer is the fake side of the clock seam: it never fires on its own, the
// test fires it explicitly, and duration records the most recent Reset/NewTimer
// value so tests can assert the exponential cooldown sequence.
type fakeTimer struct {
	mu      sync.Mutex
	ch      chan time.Time
	dur     time.Duration
	stopped bool
}

func newFakeTimer(d time.Duration) *fakeTimer {
	return &fakeTimer{ch: make(chan time.Time, 1), dur: d}
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dur = d
	t.stopped = false
	return true
}

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	return true
}

func (t *fakeTimer) fire() {
	select {
	case t.ch <- time.Time{}:
	default:
	}
}

func (t *fakeTimer) duration() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dur
}

// fakeClock is the fake side of the clock seam. Time only moves when the test
// calls advance; timers fire only when the test calls fire.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: epoch}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := newFakeTimer(d)
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) snapshot() []*fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*fakeTimer(nil), c.timers...)
}

// lastCooldownTimer returns the most recently created timer whose duration is
// not the flush interval, i.e. the currently active cooldown timer. Needed
// after a worker panic: the restarted worker creates a fresh flush timer, so
// plain indexing no longer distinguishes the timers.
func lastCooldownTimer(t *testing.T, clk *fakeClock, flushInterval time.Duration) *fakeTimer {
	t.Helper()
	var found *fakeTimer
	require.Eventually(t, func() bool {
		found = nil
		for _, tm := range clk.snapshot() {
			if tm.duration() != flushInterval {
				found = tm
			}
		}
		return found != nil
	}, 2*time.Second, time.Millisecond)
	return found
}

// circuitStateVal is a test-only view of the circuit state machine.
func (d *Dispatcher) circuitStateVal() circuitStateVal {
	return circuitStateVal(d.circuitState.Load())
}

// newTestDispatcher builds a started dispatcher with a fake clock and a
// cleanup Stop under a generous timeout. The fake clock means the flush timer
// only fires when a test fires it.
func newTestDispatcher(t *testing.T, store Store, mutate func(*Config)) (*Dispatcher, *fakeClock) {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	d := NewDispatcher(cfg, store)
	clk := newFakeClock()
	d.clock = clk
	d.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		d.Stop(ctx)
	})
	return d, clk
}

func waitNotify(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for store write")
	}
}

// TestTryEnqueueRejectsInvalidReservation locks the admission rejects: a
// negative reservation, a reservation above MaxRequestBytes, and a nil event
// are all dropped without touching the queue or the byte budget.
func TestTryEnqueueRejectsInvalidReservation(t *testing.T) {
	d, _ := newTestDispatcher(t, &scriptedStore{}, nil)

	require.False(t, d.TryEnqueue(sampleEventPtr(), -1))
	require.False(t, d.TryEnqueue(sampleEventPtr(), d.cfg.MaxRequestBytes+1))
	require.False(t, d.TryEnqueue(nil, 0))

	assert.Equal(t, int64(3), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestMaxRequestBytesBoundaryAccepted verifies the boundary: a reservation
// exactly at MaxRequestBytes is admitted and written.
func TestMaxRequestBytesBoundaryAccepted(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 16)}
	d, _ := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), d.cfg.MaxRequestBytes))
	waitNotify(t, store.writeNotify)

	// The notify fires when the write starts; the counters land when it
	// returns, so wait for them.
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestTryEnqueueRejectsWhenStopped verifies the stopped dispatcher rejects
// every new event.
func TestTryEnqueueRejectsWhenStopped(t *testing.T) {
	d, _ := newTestDispatcher(t, &scriptedStore{}, nil)

	d.Stop(context.Background())
	require.False(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.False(t, d.TryEnqueue(sampleEventPtr(), 0))

	assert.Equal(t, int64(2), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
}

// TestQueueFullRejectsAndRollsBack drives the worker into a blocked write so
// the queue deterministically fills up: the next admission hits the count
// limit, and its byte reservation is rolled back.
func TestQueueFullRejectsAndRollsBack(t *testing.T) {
	store := &scriptedStore{blockWrites: make(chan struct{}), writeNotify: make(chan struct{}, 16)}
	d, _ := newTestDispatcher(t, store, func(c *Config) {
		c.BatchSize = 1
		c.QueueSize = 2
	})

	// First event is received by the worker and blocks in the store write.
	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	waitNotify(t, store.writeNotify)

	// Fill the queue; the channel is empty because the worker is blocked in
	// the write, so two admissions fit and two are pending.
	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	require.False(t, d.TryEnqueue(sampleEventPtr(), 100)) // queue full

	assert.Equal(t, int64(1), d.droppedTotal.Load())
	assert.Equal(t, int64(2), d.pendingCount.Load())
	assert.Equal(t, int64(200), d.pendingBytes.Load())

	close(store.blockWrites)
	// All three admitted events are written; the rejected one was dropped.
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 3 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(1), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestByteBudgetFullRejects drives the worker into a blocked write so a queued
// reservation occupies the byte budget: the next admission would overflow the
// total budget and is rejected with its reservation rolled back.
func TestByteBudgetFullRejects(t *testing.T) {
	store := &scriptedStore{blockWrites: make(chan struct{}), writeNotify: make(chan struct{}, 16)}
	d, _ := newTestDispatcher(t, store, func(c *Config) {
		c.BatchSize = 1
		c.QueueSize = 10
		c.QueueBytes = 150
	})

	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	waitNotify(t, store.writeNotify) // worker blocked in the write

	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))  // queued, 100 of 150 used
	require.False(t, d.TryEnqueue(sampleEventPtr(), 100)) // 200 > 150: byte full

	assert.Equal(t, int64(1), d.droppedTotal.Load())
	assert.Equal(t, int64(100), d.pendingBytes.Load())

	close(store.blockWrites)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 2 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestBatchSuccess verifies a full batch is written once with the configured
// batch size, and its reservations are released.
func TestBatchSuccess(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 16)}
	d, _ := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 2 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	require.True(t, d.TryEnqueue(sampleEventPtr(), 200))
	waitNotify(t, store.writeNotify)

	// WrittenTotal counts events, not batches: both events of the one batch.
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 2 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(0), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
	require.Len(t, store.batches[0], 2)
	assert.Equal(t, "req_123", store.batches[0][0].EventID)
}

// TestFlushIntervalFlushesPartialBatch verifies the flush timer flushes a
// partial batch: the worker receives the event first (pendingCount returns to
// zero), then the timer fires and the write happens.
func TestFlushIntervalFlushesPartialBatch(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) {
		c.BatchSize = 100 // never fills; only the timer flushes
	})

	require.True(t, d.TryEnqueue(sampleEventPtr(), 10))
	require.Eventually(t, func() bool { return d.pendingCount.Load() == 0 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) > 0 }, 2*time.Second, time.Millisecond)

	clk.snapshot()[0].fire() // flush timer
	waitNotify(t, store.writeNotify)

	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)
	store.mu.Lock()
	require.Len(t, store.batches, 1)
	assert.Len(t, store.batches[0], 1)
	store.mu.Unlock()
}

// TestBatchErrorOpensCircuitAndDropsBatch verifies a failed batch is dropped
// in full, never retried, and opens the circuit with the initial cooldown.
func TestBatchErrorOpensCircuitAndDropsBatch(t *testing.T) {
	store := &scriptedStore{err: errBoom, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 2 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)

	assert.Equal(t, int64(2), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.writtenTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, 5*time.Second, clk.snapshot()[1].duration())

	// The circuit stays open: new events are dropped without any write.
	require.False(t, d.TryEnqueue(sampleEventPtr(), 100))
	assert.Equal(t, int64(3), d.droppedTotal.Load())
	assert.Equal(t, 1, store.writeCountSnapshot())
}

// TestCircuitCooldownExponentialCap verifies the cooldown doubles on every
// failure and caps at five minutes: 5s, 10s, 20s, 40s, 80s, 160s, 5m, 5m.
func TestCircuitCooldownExponentialCap(t *testing.T) {
	store := &scriptedStore{err: errBoom, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, 5*time.Second, clk.snapshot()[1].duration())

	wants := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	for i, want := range wants {
		idx := i + 1
		require.Eventually(t, func() bool { return len(clk.snapshot()) > idx }, 2*time.Second, time.Millisecond)
		ct := clk.snapshot()[idx]
		assert.Equal(t, want, ct.duration(), "cooldown %d", i)

		ct.fire()
		require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)
		// The probe event is admitted through the single half-open slot.
		require.Eventually(t, func() bool { return d.TryEnqueue(sampleEventPtr(), 0) }, 2*time.Second, time.Millisecond)
		require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	}

	// 1 initial failure + 8 probe failures; every drop was counted.
	assert.Equal(t, int64(9), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestHalfOpenSingleProbe100Concurrent verifies that with the circuit
// half-open, 100 concurrent admissions admit exactly one probe event and the
// worker performs exactly one probe write; the other 99 are dropped while the
// probe is in flight.
func TestHalfOpenSingleProbe100Concurrent(t *testing.T) {
	store := &scriptedStore{err: errBoom, writeNotify: make(chan struct{}, 64)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)

	// The probe write blocks, keeping the circuit probing while the 100
	// concurrent admissions race for the single slot.
	store.setErr(nil)
	block := make(chan struct{})
	store.setBlockWrites(block)
	clk.snapshot()[1].fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)

	const n = 100
	start := make(chan struct{})
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if d.TryEnqueue(sampleEventPtr(), 0) {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int64(1), accepted.Load())
	assert.Equal(t, int64(100), d.droppedTotal.Load()) // 1 initial failure + 99 concurrent drops
	assert.Equal(t, int64(0), d.pendingCount.Load())   // probe event already received by the worker
	assert.Equal(t, 2, store.writeCountSnapshot())     // 1 failed + 1 probe write, still blocked

	close(block)
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)

	assert.Equal(t, 2, store.writeCountSnapshot()) // 1 failed + 1 probe write
	assert.Equal(t, int64(1), d.writtenTotal.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestProbeSuccessClosesCircuitAndResetsCooldown verifies a successful probe
// closes the circuit and resets the cooldown to the initial five seconds, so
// the next failure starts over instead of continuing the old backoff.
func TestProbeSuccessClosesCircuitAndResetsCooldown(t *testing.T) {
	store := &scriptedStore{err: errBoom, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, 5*time.Second, clk.snapshot()[1].duration())

	store.setErr(nil)
	clk.snapshot()[1].fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.TryEnqueue(sampleEventPtr(), 0) }, 2*time.Second, time.Millisecond)
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)

	assert.Equal(t, int64(1), d.writtenTotal.Load())
	require.False(t, d.circuitState.Load() == int32(circuitOpen))

	// The next failure uses the reset cooldown of 5s, not 10s.
	store.setErr(errBoom)
	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 3 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, 5*time.Second, clk.snapshot()[2].duration())
}

// TestOpenCircuitDropsQueuedEvents verifies that events queued before the
// circuit opened are drained and counted while the circuit is open.
func TestOpenCircuitDropsQueuedEvents(t *testing.T) {
	store := &scriptedStore{blockWrites: make(chan struct{}), writeNotify: make(chan struct{}, 64)}
	d, _ := newTestDispatcher(t, store, func(c *Config) {
		c.BatchSize = 1
		c.QueueSize = 4
	})

	require.True(t, d.TryEnqueue(sampleEventPtr(), 10))
	waitNotify(t, store.writeNotify) // worker blocked in the first write

	// Three events queue up while the write is blocked.
	require.True(t, d.TryEnqueue(sampleEventPtr(), 10))
	require.True(t, d.TryEnqueue(sampleEventPtr(), 10))
	require.True(t, d.TryEnqueue(sampleEventPtr(), 10))

	store.setErr(errBoom)
	close(store.blockWrites)

	// The first write fails (drops its batch and opens the circuit), then the
	// three queued events are drained and dropped on the open circuit.
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.droppedTotal.Load() == 4 }, 2*time.Second, time.Millisecond)

	assert.Equal(t, int64(0), d.writtenTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestStorePanicRecovers verifies a store panic is treated as a failed write:
// the current batch is dropped, the circuit opens, and the worker keeps
// running and can recover through a successful probe.
func TestStorePanicRecovers(t *testing.T) {
	store := &scriptedStore{panicOnWrite: true, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)

	assert.Equal(t, int64(1), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())

	// The worker is still alive: a successful probe closes the circuit. The
	// panic restarted the worker, so the active cooldown timer is found by
	// duration rather than index.
	store.setPanicOnWrite(false)
	lastCooldownTimer(t, clk, d.cfg.FlushInterval).fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.TryEnqueue(sampleEventPtr(), 0) }, 2*time.Second, time.Millisecond)
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)

	assert.Equal(t, int64(1), d.writtenTotal.Load())
}

// TestStopIdempotent verifies Stop can be called repeatedly and the store is
// closed exactly once.
func TestStopIdempotent(t *testing.T) {
	store := &scriptedStore{}
	d, _ := newTestDispatcher(t, store, nil)

	d.Stop(context.Background())
	d.Stop(context.Background())

	store.mu.Lock()
	assert.True(t, store.closed)
	store.mu.Unlock()
}

// TestStopContextTimeout verifies Stop returns as soon as its context expires
// even while the worker is blocked in a store write, and that the remaining
// events are dropped and counted once the worker stops.
func TestStopContextTimeout(t *testing.T) {
	store := &scriptedStore{blockWrites: make(chan struct{}), writeNotify: make(chan struct{}, 16)}
	d, _ := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	waitNotify(t, store.writeNotify) // worker blocked in the store write

	// An already-expired context must not wait for the blocked write.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	d.Stop(ctx)
	assert.Less(t, time.Since(start), time.Second)

	// Let the blocked write finish, then a second Stop with a live context
	// waits for the worker to drain and close the store.
	close(store.blockWrites)
	d.Stop(context.Background())
	require.Eventually(t, func() bool { return d.pendingCount.Load() == 0 }, 2*time.Second, time.Millisecond)
	store.mu.Lock()
	assert.True(t, store.closed)
	store.mu.Unlock()
}

// TestEnqueueRaceWithStop hammers TryEnqueue concurrently with Stop and
// verifies the accounting invariant: every accepted event releases its
// reservation exactly once, either as a written event or as a drop.
func TestEnqueueRaceWithStop(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 256)}
	d, _ := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 32 })

	var stop atomic.Bool
	var accepted, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if d.TryEnqueue(sampleEventPtr(), 7) {
					accepted.Add(1)
				} else {
					rejected.Add(1)
				}
			}
		}()
	}

	d.Stop(context.Background()) // concurrent with the admissions
	stop.Store(true)
	wg.Wait()

	st := d.Status()
	assert.Equal(t, 0, st.QueueCount)
	assert.Equal(t, int64(0), st.QueueBytes)
	// accepted == written + drained, and drained == dropped - rejected.
	assert.Equal(t, accepted.Load(), st.WrittenTotal+st.DroppedTotal-rejected.Load())
}

// TestStatusSnapshot verifies Status is an in-memory snapshot with a stable
// reason code and cooldown derived from the fake clock.
func TestStatusSnapshot(t *testing.T) {
	store := &scriptedStore{err: errBoom, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })
	d.SetIPTrust(IPTrustProxy)

	st := d.Status()
	assert.True(t, st.Enabled)
	assert.Equal(t, IPTrustProxy, st.IPTrust)
	assert.False(t, st.CircuitOpen)
	assert.Empty(t, st.ReasonCode)
	assert.Equal(t, IPTrustProxy, d.ipTrustValue())

	require.True(t, d.TryEnqueue(sampleEventPtr(), 42))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)

	st = d.Status()
	assert.True(t, st.CircuitOpen)
	assert.Equal(t, ReasonCircuitOpen, st.ReasonCode)
	assert.Equal(t, 5*time.Second, st.CircuitCooldown)
	assert.Equal(t, int64(1), st.DroppedTotal)

	clk.advance(2 * time.Second)
	assert.Equal(t, 3*time.Second, d.Status().CircuitCooldown)
	clk.advance(10 * time.Second)
	assert.Equal(t, time.Duration(0), d.Status().CircuitCooldown)
}

// TestStatusConcurrentRead hammers Status concurrently with admissions and
// Stop; the race detector proves there is no data race on the snapshot.
func TestStatusConcurrentRead(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 256)}
	d, _ := newTestDispatcher(t, store, nil)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				d.Status()
			}
		}()
	}
	for j := 0; j < 100; j++ {
		require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	}
	d.Stop(context.Background())
	wg.Wait()
}

// TestEnqueueStopRaceNoStrandedEvent deterministically reproduces the
// enqueue-vs-Stop race: a request path that passed the stopped check before
// Stop must not land its event after the worker drained the queue. The gate
// holds the request path between the stopped check and the admission ticket
// while Stop runs to completion; a stranded event would leave the queue
// counters non-zero and the accounting broken.
func TestEnqueueStopRaceNoStrandedEvent(t *testing.T) {
	store := &scriptedStore{}
	d, _ := newTestDispatcher(t, store, nil)

	gate := make(chan struct{})
	released := make(chan struct{})
	d.enqueueGate = func() { close(gate); <-released }

	var ok atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ok.Store(d.TryEnqueue(sampleEventPtr(), 100))
	}()

	<-gate // the request path is paused between the stopped check and the ticket
	d.Stop(context.Background())
	close(released)
	wg.Wait()

	st := d.Status()
	// The paused admission must observe the stopped dispatcher and drop; it
	// must not land in the queue after the drain, and the counters must agree.
	assert.False(t, ok.Load())
	assert.Equal(t, 0, st.QueueCount)
	assert.Equal(t, int64(0), st.QueueBytes)
	assert.Equal(t, int64(1), d.droppedTotal.Load())
}

// TestFlushTimerAndBatchFullSingleWrite verifies that a ready flush timer and
// a ready enqueue never produce a duplicate write: the single worker flushes
// each batch at most once, whichever select case wins, and no event is ever
// written twice.
func TestFlushTimerAndBatchFullSingleWrite(t *testing.T) {
	store := &scriptedStore{blockWrites: make(chan struct{}), writeNotify: make(chan struct{}, 64)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 2 })

	ev := func(id string) *Event {
		e := sampleEvent()
		e.EventID = id
		return &e
	}

	require.True(t, d.TryEnqueue(ev("e1"), 10))
	require.Eventually(t, func() bool { return d.pendingCount.Load() == 0 }, 2*time.Second, time.Millisecond)
	clk.snapshot()[0].fire() // flush timer: write #1 blocks in the store
	waitNotify(t, store.writeNotify)

	// While the write is blocked, queue two more events and arm the timer
	// again; both cases are ready when the write unblocks. A second worker
	// would start a write while #1 is still blocked.
	require.True(t, d.TryEnqueue(ev("e2"), 10))
	require.True(t, d.TryEnqueue(ev("e3"), 10))
	clk.snapshot()[0].fire()
	assert.Equal(t, 1, store.writeCountSnapshot())

	close(store.blockWrites)
	require.Eventually(t, func() bool { return store.writeCountSnapshot() == 2 }, 2*time.Second, time.Millisecond)

	// The race point produced exactly one additional write; no event was
	// written twice.
	store.mu.Lock()
	seen := map[string]int{}
	total := 0
	for _, b := range store.batches {
		for _, e := range b {
			seen[e.EventID]++
			total++
		}
	}
	store.mu.Unlock()
	require.LessOrEqual(t, total, 3)
	for id, n := range seen {
		assert.Equal(t, 1, n, "event %s written %d times", id, n)
	}

	// One more timer fire flushes whatever remains; all three events are
	// written exactly once in the end.
	clk.snapshot()[0].fire()
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 3 }, 2*time.Second, time.Millisecond)
	store.mu.Lock()
	seen = map[string]int{}
	total = 0
	for _, b := range store.batches {
		for _, e := range b {
			seen[e.EventID]++
			total++
		}
	}
	store.mu.Unlock()
	assert.Equal(t, 3, total)
	for id, n := range seen {
		assert.Equal(t, 1, n, "event %s written %d times", id, n)
	}
}

// TestStorePanicDropsBatchAndBlocksStore verifies a store panic drops the
// whole batch, opens the circuit, and keeps the worker alive: while the
// circuit is open no further event touches the store, and a later successful
// probe proves the worker still runs.
func TestStorePanicDropsBatchAndBlocksStore(t *testing.T) {
	store := &scriptedStore{panicOnWrite: true, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 2 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.droppedTotal.Load() == 2 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, 1, store.writeCountSnapshot())

	for i := 0; i < 5; i++ {
		require.False(t, d.TryEnqueue(sampleEventPtr(), 0))
	}
	assert.Equal(t, 1, store.writeCountSnapshot(), "open circuit must not touch the store")
	assert.Equal(t, int64(7), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())

	store.setPanicOnWrite(false)
	lastCooldownTimer(t, clk, d.cfg.FlushInterval).fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.TryEnqueue(sampleEventPtr(), 0) }, 2*time.Second, time.Millisecond)
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)

	assert.Equal(t, 2, store.writeCountSnapshot())
	assert.Equal(t, int64(1), d.writtenTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
}

// TestTimerWinsBacklogRaceStillDropped locks the full user-visible sequence:
// events B and C accepted while closed are still queued when the first write
// fails; the cooldown timer can win the select race against the open-circuit
// drain, entering half-open with backlog queued; a successful probe must then
// close the circuit with the queue empty. Whichever select case wins, the
// invariant holds and all counters agree; the select race is the behavior
// under test, not test randomness.
func TestTimerWinsBacklogRaceStillDropped(t *testing.T) {
	store := &scriptedStore{blockWrites: make(chan struct{}), writeNotify: make(chan struct{}, 64)}
	d, clk := newTestDispatcher(t, store, func(c *Config) {
		c.BatchSize = 1
		c.QueueSize = 8
	})

	// A is received and blocks in the store write; B and C are accepted while
	// the circuit is still closed and queue up behind it.
	require.True(t, d.TryEnqueue(sampleEventPtr(), 10))
	waitNotify(t, store.writeNotify)
	require.True(t, d.TryEnqueue(sampleEventPtr(), 10)) // B
	require.True(t, d.TryEnqueue(sampleEventPtr(), 10)) // C

	// A fails: the circuit opens with B and C as backlog.
	store.setErr(errBoom)
	close(store.blockWrites)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)

	// Fire the cooldown timer while backlog may still be queued: the drain
	// and the timer race, and the timer may win. Wait for half-open so the
	// probe admission below does not count a failed attempt while open.
	clk.snapshot()[1].fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)
	store.setErr(nil)

	// The probe succeeds and closes the circuit; backlog is never written.
	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)

	// Accounting: 1 probe written, A's failed batch plus B and C dropped,
	// reservations fully released.
	assert.Equal(t, int64(3), d.droppedTotal.Load())
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
	assert.Equal(t, 2, store.writeCountSnapshot()) // 1 failed + 1 probe write
}

// TestHalfOpenDropsBacklogWhileWaitingProbe verifies the half-open branch
// keeps dropping backlog admitted before the failure while the probe is
// pending: the cooldown timer can win the select race against the
// open-circuit drain, and the backlog must not stay queued until the probe
// decides the circuit. The backlog state is injected directly (the request
// path cannot produce it while the circuit is open or probing).
func TestHalfOpenDropsBacklogWhileWaitingProbe(t *testing.T) {
	store := &scriptedStore{err: errBoom, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0)) // A: first write fails
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)

	// The cooldown expires while backlog admitted before the failure is still
	// queued; the worker enters half-open.
	clk.snapshot()[1].fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)

	// Backlog injected directly once the worker is in the half-open branch
	// (injecting earlier would be consumed by the open-circuit drain).
	d.enqueue <- &queuedEvent{ev: sampleEventPtr(), reservation: 10}
	d.pendingCount.Add(1)
	d.pendingBytes.Add(10)

	// The backlog must be dropped while the worker waits for the probe, with
	// its count and byte reservation released, and no write performed.
	require.Eventually(t, func() bool { return d.droppedTotal.Load() == 2 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
	assert.Equal(t, 1, store.writeCountSnapshot())

	// The backlog drop must not disturb the probe: it succeeds and closes.
	store.setErr(nil)
	require.Eventually(t, func() bool { return d.TryEnqueue(sampleEventPtr(), 0) }, 2*time.Second, time.Millisecond)
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(1), d.writtenTotal.Load())
	assert.Equal(t, 2, store.writeCountSnapshot()) // 1 failed + 1 probe write
}

// TestProbeSuccessDropsBacklog verifies that backlog admitted before the
// failure is never rescued into a post-recovery write: the cooldown timer can
// win the select race against the open-circuit drain, leaving backlog in the
// queue while the probe runs. A successful probe must drop that backlog
// before closing the circuit. The backlog state is injected directly (the
// request path cannot produce it while the circuit is open or probing).
func TestProbeSuccessDropsBacklog(t *testing.T) {
	store := &scriptedStore{err: errBoom, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)

	// The cooldown expires before the backlog is drained; the worker enters
	// half-open with the backlog still queued.
	clk.snapshot()[1].fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)

	// Backlog admitted before the failure, restored as queue state directly.
	d.enqueue <- &queuedEvent{ev: sampleEventPtr(), reservation: 0}
	d.pendingCount.Add(1)

	// The probe succeeds; the backlog must be dropped, not written.
	store.setErr(nil)
	require.Eventually(t, func() bool { return d.TryEnqueue(sampleEventPtr(), 0) }, 2*time.Second, time.Millisecond)
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.droppedTotal.Load() == 2 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)

	assert.Equal(t, int64(1), d.writtenTotal.Load())
	assert.Equal(t, 2, store.writeCountSnapshot()) // 1 failed + 1 probe write
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestStartAfterStopFailsOpen verifies a late Start after a Stop on an
// unstarted dispatcher must not panic, must keep rejecting admissions, and
// must still shut down cleanly on a second Stop.
func TestStartAfterStopFailsOpen(t *testing.T) {
	store := &scriptedStore{}
	d := NewDispatcher(DefaultConfig(), store)
	clk := newFakeClock()
	d.clock = clk

	d.Stop(context.Background()) // not started: closes the store directly
	store.mu.Lock()
	assert.True(t, store.closed)
	store.mu.Unlock()

	d.Start() // late start: the worker runs against the closed store, fail-open
	require.False(t, d.TryEnqueue(sampleEventPtr(), 0))

	d.Stop(context.Background()) // second stop: waits for the worker
	store.mu.Lock()
	assert.True(t, store.closed)
	store.mu.Unlock()
}

// TestStaleClosedDecisionDroppedAfterCircuitCycle verifies a request path
// paused between the closed-circuit decision and the send is dropped, not
// written, if a full failure cycle (open, cooldown, probe, close) completes
// while it is paused: its admission decision predates the failure and must
// not land after the recovery.
func TestStaleClosedDecisionDroppedAfterCircuitCycle(t *testing.T) {
	store := &scriptedStore{err: errBoom, writeNotify: make(chan struct{}, 16)}
	d, clk := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	gate := make(chan struct{})
	released := make(chan struct{})
	var gated atomic.Bool
	d.sendGate = func() {
		if !gated.CompareAndSwap(false, true) {
			return // only the first admission is paused; the cycle drivers pass
		}
		close(gate)
		<-released
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.TryEnqueue(sampleEventPtr(), 0)
	}()
	<-gate // the request path holds its closed decision, before the send

	// A full failure cycle completes while the request path is paused:
	// another event fails, the cooldown expires, and a probe recovers.
	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitOpen }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(clk.snapshot()) >= 2 }, 2*time.Second, time.Millisecond)
	clk.snapshot()[1].fire()
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitHalfOpen }, 2*time.Second, time.Millisecond)
	store.setErr(nil)
	require.True(t, d.TryEnqueue(sampleEventPtr(), 0)) // probe
	require.Eventually(t, func() bool { return d.circuitStateVal() == circuitClosed }, 2*time.Second, time.Millisecond)

	// The paused request resumes with its stale closed decision; its event
	// must be dropped, not written after the recovery.
	close(released)
	wg.Wait()

	require.Eventually(t, func() bool { return d.droppedTotal.Load() == 2 }, 2*time.Second, time.Millisecond) // failed batch + stale decision
	assert.Equal(t, int64(1), d.writtenTotal.Load())                                                          // the probe only
	require.Eventually(t, func() bool { return d.pendingCount.Load() == 0 }, 2*time.Second, time.Millisecond)
	st := d.Status()
	assert.Equal(t, 0, st.QueueCount)
	assert.Equal(t, int64(0), st.QueueBytes)
	// Accounting identity: 3 accepted (paused + trigger + probe) == 1 written + 2 dropped.
	assert.Equal(t, int64(3), d.acceptedTotal.Load())
	assert.Equal(t, st.WrittenTotal+st.DroppedTotal, d.acceptedTotal.Load())
}

// TestExpiredStopCtxWaitsForTicketHoldingAdmission verifies the drain waits
// unconditionally for ticket-holding admissions: an expired stop context must
// not let the worker exit while a request path that passed the stopped
// re-check is between the ticket and the send, or its event would be stranded
// in the queue with no consumer.
func TestExpiredStopCtxWaitsForTicketHoldingAdmission(t *testing.T) {
	store := &scriptedStore{}
	d, _ := newTestDispatcher(t, store, nil)

	gate := make(chan struct{})
	released := make(chan struct{})
	d.sendGate = func() { close(gate); <-released }

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.TryEnqueue(sampleEventPtr(), 100)
	}()

	<-gate // the request path holds its ticket, past the stopped re-check
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Stop(ctx) // returns immediately; the worker must still wait for the admission
	close(released)
	wg.Wait()

	// The event was sent and then drained, never stranded: the queue is
	// empty and the accounting identity holds (1 accepted, 0 written,
	// 1 drained drop).
	require.Eventually(t, func() bool { return d.pendingCount.Load() == 0 }, 2*time.Second, time.Millisecond)
	st := d.Status()
	assert.Equal(t, 0, st.QueueCount)
	assert.Equal(t, int64(0), st.QueueBytes)
	assert.Equal(t, int64(1), d.droppedTotal.Load())
}

// TestPGLatencyMSStartsMinusOne locks the frozen Status contract: -1 until
// the first write completes, then the duration of the most recent write.
func TestPGLatencyMSStartsMinusOne(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 16)}
	d, _ := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })

	assert.Equal(t, int64(-1), d.Status().PGLatencyMS)

	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	waitNotify(t, store.writeNotify)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)
	assert.GreaterOrEqual(t, d.Status().PGLatencyMS, int64(0))
}

// TestQueueViewsNonNegativeAndConsistent locks the bounded-count semantics:
// the queue count is registered before the channel send, so the worker's
// release can never make the observable count negative, and the views agree
// with the accounting identity after Stop.
func TestQueueViewsNonNegativeAndConsistent(t *testing.T) {
	store := &scriptedStore{writeNotify: make(chan struct{}, 256)}
	d, _ := newTestDispatcher(t, store, func(c *Config) { c.BatchSize = 8 })

	var stop atomic.Bool
	var accepted, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if d.TryEnqueue(sampleEventPtr(), 7) {
					accepted.Add(1)
				} else {
					rejected.Add(1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			st := d.Status()
			if st.QueueCount < 0 || st.QueueBytes < 0 {
				t.Errorf("negative queue view: count=%d bytes=%d", st.QueueCount, st.QueueBytes)
				return
			}
		}
	}()

	d.Stop(context.Background())
	stop.Store(true)
	wg.Wait()

	st := d.Status()
	assert.Equal(t, 0, st.QueueCount)
	assert.Equal(t, int64(0), st.QueueBytes)
	assert.Equal(t, accepted.Load(), st.WrittenTotal+st.DroppedTotal-rejected.Load())
}

// TestZeroReservationAtByteCapAdmitted verifies the byte-budget boundary: a
// zero-reservation (metadata-only style) event is still admitted while the
// budget is exhausted, and any positive reservation is rejected.
func TestZeroReservationAtByteCapAdmitted(t *testing.T) {
	store := &scriptedStore{blockWrites: make(chan struct{}), writeNotify: make(chan struct{}, 16)}
	d, _ := newTestDispatcher(t, store, func(c *Config) {
		c.BatchSize = 1
		c.QueueBytes = c.MaxRequestBytes
	})

	require.True(t, d.TryEnqueue(sampleEventPtr(), d.cfg.QueueBytes))
	waitNotify(t, store.writeNotify) // worker blocked in the write

	// Occupy the whole budget with a queued event (the worker is blocked and
	// cannot receive it), then the boundary: a zero-reservation event still
	// fits while any positive reservation is rejected.
	require.True(t, d.TryEnqueue(sampleEventPtr(), d.cfg.QueueBytes))
	require.True(t, d.TryEnqueue(sampleEventPtr(), 0))
	require.False(t, d.TryEnqueue(sampleEventPtr(), 1))

	assert.Equal(t, int64(1), d.droppedTotal.Load())
	assert.Equal(t, int64(2), d.pendingCount.Load())
	assert.Equal(t, d.cfg.QueueBytes, d.pendingBytes.Load())

	close(store.blockWrites)
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 3 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(0), d.pendingCount.Load())
	assert.Equal(t, int64(0), d.pendingBytes.Load())
}

// TestTryEnqueuePanicAfterReserveReleasesBudget is the P2-1 regression: a
// panic inside TryEnqueue after the byte reservation but before the event
// reaches the queue must roll the reservation back. The old code recovered
// without releasing, so one bug panic leaked the reservation permanently and
// the admission budget silently degraded toward zero (everything dropped). The
// reserveGate seam panics exactly in that window; afterwards the budget must
// be fully usable again and the counters must show the drop.
func TestTryEnqueuePanicAfterReserveReleasesBudget(t *testing.T) {
	d, _ := newTestDispatcher(t, &scriptedStore{}, func(c *Config) { c.BatchSize = 1 })

	d.reserveGate = func() { panic("scripted panic in the reserved window") }
	defer func() { d.reserveGate = nil }()

	require.False(t, d.TryEnqueue(sampleEventPtr(), 100))
	assert.Equal(t, int64(0), d.pendingCount.Load(), "the count registration must be rolled back")
	assert.Equal(t, int64(0), d.pendingBytes.Load(), "the byte reservation must be rolled back")
	assert.Equal(t, int64(1), d.droppedTotal.Load())

	// The budget must be usable after the panic: a normal event is admitted
	// with its full reservation (TryEnqueue returning true proves the byte
	// reserve succeeded), and the worker drains it to completion — the
	// pending counters settle at zero instead of leaking the reservation.
	// The drain is asserted with Eventually: the worker processes the queue
	// asynchronously, so the instantaneous pending value is not stable.
	d.reserveGate = nil
	require.True(t, d.TryEnqueue(sampleEventPtr(), 100))
	// BatchSize=1 makes the worker flush on arrival, so the write is
	// deterministic instead of racing the flush timer on the instantaneous
	// pending value.
	require.Eventually(t, func() bool { return d.writtenTotal.Load() == 1 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		return d.pendingBytes.Load() == 0 && d.pendingCount.Load() == 0
	}, 2*time.Second, time.Millisecond)
}
