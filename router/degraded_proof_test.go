package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relayobserver "github.com/QuantumNous/new-api/pkg/relay_observer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ProofOnlyDegradedResponse is the reverse-validation harness: a scripted
// timeout proves the degraded envelope responds with HTTP 200 and reason
// "timeout", and the raw error text never leaks into the response body.
func TestProofOnlyDegradedResponse(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			sessionsErr: &relayobserver.QueryError{Kind: relayobserver.QueryErrTimeout, Msg: "query slot busy: context deadline exceeded while waiting for the query semaphore"},
		},
		timeout: time.Second,
		ok:      true,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/relay-observer/sessions?node_scope=proof", nil)
	req.Header.Set("Authorization", "Bearer "+rootToken)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	t.Logf("RESPONSE BODY: %s", body)
	var parsed map[string]any
	require.NoError(t, common.Unmarshal([]byte(body), &parsed))
	data := parsed["data"].(map[string]any)
	assert.Equal(t, true, data["degraded"])
	assert.Equal(t, "timeout", data["reason"])
	assert.NotContains(t, body, "query slot busy")
}
