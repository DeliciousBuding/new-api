package service

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// responsesStreamEvent parses a raw SSE data payload the same way a stream
// handler does before calling NewResponsesStreamEventError.
func responsesStreamEvent(t *testing.T, payload string) *dto.ResponsesStreamResponse {
	t.Helper()
	var event dto.ResponsesStreamResponse
	require.NoError(t, common.UnmarshalJsonStr(payload, &event))
	return &event
}

func TestNewResponsesStreamEventError(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		// wantNil marks event types that must never produce an error.
		wantNil bool
		// expected error fields
		statusCode int
		message    string
		errorCode  types.ErrorCode
		// expected admin-facing detail
		wantDetail  bool
		detailCode  string
		detailType  string
		detailReqID string
		// expected classification
		accountFatal bool
		nonRetryable bool
	}{
		{
			name:         "dashscope flat error event keeps code message and request id",
			payload:      `{"type":"error","code":"Model.AccessDenied","message":"Model access denied.","request_id":"req-1"}`,
			statusCode:   http.StatusInternalServerError,
			message:      "Model access denied.",
			errorCode:    "Model.AccessDenied",
			wantDetail:   true,
			detailCode:   "Model.AccessDenied",
			detailReqID:  "req-1",
			nonRetryable: true,
		},
		{
			name:       "nested top level error object",
			payload:    `{"type":"error","error":{"type":"invalid_request_error","code":"rate_limit_exceeded","message":"Rate limit reached"}}`,
			statusCode: http.StatusInternalServerError,
			message:    "Rate limit reached",
			errorCode:  "rate_limit_exceeded",
			wantDetail: true,
			detailCode: "rate_limit_exceeded",
			detailType: "invalid_request_error",
		},
		{
			name:       "response failed carries nested response error",
			payload:    `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"upstream boom"}}}`,
			statusCode: http.StatusInternalServerError,
			message:    "upstream boom",
			errorCode:  "server_error",
			wantDetail: true,
			detailCode: "server_error",
		},
		{
			name:         "arrearage is account fatal",
			payload:      `{"type":"error","code":"Arrearage","message":"Access denied, please make sure your account is in good standing"}`,
			statusCode:   http.StatusInternalServerError,
			message:      "Access denied, please make sure your account is in good standing",
			errorCode:    "Arrearage",
			wantDetail:   true,
			detailCode:   "Arrearage",
			accountFatal: true,
			nonRetryable: true,
		},
		{
			name:       "response failed without error detail falls back to event type",
			payload:    `{"type":"response.failed","response":{"id":"resp_1","status":"failed"}}`,
			statusCode: http.StatusInternalServerError,
			message:    "responses stream error: response.failed",
			errorCode:  types.ErrorCodeBadResponse,
			wantDetail: false,
		},
		{
			name:    "completed event is not an error",
			payload: `{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
			wantNil: true,
		},
		{
			name:    "text delta event is not an error",
			payload: `{"type":"response.output_text.delta","delta":"hello"}`,
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := NewResponsesStreamEventError(responsesStreamEvent(t, tt.payload), tt.payload)
			if tt.wantNil {
				require.Nil(t, apiErr)
				return
			}

			require.NotNil(t, apiErr)
			assert.Equal(t, tt.statusCode, apiErr.StatusCode)
			assert.Equal(t, tt.message, apiErr.Error())
			assert.Equal(t, tt.errorCode, apiErr.GetErrorCode())

			detail := apiErr.GetUpstreamErrorDetail()
			if !tt.wantDetail {
				assert.Nil(t, detail)
			} else {
				require.NotNil(t, detail)
				assert.Equal(t, tt.detailCode, detail.Code)
				assert.Equal(t, tt.detailType, detail.Type)
				assert.Equal(t, tt.detailReqID, detail.RequestID)
			}

			assert.Equal(t, tt.accountFatal, IsAccountFatalError(apiErr))
			assert.Equal(t, tt.nonRetryable, IsNonRetryableUpstreamError(apiErr))
		})
	}
}

func TestNewResponsesStreamEventErrorNilEvent(t *testing.T) {
	assert.Nil(t, NewResponsesStreamEventError(nil, `{"type":"error"}`))
}
