//go:build relay_observer_pg_integration

package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	relayobserver "github.com/QuantumNous/new-api/pkg/relay_observer"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegrationLockedTurnTableDegradedEnvelope is the locked fault
// dimension end to end on the real database: a concurrent ACCESS EXCLUSIVE
// lock on observer_turns blocks the Root overview query past its budget, and
// the handler answers HTTP 200 with the SSOT degraded envelope and the
// stable "timeout" reason — never the raw error, never a hang. It compiles
// only under relay_observer_pg_integration and is refused against any
// non-disposable database, mirroring the observer-side guard.
func TestIntegrationLockedTurnTableDegradedEnvelope(t *testing.T) {
	dsn := os.Getenv("TEST_RELAY_OBSERVER_POSTGRES_DSN")
	require.NoError(t, validateControllerIntegrationDSN(dsn), "refusing to run integration tests against a non-disposable database")

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Ping())
	dropObserverSchemaForTest(t, db)

	// Real runtime: Init reads the environment and boots the dedicated store
	// against the disposable database, and the controller is wired to it.
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
	t.Setenv("RELAY_OBSERVER_SQL_DSN", dsn)
	t.Setenv("RELAY_OBSERVER_SCHEMA_MODE", "bootstrap")
	t.Setenv("RELAY_OBSERVER_QUERY_TIMEOUT_MS", "200")
	rt := relayobserver.NewRuntime()
	rt.Init()
	require.True(t, rt.Status().Enabled, "the runtime must boot against the disposable database")
	SetRelayObserverRuntime(rt)
	defer SetRelayObserverRuntime(nil)
	defer rt.Close(context.Background())

	// Hold an ACCESS EXCLUSIVE lock on the turn table in a separate
	// transaction: the overview aggregate over observer_turns blocks behind
	// it and expires inside the handler's query budget.
	lockConn, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer lockConn.Close()
	lockTx, err := lockConn.Begin()
	require.NoError(t, err)
	defer lockTx.Rollback() // releases the lock on every path
	_, err = lockTx.Exec("LOCK TABLE observer_turns IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/relay-observer/overview", nil)

	start := time.Now()
	GetRelayObserverOverview(c)
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Less(t, elapsed, 3*time.Second, "a locked query must degrade, never hang")

	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Degraded bool   `json:"degraded"`
			Reason   string `json:"reason"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.True(t, body.Success)
	assert.True(t, body.Data.Degraded, "the locked query must answer the degraded envelope")
	assert.Equal(t, "timeout", body.Data.Reason, "a lock timeout degrades with the stable timeout reason")
	assert.NotContains(t, recorder.Body.String(), dsn, "the response must never leak the DSN")
}

// validateControllerIntegrationDSN refuses any DSN that is not the disposable
// local PostgreSQL, mirroring the observer package's own guard: only loopback,
// only port 55433, and only the relay_observer database.
func validateControllerIntegrationDSN(dsn string) error {
	if strings.TrimSpace(dsn) == "" {
		return fmt.Errorf("TEST_RELAY_OBSERVER_POSTGRES_DSN must be set for relay_observer_pg_integration tests")
	}
	trimmed := strings.TrimPrefix(dsn, "postgres://")
	trimmed = strings.TrimPrefix(trimmed, "postgresql://")
	hostPort := trimmed
	if at := strings.LastIndex(trimmed, "@"); at >= 0 {
		hostPort = trimmed[at+1:]
	}
	hostPort = strings.Split(hostPort, "/")[0]
	if !strings.HasPrefix(hostPort, "127.0.0.1:55433") && !strings.HasPrefix(hostPort, "localhost:55433") {
		return fmt.Errorf("TEST_RELAY_OBSERVER_POSTGRES_DSN must target loopback port 55433, got %q", hostPort)
	}
	if !strings.Contains(trimmed, "/relay_observer") && !strings.HasSuffix(trimmed, "/relay_observer") {
		return fmt.Errorf("TEST_RELAY_OBSERVER_POSTGRES_DSN must target the relay_observer database")
	}
	return nil
}

// dropObserverSchemaForTest drops every observer_* table so the test starts
// from a known-empty observer schema. Table names come from the database's
// own information_schema and are fixed-prefix, never user input.
func dropObserverSchemaForTest(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name LIKE 'observer\_%' ESCAPE '\'`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	for _, name := range names {
		_, err := db.Exec("DROP TABLE " + name + " CASCADE")
		require.NoError(t, err, "drop %s", name)
	}
}
