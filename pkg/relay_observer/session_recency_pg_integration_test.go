//go:build relay_observer_pg_integration

package relayobserver

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file locks the session-recency billing/retention safety invariant: the session
// recency bump in WriteBatch must be monotonic. The invariant is
// `UPDATE observer_sessions SET last_seen = GREATEST(last_seen, $1)`. If a
// future refactor regresses it to a plain assignment (or an out-of-order
// batch rewrites last_seen backward), a live turn's session can be listed as
// expired and its not-yet-expired turns deleted by retention. These tests
// guard the exact data-integrity path.

// TestIntegrationSessionRecencyNeverRegresses locks the monotonic bump: a
// turn whose occurred_at is older than the session's current last_seen must
// not move last_seen backward, and a newer turn must move it forward.
func TestIntegrationSessionRecencyNeverRegresses(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	db := openFixturePool(t, dsn)
	scope := uniqueScope("recency")
	base := epoch.Add(1000 * time.Second)
	sid := insertSessionRow(t, db, scope, base) // last_seen = base
	sessionID := uuid.MustParse(sid)

	// A turn older than last_seen must not regress it.
	older := sampleEvent()
	older.NodeScope = scope
	older.EventID = "older"
	older.SessionID = &sessionID
	older.OccurredAt = base.Add(-500 * time.Second)
	require.NoError(t, store.WriteBatch(context.Background(), []Event{older}))

	var lastSeen time.Time
	require.NoError(t, db.QueryRow("SELECT last_seen FROM observer_sessions WHERE id = $1", sid).Scan(&lastSeen))
	assert.Equal(t, base, lastSeen, "GREATEST must not regress last_seen to an older turn")

	// A turn newer than last_seen must advance it.
	newer := sampleEvent()
	newer.NodeScope = scope
	newer.EventID = "newer"
	newer.SessionID = &sessionID
	newer.OccurredAt = base.Add(500 * time.Second)
	require.NoError(t, store.WriteBatch(context.Background(), []Event{newer}))

	require.NoError(t, db.QueryRow("SELECT last_seen FROM observer_sessions WHERE id = $1", sid).Scan(&lastSeen))
	assert.Equal(t, base.Add(500*time.Second), lastSeen, "a newer turn must advance last_seen")
}

// TestIntegrationSessionRecencyOutOfOrderBatch locks the out-of-order batch
// case: regardless of the order the turns arrive in one WriteBatch, the
// session's last_seen must equal the maximum occurred_at, never the last
// turn processed.
func TestIntegrationSessionRecencyOutOfOrderBatch(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	db := openFixturePool(t, dsn)
	scope := uniqueScope("ooo")
	base := epoch.Add(10_000 * time.Second)
	sid := insertSessionRow(t, db, scope, base)
	sessionID := uuid.MustParse(sid)

	// Out-of-order occurred_at values; the max is 400s after base.
	offsets := []int64{50, 400, 10, 200}
	events := make([]Event, len(offsets))
	for i, sec := range offsets {
		ev := sampleEvent()
		ev.NodeScope = scope
		ev.EventID = "ooo-" + string(rune('a'+i))
		ev.SessionID = &sessionID
		ev.OccurredAt = base.Add(time.Duration(sec) * time.Second)
		events[i] = ev
	}
	require.NoError(t, store.WriteBatch(context.Background(), events))

	var lastSeen time.Time
	require.NoError(t, db.QueryRow("SELECT last_seen FROM observer_sessions WHERE id = $1", sid).Scan(&lastSeen))
	assert.Equal(t, base.Add(400*time.Second), lastSeen, "last_seen must equal the max occurred_at regardless of batch order")
}

// TestIntegrationSessionRecencyKeepsLiveTurnsOutOfExpiry locks the retention
// boundary: a session whose last_seen is stale but that received a live turn
// must have its last_seen bumped past the cutoff, so ListExpiredSessions
// never lists it. A monotonicity regression would list it and retention would
// delete live rows.
func TestIntegrationSessionRecencyKeepsLiveTurnsOutOfExpiry(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	db := openFixturePool(t, dsn)
	scope := uniqueScope("expiry")
	stale := time.Now().Add(-30 * 24 * time.Hour)
	sid := insertSessionRow(t, db, scope, stale) // last_seen is 30 days old
	sessionID := uuid.MustParse(sid)

	// A live turn must bump last_seen to now.
	live := sampleEvent()
	live.NodeScope = scope
	live.EventID = "live"
	live.SessionID = &sessionID
	live.OccurredAt = time.Now()
	require.NoError(t, store.WriteBatch(context.Background(), []Event{live}))

	cutoff := time.Now().Add(-24 * time.Hour)
	retention, ok := store.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	expired, err := retention.ListExpiredSessions(context.Background(), cutoff, 100)
	require.NoError(t, err)
	assert.NotContains(t, expired, sessionID, "a session with a live turn must not be listed as expired")
}
