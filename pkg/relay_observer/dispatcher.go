package relayobserver

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file implements the bounded, fail-open runtime core of the observer:
// the dispatcher. It admits turn events from the request path under a queue
// count and a byte budget, drains them on one worker, and writes bounded
// batches to the Store port. The request path never blocks: TryEnqueue only
// performs an atomic byte reservation and a non-blocking channel send, and
// rolls the reservation back on every failure. Delivery is best effort — a
// failed batch is dropped and opens a write circuit whose cooldown grows
// exponentially; the worker never retries a batch and never writes a spool.

// Circuit cooldown bounds. The cooldown starts at initialCooldown after the
// first write failure and doubles on every subsequent failure until it caps
// at maxCooldown. A successful half-open probe resets it to initialCooldown.
const (
	initialCooldown = 5 * time.Second
	maxCooldown     = 5 * time.Minute
)

// retentionInterval keeps bounded cleanup frequent enough to outpace normal
// observer ingestion without turning a failure into a tight retry loop.
const retentionInterval = time.Minute

// maxPendingAppendEvents complements Config.PendingAppendBytes with a fixed
// per-append overhead bound.
const maxPendingAppendEvents = 4 * MaxBatchSize

// recentVolume packs a fixed one-second admission bucket into one atomic
// word: the upper bits hold Unix seconds and the lower 24 bits the count.
// A packed CAS keeps requests on opposite sides of a second boundary from
// being mixed by a reset race. The count saturates at ~16.7M admissions/s.
const (
	recentVolumeCountBits = 24
	recentVolumeCountMask = uint64(1<<recentVolumeCountBits) - 1
)

// circuitStateVal is the atomic state of the write circuit. The worker
// transitions open -> half-open when the cooldown expires; the request path
// transitions half-open -> probing with a single CAS that admits exactly one
// probe event; the worker ends the probe with closed or open.
type circuitStateVal int32

const (
	// circuitClosed admits events normally and flushes batches.
	circuitClosed circuitStateVal = iota
	// circuitOpen drops every new event; the worker drains stale queue items
	// and waits out the cooldown.
	circuitOpen
	// circuitHalfOpen admits exactly one probe event (via CAS); the worker
	// writes it as a single-event batch once it arrives.
	circuitHalfOpen
	// circuitProbing marks the probe event admitted and in flight; every
	// concurrent request is dropped until the probe write finishes.
	circuitProbing
)

// timer is the minimal seam the worker needs from time.Timer so tests can
// drive the worker deterministically.
type timer interface {
	C() <-chan time.Time
	Reset(time.Duration) bool
	Stop() bool
}

// clock is the minimal time seam of the dispatcher. It is the only
// testability seam in the runtime core; no general clock framework is used.
type clock interface {
	Now() time.Time
	NewTimer(time.Duration) timer
}

// realClock is the production clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) NewTimer(d time.Duration) timer {
	return realTimer{t: time.NewTimer(d)}
}

// realTimer adapts *time.Timer to the timer interface (Timer.C is a field).
type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
func (r realTimer) Stop() bool                 { return r.t.Stop() }

// queuedEvent couples an admitted event with the byte reservation taken for
// it on the request path. The worker releases both together when it receives
// the event, so every accepted item releases its reservation exactly once, on
// whichever path the event leaves the queue. epoch records the circuit epoch
// of the admission decision; the worker drops events whose decision predates
// a circuit failure (see circuitEpoch).
type queuedEvent struct {
	ev          *Event
	reservation int64
	epoch       int64
}

// pendingContentAppend couples one content persistence input with its exact
// serialized byte charge. The worker reserves count and bytes before writing
// the turn metadata, so a budget miss can backfill metadata_only honestly.
// retry marks an input retained after a failed AppendTurns call.
type pendingContentAppend struct {
	input              ContentInput
	bytes              int64
	retry              bool
	gapCounted         bool
	degradationCounted bool
}

// Dispatcher is the bounded, fail-open runtime core of the observer: one
// worker consumes an admission queue under a count and byte budget and writes
// bounded batches to a Store. Construct with NewDispatcher, start with Start,
// and stop with Stop. All request-path entry points are non-blocking and
// never touch the database.
type Dispatcher struct {
	cfg   Config
	store Store

	enqueue chan *queuedEvent
	probeCh chan *queuedEvent

	clock clock

	stopNotify chan struct{}
	workerDone chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	started    atomic.Bool
	stopped    atomic.Bool

	// stopMu guards stopCtx, which Stop swaps in under the once so the worker
	// observes the shutdown deadline.
	stopMu  sync.Mutex
	stopCtx context.Context

	// tryInFlight counts requests that passed the stopped check and are inside
	// the TryEnqueue critical section; the worker drains the queue only after
	// this reaches zero so no in-flight send lands after the drain.
	tryInFlight atomic.Int64

	// enqueueGate is a test-only pause point in the admission path, after the
	// first stopped check and before the admission ticket is taken. It is nil
	// in production; the shutdown-race tests use it to hold a request path in
	// flight while Stop completes.
	enqueueGate func()

	// sendGate is a test-only pause point after the stopped re-check and
	// before the byte reservation and channel send, while the admission
	// ticket is held. It is nil in production; the shutdown tests use it to
	// hold a ticket-holding request path in flight while Stop drains.
	sendGate func()

	// reserveGate is a test-only pause point after the byte reservation and
	// before the channel send, while the reservation is held. It is nil in
	// production; the reservation-leak regression test panics here to prove
	// a bug inside the reserved-but-not-yet-sent window rolls the budget
	// back instead of leaking it.
	reserveGate func()

	pendingCount atomic.Int64
	pendingBytes atomic.Int64

	pendingContentCount atomic.Int64
	pendingContentBytes atomic.Int64

	acceptedTotal  atomic.Int64
	writtenTotal   atomic.Int64
	droppedTotal   atomic.Int64
	contentGaps    atomic.Int64
	contentRetried atomic.Int64
	contentDropped atomic.Int64
	recentVolume   atomic.Uint64
	pgLatencyMS    atomic.Int64

	// contentFailureLoggedAt is the unix-nanosecond timestamp of the last
	// content-append failure log. It rate-limits the operator-visible
	// content-health signal so a persistent poison turn does not flood the
	// error log: one log per contentFailureLogInterval.
	contentFailureLoggedAt atomic.Int64

	// circuitState is the atomic circuit state machine; circuitCooldown is the
	// next cooldown value and is written only by the worker; circuitUntilNano
	// is the cooldown deadline in unix nanoseconds, read by Status and the
	// worker timer.
	circuitState     atomic.Int32
	circuitCooldown  time.Duration
	circuitUntilNano atomic.Int64

	// circuitEpoch increments on every failure cycle (openCircuit); a queued
	// event records the epoch of its admission decision, and the worker drops
	// events whose decision predates the current epoch: a request path paused
	// between its closed decision and the send could otherwise land a stale
	// event after a probe recovered the circuit.
	circuitEpoch atomic.Int64

	// pendingAppends retains the content appends whose AppendTurns failed
	// after their metadata batch was already written. The worker merges them
	// into the next flush pass and retries; metadata rows are idempotent
	// (ON CONFLICT DO NOTHING) and content objects dedup, so a retry never
	// duplicates anything. Count and serialized bytes are reserved before the
	// metadata write and remain charged until success or an explicit drop.
	// Worker-only.
	pendingAppends []pendingContentAppend

	// retention is the T5.1 retention surface of the store, set when the
	// store implements it. When set, Start launches the retention worker as
	// an independent goroutine sharing the store and the stop signal; the
	// retention pass never enqueues, probes, or touches the circuit.
	retention RetentionStore

	retentionDone chan struct{}

	// Retention counters, all totals since process start. lastRetentionPass
	// holds the last pass completion in unix nanoseconds; zero means no pass
	// has completed yet.
	lastRetentionPass         atomic.Int64
	retentionTurnsDeleted     atomic.Int64
	retentionSessionsDeleted  atomic.Int64
	retentionObjectsDeleted   atomic.Int64
	retentionFailures         atomic.Int64
	retentionTurnsPending     atomic.Int64
	retentionSessionsPending  atomic.Int64
	retentionObjectsPending   atomic.Int64
	retentionBacklogOldest    atomic.Int64
	retentionBacklogTruncated atomic.Bool

	ipTrust atomic.Pointer[IPTrust]
}

// NewDispatcher builds a dispatcher around store. The configuration's
// QueueSize, QueueBytes, and MaxRequestBytes bound admission; BatchSize,
// FlushInterval, and WriteTimeout bound worker writes.
func NewDispatcher(cfg Config, store Store) *Dispatcher {
	d := &Dispatcher{
		cfg:             cfg,
		store:           store,
		enqueue:         make(chan *queuedEvent, cfg.QueueSize),
		probeCh:         make(chan *queuedEvent, 1),
		clock:           realClock{},
		stopNotify:      make(chan struct{}),
		workerDone:      make(chan struct{}),
		stopCtx:         context.Background(),
		circuitCooldown: initialCooldown,
	}
	if rs, ok := store.(RetentionStore); ok {
		d.retention = rs
		d.retentionDone = make(chan struct{})
	}
	// Status contract: PGLatencyMS is -1 until the first write completes.
	d.pgLatencyMS.Store(-1)
	return d
}

// Start launches the single worker. It is idempotent.
func (d *Dispatcher) Start() {
	d.startOnce.Do(func() {
		d.started.Store(true)
		go d.run()
		if d.retention != nil {
			go d.runRetention()
		}
	})
}

// SetIPTrust sets the effective IP trust tier reported by Status. It is
// configuration-owned and carries no secrets.
func (d *Dispatcher) SetIPTrust(t IPTrust) {
	v := t
	d.ipTrust.Store(&v)
}

// TryEnqueue admits one turn event under the queue count and byte budgets.
// It is the only request-path entry point: it performs an atomic byte
// reservation and a non-blocking channel send, and rolls the reservation back
// on every failure. It never waits, marshals, copies the request, or touches
// the database. A nil event, a negative reservation, or a reservation above
// MaxRequestBytes is rejected; a stopped dispatcher, an open or probing
// circuit, a full queue, or an exhausted byte budget is rejected. Returns
// false when the event was dropped.
func (d *Dispatcher) TryEnqueue(ev *Event, reservation int64) (ok bool) {
	// reserved/registered track the admission state for the deferred recover:
	// a panic after the byte reservation but before the event reaches the
	// queue must roll the reservation and the count registration back, or a
	// bug would leak the byte budget permanently and silently zero admission
	// (P2-1 hardening). Once the event is queued the worker owns the release,
	// and both flags are cleared so the defer never double-releases.
	reserved := false
	registered := false
	defer func() {
		if r := recover(); r != nil {
			// Defensive: the request path must stay fail-open even if a bug
			// panics here. A reservation, if already taken, is rolled back so
			// the admission budget keeps working; fail-open is unaffected but
			// the degradation is no longer silently permanent.
			ok = false
			if reserved {
				d.releaseBytes(reservation)
			}
			if registered {
				d.pendingCount.Add(-1)
			}
			d.droppedTotal.Add(1)
		}
	}()
	if ev == nil || reservation < 0 || reservation > d.cfg.MaxRequestBytes {
		d.droppedTotal.Add(1)
		return false
	}
	if d.stopped.Load() {
		d.droppedTotal.Add(1)
		return false
	}
	if d.enqueueGate != nil {
		d.enqueueGate()
	}
	d.tryInFlight.Add(1)
	defer d.tryInFlight.Add(-1)
	// Stop may have set stopped and drained while the gate held this request
	// path; the worker cannot observe an admission that has not taken its
	// in-flight ticket yet, so re-check before reserving and sending.
	if d.stopped.Load() {
		d.droppedTotal.Add(1)
		return false
	}
	// Record the admission-decision epoch before the send gate and the
	// circuit switch: if a failure cycle completes while this request path is
	// paused, the worker drops the stale event instead of writing it.
	qe := &queuedEvent{ev: ev, reservation: reservation, epoch: d.circuitEpoch.Load()}
	if d.sendGate != nil {
		d.sendGate()
	}

	switch circuitStateVal(d.circuitState.Load()) {
	case circuitOpen, circuitProbing:
		d.droppedTotal.Add(1)
		return false
	case circuitHalfOpen:
		// Admit exactly one probe event: the CAS wins for a single caller and
		// everyone else drops until the probe write finishes.
		if !d.circuitState.CompareAndSwap(int32(circuitHalfOpen), int32(circuitProbing)) {
			d.droppedTotal.Add(1)
			return false
		}
		if !d.reserveBytes(reservation) {
			d.circuitState.Store(int32(circuitHalfOpen)) // give the slot back
			d.droppedTotal.Add(1)
			return false
		}
		reserved = true
		if d.reserveGate != nil {
			d.reserveGate()
		}
		// Register the queue count before the send: the worker can release it
		// as soon as it receives the event, and the release must never be
		// observable before the registration (no negative queue view).
		d.pendingCount.Add(1)
		registered = true
		select {
		case d.probeCh <- qe:
			reserved, registered = false, false
		default:
			d.pendingCount.Add(-1)
			registered = false
			d.releaseBytes(reservation)
			reserved = false
			d.circuitState.Store(int32(circuitHalfOpen))
			d.droppedTotal.Add(1)
			return false
		}
	default: // circuitClosed
		if !d.reserveBytes(reservation) {
			d.droppedTotal.Add(1)
			return false
		}
		reserved = true
		if d.reserveGate != nil {
			d.reserveGate()
		}
		d.pendingCount.Add(1)
		registered = true
		select {
		case d.enqueue <- qe:
			reserved, registered = false, false
		default:
			d.pendingCount.Add(-1)
			registered = false
			d.releaseBytes(reservation)
			reserved = false
			d.droppedTotal.Add(1)
			return false
		}
	}
	d.acceptedTotal.Add(1)
	d.recordRecentVolume(d.clock.Now())
	return true
}

func (d *Dispatcher) recordRecentVolume(now time.Time) {
	second := uint64(now.Unix())
	for {
		old := d.recentVolume.Load()
		oldSecond := old >> recentVolumeCountBits
		oldCount := old & recentVolumeCountMask
		var next uint64
		if oldSecond == second {
			if oldCount == recentVolumeCountMask {
				return
			}
			next = second<<recentVolumeCountBits | (oldCount + 1)
		} else {
			next = second<<recentVolumeCountBits | 1
		}
		if d.recentVolume.CompareAndSwap(old, next) {
			return
		}
	}
}

func (d *Dispatcher) recentVolumeAt(now time.Time) int64 {
	packed := d.recentVolume.Load()
	if packed>>recentVolumeCountBits != uint64(now.Unix()) {
		return 0
	}
	return int64(packed & recentVolumeCountMask)
}

// Stop shuts the dispatcher down. It is idempotent. The enqueue channels are
// never closed, so a racing request path can never send on a closed channel.
// After the stop signal the worker waits for the TryEnqueue critical section
// to empty, drains and counts the remaining events, and exits; the drain is
// bounded by ctx. Stop returns as soon as ctx expires, drops what is left,
// and passes the remaining budget to Store.Close.
func (d *Dispatcher) Stop(ctx context.Context) {
	d.stopOnce.Do(func() {
		d.stopped.Store(true)
		d.stopMu.Lock()
		d.stopCtx = ctx
		d.stopMu.Unlock()
		close(d.stopNotify)
	})
	if !d.started.Load() {
		d.dropPendingContent(d.pendingAppends)
		d.pendingAppends = nil
		d.store.Close(ctx)
		return
	}
	select {
	case <-d.workerDone:
	case <-ctx.Done():
	}
	if d.retention != nil {
		// The retention pass aborts on the stop context or its segment
		// timeout, whichever comes first; wait for the goroutine to exit
		// within the remaining shutdown budget.
		select {
		case <-d.retentionDone:
		case <-ctx.Done():
		}
	}
	d.store.Close(ctx)
}

// Status returns a point-in-time snapshot assembled from in-memory counters.
// It never contains secrets, DSNs, or event content; ReasonCode is a stable
// code and IPTrust the effective configuration tier. Safe to call
// concurrently with any other method.
func (d *Dispatcher) Status() Status {
	now := d.clock.Now()
	st := Status{
		Enabled:             true,
		IPTrust:             d.ipTrustValue(),
		QueueCount:          int(d.pendingCount.Load()),
		QueueBytes:          d.pendingBytes.Load(),
		PendingContentCount: int(d.pendingContentCount.Load()),
		PendingContentBytes: d.pendingContentBytes.Load(),
		AcceptedTotal:       d.acceptedTotal.Load(),
		WrittenTotal:        d.writtenTotal.Load(),
		DroppedTotal:        d.droppedTotal.Load(),
		PGLatencyMS:         d.pgLatencyMS.Load(),
		ContentGapsTotal:    d.contentGaps.Load(),
		ContentRetriedTotal: d.contentRetried.Load(),
		ContentDroppedTotal: d.contentDropped.Load(),
		RecentVolume:        d.recentVolumeAt(now),

		RetentionTurnsDeleted:     d.retentionTurnsDeleted.Load(),
		RetentionSessionsDeleted:  d.retentionSessionsDeleted.Load(),
		RetentionObjectsDeleted:   d.retentionObjectsDeleted.Load(),
		RetentionFailures:         d.retentionFailures.Load(),
		RetentionTurnsPending:     d.retentionTurnsPending.Load(),
		RetentionSessionsPending:  d.retentionSessionsPending.Load(),
		RetentionObjectsPending:   d.retentionObjectsPending.Load(),
		RetentionBacklogTruncated: d.retentionBacklogTruncated.Load(),
	}
	if nano := d.lastRetentionPass.Load(); nano != 0 {
		st.LastRetentionPass = time.Unix(0, nano)
	}
	if nano := d.retentionBacklogOldest.Load(); nano != 0 {
		st.RetentionBacklogAge = now.Sub(time.Unix(0, nano))
		if st.RetentionBacklogAge < 0 {
			st.RetentionBacklogAge = 0
		}
	}
	if circuitStateVal(d.circuitState.Load()) != circuitClosed {
		st.CircuitOpen = true
		st.ReasonCode = ReasonCircuitOpen
		remaining := d.circuitUntilNano.Load() - now.UnixNano()
		if remaining < 0 {
			remaining = 0
		}
		st.CircuitCooldown = time.Duration(remaining)
	}
	return st
}

// run owns the worker goroutine. It re-enters the loop after a recovered
// panic so the worker stays recoverable, and closes workerDone exactly once
// when it finally exits.
func (d *Dispatcher) run() {
	defer close(d.workerDone)
	for {
		if d.loop() {
			return
		}
	}
}

// loop is one worker pass. It returns true when the worker should exit (stop
// signal) and false when it was restarted after a recovered panic.
func (d *Dispatcher) loop() (normalStop bool) {
	var batch []queuedEvent
	flushTimer := d.clock.NewTimer(d.cfg.FlushInterval)
	var circuitTimer timer
	defer func() {
		if r := recover(); r != nil {
			d.onWorkerPanic(r, batch)
			normalStop = false
		}
		flushTimer.Stop()
		if circuitTimer != nil {
			circuitTimer.Stop()
		}
	}()

	for {
		switch circuitStateVal(d.circuitState.Load()) {
		case circuitOpen:
			if circuitTimer == nil {
				// Timer for the remaining cooldown; the deadline is the single
				// source of truth for the exponential backoff.
				remaining := time.Duration(d.circuitUntilNano.Load() - d.clock.Now().UnixNano())
				circuitTimer = d.clock.NewTimer(remaining)
			}
			select {
			case qe := <-d.enqueue:
				d.releaseReservation(qe)
				d.droppedTotal.Add(1)
			case qe := <-d.probeCh:
				d.releaseReservation(qe)
				d.droppedTotal.Add(1)
			case <-circuitTimer.C():
				circuitTimer.Stop()
				circuitTimer = nil
				d.circuitState.Store(int32(circuitHalfOpen))
			case <-d.stopNotify:
				d.drainDrop(batch)
				return true
			case <-d.stopDone():
				d.drainDrop(batch)
				return true
			}
		case circuitHalfOpen, circuitProbing:
			// Wait for the single admitted probe event; concurrent requests
			// are dropped by the request path until this write finishes.
			// Backlog admitted before the failure is still dropped while the
			// probe is pending: it must never reach the closed circuit.
			select {
			case qe := <-d.probeCh:
				d.releaseReservation(qe)
				batch = append(batch, *qe)
				if err := d.flush(&batch); err != nil {
					d.openCircuit()
				} else {
					d.closeCircuitAfterDrainingBacklog()
				}
			case qe := <-d.enqueue:
				d.releaseReservation(qe)
				d.droppedTotal.Add(1)
			case <-d.stopNotify:
				d.drainDrop(batch)
				return true
			case <-d.stopDone():
				d.drainDrop(batch)
				return true
			}
		default: // circuitClosed
			select {
			case qe := <-d.enqueue:
				d.releaseReservation(qe)
				// A decision made before the current failure cycle is stale:
				// the circuit failed, recovered, or is recovering since this
				// event was admitted, and it must be dropped instead of
				// written after the recovery.
				if qe.epoch != d.circuitEpoch.Load() {
					d.droppedTotal.Add(1)
					continue
				}
				batch = append(batch, *qe)
				if len(batch) >= d.cfg.BatchSize {
					if err := d.flush(&batch); err != nil {
						d.openCircuit()
					}
				}
			case <-flushTimer.C():
				flushTimer.Reset(d.cfg.FlushInterval)
				if len(batch) > 0 {
					if err := d.flush(&batch); err != nil {
						d.openCircuit()
					}
				} else if len(d.pendingAppends) > 0 {
					d.retryPendingContent()
				}
			case <-d.stopNotify:
				d.drainDrop(batch)
				return true
			case <-d.stopDone():
				d.drainDrop(batch)
				return true
			}
		}
	}
}

// drainDrop drops the in-progress batch, waits for the TryEnqueue critical
// section to empty, and then drops every event still in the queues, releasing
// reservations and counting each drop. It is called only from the worker after
// the stop signal. The wait is unconditional: every admission in flight
// completes without blocking (the critical section is atomics, a CAS, and a
// non-blocking send, and the deferred ticket release always runs), so
// abandoning the wait on the stop context could let a ticket-holding request
// path send into the queue after the worker exited, stranding the event. The
// stop context bounds Stop's own wait and Store.Close, not this drain.
func (d *Dispatcher) drainDrop(batch []queuedEvent) {
	if len(batch) > 0 {
		d.droppedTotal.Add(int64(len(batch)))
	}
	for d.tryInFlight.Load() > 0 {
		runtime.Gosched()
	}
	for {
		select {
		case qe := <-d.enqueue:
			d.releaseReservation(qe)
			d.droppedTotal.Add(1)
		case qe := <-d.probeCh:
			d.releaseReservation(qe)
			d.droppedTotal.Add(1)
		default:
			d.flushPendingContentOnStop()
			return
		}
	}
}

// flushChunkMaxEvents bounds one flush chunk. A batch is split into
// chunks of at most this many events; each chunk gets its own deadline derived
// from the remaining WriteTimeout so a large batch cannot starve its tail
// under one shared deadline.
const flushChunkMaxEvents = 32

// flush writes one batch in bounded chunks. Each chunk gets its own
// deadline derived from the remaining WriteTimeout. A metadata-write failure
// drops the rest of the batch and returns an error so the caller opens the
// circuit (unchanged). A content-append failure does NOT open the circuit:
// the appends are isolated and the flush continues, so a deterministic
// poison turn can never stop the metadata stream. The batch is always
// emptied.
func (d *Dispatcher) flush(batch *[]queuedEvent) error {
	start := d.clock.Now()
	// The chunk deadline is real wall-clock: context.WithTimeout needs a real
	// duration, and the fake clock seam drives worker timers, not context
	// cancellation. Each chunk derives its deadline from the same start so a
	// large batch cannot exhaust the budget before its tail chunks run.
	deadline := time.Now().Add(d.cfg.WriteTimeout)
	for len(*batch) > 0 {
		chunkSize := len(*batch)
		if chunkSize > flushChunkMaxEvents {
			chunkSize = flushChunkMaxEvents
		}
		chunk := (*batch)[:chunkSize]
		remaining := time.Until(deadline)
		if remaining <= 0 {
			d.droppedTotal.Add(int64(len(*batch)))
			*batch = (*batch)[:0]
			d.pgLatencyMS.Store(int64(d.clock.Now().Sub(start) / time.Millisecond))
			return fmt.Errorf("relayobserver: flush deadline exhausted")
		}
		ctx, cancel := context.WithTimeout(d.stopCtxSnapshot(), remaining)
		err := d.flushChunk(ctx, &chunk)
		cancel()
		if err != nil {
			// The metadata write failed: drop the rest of the batch (never
			// retried, never spooled) and return so the caller opens the
			// circuit.
			d.droppedTotal.Add(int64(len(*batch)))
			*batch = (*batch)[:0]
			d.pgLatencyMS.Store(int64(d.clock.Now().Sub(start) / time.Millisecond))
			return err
		}
		*batch = (*batch)[chunkSize:]
	}
	d.pgLatencyMS.Store(int64(d.clock.Now().Sub(start) / time.Millisecond))
	return nil
}

// flushChunk writes one chunk in two phases (T2.6): the metadata write first,
// then the captured content appends. Content planning runs before the
// metadata write so the content_state column carries the true normalization
// outcome. A metadata-write failure returns an error (the caller opens the
// circuit). A content-append failure is absorbed here: the appends are
// isolated and nil is returned so the circuit stays closed.
func (d *Dispatcher) flushChunk(ctx context.Context, chunk *[]queuedEvent) (err error) {
	n := len(*chunk)
	appends := d.planContent(*chunk)
	contentTransferred := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if !contentTransferred {
				d.releasePendingContent(appends)
			}
			panic(recovered)
		}
	}()
	events := make([]Event, n)
	for i := range *chunk {
		events[i] = *(*chunk)[i].ev
	}
	if err := d.store.WriteBatch(ctx, events); err != nil {
		// These inputs never became retryable because their metadata rows did
		// not commit. Release their content reservations without counting an
		// additional content drop; the metadata batch drop is counted by flush.
		d.releasePendingContent(appends)
		contentTransferred = true
		return err
	}
	d.writtenTotal.Add(int64(n))
	combined := make([]pendingContentAppend, 0, len(d.pendingAppends)+len(appends))
	combined = append(combined, d.pendingAppends...)
	combined = append(combined, appends...)
	clear(d.pendingAppends)
	d.pendingAppends = nil
	contentTransferred = true
	d.persistContent(ctx, combined)
	return nil
}

// persistContent attempts one bounded bulk append. A failure is isolated per
// turn so deterministic poison is dropped while transient failures keep their
// existing count and byte reservations for a later metadata or idle flush.
func (d *Dispatcher) persistContent(ctx context.Context, appends []pendingContentAppend) {
	if len(appends) == 0 {
		return
	}
	if err := d.appendPendingContent(ctx, appends); err != nil {
		d.handleContentAppendFailure(appends, err)
		return
	}
	d.completePendingContent(appends)
}

func (d *Dispatcher) appendPendingContent(ctx context.Context, appends []pendingContentAppend) error {
	inputs := make([]ContentInput, len(appends))
	for i := range appends {
		inputs[i] = appends[i].input
	}
	return d.appendContentInputs(ctx, inputs)
}

func (d *Dispatcher) appendContentInputs(ctx context.Context, inputs []ContentInput) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("relayobserver: content append panic")
		}
	}()
	return d.store.AppendTurns(ctx, inputs)
}

// handleContentAppendFailure isolates failed content appends. Without
// isolation a single deterministic poison turn (constraint violation, invalid
// digest, encode error) would be retained and retried forever, blocking every
// healthy turn behind it in the pending queue. Each append is retried
// individually under a fresh budget bounded by one WriteTimeout: turns that
// now succeed are already persisted (idempotent); turns that fail
// deterministically are dropped and counted as content gaps; transient
// failures (deadlock, timeout, connection loss) are retained for the next
// pass. The metadata rows stay written either way.
func (d *Dispatcher) handleContentAppendFailure(appends []pendingContentAppend, firstErr error) {
	d.logContentHealth(firstErr, len(appends))
	deadline := time.Now().Add(d.cfg.WriteTimeout)
	retained := make([]pendingContentAppend, 0, len(appends))
	for i := range appends {
		appends[i].retry = true
	}
	for i := range appends {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Isolation budget exhausted: retain the rest for the next pass.
			retained = append(retained, appends[i:]...)
			break
		}
		ctx, cancel := context.WithTimeout(d.stopCtxSnapshot(), remaining)
		err := d.appendContentInputs(ctx, []ContentInput{appends[i].input})
		cancel()
		if err == nil {
			d.completePendingContent(appends[i : i+1])
			continue
		}
		if isDeterministicContentError(err) {
			// Deterministic poison cannot make progress on a future pass.
			d.dropPendingContent(appends[i : i+1])
			continue
		}
		// Transient or unknown failure: retain for the next pass.
		retained = append(retained, appends[i])
	}
	d.pendingAppends = retained
}

// isDeterministicContentError classifies a content-append failure as a
// permanent poison. Only PostgreSQL data exceptions (22xxx), integrity
// constraint violations (23xxx), and datatype mismatches (42804) are
// deterministic: the same turn will fail on every retry, so it must be
// dropped. Everything else — deadlock (40P01), serialization failure (40001),
// query cancellation (57014), connection loss, context deadline, and unknown
// errors — is retained for the next pass, because it may succeed later.
func isDeterministicContentError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "22", "23": // data exception / integrity constraint violation
		return true
	}
	return pgErr.Code == "42804" // datatype mismatch
}

// contentFailureLogInterval rate-limits the operator-visible content-health
// signal.
const contentFailureLogInterval = time.Minute

// logContentHealth records a rate-limited operator-visible signal when content
// appends fail. The observer stays fail-open and the circuit stays
// closed, but the degradation must not be silent: without it a persistently
// failing content store looks identical to a healthy one in the status
// counters until someone polls ContentGapsTotal.
func (d *Dispatcher) logContentHealth(err error, turns int) {
	now := time.Now().UnixNano()
	last := d.contentFailureLoggedAt.Load()
	if now-last < int64(contentFailureLogInterval) {
		return
	}
	if !d.contentFailureLoggedAt.CompareAndSwap(last, now) {
		return
	}
	common.SysError(fmt.Sprintf("relayobserver: content append failed (turns=%d): %v", turns, err))
}

// retryPendingContent gives retained content a chance to make progress even
// when no later metadata event arrives to trigger a normal flush.
func (d *Dispatcher) retryPendingContent() {
	appends := d.pendingAppends
	d.pendingAppends = nil
	ctx, cancel := context.WithTimeout(d.stopCtxSnapshot(), d.cfg.WriteTimeout)
	d.persistContent(ctx, appends)
	cancel()
}

// flushPendingContentOnStop performs one final bulk attempt within the
// caller's shutdown context. Failure is not retried during shutdown: all
// references and reservations are released before the worker exits.
func (d *Dispatcher) flushPendingContentOnStop() {
	appends := d.pendingAppends
	d.pendingAppends = nil
	if len(appends) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(d.stopCtxSnapshot(), d.cfg.WriteTimeout)
	err := d.appendPendingContent(ctx, appends)
	cancel()
	if err == nil {
		d.completePendingContent(appends)
		return
	}
	d.logContentHealth(err, len(appends))
	d.dropPendingContent(appends)
}

func (d *Dispatcher) completePendingContent(appends []pendingContentAppend) {
	for i := range appends {
		if appends[i].retry {
			d.contentRetried.Add(1)
		}
		d.pendingContentCount.Add(-1)
		d.pendingContentBytes.Add(-appends[i].bytes)
		appends[i].input = ContentInput{}
	}
}

func (d *Dispatcher) dropPendingContent(appends []pendingContentAppend) {
	for i := range appends {
		if !appends[i].gapCounted {
			d.contentGaps.Add(1)
		}
		if !appends[i].degradationCounted {
			d.contentDropped.Add(1)
		}
		d.pendingContentCount.Add(-1)
		d.pendingContentBytes.Add(-appends[i].bytes)
		appends[i].input = ContentInput{}
	}
}

func (d *Dispatcher) releasePendingContent(appends []pendingContentAppend) {
	for i := range appends {
		d.pendingContentCount.Add(-1)
		d.pendingContentBytes.Add(-appends[i].bytes)
		appends[i].input = ContentInput{}
	}
}

// reservePendingContent measures the complete persistence input with the
// project JSON wrapper, then charges both worker-owned budgets. The worker is
// the only writer of these counters; atomics let Status read them safely.
func (d *Dispatcher) reservePendingContent(input ContentInput) (pendingContentAppend, bool) {
	encoded, err := common.Marshal(input)
	if err != nil {
		return pendingContentAppend{}, false
	}
	retainedBytes := int64(len(encoded))
	if retainedBytes < 1 {
		retainedBytes = 1
	}
	if d.pendingContentCount.Load() >= maxPendingAppendEvents {
		return pendingContentAppend{}, false
	}
	currentBytes := d.pendingContentBytes.Load()
	if currentBytes > d.cfg.PendingAppendBytes-retainedBytes {
		return pendingContentAppend{}, false
	}
	d.pendingContentCount.Add(1)
	d.pendingContentBytes.Add(retainedBytes)
	return pendingContentAppend{input: input, bytes: retainedBytes}, true
}

// planContent runs the worker-side content capture pipeline (T2.6) over one
// batch: every event that carries a parsed request is normalized, its
// session aliases are resolved in the worker, and the event's ContentState is
// backfilled so the metadata write carries the true outcome. It returns the
// content appends to persist after the metadata write succeeds. Strictly
// fail-open per event: a panicking or unknown request degrades that event to
// metadata-only (counted as a content gap) and never affects the batch, the
// store, or the request path.
func (d *Dispatcher) planContent(batch []queuedEvent) (appends []pendingContentAppend) {
	km := KeyMaterial{
		CurrentKey:      d.cfg.HMACKey,
		CurrentVersion:  d.cfg.HMACKeyVersion,
		PreviousKey:     d.cfg.PreviousHMACKey,
		PreviousVersion: d.cfg.PreviousHMACKeyVersion,
	}
	excluded := GetExcludedUsers(d.cfg)
	appends = make([]pendingContentAppend, 0, len(batch))
	defer func() {
		if recovered := recover(); recovered != nil {
			d.releasePendingContent(appends)
			appends = nil
			panic(recovered)
		}
	}()
	for i := range batch {
		qe := &batch[i]
		ev := qe.ev
		if ev == nil {
			continue
		}
		if len(excluded) > 0 && excluded[ev.UserID] {
			// Blacklisted user: record the metadata turn only. Content capture
			// is skipped entirely (no normalization, no identity resolution, no
			// content append), so no request bytes or canonical items are ever
			// retained for the excluded user.
			ev.ContentState = ContentStateMetadataOnly
			continue
		}
		plan := d.normalizeOne(ev)
		ev.ContentState = plan.state
		gapCounted := plan.state == ContentStateGap || plan.state == ContentStateMetadataOnly
		// Identity resolution is decoupled from content capture (T2
		// decoupling): a turn with a resolvable session identity is tracked
		// even when normalization produced no items, so session views see
		// every turn of an identity chain regardless of capture outcome.
		idRes, err := ResolveIdentity(ev.Identity, km)
		var aliases []Alias
		var previousAliases []Alias
		transient := false
		switch {
		case err == nil && idRes.Primary.Digest != "":
			aliases = append(aliases, idRes.Primary)
			aliases = append(aliases, idRes.Auxiliary...)
			if idRes.PreviousPrimary.Digest != "" {
				previousAliases = append(previousAliases, idRes.PreviousPrimary)
				previousAliases = append(previousAliases, idRes.PreviousAuxiliary...)
			}
		case plan.state == ContentStateMetadataOnly:
			// No resolvable session identity and no content to bind: a turn
			// with neither aliases nor items is a no-op. The event keeps its
			// normalized ContentState and the metadata turn stands alone.
			if gapCounted {
				d.contentGaps.Add(1)
			}
			continue
		default:
			// No resolvable session identity, but content was captured
			// (full or gap). Stateless traffic gets a per-turn transient
			// session so its content is persisted and reconstructable; the
			// deterministic turn row id keys the synthetic alias, so every
			// turn resolves to exactly its own transient session. A
			// non-HMAC identity error (or a keyless config that cannot even
			// mint the transient alias) still fails open to metadata-only.
			turnID := turnRowID(ev.NodeScope, ev.EventID)
			transientAlias, genErr := GenerateAlias(turnID.String(), SourceTransientTurn, ScopeUnknown, km)
			if genErr != nil {
				if gapCounted {
					d.contentGaps.Add(1)
				}
				continue
			}
			aliases = append(aliases, transientAlias)
			transient = true
		}
		input := ContentInput{
			NodeScope:       ev.NodeScope,
			UserID:          ev.UserID,
			ClientProfile:   ev.ClientProfile,
			Aliases:         aliases,
			PreviousAliases: previousAliases,
			TurnID:          turnRowID(ev.NodeScope, ev.EventID),
			ContentState:    plan.state,
			Transient:       transient,
			Items:           plan.items,
		}
		pending, reserved := d.reservePendingContent(input)
		degradationCounted := false
		if !reserved {
			// The canonical append cannot be retained safely. Backfill the turn
			// before WriteBatch and keep a smaller session-only append when it
			// fits; otherwise drop the append explicitly.
			ev.ContentState = ContentStateMetadataOnly
			input.ContentState = ContentStateMetadataOnly
			input.Items = nil
			gapCounted = true
			degradationCounted = true
			d.contentDropped.Add(1)
			pending, reserved = d.reservePendingContent(input)
		}
		if gapCounted {
			d.contentGaps.Add(1)
		}
		if !reserved {
			continue
		}
		pending.gapCounted = gapCounted
		pending.degradationCounted = degradationCounted
		appends = append(appends, pending)
	}
	return appends
}

// contentPlan is one event's normalization outcome inside the worker.
type contentPlan struct {
	state string
	items []CanonicalItem
}

// normalizeOne normalizes one event's parsed request with the observer's
// configured canonical capture cap. CaptureRelayFormat is the original client
// format paired with the retained request DTO; RelayFormat remains the final
// upstream format stored on the turn. Queue reservation remains admission-only:
// it bounds queued raw request bytes and never changes which evidence the
// semantic selector retains. A panic inside the normalizer is absorbed here
// (the normalizer also recovers internally), so the event degrades to
// metadata-only and the worker keeps running.
func (d *Dispatcher) normalizeOne(ev *Event) (plan contentPlan) {
	defer func() {
		if recover() != nil {
			plan = contentPlan{state: ContentStateMetadataOnly}
		}
	}()
	captureFormat := ev.CaptureRelayFormat
	if captureFormat == "" {
		captureFormat = ev.RelayFormat
	}
	res := NormalizeRequest(captureFormat, *ev.Request, NormalizeOptions{
		CaptureLimit:    d.cfg.MaxCaptureBytesPerTurn,
		MaxRequestBytes: d.cfg.MaxRequestBytes,
		HMACKey:         d.cfg.HMACKey,
	})
	return contentPlan{state: res.ContentState, items: res.Items}
}

// openCircuit marks the circuit open and starts the cooldown clock. The
// cooldown grows exponentially on every failure and caps at maxCooldown; the
// current deadline is stored so Status and the worker timer share one value.
// Every failure cycle bumps the circuit epoch so stale admission decisions
// made before the failure are dropped instead of written. Worker-only.
func (d *Dispatcher) openCircuit() {
	d.circuitState.Store(int32(circuitOpen))
	d.circuitEpoch.Add(1)
	cd := d.circuitCooldown
	d.circuitUntilNano.Store(d.clock.Now().Add(cd).UnixNano())
	d.circuitCooldown = cd * 2
	if d.circuitCooldown > maxCooldown {
		d.circuitCooldown = maxCooldown
	}
	// Circuit transitions are operator-visible events: a silently open
	// circuit looks identical to a healthy one in the status counters until
	// someone polls CircuitOpen. Log the transition so a store outage or code
	// bug is not mistaken for a quiet observer.
	common.SysError("relayobserver: circuit opened (cooldown " + cd.String() + ")")
}

// closeCircuit closes the circuit and resets the cooldown to its initial
// value; the next failure starts over from initialCooldown. Worker-only.
func (d *Dispatcher) closeCircuit() {
	d.circuitState.Store(int32(circuitClosed))
	d.circuitCooldown = initialCooldown
	common.SysLog("relayobserver: circuit closed")
}

// closeCircuitAfterDrainingBacklog drops every event still queued before the
// circuit closes: a failed batch opens the circuit, and backlog admitted
// before that failure must be dropped, never rescued into a post-recovery
// burst write by a successful probe. The circuit is open or probing while
// this runs, so TryEnqueue drops new events and the loop terminates. A
// current-epoch event cannot be queued here (no admission is decided while
// probing); the epoch check keeps the drop predicate identical to the closed
// enqueue case. Worker-only.
func (d *Dispatcher) closeCircuitAfterDrainingBacklog() {
	for {
		select {
		case qe := <-d.enqueue:
			d.releaseReservation(qe)
			if qe.epoch != d.circuitEpoch.Load() {
				d.droppedTotal.Add(1)
				continue
			}
			// Unreachable in practice: no admission is decided while the
			// circuit is probing, so every queued item predates the failure
			// cycle. Never write a backlog item; drop defensively.
			d.droppedTotal.Add(1)
		default:
			d.closeCircuit()
			return
		}
	}
}

// onWorkerPanic treats any worker panic as a failed write: the current batch
// is dropped and counted, the circuit opens, and the worker is restarted by
// run. Reservations were already released when each event was received. The
// recovered value is logged so a code bug (not just a store outage) leaves
// evidence; without it a crash-looping worker looks identical to a healthy
// circuit cycle in the status counters.
func (d *Dispatcher) onWorkerPanic(r any, batch []queuedEvent) {
	d.droppedTotal.Add(int64(len(batch)))
	common.SysError(fmt.Sprintf("relayobserver: worker panic: %v", r))
	d.openCircuit()
}

// releaseReservation releases the count and byte admission of one queued
// event; called exactly once per accepted event, when the worker receives it.
func (d *Dispatcher) releaseReservation(qe *queuedEvent) {
	d.pendingCount.Add(-1)
	d.pendingBytes.Add(-qe.reservation)
}

// reserveBytes takes reservation bytes out of the byte budget with a CAS
// loop; it returns false when the budget would overflow.
func (d *Dispatcher) reserveBytes(reservation int64) bool {
	for {
		cur := d.pendingBytes.Load()
		if cur > d.cfg.QueueBytes-reservation {
			return false
		}
		if d.pendingBytes.CompareAndSwap(cur, cur+reservation) {
			return true
		}
	}
}

func (d *Dispatcher) releaseBytes(reservation int64) {
	d.pendingBytes.Add(-reservation)
}

func (d *Dispatcher) stopDone() <-chan struct{} {
	d.stopMu.Lock()
	defer d.stopMu.Unlock()
	return d.stopCtx.Done()
}

func (d *Dispatcher) stopCtxSnapshot() context.Context {
	d.stopMu.Lock()
	defer d.stopMu.Unlock()
	return d.stopCtx
}

func (d *Dispatcher) stopCtxErr() error {
	d.stopMu.Lock()
	defer d.stopMu.Unlock()
	return d.stopCtx.Err()
}

func (d *Dispatcher) ipTrustValue() IPTrust {
	if p := d.ipTrust.Load(); p != nil {
		return *p
	}
	return IPTrustNone
}

// runRetention owns the retention worker goroutine: one pass every
// retentionInterval, stopped by the dispatcher stop signal. It shares the
// store, the stop context, and the clock with the write worker but is fully
// isolated from the write circuit.
func (d *Dispatcher) runRetention() {
	defer close(d.retentionDone)
	timer := d.clock.NewTimer(retentionInterval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C():
			d.retentionPass()
			timer.Reset(retentionInterval)
		case <-d.stopNotify:
			return
		case <-d.stopDone():
			return
		}
	}
}

// retentionPass runs one bounded retention pass: expired turns, sessions,
// orphan content, then a payload-free backlog inspection. A delete failure
// aborts later delete segments, but the independent inspection still runs so
// operators can see the remaining count and age. The one-minute scheduler is
// the only retry loop.
func (d *Dispatcher) retentionPass() {
	start := d.clock.Now()
	// Resolve retention days through the hot-reloadable runtime snapshot so an
	// operator can retune them via the DB option without a restart.
	tunable := GetRuntimeTunable(d.cfg)
	turnCutoff := start.Add(-retentionDays(tunable.RetentionTurnDays))
	contentCutoff := start.Add(-retentionDays(tunable.RetentionContentDays))

	var passErr error
	if err := d.retentionTurnsSegment(turnCutoff); err != nil {
		passErr = err
	}
	if passErr == nil {
		runtime.Gosched()
		if err := d.retentionSessionsSegment(turnCutoff); err != nil {
			passErr = err
		}
	}
	if passErr == nil {
		runtime.Gosched()
		if err := d.retentionOrphansSegment(contentCutoff); err != nil {
			passErr = err
		}
	}
	runtime.Gosched()
	if err := d.retentionBacklogSegment(turnCutoff, contentCutoff); err != nil && passErr == nil {
		passErr = err
	}
	if passErr != nil {
		d.failRetentionPass(passErr)
	}
	d.lastRetentionPass.Store(start.UnixNano())
}

// failRetentionPass counts and logs one aborted retention pass. The error
// comes from the retention store methods, which carry only redacted
// classifications, never DSNs or secrets.
func (d *Dispatcher) failRetentionPass(err error) {
	d.retentionFailures.Add(1)
	common.SysError(fmt.Sprintf("relayobserver: retention pass failed: %v", err))
}

// retentionSegmentCtx bounds one segment by the independent retention budget,
// inherited from the stop context so shutdown aborts the segment. The
// budget is deliberately separate from QueryTimeout: retention deletes at
// scale on its own goroutine and must not share the 500ms Root-query budget.
func (d *Dispatcher) retentionSegmentCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(d.stopCtxSnapshot(), d.cfg.RetentionTimeout)
}

// retentionTurnsSegment deletes expired turns within one segment budget: the
// listing and every deletion share the segment context, so a segment timeout
// stops mid-pass and the next scheduled pass picks up the rest.
func (d *Dispatcher) retentionTurnsSegment(cutoff time.Time) error {
	ctx, cancel := d.retentionSegmentCtx()
	defer cancel()
	refs, err := d.retention.ListExpiredTurns(ctx, cutoff, retentionMaxTurnsPerPass)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleted, err := d.retention.DeleteTurnRetention(ctx, ref.TurnID)
		if err != nil {
			return err
		}
		if deleted {
			d.retentionTurnsDeleted.Add(1)
		}
	}
	return nil
}

// retentionSessionsSegment deletes expired sessions within one segment
// budget, mirroring the turns segment.
func (d *Dispatcher) retentionSessionsSegment(cutoff time.Time) error {
	ctx, cancel := d.retentionSegmentCtx()
	defer cancel()
	ids, err := d.retention.ListExpiredSessions(ctx, cutoff, retentionMaxSessionsPerPass)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleted, err := d.retention.DeleteSessionRetention(ctx, id, cutoff)
		if err != nil {
			return err
		}
		if deleted {
			d.retentionSessionsDeleted.Add(1)
		}
	}
	return nil
}

// retentionOrphansSegment deletes orphan content past its grace period within
// one segment budget.
func (d *Dispatcher) retentionOrphansSegment(cutoff time.Time) error {
	ctx, cancel := d.retentionSegmentCtx()
	defer cancel()
	n, err := d.retention.DeleteOrphanContent(ctx, cutoff, retentionMaxOrphansPerPass)
	if err != nil {
		return err
	}
	d.retentionObjectsDeleted.Add(int64(n))
	return nil
}

// retentionBacklogSegment refreshes bounded in-memory backlog signals. The
// status endpoint only reads these atomics and never queries PostgreSQL.
func (d *Dispatcher) retentionBacklogSegment(turnCutoff, contentCutoff time.Time) error {
	ctx, cancel := d.retentionSegmentCtx()
	defer cancel()
	backlog, err := d.retention.InspectRetentionBacklog(ctx, turnCutoff, contentCutoff, retentionBacklogInspectLimit)
	if err != nil {
		return err
	}
	d.retentionTurnsPending.Store(backlog.Turns)
	d.retentionSessionsPending.Store(backlog.Sessions)
	d.retentionObjectsPending.Store(backlog.Objects)
	d.retentionBacklogTruncated.Store(backlog.Truncated)
	if backlog.Oldest.IsZero() {
		d.retentionBacklogOldest.Store(0)
	} else {
		d.retentionBacklogOldest.Store(backlog.Oldest.UnixNano())
	}
	return nil
}

// retentionDays converts a day count to a duration.
func retentionDays(days int) time.Duration {
	return time.Duration(days) * 24 * time.Hour
}
