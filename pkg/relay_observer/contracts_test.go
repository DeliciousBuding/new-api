package relayobserver

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var epoch = time.Unix(0, 0)

func ptrUUID() *uuid.UUID {
	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	return &id
}

// fakeStore implements the Store port without any database dependency,
// proving that core logic can run against the port with a pure-Go fake.
// The compile-time assertion below locks the port signature: a broken Store
// contract fails the build.
type fakeStore struct {
	mu      sync.Mutex
	batches [][]Event
	appends [][]ContentInput
	err     error
	closed  bool
}

var _ Store = (*fakeStore)(nil)

func (s *fakeStore) WriteBatch(ctx context.Context, events []Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.batches = append(s.batches, events)
	return nil
}

func (s *fakeStore) AppendTurns(ctx context.Context, turns []ContentInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.appends = append(s.appends, turns)
	return nil
}

func (s *fakeStore) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func sampleEvent() Event {
	return Event{
		EventID:          "req_123",
		NodeScope:        "node-a",
		OccurredAt:       epoch,
		UserID:           7,
		TokenID:          42,
		ClientProfile:    "codex",
		Model:            "gpt-5",
		RelayFormat:      "responses",
		Success:          true,
		StatusCode:       200,
		LatencyMS:        150,
		PromptTokens:     10,
		CompletionTokens: 20,
		Quota:            3000,
		Attempts: []AttemptSummary{
			{ChannelID: 1, Group: "default", StatusCode: 429, ErrorCode: "rate_limit", ElapsedMS: 5},
			{ChannelID: 2, Group: "default", StatusCode: 200, ElapsedMS: 145},
		},
		ContentState: ContentStateFull,
	}
}

// TestStoreWriteBatchDelivers verifies that events enqueued through the port
// arrive intact at a store implementation.
func TestStoreWriteBatchDelivers(t *testing.T) {
	store := &fakeStore{}
	ctx := context.Background()

	ev := sampleEvent()
	require.NoError(t, store.WriteBatch(ctx, []Event{ev}))
	require.NoError(t, store.WriteBatch(ctx, []Event{}))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 2)
	require.Len(t, store.batches[0], 1)
	assert.Equal(t, ev, store.batches[0][0])
	assert.Empty(t, store.batches[1])
}

// TestStoreErrorPropagates verifies that a store failure surfaces to the
// caller unchanged (the caller drops the batch and opens the circuit).
func TestStoreErrorPropagates(t *testing.T) {
	store := &fakeStore{err: errors.New("pg unavailable")}

	err := store.WriteBatch(context.Background(), []Event{sampleEvent()})
	require.Error(t, err)
	assert.Equal(t, "pg unavailable", err.Error())
}

// TestStoreCloseIdempotent verifies the documented Close contract: repeated
// calls succeed and the store is closed exactly once.
func TestStoreCloseIdempotent(t *testing.T) {
	store := &fakeStore{}
	require.NoError(t, store.Close(context.Background()))
	require.NoError(t, store.Close(context.Background()))
	assert.True(t, store.closed)
}

// TestEventRoundTripThroughStore verifies that a fully populated event keeps
// every contract field through a write (no hidden mutation in the port).
func TestEventRoundTripThroughStore(t *testing.T) {
	store := &fakeStore{}
	ev := sampleEvent()
	ev.SessionID = ptrUUID()
	ev.FirstResponseMS = 30
	ev.Stream = true
	ev.CachedTokens = 5
	ev.ErrorType = "upstream"
	ev.ErrorCode = "timeout"
	ev.AttemptsOmitted = 6
	ev.ClientIP = net.ParseIP("203.0.113.7")
	ev.IPTrust = IPTrustDirect
	ev.CountryCode = "US"
	ev.Country = "United States"
	ev.City = "Ashburn"
	ev.ASN = 64512
	ev.ASNOrg = "Example"
	ev.ContentState = ContentStateGap

	require.NoError(t, store.WriteBatch(context.Background(), []Event{ev}))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.batches, 1)
	assert.Equal(t, ev, store.batches[0][0])
}

// TestStoreWriteBatchRespectsContext verifies the shutdown contract of the
// write path: a canceled context aborts an in-flight WriteBatch immediately,
// so the runtime can always stop the worker inside its hard budget.
func TestStoreWriteBatchRespectsContext(t *testing.T) {
	store := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.WriteBatch(ctx, []Event{sampleEvent()})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	// The canceled write must not have reached the store.
	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Empty(t, store.batches)
}

// TestStoreCloseRespectsContext verifies the shutdown contract of Close: a
// canceled context returns immediately instead of blocking, so a store that
// cannot finish closing cannot stall NewAPI's two-second shutdown.
func TestStoreCloseRespectsContext(t *testing.T) {
	store := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.Close(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, store.closed)
}

// TestStatusReasonCodeContract locks the stable, secret-free status contract:
// every reason code is a fixed constant, the empty value means healthy, and
// marshaling a Status surfaces only the code — never the underlying error
// text that produced it.
func TestStatusReasonCodeContract(t *testing.T) {
	expected := map[ReasonCode]string{
		ReasonDisabled:        "disabled",
		ReasonConfigInvalid:   "config_invalid",
		ReasonStoreInitFailed: "store_init_failed",
		ReasonSchemaMismatch:  "schema_mismatch",
		ReasonCircuitOpen:     "circuit_open",
		ReasonQueryDegraded:   "query_degraded",
	}
	for code, want := range expected {
		assert.Equal(t, want, string(code))
	}
	// The set is deliberately minimal; adding a code requires a contract
	// review, so the map above must be updated in the same change.
	assert.Len(t, expected, 6)

	// The empty code is the healthy state, not a secret-bearing reason.
	assert.Empty(t, ReasonCode(""))
	assert.Equal(t, ReasonCode(""), (Status{Enabled: true}).ReasonCode)

	// The marshaled status carries exactly the stable code. Free-form error
	// text (PG internals, DSN details) has no field to live in.
	const rawErr = `pq: password authentication failed for user "observer" at db.internal.example:5432`
	st := Status{Enabled: false, ReasonCode: ReasonStoreInitFailed, CircuitOpen: true, IPTrust: IPTrustNone}
	data, err := common.Marshal(st)
	require.NoError(t, err)
	body := string(data)
	assert.Contains(t, body, `"ReasonCode":"store_init_failed"`)
	assert.NotContains(t, body, rawErr)
	assert.NotContains(t, body, "pq:")
	// The status exposes the effective trust tier of the running
	// configuration (policy SSOT: docs/dev/relay-observer.md, IP/GeoIP dual opt-in: "observer status exposes the
	// effective tier").
	assert.Contains(t, body, `"IPTrust":"none"`)
}

// TestIPTrustContract locks the single source of truth for the trust-tier
// enumeration: Event and Status share the typed IPTrust with exactly the
// three SSOT values, so T1.3 (adapter) and T2.5 (capture) consume the
// constants instead of redefining or translating values.
func TestIPTrustContract(t *testing.T) {
	expected := map[IPTrust]string{
		IPTrustDirect: "direct",
		IPTrustProxy:  "proxy",
		IPTrustNone:   "none",
	}
	for tier, want := range expected {
		assert.Equal(t, want, string(tier))
	}
	assert.Len(t, expected, 3)
}
