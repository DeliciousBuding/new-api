package service

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractUpstreamErrorFromSSE(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		body          string
		expectNil     bool
		expectedCode  string
		expectedType  string
		expectedMsg   string
		expectedReqID string
	}{
		{
			// Exact wire shape observed from DashScope/Bailian in production:
			// non-2xx response, SSE event without spaces after colons.
			name: "DashScope Arrearage error event",
			body: "id:1\nevent:error\n:HTTP_STATUS/400\n" +
				"data:{\"request_id\":\"req-1\",\"code\":\"Arrearage\",\"type\":\"Arrearage\"," +
				"\"message\":\"Access denied, please make sure your account is in good standing.\"}\n\n",
			expectedCode:  "Arrearage",
			expectedType:  "Arrearage",
			expectedMsg:   "Access denied, please make sure your account is in good standing.",
			expectedReqID: "req-1",
		},
		{
			name: "Anthropic style nested error object with spaces",
			body: "event: error\n" +
				"data: {\"type\": \"error\", \"error\": {\"type\": \"overloaded_error\", \"message\": \"Overloaded\"}}\n\n",
			expectedType: "overloaded_error",
			expectedMsg:  "Overloaded",
		},
		{
			name: "OpenAI style nested error with code in unnamed event",
			body: "data:{\"error\":{\"code\":\"data_inspection_failed\",\"type\":\"data_inspection_failed\"," +
				"\"message\":\"Input data may contain inappropriate content.\"},\"id\":\"chatcmpl-1\"}\n\n",
			expectedCode: "data_inspection_failed",
			expectedType: "data_inspection_failed",
			expectedMsg:  "Input data may contain inappropriate content.",
		},
		{
			name: "error string form",
			body: "data:{\"error\":\"quota exceeded\"}\n\n",
			expectedMsg: "quota exceeded",
		},
		{
			name: "doubly nested error message",
			body: "data:{\"error\":{\"error\":{\"message\":\"deep failure\"}}}\n\n",
			expectedMsg: "deep failure",
		},
		{
			name: "empty error envelope falls back to a marker",
			body: "data:{\"error\":{}}\n\n",
			expectedMsg: upstreamErrorFallbackMessage,
		},
		{
			name: "error event wins over earlier normal events",
			body: "event:message_start\ndata:{\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":1}}}\n\n" +
				"event:error\ndata:{\"request_id\":\"req-2\",\"code\":\"Throttling\",\"message\":\"Requests throttled\"}\n\n",
			expectedCode:  "Throttling",
			expectedMsg:   "Requests throttled",
			expectedReqID: "req-2",
		},
		{
			name: "multi-line data joined into one payload",
			body: "event:error\n" +
				"data:{\"code\":\"InternalError\",\n" +
				"data:\"message\":\"upstream internal error\"}\n\n",
			expectedCode: "InternalError",
			expectedMsg:  "upstream internal error",
		},
		{
			name: "numeric code is stringified",
			body: "event:error\ndata:{\"code\":429,\"message\":\"rate limited\"}\n\n",
			expectedCode: "429",
			expectedMsg:  "rate limited",
		},
		{
			name:         "leading BOM does not drop the first event",
			body:         "\xEF\xBB\xBFevent:error\ndata:{\"code\":\"Arrearage\",\"message\":\"Access denied\"}\n\n",
			expectedCode: "Arrearage",
			expectedMsg:  "Access denied",
		},
		{
			name:      "unnamed flat payload without error envelope is not detected",
			body:      "data:{\"code\":\"InvalidApiKey\",\"message\":\"Invalid API-key provided.\"}\n\n",
			expectNil: true,
		},
		{
			name:      "bare code field on a success chunk is not detected",
			body:      "data:{\"code\":0,\"message\":\"ok\"}\n\n",
			expectNil: true,
		},
		{
			name: "normal Anthropic success stream has no error",
			body: "event:message_start\ndata:{\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
				"event:content_block_delta\ndata:{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
				"event:message_stop\ndata:{\"type\":\"message_stop\"}\n\n",
			expectNil: true,
		},
		{
			name:      "normal OpenAI stream with DONE has no error",
			body:      "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			expectNil: true,
		},
		{
			name:      "non-SSE body returns nil",
			body:      "<html>502 Bad Gateway</html>",
			expectNil: true,
		},
		{
			name:      "empty body returns nil",
			body:      "",
			expectNil: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			detail := ExtractUpstreamErrorFromSSE([]byte(tc.body))
			if tc.expectNil {
				require.Nil(t, detail)
				return
			}
			require.NotNil(t, detail)
			assert.Equal(t, tc.expectedCode, detail.Code)
			assert.Equal(t, tc.expectedType, detail.Type)
			assert.Equal(t, tc.expectedMsg, detail.Message)
			assert.Equal(t, tc.expectedReqID, detail.RequestID)
		})
	}
}

func TestExtractUpstreamErrorFromSSEPrefersNamedErrorEventOverUnnamedPayload(t *testing.T) {
	t.Parallel()

	body := "data:{\"error\":{\"message\":\"first payload\"}}\n\n" +
		"event:error\ndata:{\"code\":\"RealError\",\"message\":\"named error payload\"}\n\n"

	detail := ExtractUpstreamErrorFromSSE([]byte(body))

	require.NotNil(t, detail)
	assert.Equal(t, "RealError", detail.Code)
	assert.Equal(t, "named error payload", detail.Message)
}

func TestExtractUpstreamErrorFromSSEBoundsFields(t *testing.T) {
	t.Parallel()

	longMessage := strings.Repeat("m", maxUpstreamErrorMessageBytes+100)
	longCode := strings.Repeat("c", maxUpstreamErrorFieldBytes+50)
	longRequestID := strings.Repeat("r", maxUpstreamErrorFieldBytes+50)

	body := "event:error\ndata:{\"code\":\"" + longCode + "\",\"message\":\"" + longMessage +
		"\",\"request_id\":\"" + longRequestID + "\"}\n\n"

	detail := ExtractUpstreamErrorFromSSE([]byte(body))

	require.NotNil(t, detail)
	assert.Len(t, detail.Message, maxUpstreamErrorMessageBytes)
	assert.Len(t, detail.Code, maxUpstreamErrorFieldBytes)
	assert.Len(t, detail.RequestID, maxUpstreamErrorFieldBytes)
}

func TestExtractUpstreamErrorFromSSECapsInputSize(t *testing.T) {
	t.Parallel()

	body := "event:error\ndata:{\"code\":\"Arrearage\",\"message\":\"Access denied\"}\n\n" +
		strings.Repeat("data:padding\n\n", maxUpstreamErrorBodyBytes/10)

	detail := ExtractUpstreamErrorFromSSE([]byte(body))

	require.NotNil(t, detail)
	assert.Equal(t, "Arrearage", detail.Code)
}

func TestFormatUpstreamErrorDetail(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		detail   *types.UpstreamErrorDetail
		expected string
	}{
		{
			name:     "nil detail renders empty",
			detail:   nil,
			expected: "",
		},
		{
			name: "code and type deduplicated when equal",
			detail: &types.UpstreamErrorDetail{
				Code:      "Arrearage",
				Type:      "Arrearage",
				RequestID: "req-1",
				Message:   "Access denied",
			},
			expected: "code=Arrearage, request_id=req-1, message=Access denied",
		},
		{
			name: "distinct type is kept",
			detail: &types.UpstreamErrorDetail{
				Message: "Overloaded",
				Type:    "overloaded_error",
			},
			expected: "type=overloaded_error, message=Overloaded",
		},
		{
			name: "control characters are escaped",
			detail: &types.UpstreamErrorDetail{
				Code:    "X",
				Message: "line1\nline2",
			},
			expected: "code=X, message=line1\\nline2",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, FormatUpstreamErrorDetail(tc.detail))
		})
	}
}
