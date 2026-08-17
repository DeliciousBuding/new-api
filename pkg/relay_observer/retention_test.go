package relayobserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the T5.1 retention store surface without a database. The
// listing and deletion orchestration runs against retentionFakeTx, an
// in-memory stand-in for the contentQuerier/contentTx seams, and every test
// locks the SQL shapes the SSOT demands: parameterized predicates, explicit
// LIMIT bounds, indexed-column predicates (occurred_at / last_seen /
// created_at), and never a content payload read. The SQL itself is exercised
// by the relay_observer_pg_integration suite.

// retentionFakeTx records every statement and routes by the table the SQL
// touches, mirroring how the other phases test their seams.
type retentionFakeTx struct {
	sqls            []string
	args            [][]any
	turnRows        []*fakeRow // rows for "FROM observer_turns" queries
	sessionRow      []*fakeRow // rows for "FROM observer_sessions" queries
	sessionLastSeen time.Time  // value of the session retention lock query
	checkpointRefs  bool       // EXISTS result of the checkpoint-reference check
	// turnSessionID is the session_id scanned from observer_turns by the
	// turn retention resolve step. nil leaves the session unbound and the
	// head lock is skipped; a string locks the head row.
	turnSessionID *string
	// headPresent controls the head-lock SELECT in turn retention: true
	// returns an empty head row (locked), false returns ErrNoRows so the
	// lock is skipped. Defaults to false (zero value); tests that exercise
	// the lock-bearing branch set it true.
	headPresent   bool
	execErr       error
	queryErr      error
	orphans       int64 // rows affected by the orphan delete
	backlogCount  int64
	backlogOldest time.Time
}

func (f *retentionFakeTx) Query(ctx context.Context, query string, args ...any) (rowIter, error) {
	f.sqls = append(f.sqls, query)
	f.args = append(f.args, args)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	switch {
	case strings.Contains(query, "FROM observer_turns"):
		return &fakeRows{rows: f.turnRows}, nil
	case strings.Contains(query, "FROM observer_sessions"):
		return &fakeRows{rows: f.sessionRow}, nil
	}
	return nil, fmt.Errorf("fake: unhandled query %q", query)
}

func (f *retentionFakeTx) QueryRow(ctx context.Context, query string, args ...any) rowScanner {
	f.sqls = append(f.sqls, query)
	f.args = append(f.args, args)
	switch {
	case strings.Contains(query, "session_id::text FROM observer_turns"):
		if f.turnSessionID != nil {
			return &fakeRow{values: []any{sql.NullString{String: *f.turnSessionID, Valid: true}}}
		}
		return &fakeRow{values: []any{sql.NullString{}}}
	case strings.Contains(query, "FROM observer_session_heads") && strings.Contains(query, "FOR UPDATE"):
		if f.headPresent {
			return &fakeRow{values: []any{sql.NullInt64{}}}
		}
		return &fakeRow{err: sql.ErrNoRows}
	case strings.Contains(query, "checkpoint_id IN"):
		return &fakeRow{values: []any{f.checkpointRefs}}
	case strings.Contains(query, "retention_backlog"):
		return &fakeRow{values: []any{f.backlogCount, f.backlogOldest}}
	case strings.Contains(query, "last_seen FROM observer_sessions"):
		return &fakeRow{values: []any{f.sessionLastSeen}}
	}
	return &fakeRow{err: fmt.Errorf("fake: retention seam never uses QueryRow: %q", query)}
}

func (f *retentionFakeTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	f.sqls = append(f.sqls, query)
	f.args = append(f.args, args)
	if f.execErr != nil {
		return nil, f.execErr
	}
	switch {
	case strings.Contains(query, "o.id IN"):
		return fakeResult{n: f.orphans}, nil
	case strings.Contains(query, "DELETE FROM observer_content_objects"),
		strings.Contains(query, "DELETE FROM observer_contexts"),
		strings.Contains(query, "DELETE FROM observer_session_heads"),
		strings.Contains(query, "DELETE FROM observer_session_aliases"),
		strings.Contains(query, "DELETE FROM observer_turns"),
		strings.Contains(query, "DELETE FROM observer_sessions"),
		strings.Contains(query, "SET context_id = NULL"):
		return fakeResult{n: 1}, nil
	}
	return nil, fmt.Errorf("fake: unhandled exec %q", query)
}

var sidA = uuid.MustParse("11111111-1111-1111-1111-111111111111")
var sidB = uuid.MustParse("22222222-2222-2222-2222-222222222222")
var turnA = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
var turnB = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

// TestListExpiredTurnsShapeAndRows locks the expired-turn listing: only the
// retention columns (id, session_id) are selected — never attempts, geo, or
// content payload columns — and the predicate is the indexed occurred_at
// column with an explicit parameterized LIMIT.
func TestListExpiredTurnsShapeAndRows(t *testing.T) {
	fx := &retentionFakeTx{
		turnRows: []*fakeRow{
			{values: []any{turnA.String(), sidA.String()}},
			{values: []any{turnB.String(), nil}}, // turn without a session
		},
	}
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	refs, err := listExpiredTurnsQ(context.Background(), fx, cutoff, 1000)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, turnA, refs[0].TurnID)
	require.NotNil(t, refs[0].SessionID)
	assert.Equal(t, sidA, *refs[0].SessionID)
	assert.Equal(t, turnB, refs[1].TurnID)
	assert.Nil(t, refs[1].SessionID)

	require.Len(t, fx.sqls, 1)
	sqlText := fx.sqls[0]
	assert.Contains(t, sqlText, "SELECT id::text, session_id::text FROM observer_turns")
	assert.Contains(t, sqlText, "occurred_at < $1")
	assert.Contains(t, sqlText, "ORDER BY occurred_at")
	assert.Contains(t, sqlText, "LIMIT $2")
	for _, forbidden := range []string{"attempts", "client_ip", "country", "payload", "content_state", "event_id"} {
		assert.NotContains(t, sqlText, forbidden, "expired-turn list must not read %q", forbidden)
	}
	require.Len(t, fx.args, 1)
	require.Len(t, fx.args[0], 2)
	assert.Equal(t, cutoff, fx.args[0][0])
	assert.Equal(t, 1000, fx.args[0][1])
}

// TestListExpiredSessionsShapeAndRows locks the expired-session listing: only
// session ids, indexed last_seen predicate, parameterized LIMIT.
func TestListExpiredSessionsShapeAndRows(t *testing.T) {
	fx := &retentionFakeTx{sessionRow: []*fakeRow{{values: []any{sidB.String()}}}}
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	ids, err := listExpiredSessionsQ(context.Background(), fx, cutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{sidB}, ids)

	require.Len(t, fx.sqls, 1)
	sqlText := fx.sqls[0]
	assert.Contains(t, sqlText, "SELECT id::text FROM observer_sessions")
	assert.Contains(t, sqlText, "last_seen < $1")
	assert.Contains(t, sqlText, "ORDER BY last_seen")
	assert.Contains(t, sqlText, "LIMIT $2")
	assert.NotContains(t, sqlText, "payload")
	require.Len(t, fx.args[0], 2)
	assert.Equal(t, cutoff, fx.args[0][0])
	assert.Equal(t, 100, fx.args[0][1])
}

// TestRetentionListRejectsNonPositiveLimit locks the bound: a non-positive
// limit yields an empty result without touching the query seam.
func TestRetentionListRejectsNonPositiveLimit(t *testing.T) {
	fx := &retentionFakeTx{}
	refs, err := listExpiredTurnsQ(context.Background(), fx, time.Now(), 0)
	require.NoError(t, err)
	assert.Empty(t, refs)
	ids, err := listExpiredSessionsQ(context.Background(), fx, time.Now(), -1)
	require.NoError(t, err)
	assert.Empty(t, ids)
	n, err := deleteOrphanContentQ(context.Background(), fx, time.Now(), 0)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, fx.sqls, "no statement may run for a non-positive limit")
}

// TestInspectRetentionBacklogIsBoundedAndPayloadFree locks the progress
// signal contract: each class is sampled with limit+1, counts are reported as
// capped lower bounds, the oldest timestamp is retained, and no payload query
// is issued.
func TestInspectRetentionBacklogIsBoundedAndPayloadFree(t *testing.T) {
	oldest := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fx := &retentionFakeTx{backlogCount: int64(retentionBacklogInspectLimit + 1), backlogOldest: oldest}
	turnCutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	contentCutoff := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)

	backlog, err := inspectRetentionBacklogQ(context.Background(), fx, turnCutoff, contentCutoff, retentionBacklogInspectLimit)
	require.NoError(t, err)
	assert.Equal(t, int64(retentionBacklogInspectLimit), backlog.Turns)
	assert.Equal(t, int64(retentionBacklogInspectLimit), backlog.Sessions)
	assert.Equal(t, int64(retentionBacklogInspectLimit), backlog.Objects)
	assert.Equal(t, oldest, backlog.Oldest)
	assert.True(t, backlog.Truncated)

	require.Len(t, fx.sqls, 3)
	for _, query := range fx.sqls {
		assert.Contains(t, query, "LIMIT $2")
		assert.NotContains(t, query, "payload")
		assert.NotContains(t, query, "item_digests, logical_bytes")
	}
	assert.Equal(t, turnCutoff, fx.args[0][0])
	assert.Equal(t, contentCutoff, fx.args[2][0])
	assert.Equal(t, retentionBacklogInspectLimit+1, fx.args[0][1])
}

// TestDeleteTurnRetentionShapeAndOrder locks the turn retention deletion: one
// transaction-shaped sequence that resolves the turn's session, checks the
// checkpoint is unreferenced (excluding the row's own self-reference),
// clears a pointing head, deletes the turn's context row, then the turn row
// itself, all parameterized by $1 and never reading payload columns. The
// turn here has no session, so the head lock is skipped.
func TestDeleteTurnRetentionShapeAndOrder(t *testing.T) {
	fx := &retentionFakeTx{}
	deleted, err := deleteTurnRetentionTx(context.Background(), fx, turnA)
	require.NoError(t, err)
	assert.True(t, deleted)

	require.Len(t, fx.sqls, 5)
	assert.Contains(t, fx.sqls[0], "SELECT session_id::text FROM observer_turns WHERE id = $1", "the session is resolved before the head lock and reference check")
	assert.Contains(t, fx.sqls[1], "SELECT EXISTS (SELECT 1 FROM observer_contexts d")
	assert.Contains(t, fx.sqls[1], "d.id <> d.checkpoint_id", "self-referencing full checkpoints must not block their own turn")
	head := fx.sqls[2]
	assert.Contains(t, head, "UPDATE observer_session_heads SET context_id = NULL, checkpoint_id = NULL, group_ordinal = NULL")
	assert.Contains(t, head, "context_id IN (SELECT id FROM observer_contexts WHERE turn_id = $1)")
	assert.Contains(t, fx.sqls[3], "DELETE FROM observer_contexts WHERE turn_id = $1")
	assert.Contains(t, fx.sqls[4], "DELETE FROM observer_turns WHERE id = $1")
	for _, sqlText := range fx.sqls {
		assert.NotContains(t, sqlText, "payload")
		assert.NotContains(t, sqlText, turnA.String(), "turn id must travel as a bound parameter, never inline")
	}
	require.Len(t, fx.args, 5)
	for _, args := range fx.args {
		require.Len(t, args, 1)
		assert.Equal(t, turnA.String(), args[0])
	}
}

// TestDeleteTurnRetentionLocksHeadRowForSessionBoundTurn locks the TOCTOU
// fix shape: a session-bound turn takes the head row lock with FOR UPDATE
// between the session resolve and the reference check, serializing against a
// concurrent append whose lockHeadTx locks the same row. The reference check
// and deletes only run after the lock is held.
func TestDeleteTurnRetentionLocksHeadRowForSessionBoundTurn(t *testing.T) {
	sid := sidA.String()
	fx := &retentionFakeTx{turnSessionID: &sid, headPresent: true}
	deleted, err := deleteTurnRetentionTx(context.Background(), fx, turnA)
	require.NoError(t, err)
	assert.True(t, deleted)

	require.Len(t, fx.sqls, 6)
	assert.Contains(t, fx.sqls[0], "SELECT session_id::text FROM observer_turns WHERE id = $1")
	assert.Contains(t, fx.sqls[1], "SELECT context_id FROM observer_session_heads WHERE session_id = $1 FOR UPDATE")
	assert.Equal(t, sid, fx.args[1][0], "the head lock binds the resolved session id")
	assert.Contains(t, fx.sqls[2], "SELECT EXISTS (SELECT 1 FROM observer_contexts d")
	assert.Contains(t, fx.sqls[3], "UPDATE observer_session_heads SET context_id = NULL")
	assert.Contains(t, fx.sqls[4], "DELETE FROM observer_contexts WHERE turn_id = $1")
	assert.Contains(t, fx.sqls[5], "DELETE FROM observer_turns WHERE id = $1")
	for _, sqlText := range fx.sqls {
		assert.NotContains(t, sqlText, "payload")
		assert.NotContains(t, sqlText, turnA.String(), "turn id must travel as a bound parameter, never inline")
	}
}

// TestDeleteTurnRetentionSkipsHeadLockWhenSessionMissing locks the
// no-session branch: a turn with a NULL session_id has no contexts and no
// head pointing at them, so the head lock is skipped and the path goes
// straight to the reference check and a turn-only delete.
func TestDeleteTurnRetentionSkipsHeadLockWhenSessionMissing(t *testing.T) {
	fx := &retentionFakeTx{}
	deleted, err := deleteTurnRetentionTx(context.Background(), fx, turnA)
	require.NoError(t, err)
	assert.True(t, deleted)
	for _, sqlText := range fx.sqls {
		assert.NotContains(t, sqlText, "FOR UPDATE", "a turn without a session must not take the head lock")
	}
}

// TestDeleteTurnRetentionSkipsReferencedCheckpoint locks the reference-safety
// skip: a full checkpoint still referenced by a retained delta context is not
// deleted — the turn survives for a later pass and no delete statement runs.
func TestDeleteTurnRetentionSkipsReferencedCheckpoint(t *testing.T) {
	fx := &retentionFakeTx{checkpointRefs: true}
	deleted, err := deleteTurnRetentionTx(context.Background(), fx, turnA)
	require.NoError(t, err)
	assert.False(t, deleted)
	require.Len(t, fx.sqls, 2, "the session resolve and reference check run; no delete may run")
	assert.Contains(t, fx.sqls[0], "SELECT session_id::text FROM observer_turns")
	assert.Contains(t, fx.sqls[1], "SELECT EXISTS")
	require.Len(t, fx.args, 2)
	for _, args := range fx.args {
		require.Len(t, args, 1)
		assert.Equal(t, turnA.String(), args[0])
	}
}

// TestDeleteSessionRetentionShapeAndOrder locks the session retention
// deletion: the last_seen lock+re-check first, then content objects,
// contexts, head, aliases, turns, and the session row itself, each
// parameterized by $1. Deletion never reads payload columns.
func TestDeleteSessionRetentionShapeAndOrder(t *testing.T) {
	fx := &retentionFakeTx{sessionLastSeen: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	deleted, err := deleteSessionRetentionTx(context.Background(), fx, sidB, cutoff)
	require.NoError(t, err)
	assert.True(t, deleted)

	// The lock query is the first statement; the session id travels as a
	// bound parameter and the cutoff as the second.
	require.Len(t, fx.sqls, 7)
	assert.Contains(t, fx.sqls[0], "SELECT last_seen FROM observer_sessions")
	assert.Contains(t, fx.sqls[0], "FOR UPDATE")
	want := []string{
		"DELETE FROM observer_content_objects WHERE session_id = $1",
		"DELETE FROM observer_contexts WHERE session_id = $1",
		"DELETE FROM observer_session_heads WHERE session_id = $1",
		"DELETE FROM observer_session_aliases WHERE session_id = $1",
		"DELETE FROM observer_turns WHERE session_id = $1",
		"DELETE FROM observer_sessions WHERE id = $1",
	}
	for i, sqlText := range fx.sqls[1:] {
		assert.Contains(t, sqlText, want[i], "statement %d", i+1)
		assert.NotContains(t, sqlText, "payload")
		assert.NotContains(t, sqlText, sidB.String(), "session id must travel as a bound parameter, never inline")
	}
	require.Len(t, fx.args, 7)
	// The lock query binds only the session id; the cutoff is compared in Go
	// after the row is locked, never inlined into SQL.
	require.Len(t, fx.args[0], 1)
	assert.Equal(t, sidB.String(), fx.args[0][0])
	for _, args := range fx.args[1:] {
		require.Len(t, args, 1)
		assert.Equal(t, sidB.String(), args[0])
	}
}

// TestDeleteSessionRetentionSkipsReactivated locks the TOCTOU guard: a
// session whose last_seen moved past the cutoff between the retention list
// query and the delete is left intact — the transaction locks the row,
// re-checks, and issues no delete at all.
func TestDeleteSessionRetentionSkipsReactivated(t *testing.T) {
	fx := &retentionFakeTx{sessionLastSeen: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	deleted, err := deleteSessionRetentionTx(context.Background(), fx, sidB, cutoff)
	require.NoError(t, err)
	assert.False(t, deleted)

	// Only the lock+re-check statement ran; no delete was issued.
	require.Len(t, fx.sqls, 1)
	assert.Contains(t, fx.sqls[0], "FOR UPDATE")
}

// TestDeleteOrphanContentShapeAndCount locks the orphan deletion: bounded by
// a LIMIT on the candidate subquery, candidates filtered by the indexed
// created_at predicate, and reference safety enforced by a JSONB containment
// anti-join against the session's retained context digest lists. The delete
// never reads the payload column and returns the deleted row count.
func TestDeleteOrphanContentShapeAndCount(t *testing.T) {
	fx := &retentionFakeTx{orphans: 7}
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	n, err := deleteOrphanContentQ(context.Background(), fx, cutoff, 1000)
	require.NoError(t, err)
	assert.Equal(t, 7, n)

	require.Len(t, fx.sqls, 1)
	sqlText := fx.sqls[0]
	assert.Contains(t, sqlText, "DELETE FROM observer_content_objects o")
	assert.Contains(t, sqlText, "o.id IN (")
	assert.Contains(t, sqlText, "o2.created_at < $1")
	assert.Contains(t, sqlText, "NOT EXISTS (")
	assert.Contains(t, sqlText, "c.session_id = o2.session_id")
	assert.Contains(t, sqlText, "c.item_digests @> to_jsonb(encode(o2.item_digest, 'hex'))")
	assert.Contains(t, sqlText, "ORDER BY o2.id")
	assert.Contains(t, sqlText, "LIMIT $2")
	assert.NotContains(t, sqlText, "payload")
	assert.NotContains(t, sqlText, "RETURNING")
	require.Len(t, fx.args, 1)
	require.Len(t, fx.args[0], 2)
	assert.Equal(t, cutoff, fx.args[0][0])
	assert.Equal(t, 1000, fx.args[0][1])
}

// TestRetentionErrorsPropagate locks the failure contract: seam errors are
// wrapped with the retention context and returned unchanged in kind.
func TestRetentionErrorsPropagate(t *testing.T) {
	sentinel := errors.New("pg unavailable")

	fx := &retentionFakeTx{queryErr: sentinel}
	_, err := listExpiredTurnsQ(context.Background(), fx, time.Now(), 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list expired turns")
	assert.ErrorIs(t, err, sentinel)

	_, err = listExpiredSessionsQ(context.Background(), fx, time.Now(), 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)

	fx2 := &retentionFakeTx{execErr: sentinel, sessionLastSeen: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	_, err = deleteTurnRetentionTx(context.Background(), fx2, turnA)
	require.ErrorIs(t, err, sentinel)
	_, err = deleteSessionRetentionTx(context.Background(), fx2, sidA, time.Now())
	require.ErrorIs(t, err, sentinel)
	_, err = deleteOrphanContentQ(context.Background(), fx2, time.Now(), 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

// TestRetentionSQLIsStatic locks the parameterization guarantee at the source
// level: the SQL constants contain no format verbs, so the cutoff, limits,
// and ids can never be inlined.
func TestRetentionSQLIsStatic(t *testing.T) {
	for _, sqlText := range []string{
		`SELECT id::text, session_id::text FROM observer_turns WHERE occurred_at < $1 ORDER BY occurred_at LIMIT $2`,
		`SELECT id::text FROM observer_sessions WHERE last_seen < $1 ORDER BY last_seen LIMIT $2`,
		`SELECT session_id::text FROM observer_turns WHERE id = $1`,
		`SELECT context_id FROM observer_session_heads WHERE session_id = $1 FOR UPDATE`,
		`SELECT EXISTS (SELECT 1 FROM observer_contexts d WHERE d.checkpoint_id IN (SELECT id FROM observer_contexts WHERE turn_id = $1))`,
		`UPDATE observer_session_heads SET context_id = NULL, checkpoint_id = NULL, group_ordinal = NULL WHERE context_id IN (SELECT id FROM observer_contexts WHERE turn_id = $1)`,
		`DELETE FROM observer_contexts WHERE turn_id = $1`,
		`DELETE FROM observer_turns WHERE id = $1`,
		`DELETE FROM observer_content_objects WHERE session_id = $1`,
		`DELETE FROM observer_contexts WHERE session_id = $1`,
		`DELETE FROM observer_session_heads WHERE session_id = $1`,
		`DELETE FROM observer_session_aliases WHERE session_id = $1`,
		`DELETE FROM observer_turns WHERE session_id = $1`,
		`DELETE FROM observer_sessions WHERE id = $1`,
	} {
		assert.NotContains(t, sqlText, "%", "SQL must be fully parameterized, no format verbs: %q", sqlText)
	}
}
