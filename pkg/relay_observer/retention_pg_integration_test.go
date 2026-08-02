//go:build relay_observer_pg_integration

package relayobserver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file runs the T5.1 retention integration suite against the disposable
// local PostgreSQL (see store_pg_integration_test.go for the DSN guard).
// Fixtures are written with direct SQL so the tests control the timestamps
// exactly; every assertion targets the retention contract: expiry by
// occurred_at / last_seen / created_at, reference-safe orphan cleanup, the
// per-pass bounds, idempotence across passes, and the indexed predicates.

// Fixed content digests (64-hex) shared by fixtures; the values are opaque
// bytes to the store, so fixed hex keeps the tests deterministic.
const (
	digestAHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestBHex = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// digestA is the raw bytes of digestAHex.
var digestA = func() []byte {
	b, err := hex.DecodeString(digestAHex)
	if err != nil {
		panic(err)
	}
	return b
}()

// digestB is the raw bytes of digestBHex.
var digestB = func() []byte {
	b, err := hex.DecodeString(digestBHex)
	if err != nil {
		panic(err)
	}
	return b
}()

// insertTurnRow writes one observer_turns row with only the columns the
// retention pass reads; the rest stay NULL. Returns the row id.
func insertTurnRow(t *testing.T, db *sql.DB, scope, eventID string, occurredAt time.Time, sessionID *string) string {
	t.Helper()
	id := uuid.New().String()
	_, err := db.Exec(`INSERT INTO observer_turns (id, node_scope, event_id, occurred_at, session_id) VALUES ($1, $2, $3, $4, $5)`,
		id, scope, eventID, occurredAt, sessionID)
	require.NoError(t, err)
	return id
}

// insertSessionRow writes one observer_sessions row and returns its id.
func insertSessionRow(t *testing.T, db *sql.DB, scope string, lastSeen time.Time) string {
	t.Helper()
	id := uuid.New().String()
	_, err := db.Exec(`INSERT INTO observer_sessions (id, node_scope, user_id, client_family, first_seen, last_seen, turn_count, gap_count)
		VALUES ($1, $2, 0, '', $3, $3, 0, 0)`, id, scope, lastSeen)
	require.NoError(t, err)
	return id
}

// insertAliasRow binds one alias to a session.
func insertAliasRow(t *testing.T, db *sql.DB, scope, sessionID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO observer_session_aliases (node_scope, user_id, key_version, provider, source, alias_digest, session_id, first_seen, last_seen)
		VALUES ($1, 0, 1, 'codex', 'header', $2, $3, now(), now())`, scope, digestA, sessionID)
	require.NoError(t, err)
}

// insertObjectRow writes one content object with an explicit created_at.
func insertObjectRow(t *testing.T, db *sql.DB, sessionID, digestHex string, created time.Time) {
	t.Helper()
	raw, err := hex.DecodeString(digestHex)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO observer_content_objects (session_id, item_digest, kind, role, codec, payload, logical_bytes, stored_bytes, truncated, created_at)
		VALUES ($1, $2, 'text', 'user', 'zstd', $3, 10, 10, false, $4)`, sessionID, raw, []byte("payload"), created)
	require.NoError(t, err)
}

// insertContextRow writes one context row whose digest list is the JSON
// array of the given hex digests.
func insertContextRow(t *testing.T, db *sql.DB, sessionID, turnID string, digestsJSON string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`INSERT INTO observer_contexts (session_id, turn_id, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes)
		VALUES ($1, $2, 0, 0, 0, $4, $3::jsonb, 10) RETURNING id`, sessionID, turnID, digestsJSON, 1).Scan(&id)
	require.NoError(t, err)
	return id
}

// insertHeadRow writes a session head pointing at a context.
func insertHeadRow(t *testing.T, db *sql.DB, sessionID string, contextID int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO observer_session_heads (session_id, context_id, checkpoint_id, group_ordinal)
		VALUES ($1, $2, $2, 0)`, sessionID, contextID)
	require.NoError(t, err)
}

// digestsJSON renders the JSON array of digest hex values.
func digestsJSON(digests ...string) string {
	quoted := make([]string, len(digests))
	for i, d := range digests {
		quoted[i] = `"` + d + `"`
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// uniqueScope returns a test-unique node scope so repeated runs never
// collide on the (node_scope, event_id) unique key.
func uniqueScope(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// openRetentionStores opens the verify store and its retention surface: the
// same adapter, asserted at compile time to implement RetentionStore.
func openRetentionStores(t *testing.T, dsn string) (Store, RetentionStore) {
	t.Helper()
	s := openVerifyStore(t, dsn)
	rs, ok := s.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	return s, rs
}

// TestIntegrationMigrationV2Lifecycle covers the versioned migration path on
// the live database: an empty schema bootstraps straight to v2, a v1 schema
// upgrades in place, repeated bootstrap is idempotent, verify rejects an
// unknown version, and the v2 column is present with a non-null value.
func TestIntegrationMigrationV2Lifecycle(t *testing.T) {
	dsn := integrationDSN(t)
	db := openFixturePool(t, dsn)
	cleanupObserverSchema(t, db)
	ctx := context.Background()

	// Build a complete v1 schema by hand (001 only), then verify accepts it
	// as upgrade-pending.
	v1SQL, err := migrationsFS.ReadFile(observerMigrations[0])
	require.NoError(t, err)
	_, err = db.Exec(string(v1SQL))
	require.NoError(t, err)
	store, err := OpenPGStore(ctx, dsn, SchemaModeVerify)
	require.NoError(t, err, "verify must accept a complete v1 schema")
	require.NoError(t, store.Close(ctx))

	var colExists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'observer_content_objects' AND column_name = 'created_at')`).Scan(&colExists))
	assert.False(t, colExists, "a hand-built v1 schema must not have the v2 column yet")

	// Bootstrap upgrades v1 -> v2 inside its transaction.
	store, err = OpenPGStore(ctx, dsn, SchemaModeBootstrap)
	require.NoError(t, err, "bootstrap must upgrade a complete v1 schema to v2")
	require.NoError(t, store.Close(ctx))
	assertSchemaVersions(t, db, []int{1, 2})
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'observer_content_objects' AND column_name = 'created_at')`).Scan(&colExists))
	assert.True(t, colExists, "bootstrap must add the v2 created_at column")
	var notNull int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_content_objects WHERE created_at IS NULL`).Scan(&notNull))
	assert.Zero(t, notNull, "created_at is NOT NULL")

	// Repeated bootstrap is an idempotent no-op on the current schema.
	store, err = OpenPGStore(ctx, dsn, SchemaModeBootstrap)
	require.NoError(t, err, "repeated bootstrap must be idempotent")
	require.NoError(t, store.Close(ctx))
	assertSchemaVersions(t, db, []int{1, 2})

	// Verify rejects an unknown version row.
	_, err = db.Exec("INSERT INTO observer_schema_versions (version, applied_at) VALUES (99, now())")
	require.NoError(t, err)
	store, err = OpenPGStore(ctx, dsn, SchemaModeVerify)
	require.Error(t, err, "verify must reject an unknown schema version")
	assert.Nil(t, store)
	_, err = db.Exec("DELETE FROM observer_schema_versions WHERE version = 99")
	require.NoError(t, err)

	// A fully empty schema bootstraps straight to the current version.
	cleanupObserverSchema(t, db)
	store, err = OpenPGStore(ctx, dsn, SchemaModeBootstrap)
	require.NoError(t, err, "bootstrap must create the current schema on an empty database")
	require.NoError(t, store.Close(ctx))
	assertSchemaVersions(t, db, []int{1, 2})
}

// assertSchemaVersions asserts the exact version list of the live schema.
func assertSchemaVersions(t *testing.T, db *sql.DB, want []int) {
	t.Helper()
	rows, err := db.Query("SELECT version FROM observer_schema_versions ORDER BY version")
	require.NoError(t, err)
	defer rows.Close()
	var got []int
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		got = append(got, v)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, want, got)
}

// TestIntegrationRetentionExpiredTurnDeletes covers the expired-turn path on
// live data: listing picks only expired turns, and DeleteTurnRetention
// removes the turn, its context row, and clears a head that pointed at it,
// leaving fresh turns untouched.
func TestIntegrationRetentionExpiredTurnDeletes(t *testing.T) {
	dsn := integrationDSN(t)
	s := resetObserverSchema(t, dsn)
	defer s.Close(context.Background())
	store, ok := s.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	db := openFixturePool(t, dsn)
	ctx := context.Background()
	scope := uniqueScope("t51-exp-turn")
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	sid := insertSessionRow(t, db, scope, time.Now())
	oldTurn := insertTurnRow(t, db, scope, "old", time.Now().Add(-31*24*time.Hour), &sid)
	freshTurn := insertTurnRow(t, db, scope, "fresh", time.Now(), &sid)
	insertObjectRow(t, db, sid, digestAHex, time.Now().Add(-31*24*time.Hour))
	contextID := insertContextRow(t, db, sid, oldTurn, digestsJSON(digestAHex))
	insertHeadRow(t, db, sid, contextID)

	refs, err := store.ListExpiredTurns(ctx, cutoff, 1000)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, oldTurn, refs[0].TurnID.String())
	require.NotNil(t, refs[0].SessionID)
	assert.Equal(t, sid, refs[0].SessionID.String())

	require.NoError(t, store.DeleteTurnRetention(ctx, uuid.MustParse(oldTurn)))

	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_turns WHERE id = $1", oldTurn).Scan(&count))
	assert.Zero(t, count, "the expired turn row must be deleted")
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_contexts WHERE turn_id = $1", oldTurn).Scan(&count))
	assert.Zero(t, count, "the turn's context row must be deleted")
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_session_heads WHERE context_id = $1", contextID).Scan(&count))
	assert.Zero(t, count, "the head pointing at the deleted context must be cleared")
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_turns WHERE id = $1", freshTurn).Scan(&count))
	assert.Equal(t, 1, count, "fresh turns must survive")
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_content_objects WHERE session_id = $1", sid).Scan(&count))
	assert.Equal(t, 1, count, "content objects are not deleted by the turn retention path")
}

// TestIntegrationRetentionSessionDeletion covers the expired-session path:
// DeleteSessionRetention removes the session, its aliases, head, contexts,
// content objects, and turns in one transaction.
func TestIntegrationRetentionSessionDeletion(t *testing.T) {
	dsn := integrationDSN(t)
	s := resetObserverSchema(t, dsn)
	defer s.Close(context.Background())
	store, ok := s.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	db := openFixturePool(t, dsn)
	ctx := context.Background()
	scope := uniqueScope("t51-exp-session")
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	sid := insertSessionRow(t, db, scope, time.Now().Add(-31*24*time.Hour))
	insertAliasRow(t, db, scope, sid)
	turn := insertTurnRow(t, db, scope, "t", time.Now().Add(-31*24*time.Hour), &sid)
	insertObjectRow(t, db, sid, digestAHex, time.Now().Add(-31*24*time.Hour))
	contextID := insertContextRow(t, db, sid, turn, digestsJSON(digestAHex))
	insertHeadRow(t, db, sid, contextID)

	ids, err := store.ListExpiredSessions(ctx, cutoff, 100)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	assert.Equal(t, sid, ids[0].String())

	require.NoError(t, store.DeleteSessionRetention(ctx, uuid.MustParse(sid)))

	for _, q := range []string{
		"SELECT count(*) FROM observer_sessions WHERE id = $1",
		"SELECT count(*) FROM observer_session_aliases WHERE session_id = $1",
		"SELECT count(*) FROM observer_session_heads WHERE session_id = $1",
		"SELECT count(*) FROM observer_contexts WHERE session_id = $1",
		"SELECT count(*) FROM observer_content_objects WHERE session_id = $1",
		"SELECT count(*) FROM observer_turns WHERE session_id = $1",
	} {
		var count int
		require.NoError(t, db.QueryRow(q, sid).Scan(&count))
		assert.Zero(t, count, "nothing may reference the deleted session: %s", q)
	}
}

// TestIntegrationRetentionOrphanGraceAndReferenceSafety covers the orphan
// rules: content past its grace period with no retained context reference is
// deleted; content still referenced by any retained context of its session is
// kept no matter how old; content inside its grace period is kept even
// without references.
func TestIntegrationRetentionOrphanGraceAndReferenceSafety(t *testing.T) {
	dsn := integrationDSN(t)
	s := resetObserverSchema(t, dsn)
	defer s.Close(context.Background())
	store, ok := s.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	db := openFixturePool(t, dsn)
	ctx := context.Background()
	scope := uniqueScope("t51-orphan")
	cutoff := time.Now().Add(-14 * 24 * time.Hour)

	// Session A: one retained context referencing digest A, plus one
	// unreferenced object with digest B. Session B: one retained context
	// referencing digest A as well (cross-session digest sharing must not
	// interfere — references are per (session, digest)).
	sidA := insertSessionRow(t, db, scope, time.Now())
	sidB := insertSessionRow(t, db, scope, time.Now())
	turnA := insertTurnRow(t, db, scope, "a", time.Now(), &sidA)
	turnB := insertTurnRow(t, db, scope, "b", time.Now(), &sidB)

	// Objects: digest A of session A is old but referenced; digest B of
	// session A is old and unreferenced; digest A of session B is fresh and
	// referenced.
	insertObjectRow(t, db, sidA, digestAHex, time.Now().Add(-31*24*time.Hour))
	insertObjectRow(t, db, sidA, digestBHex, time.Now().Add(-31*24*time.Hour))
	insertObjectRow(t, db, sidB, digestAHex, time.Now())
	insertContextRow(t, db, sidA, turnA, digestsJSON(digestAHex))
	insertContextRow(t, db, sidB, turnB, digestsJSON(digestAHex))

	n, err := store.DeleteOrphanContent(ctx, cutoff, 1000)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the old unreferenced object may be deleted")

	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_content_objects WHERE session_id = $1 AND item_digest = $2`, sidA, digestA).Scan(&count))
	assert.Equal(t, 1, count, "old content still referenced by a retained context must survive")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_content_objects WHERE session_id = $1 AND item_digest = $2`, sidA, digestB).Scan(&count))
	assert.Zero(t, count, "old unreferenced content must be deleted")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_content_objects WHERE session_id = $1 AND item_digest = $2`, sidB, digestA).Scan(&count))
	assert.Equal(t, 1, count, "fresh content must survive its grace period regardless of references")

	// Once the retained context goes, the old object becomes deletable.
	require.NoError(t, store.DeleteTurnRetention(ctx, uuid.MustParse(turnA)))
	n, err = store.DeleteOrphanContent(ctx, cutoff, 1000)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "content becomes an orphan once its last retained context is gone")
}

// TestIntegrationRetentionSharedDigestAcrossContexts covers the reference
// safety of a digest shared by two retained contexts of one session: the
// object survives while either context lives and is deleted only when both
// are gone.
func TestIntegrationRetentionSharedDigestAcrossContexts(t *testing.T) {
	dsn := integrationDSN(t)
	s := resetObserverSchema(t, dsn)
	defer s.Close(context.Background())
	store, ok := s.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	db := openFixturePool(t, dsn)
	ctx := context.Background()
	scope := uniqueScope("t51-shared")
	cutoff := time.Now().Add(-14 * 24 * time.Hour)

	sid := insertSessionRow(t, db, scope, time.Now())
	turn1 := insertTurnRow(t, db, scope, "t1", time.Now(), &sid)
	turn2 := insertTurnRow(t, db, scope, "t2", time.Now(), &sid)
	insertObjectRow(t, db, sid, digestAHex, time.Now().Add(-31*24*time.Hour))
	insertContextRow(t, db, sid, turn1, digestsJSON(digestAHex))
	insertContextRow(t, db, sid, turn2, digestsJSON(digestAHex))

	require.NoError(t, store.DeleteTurnRetention(ctx, uuid.MustParse(turn1)))
	n, err := store.DeleteOrphanContent(ctx, cutoff, 1000)
	require.NoError(t, err)
	assert.Zero(t, n, "the shared object must survive while a second context still references it")

	require.NoError(t, store.DeleteTurnRetention(ctx, uuid.MustParse(turn2)))
	n, err = store.DeleteOrphanContent(ctx, cutoff, 1000)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the shared object is deleted only after its last reference is gone")
}

// TestIntegrationRetentionBoundsAndIdempotence covers the per-pass bounds on
// live data: the turn list stops at 1000, the session list stops at 100, and
// a second pass over already-cleaned data deletes nothing.
func TestIntegrationRetentionBoundsAndIdempotence(t *testing.T) {
	dsn := integrationDSN(t)
	s := resetObserverSchema(t, dsn)
	defer s.Close(context.Background())
	store, ok := s.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	db := openFixturePool(t, dsn)
	ctx := context.Background()
	scope := uniqueScope("t51-bounds")
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	// 1005 expired turns and 105 expired sessions: the lists must cap.
	expiredAt := time.Now().Add(-31 * 24 * time.Hour)
	for i := 0; i < 1005; i++ {
		insertTurnRow(t, db, scope, fmt.Sprintf("e-%d", i), expiredAt, nil)
	}
	for i := 0; i < 105; i++ {
		insertSessionRow(t, db, scope, expiredAt)
	}

	refs, err := store.ListExpiredTurns(ctx, cutoff, retentionMaxTurnsPerPass)
	require.NoError(t, err)
	assert.Len(t, refs, retentionMaxTurnsPerPass, "the turn list must cap at the per-pass limit")
	for _, ref := range refs {
		require.NoError(t, store.DeleteTurnRetention(ctx, ref.TurnID))
	}
	refs, err = store.ListExpiredTurns(ctx, cutoff, retentionMaxTurnsPerPass)
	require.NoError(t, err)
	assert.Len(t, refs, 5, "the remaining expired turns are picked up by the next pass")
	for _, ref := range refs {
		require.NoError(t, store.DeleteTurnRetention(ctx, ref.TurnID))
	}

	ids, err := store.ListExpiredSessions(ctx, cutoff, retentionMaxSessionsPerPass)
	require.NoError(t, err)
	assert.Len(t, ids, retentionMaxSessionsPerPass, "the session list must cap at the per-pass limit")
	for _, id := range ids {
		require.NoError(t, store.DeleteSessionRetention(ctx, id))
	}
	ids, err = store.ListExpiredSessions(ctx, cutoff, retentionMaxSessionsPerPass)
	require.NoError(t, err)
	assert.Len(t, ids, 5, "the remaining expired sessions are picked up by the next pass")
	for _, id := range ids {
		require.NoError(t, store.DeleteSessionRetention(ctx, id))
	}

	// Idempotence: a repeated pass over the cleaned data deletes nothing.
	refs, err = store.ListExpiredTurns(ctx, cutoff, retentionMaxTurnsPerPass)
	require.NoError(t, err)
	assert.Empty(t, refs)
	ids, err = store.ListExpiredSessions(ctx, cutoff, retentionMaxSessionsPerPass)
	require.NoError(t, err)
	assert.Empty(t, ids)
	n, err := store.DeleteOrphanContent(ctx, time.Now().Add(-14*24*time.Hour), retentionMaxOrphansPerPass)
	require.NoError(t, err)
	assert.Zero(t, n, "a repeated pass over clean data must delete nothing")
}

// TestIntegrationRetentionIndexedPredicates proves the retention predicates
// are served by the v1/v2 indexes, not sequential scans: with enough
// non-expired rows around, the planner must pick the occurred_at, last_seen,
// and created_at indexes, and the orphan reference check must probe the
// contexts GIN index.
func TestIntegrationRetentionIndexedPredicates(t *testing.T) {
	dsn := integrationDSN(t)
	s := resetObserverSchema(t, dsn)
	defer s.Close(context.Background())
	db := openFixturePool(t, dsn)
	scope := uniqueScope("t51-idx")
	now := time.Now()

	// 3000 fresh rows + 100 expired rows make the indexed paths attractive.
	for i := 0; i < 3000; i++ {
		insertTurnRow(t, db, scope, fmt.Sprintf("f-%d", i), now, nil)
	}
	for i := 0; i < 100; i++ {
		insertTurnRow(t, db, scope, fmt.Sprintf("e-%d", i), now.Add(-31*24*time.Hour), nil)
	}
	for i := 0; i < 3000; i++ {
		insertSessionRow(t, db, scope, now)
	}
	for i := 0; i < 100; i++ {
		insertSessionRow(t, db, scope, now.Add(-31*24*time.Hour))
	}
	for i := 0; i < 3000; i++ {
		sid := insertSessionRow(t, db, scope, now)
		insertObjectRow(t, db, sid, digestAHex, now)
	}
	for i := 0; i < 100; i++ {
		sid := insertSessionRow(t, db, scope, now)
		insertObjectRow(t, db, sid, digestAHex, now.Add(-31*24*time.Hour))
	}
	_, err := db.Exec("ANALYZE observer_turns, observer_sessions, observer_content_objects, observer_contexts")
	require.NoError(t, err)

	planContains := func(t *testing.T, plan string, needles ...string) {
		t.Helper()
		for _, needle := range needles {
			assert.Contains(t, plan, needle, "execution plan must use the indexed predicate %q", needle)
		}
	}

	cutoff := now.Add(-30 * 24 * time.Hour)
	plan := explainJSON(t, db, `SELECT id FROM observer_turns WHERE occurred_at < $1 ORDER BY occurred_at LIMIT $2`, cutoff, 1000)
	planContains(t, plan, "idx_observer_turns_occurred_at")

	plan = explainJSON(t, db, `SELECT id FROM observer_sessions WHERE last_seen < $1 ORDER BY last_seen LIMIT $2`, cutoff, 100)
	planContains(t, plan, "idx_observer_sessions_last_seen")

	plan = explainJSON(t, db, `DELETE FROM observer_content_objects o
WHERE o.id IN (
    SELECT o2.id FROM observer_content_objects o2
    WHERE o2.created_at < $1
      AND NOT EXISTS (
        SELECT 1 FROM observer_contexts c
        WHERE c.session_id = o2.session_id
          AND c.item_digests @> to_jsonb(encode(o2.item_digest, 'hex'))
      )
    ORDER BY o2.id
    LIMIT $2
)`, now.Add(-14*24*time.Hour), 1000)
	// The candidate scan must be served by the created_at index. The
	// reference-safety predicate (JSONB @> with the outer row's digest) is
	// de-correlated by the planner into a hash anti join on small tables —
	// the cheapest correct plan — so its correctness is proven by the row
	// assertions of TestIntegrationRetentionOrphanGraceAndReferenceSafety
	// and TestIntegrationRetentionSharedDigestAcrossContexts; the GIN index
	// on observer_contexts (item_digests) exists for large-table plans and
	// its presence is locked by TestMigrationsEmbedded.
	planContains(t, plan, "idx_observer_content_objects_created_at")
}

// TestIntegrationRetentionFullCheckpointKeepsDeltaAlive covers the group
// integrity of the turn retention path: an expired full checkpoint turn must
// not be deleted while a retained delta context of the same session still
// references it as its base — deleting it would leave the delta dangling and
// its (not yet expired) turn unreconstructable. The full turn stays for a
// later pass, and is deleted once the delta has expired too.
func TestIntegrationRetentionFullCheckpointKeepsDeltaAlive(t *testing.T) {
	dsn := integrationDSN(t)
	s := resetObserverSchema(t, dsn)
	defer s.Close(context.Background())
	store, ok := s.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	db := openFixturePool(t, dsn)
	ctx := context.Background()
	scope := uniqueScope("t51-group")

	sid := insertSessionRow(t, db, scope, time.Now())
	fullTurn := insertTurnRow(t, db, scope, "full", time.Now().Add(-31*24*time.Hour), &sid)
	deltaTurn := insertTurnRow(t, db, scope, "delta", time.Now(), &sid)
	insertObjectRow(t, db, sid, digestAHex, time.Now().Add(-31*24*time.Hour))
	insertObjectRow(t, db, sid, digestBHex, time.Now())
	var fullCtxID int64
	require.NoError(t, db.QueryRow(`INSERT INTO observer_contexts (session_id, turn_id, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes)
		VALUES ($1, $2, 0, 0, 0, 1, $3::jsonb, 10) RETURNING id`, sid, fullTurn, digestsJSON(digestAHex)).Scan(&fullCtxID))
	_, err := db.Exec(`INSERT INTO observer_contexts (session_id, turn_id, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes)
		VALUES ($1, $2, $3, 1, 1, 2, $4::jsonb, 10)`, sid, deltaTurn, fullCtxID, digestsJSON(digestBHex))
	require.NoError(t, err)

	require.NoError(t, store.DeleteTurnRetention(ctx, uuid.MustParse(fullTurn)))

	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_turns WHERE id = $1", fullTurn).Scan(&count))
	assert.Equal(t, 1, count, "a full checkpoint referenced by a retained delta must not be deleted")
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_contexts WHERE id = $1", fullCtxID).Scan(&count))
	assert.Equal(t, 1, count, "the full checkpoint context row must survive while a delta references it")

	// The delta's base reference is intact: its checkpoint row still exists.
	var deltaCtxID int64
	require.NoError(t, db.QueryRow("SELECT id FROM observer_contexts WHERE turn_id = $1", deltaTurn).Scan(&deltaCtxID))
	var checkpointCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_contexts c, observer_contexts d WHERE c.id = d.checkpoint_id AND d.id = $1`, deltaCtxID).Scan(&checkpointCount))
	assert.Equal(t, 1, checkpointCount, "the delta must still resolve its full checkpoint")

	// Once the delta expires too, its own turn deletion frees the full turn
	// for the next pass.
	require.NoError(t, store.DeleteTurnRetention(ctx, uuid.MustParse(deltaTurn)))
	require.NoError(t, store.DeleteTurnRetention(ctx, uuid.MustParse(fullTurn)))
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_turns WHERE id = $1", fullTurn).Scan(&count))
	assert.Zero(t, count, "the full turn is deleted once nothing references its checkpoint")
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_contexts WHERE id = $1", fullCtxID).Scan(&count))
	assert.Zero(t, count, "the checkpoint context row is deleted once unreferenced")
}

// TestIntegrationRetentionWriteBumpsSessionLastSeen locks the session recency
// invariant the session retention path relies on: writing any turn bound to a
// session — including a metadata-only turn that persists no content — must
// advance the session's last_seen at least to the turn's occurred_at, so an
// expired session can never carry a not-yet-expired turn. A session whose
// last_seen is behind its latest turn's occurred_at would be deleted while
// that turn still has retention left.
func TestIntegrationRetentionWriteBumpsSessionLastSeen(t *testing.T) {
	dsn := integrationDSN(t)
	s := resetObserverSchema(t, dsn)
	defer s.Close(context.Background())
	store, ok := s.(RetentionStore)
	require.True(t, ok, "the pg adapter must implement RetentionStore")
	db := openFixturePool(t, dsn)
	ctx := context.Background()
	scope := uniqueScope("t51-lastseen")
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	// A session whose last content activity is long past.
	sid := insertSessionRow(t, db, scope, time.Now().Add(-40*24*time.Hour))

	// A fresh metadata-only turn bound to that session: no content rows, but
	// the turn itself must still count as session activity.
	occurred := time.Now()
	sidUUID := uuid.MustParse(sid)
	require.NoError(t, s.WriteBatch(ctx, []Event{{
		EventID:      "meta-only",
		NodeScope:    scope,
		SessionID:    &sidUUID,
		OccurredAt:   occurred,
		ContentState: ContentStateMetadataOnly,
	}}))

	var lastSeen time.Time
	require.NoError(t, db.QueryRow("SELECT last_seen FROM observer_sessions WHERE id = $1", sid).Scan(&lastSeen))
	assert.False(t, lastSeen.Before(occurred.Add(-time.Minute)), "writing a turn must keep last_seen at least as fresh as the turn's occurred_at")

	ids, err := store.ListExpiredSessions(ctx, cutoff, 100)
	require.NoError(t, err)
	assert.Empty(t, ids, "a session with a fresh turn must not be expired")
}

// explainJSON runs EXPLAIN (FORMAT JSON) over the given query and returns the
// serialized plan for index-name assertions.
func explainJSON(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var raw string
	err := db.QueryRow("EXPLAIN (FORMAT JSON) "+query, args...).Scan(&raw)
	require.NoError(t, err)
	return raw
}
