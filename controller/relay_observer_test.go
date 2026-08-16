package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relayobserver "github.com/QuantumNous/new-api/pkg/relay_observer"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the observer status controller: it only returns the safe
// in-memory Status snapshot, reports disabled when no runtime is wired, and
// passes the runtime snapshot through verbatim. The Root/Admin/User 403
// matrix lives in the router tests, where the real middleware chain runs.

func TestGetRelayObserverStatusUnwired(t *testing.T) {
	t.Cleanup(func() { SetRelayObserverRuntime(nil) })
	SetRelayObserverRuntime(nil)
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/relay-observer/status", nil)

	GetRelayObserverStatus(c)

	assert.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Success bool                 `json:"success"`
		Data    relayobserver.Status `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &payload))
	assert.True(t, payload.Success)
	assert.False(t, payload.Data.Enabled)
	assert.Equal(t, relayobserver.ReasonDisabled, payload.Data.ReasonCode)
}

// TestGetRelayObserverStatusWired proves the controller reads the runtime
// snapshot: a runtime disabled with a store init failure surfaces that exact
// reason, not a hardcoded value.
func TestGetRelayObserverStatusWired(t *testing.T) {
	t.Cleanup(func() { SetRelayObserverRuntime(nil) })
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
	t.Setenv("RELAY_OBSERVER_HMAC_KEY", "controller-test-hmac-key")
	t.Setenv("RELAY_OBSERVER_SQL_DSN", "mysql://user:pass@127.0.0.1:3306/obs")
	rt := relayobserver.NewRuntime()
	rt.Init()
	require.Equal(t, relayobserver.ReasonStoreInitFailed, rt.Status().ReasonCode)
	SetRelayObserverRuntime(rt)
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/relay-observer/status", nil)

	GetRelayObserverStatus(c)

	assert.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Success bool                 `json:"success"`
		Data    relayobserver.Status `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &payload))
	assert.True(t, payload.Success)
	assert.False(t, payload.Data.Enabled)
	assert.Equal(t, relayobserver.ReasonStoreInitFailed, payload.Data.ReasonCode)
	assert.Equal(t, relayobserver.IPTrustNone, payload.Data.IPTrust)
}

// TestObserverTurnsDTOExposesClientProfile locks the Root turn-list API
// contract: the per-turn request-path profile survives the query summary and
// is serialized under the frontend's snake_case field name.
func TestObserverTurnsDTOExposesClientProfile(t *testing.T) {
	turnID := uuid.MustParse("00000000-0000-4000-8000-000000000118")
	items := observerTurnsDTO([]relayobserver.TurnSummary{{
		TurnID:        turnID,
		OccurredAt:    time.Unix(1, 0).UTC(),
		ClientProfile: "claude_vscode",
	}})

	require.Len(t, items, 1)
	assert.Equal(t, "claude_vscode", items[0].ClientProfile)
	payload, err := common.Marshal(items[0])
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, common.Unmarshal(payload, &fields))
	assert.Equal(t, "claude_vscode", fields["client_profile"])
}
