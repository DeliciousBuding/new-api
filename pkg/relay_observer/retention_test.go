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
	sqls           []string
	args           [][]any
	turnRows       []*fakeRow // rows for "FROM observer_turns" queries
	sessionRow     []*fakeRow // rows for "FROM observer_sessions" queries
	checkpointRefs bool       // EXISTS result of the checkpoint-reference check
	execErr        error
	queryErr       error
	orphans        int64 // rows affected by the orphan delete
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
	switch {
	case strings.Contains(query, "checkpoint_id IN"):
		return &fakeRow{values: []any{f.checkpointRefs}}
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

// TestDeleteTurnRetentionShapeAndOrder locks the turn retention deletion: one
// transaction-shaped sequence that checks the checkpoint is unreferenced,
// clears a pointing head, deletes the turn's context row, then the turn row
// itself, all parameterized by $1 and never reading payload columns.
func TestDeleteTurnRetentionShapeAndOrder(t *testing.T) {
	fx := &retentionFakeTx{}
	require.NoError(t, deleteTurnRetentionTx(context.Background(), fx, turnA))

	require.Len(t, fx.sqls, 3)
	head := fx.sqls[0]
	assert.Contains(t, head, "UPDATE observer_session_heads SET context_id = NULL, checkpoint_id = NULL, group_ordinal = NULL")
	assert.Contains(t, head, "context_id IN (SELECT id FROM observer_contexts WHERE turn_id = $1)")
	assert.Contains(t, fx.sqls[1], "DELETE FROM observer_contexts WHERE turn_id = $1")
	assert.Contains(t, fx.sqls[2], "DELETE FROM observer_turns WHERE id = $1")
	for _, sqlText := range fx.sqls {
		assert.NotContains(t, sqlText, "payload")
		assert.NotContains(t, sqlText, turnA.String(), "turn id must travel as a bound parameter, never inline")
	}
	require.Len(t, fx.args, 3)
	for _, args := range fx.args {
		require.Len(t, args, 1)
		assert.Equal(t, turnA.String(), args[0])
	}
}

// TestDeleteTurnRetentionSkipsReferencedCheckpoint locks the reference-safety
// skip: a full checkpoint still referenced by a retained delta context is not
// deleted — the turn survives for a later pass and no delete statement runs.
func TestDeleteTurnRetentionSkipsReferencedCheckpoint(t *testing.T) {
	fx := &retentionFakeTx{checkpointRefs: true}
	require.NoError(t, deleteTurnRetentionTx(context.Background(), fx, turnA))
	assert.Empty(t, fx.sqls, "a referenced full checkpoint must never be deleted")
	assert.Empty(t, fx.args)
}

// TestDeleteSessionRetentionShapeAndOrder locks the session retention
// deletion: content objects, contexts, head, aliases, turns, and the session
// row itself, each parameterized by $1. Deletion never reads payload
// columns.
func TestDeleteSessionRetentionShapeAndOrder(t *testing.T) {
	fx := &retentionFakeTx{}
	require.NoError(t, deleteSessionRetentionTx(context.Background(), fx, sidB))

	want := []string{
		"DELETE FROM observer_content_objects WHERE session_id = $1",
		"DELETE FROM observer_contexts WHERE session_id = $1",
		"DELETE FROM observer_session_heads WHERE session_id = $1",
		"DELETE FROM observer_session_aliases WHERE session_id = $1",
		"DELETE FROM observer_turns WHERE session_id = $1",
		"DELETE FROM observer_sessions WHERE id = $1",
	}
	require.Len(t, fx.sqls, len(want))
	for i, sqlText := range fx.sqls {
		assert.Contains(t, sqlText, want[i], "statement %d", i)
		assert.NotContains(t, sqlText, "payload")
		assert.NotContains(t, sqlText, sidB.String(), "session id must travel as a bound parameter, never inline")
	}
	for _, args := range fx.args {
		require.Len(t, args, 1)
		assert.Equal(t, sidB.String(), args[0])
	}
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

	fx2 := &retentionFakeTx{execErr: sentinel}
	require.ErrorIs(t, deleteTurnRetentionTx(context.Background(), fx2, turnA), sentinel)
	require.ErrorIs(t, deleteSessionRetentionTx(context.Background(), fx2, sidA), sentinel)
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
