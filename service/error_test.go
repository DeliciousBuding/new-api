package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResetStatusCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		statusCode       int
		statusCodeConfig string
		expectedCode     int
		expectedUpstream int
	}{
		{
			name:             "map string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"503"}`,
			expectedCode:     503,
			expectedUpstream: 429,
		},
		{
			name:             "map int value",
			statusCode:       429,
			statusCodeConfig: `{"429":503}`,
			expectedCode:     503,
			expectedUpstream: 429,
		},
		{
			name:             "skip invalid string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"bad-code"}`,
			expectedCode:     429,
			expectedUpstream: 0,
		},
		{
			name:             "skip status code 200",
			statusCode:       200,
			statusCodeConfig: `{"200":503}`,
			expectedCode:     200,
			expectedUpstream: 0,
		},
		{
			name:             "no mapping configured leaves upstream unset",
			statusCode:       429,
			statusCodeConfig: "",
			expectedCode:     429,
			expectedUpstream: 0,
		},
		{
			name:             "empty mapping object leaves upstream unset",
			statusCode:       429,
			statusCodeConfig: `{}`,
			expectedCode:     429,
			expectedUpstream: 0,
		},
		{
			name:             "unrelated mapping key leaves upstream unset",
			statusCode:       429,
			statusCodeConfig: `{"500":"503"}`,
			expectedCode:     429,
			expectedUpstream: 0,
		},
		{
			name:             "identity mapping is not recorded as a rewrite",
			statusCode:       429,
			statusCodeConfig: `{"429":429}`,
			expectedCode:     429,
			expectedUpstream: 0,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			newAPIError := &types.NewAPIError{
				StatusCode: tc.statusCode,
			}
			ResetStatusCode(newAPIError, tc.statusCodeConfig)
			require.Equal(t, tc.expectedCode, newAPIError.StatusCode)
			// Zero must unambiguously mean "StatusCode is the upstream value",
			// otherwise attribution cannot tell a mapped 503 from a real one.
			require.Equal(t, tc.expectedUpstream, newAPIError.GetUpstreamStatusCode())
		})
	}
}

func TestUpstreamStatusCodeAccessorsAreNilSafe(t *testing.T) {
	t.Parallel()

	var nilErr *types.NewAPIError
	require.Equal(t, 0, nilErr.GetUpstreamStatusCode())
	require.NotPanics(t, func() { nilErr.SetUpstreamStatusCode(429) })
}

func TestRelayErrorHandlerTruncatesInvalidJSONBodyInLog(t *testing.T) {
	withDebugEnabled(t, false)

	body := strings.Repeat("b", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, "bad response status code 500", newAPIError.Error())
	require.Contains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), fmt.Sprintf("original_length=%d", len(body)))
	require.NotContains(t, logBuffer.String(), strings.Repeat("b", common.LocalLogContentLimit+1))
}

func TestRelayErrorHandlerKeepsStructuredErrorMessage(t *testing.T) {
	message := strings.Repeat("c", common.LocalLogContentLimit+256)
	body := `{"message":"` + message + `"}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerKeepsOpenAIErrorMessage(t *testing.T) {
	message := strings.Repeat("d", common.LocalLogContentLimit+256)
	body := `{"error":{"message":"` + message + `","type":"server_error","code":"server_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerAttachesSSEDetailWithoutChangingClientError(t *testing.T) {
	message := "Access denied, please make sure your account is in good standing."
	body := "id:1\nevent:error\n:HTTP_STATUS/400\n" +
		"data:{\"request_id\":\"req-1\",\"code\":\"Arrearage\",\"type\":\"Arrearage\",\"message\":\"" + message + "\"}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	// The client-visible error keeps the bare status-code form; structured
	// upstream diagnostics are attached for admin-facing surfaces only.
	require.Equal(t, "bad response status code 400", newAPIError.Error())
	require.Equal(t, http.StatusBadRequest, newAPIError.StatusCode)

	detail := newAPIError.GetUpstreamErrorDetail()
	require.NotNil(t, detail)
	assert.Equal(t, "Arrearage", detail.Code)
	assert.Equal(t, message, detail.Message)
	assert.Equal(t, "req-1", detail.RequestID)
}

func TestRelayErrorHandlerLogsSSEDetailForAdmins(t *testing.T) {
	withDebugEnabled(t, false)

	body := "event:error\ndata:{\"request_id\":\"req-1\",\"code\":\"Arrearage\",\"message\":\"Access denied\"}\n\n"
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, "bad response status code 400", newAPIError.Error())
	assert.Contains(t, logBuffer.String(), "upstream error detail")
	assert.Contains(t, logBuffer.String(), "code=Arrearage")
	assert.Contains(t, logBuffer.String(), "request_id=req-1")
}

func TestRelayErrorHandlerKeepsFullBodyForChannelTestOnSSEError(t *testing.T) {
	body := "event:error\ndata:{\"request_id\":\"req-1\",\"code\":\"Arrearage\",\"message\":\"Access denied\"}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, true)

	require.NotNil(t, newAPIError)
	// Channel test (showBodyWhenFail=true) keeps seeing the complete raw body.
	assert.Contains(t, newAPIError.Error(), body)
	assert.Contains(t, newAPIError.Error(), "bad response status code 400")
	require.NotNil(t, newAPIError.GetUpstreamErrorDetail())
}

func TestRelayErrorHandlerKeepsInvalidJSONBodyInDebugLog(t *testing.T) {
	withDebugEnabled(t, true)

	body := strings.Repeat("e", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.NotContains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), body)
}

func withDebugEnabled(t *testing.T, enabled bool) {
	t.Helper()

	oldDebug := common.DebugEnabled
	common.DebugEnabled = enabled
	t.Cleanup(func() {
		common.DebugEnabled = oldDebug
	})
}
