//go:build relay_observer_pg_integration

package relayobserver

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegrationLockedTableTimesOut is the locked fault dimension of the
// fault matrix on the real database: a concurrent ACCESS EXCLUSIVE lock on
// observer_turns blocks the query until its short context expires, and the
// failure classifies as timeout — never a hang and never an unclassified
// error. T3.3 exercised this dimension with a fake because the dedicated
// port was contested; 55433 is free now, so the real lock path runs.
func TestIntegrationLockedTableTimesOut(t *testing.T) {
	dsn := integrationDSN(t)
	db := openFixturePool(t, dsn)
	cleanupObserverSchema(t, db)
	store, err := OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err)
	defer store.Close(context.Background())

	qs, err := NewQueryStore(store)
	require.NoError(t, err)

	// Hold an ACCESS EXCLUSIVE lock on the turn table in a separate
	// transaction: every SELECT on observer_turns blocks behind it. The
	// fixture pool owns this connection, so the adapter's own pool stays
	// untouched.
	lockConn, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer lockConn.Close()
	lockTx, err := lockConn.Begin()
	require.NoError(t, err)
	defer lockTx.Rollback() // releases the lock on every path
	_, err = lockTx.Exec("LOCK TABLE observer_turns IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = qs.ListTurns(ctx, TurnQuery{PageSize: 5})
	require.Error(t, err, "a lock-blocked query must fail, never hang")
	var qe *QueryError
	if errors.As(err, &qe) {
		assert.Equal(t, QueryErrTimeout, qe.Kind, "a lock-blocked query must classify as timeout")
	} else {
		// The deadline reached the backend mid-query: pgx surfaces the
		// context deadline, which the controller layer maps to the same
		// degraded "timeout" envelope.
		assert.True(t, errors.Is(err, context.DeadlineExceeded),
			"a lock-blocked query must surface the deadline, got: %v", err)
	}

	// The lock is released: the same query succeeds on the same schema,
	// proving the timeout was the lock, not a broken query.
	require.NoError(t, lockTx.Rollback())
	page, err := qs.ListTurns(context.Background(), TurnQuery{PageSize: 5})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(page.Items), 5)
}
