package relayobserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the T5.1 retention worker inside the dispatcher: the
// six-hour cadence, the turns -> sessions -> orphans pass order, the bounded
// limits, the per-segment timeout budget, failure counting without a tight
// retry loop, the stop contract, and the Status counters. The worker runs
// against retentionScriptedStore, an in-memory RetentionStore + Store
// stand-in; the fake clock drives the six-hour timer exactly like the write
// worker tests drive the flush timer.

// retentionScriptedStore implements Store and RetentionStore for the
// retention worker tests: it records every retention call with its cutoff,
// limit, and segment context deadline, and lets tests script lists and
// failures.
type retentionScriptedStore struct {
	mu sync.Mutex

	turnRefs   []TurnRetentionRef
	sessionIDs []uuid.UUID
	orphans    int
	backlog    RetentionBacklog

	backlogErr       error
	listTurnsErr     error
	listSessionsErr  error
	deleteTurnErr    map[uuid.UUID]error
	deleteSessionErr error
	orphanErr        error

	calls     []string
	limits    []int
	cutoffs   []time.Time
	deadlines []time.Time
}

func (s *retentionScriptedStore) WriteBatch(ctx context.Context, events []Event) error { return nil }
func (s *retentionScriptedStore) Close(ctx context.Context) error                      { return nil }

// AppendTurns completes the T2.6-extended Store port; retention tests never
// exercise content appends.
func (s *retentionScriptedStore) AppendTurns(ctx context.Context, turns []ContentInput) error {
	return nil
}

func (s *retentionScriptedStore) InspectRetentionBacklog(ctx context.Context, turnCutoff, contentCutoff time.Time, limit int) (RetentionBacklog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("inspectBacklog", ctx, turnCutoff, limit)
	if s.backlogErr != nil {
		return RetentionBacklog{}, s.backlogErr
	}
	return s.backlog, nil
}

// record captures one retention call and the timeout budget of its segment
// context; the caller holds the mutex.
func (s *retentionScriptedStore) record(call string, ctx context.Context, cutoff time.Time, limit int) {
	s.calls = append(s.calls, call)
	s.cutoffs = append(s.cutoffs, cutoff)
	s.limits = append(s.limits, limit)
	if deadline, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, deadline)
	}
}

func (s *retentionScriptedStore) ListExpiredTurns(ctx context.Context, cutoff time.Time, limit int) ([]TurnRetentionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("listTurns", ctx, cutoff, limit)
	if s.listTurnsErr != nil {
		return nil, s.listTurnsErr
	}
	return append([]TurnRetentionRef(nil), s.turnRefs...), nil
}

func (s *retentionScriptedStore) ListExpiredSessions(ctx context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("listSessions", ctx, cutoff, limit)
	if s.listSessionsErr != nil {
		return nil, s.listSessionsErr
	}
	return append([]uuid.UUID(nil), s.sessionIDs...), nil
}

func (s *retentionScriptedStore) DeleteTurnRetention(ctx context.Context, turnID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("deleteTurn", ctx, time.Time{}, 0)
	if err := s.deleteTurnErr[turnID]; err != nil {
		return false, err
	}
	return true, nil
}

func (s *retentionScriptedStore) DeleteSessionRetention(ctx context.Context, sessionID uuid.UUID, cutoff time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("deleteSession", ctx, time.Time{}, 0)
	if s.deleteSessionErr != nil {
		return false, s.deleteSessionErr
	}
	return true, nil
}

func (s *retentionScriptedStore) DeleteOrphanContent(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("deleteOrphans", ctx, cutoff, limit)
	if s.orphanErr != nil {
		return 0, s.orphanErr
	}
	return s.orphans, nil
}

// callSnapshot returns a copy of the recorded calls.
func (s *retentionScriptedStore) callSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *retentionScriptedStore) limitSnapshot() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.limits...)
}

func (s *retentionScriptedStore) deadlineSnapshot() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.deadlines...)
}

// newRetentionDispatcher builds a started dispatcher over the scripted store
// with a fake clock and a short segment timeout, and stops it on cleanup.
func newRetentionDispatcher(t *testing.T, st *retentionScriptedStore) (*Dispatcher, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	cfg := DefaultConfig()
	cfg.RetentionTimeout = 200 * time.Millisecond
	d := NewDispatcher(cfg, st)
	d.clock = clk
	d.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		d.Stop(ctx)
	})
	return d, clk
}

// fireRetentionTimer finds the one-minute retention timer and fires it.
func fireRetentionTimer(t *testing.T, clk *fakeClock) {
	t.Helper()
	var timer *fakeTimer
	require.Eventually(t, func() bool {
		for _, tm := range clk.snapshot() {
			if tm.duration() == retentionInterval {
				timer = tm
				return true
			}
		}
		return false
	}, 2*time.Second, time.Millisecond)
	timer.fire()
}

// TestRetentionWorkerPassOrderAndCounters locks one full pass: the
// turns -> sessions -> orphans order, per-item deletion calls, the Status
// counters, and LastRetentionPass at the pass time.
func TestRetentionWorkerPassOrderAndCounters(t *testing.T) {
	st := &retentionScriptedStore{
		turnRefs: []TurnRetentionRef{
			{TurnID: turnA, SessionID: &sidA},
			{TurnID: turnB},
		},
		sessionIDs: []uuid.UUID{sidB},
		orphans:    5,
	}
	d, clk := newRetentionDispatcher(t, st)

	// No pass before the first retention interval.
	assert.True(t, d.Status().LastRetentionPass.IsZero())
	assert.Zero(t, d.Status().RetentionTurnsDeleted)
	assert.Zero(t, d.Status().RetentionFailures)

	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)

	want := []string{"listTurns", "deleteTurn", "deleteTurn", "listSessions", "deleteSession", "deleteOrphans", "inspectBacklog"}
	require.Eventually(t, func() bool {
		calls := st.callSnapshot()
		if len(calls) != len(want) {
			return false
		}
		for i := range want {
			if calls[i] != want[i] {
				return false
			}
		}
		return true
	}, 2*time.Second, time.Millisecond)

	status := d.Status()
	assert.Equal(t, int64(2), status.RetentionTurnsDeleted)
	assert.Equal(t, int64(1), status.RetentionSessionsDeleted)
	assert.Equal(t, int64(5), status.RetentionObjectsDeleted)
	assert.Zero(t, status.RetentionFailures)
	assert.Equal(t, clk.Now(), status.LastRetentionPass)
}

// TestRetentionWorkerBounds locks the SSOT limits: at most 1000 turns, 100
// sessions, and 1000 orphan objects per pass.
func TestRetentionWorkerBounds(t *testing.T) {
	st := &retentionScriptedStore{}
	_, clk := newRetentionDispatcher(t, st)

	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)

	require.Eventually(t, func() bool { return len(st.callSnapshot()) == 4 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, []int{retentionMaxTurnsPerPass, retentionMaxSessionsPerPass, retentionMaxOrphansPerPass, retentionBacklogInspectLimit}, st.limitSnapshot())
}

// TestRetentionWorkerCutoffs locks the cutoff derivation: turns expire after
// the configured turn days, orphan content after the configured content days.
func TestRetentionWorkerCutoffs(t *testing.T) {
	st := &retentionScriptedStore{}
	_, clk := newRetentionDispatcher(t, st)

	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)

	require.Eventually(t, func() bool { return len(st.callSnapshot()) == 4 }, 2*time.Second, time.Millisecond)
	st.mu.Lock()
	defer st.mu.Unlock()
	now := clk.Now()
	assert.Equal(t, now.Add(-retentionDays(DefaultRetentionTurnDays)), st.cutoffs[0])
	assert.Equal(t, now.Add(-retentionDays(DefaultRetentionTurnDays)), st.cutoffs[1])
	assert.Equal(t, now.Add(-retentionDays(DefaultRetentionContentDays)), st.cutoffs[2])
}

// TestRetentionWorkerSegmentBudget locks the per-segment short timeout: every
// segment context carries the configured query timeout deadline, so a slow or
// hung segment cannot hold the pool past the budget.
func TestRetentionWorkerSegmentBudget(t *testing.T) {
	st := &retentionScriptedStore{turnRefs: []TurnRetentionRef{{TurnID: turnA}}}
	_, clk := newRetentionDispatcher(t, st)

	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)

	// listTurns + deleteTurn + listSessions + deleteOrphans + inspection.
	require.Eventually(t, func() bool { return len(st.callSnapshot()) == 5 }, 2*time.Second, time.Millisecond)
	before := time.Now()
	for _, deadline := range st.deadlineSnapshot() {
		assert.False(t, deadline.IsZero(), "every segment context must carry a timeout deadline")
		assert.WithinDuration(t, before.Add(200*time.Millisecond), deadline, 300*time.Millisecond)
	}
}

// TestRetentionWorkerListFailureAborts locks the failure contract: a segment
// failure counts and aborts the pass — later segments do not run — and the
// next scheduled pass retries instead of looping.
func TestRetentionWorkerListFailureAborts(t *testing.T) {
	sentinel := errors.New("pg unavailable")
	st := &retentionScriptedStore{listTurnsErr: sentinel}
	d, clk := newRetentionDispatcher(t, st)

	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)

	require.Eventually(t, func() bool { return d.Status().RetentionFailures == 1 }, 2*time.Second, time.Millisecond)
	calls := st.callSnapshot()
	assert.Equal(t, []string{"listTurns", "inspectBacklog"}, calls, "a failed delete segment must still refresh backlog signals")
	require.Eventually(t, func() bool {
		return !d.Status().LastRetentionPass.IsZero()
	}, 2*time.Second, time.Millisecond)

	// The next six-hour cycle retries the pass.
	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)
	require.Eventually(t, func() bool { return len(st.callSnapshot()) == 4 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return d.Status().RetentionFailures == 2 }, 2*time.Second, time.Millisecond)
}

// TestRetentionWorkerDeleteFailureMidSegment locks the mid-segment failure:
// already-deleted turns are counted, the segment stops at the failure, and
// the pass is recorded as failed.
func TestRetentionWorkerDeleteFailureMidSegment(t *testing.T) {
	sentinel := errors.New("delete failed")
	st := &retentionScriptedStore{
		turnRefs:      []TurnRetentionRef{{TurnID: turnA}, {TurnID: turnB}},
		deleteTurnErr: map[uuid.UUID]error{turnB: sentinel},
	}
	d, clk := newRetentionDispatcher(t, st)

	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)

	require.Eventually(t, func() bool { return d.Status().RetentionFailures == 1 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, int64(1), d.Status().RetentionTurnsDeleted, "deletions before the failure are counted")
	assert.Zero(t, d.Status().RetentionSessionsDeleted)
	calls := st.callSnapshot()
	assert.Equal(t, []string{"listTurns", "deleteTurn", "deleteTurn", "inspectBacklog"}, calls, "the pass aborts deletes but still refreshes backlog")
}

// TestRetentionWorkerSessionsAndOrphansFailures locks the failure contract of
// the later segments: a sessions failure skips the orphans segment; an
// orphans failure still counts the pass as failed.
func TestRetentionWorkerSessionsAndOrphansFailures(t *testing.T) {
	sentinel := errors.New("session delete failed")
	st := &retentionScriptedStore{sessionIDs: []uuid.UUID{sidA}, deleteSessionErr: sentinel}
	d, clk := newRetentionDispatcher(t, st)

	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)
	require.Eventually(t, func() bool { return d.Status().RetentionFailures == 1 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, []string{"listTurns", "listSessions", "deleteSession", "inspectBacklog"}, st.callSnapshot())

	st2 := &retentionScriptedStore{orphanErr: sentinel}
	d2, clk2 := newRetentionDispatcher(t, st2)
	clk2.advance(retentionInterval)
	fireRetentionTimer(t, clk2)
	require.Eventually(t, func() bool { return d2.Status().RetentionFailures == 1 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, []string{"listTurns", "listSessions", "deleteOrphans", "inspectBacklog"}, st2.callSnapshot())
}

// TestRetentionWorkerStopsWithDispatcher locks the stop contract: Stop makes
// the retention worker exit (no further passes) and the worker goroutine
// completes.
func TestRetentionWorkerStopsWithDispatcher(t *testing.T) {
	st := &retentionScriptedStore{}
	d, clk := newRetentionDispatcher(t, st)

	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)
	require.Eventually(t, func() bool { return len(st.callSnapshot()) == 4 }, 2*time.Second, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Stop(ctx)

	select {
	case <-d.retentionDone:
	default:
		t.Fatal("retention worker must exit on Stop")
	}
	n := len(st.callSnapshot())
	clk.advance(retentionInterval)
	fireRetentionTimer(t, clk)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, n, len(st.callSnapshot()), "no retention pass may run after Stop")
}

// TestRetentionWorkerNotArmedWithoutRetentionStore locks the arming rule: a
// store that does not implement RetentionStore keeps the retention worker
// off, so fakes and degraded adapters never run the pass.
func TestRetentionWorkerNotArmedWithoutRetentionStore(t *testing.T) {
	store := &fakeStore{}
	d := NewDispatcher(DefaultConfig(), store)
	assert.Nil(t, d.retention)
	d.Start()
	assert.Nil(t, d.retention)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Stop(ctx)
}
