package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractUpstreamErrorFromSSE(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		body           string
		expectNil      bool
		expectedCode   string
		expectedType   string
		expectedMsg    string
		expectedReqID  string
		expectedFormat string
	}{
		{
			// Exact wire shape observed from DashScope/Bailian in production:
			// non-2xx response, SSE event without spaces after colons.
			name: "DashScope Arrearage error event",
			body: "id:1\nevent:error\n:HTTP_STATUS/400\n" +
				"data:{\"request_id\":\"req-1\",\"code\":\"Arrearage\",\"type\":\"Arrearage\"," +
				"\"message\":\"Access denied, please make sure your account is in good standing.\"}\n\n",
			expectedCode:   "Arrearage",
			expectedType:   "Arrearage",
			expectedMsg:    "Access denied, please make sure your account is in good standing.",
			expectedReqID:  "req-1",
			expectedFormat: UpstreamErrorPayloadFormatSSE,
		},
		{
			name: "Anthropic style nested error object with spaces",
			body: "event: error\n" +
				"data: {\"type\": \"error\", \"error\": {\"type\": \"overloaded_error\", \"message\": \"Overloaded\"}}\n\n",
			expectedType:   "overloaded_error",
			expectedMsg:    "Overloaded",
			expectedFormat: UpstreamErrorPayloadFormatSSE,
		},
		{
			name: "OpenAI style nested error with code",
			body: "data:{\"error\":{\"code\":\"data_inspection_failed\",\"type\":\"data_inspection_failed\"," +
				"\"message\":\"Input data may contain inappropriate content.\"},\"id\":\"chatcmpl-1\"}\n\n",
			expectedCode:   "data_inspection_failed",
			expectedType:   "data_inspection_failed",
			expectedMsg:    "Input data may contain inappropriate content.",
			expectedFormat: UpstreamErrorPayloadFormatSSE,
		},
		{
			name: "error event wins over earlier normal events",
			body: "event:message_start\ndata:{\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":1}}}\n\n" +
				"event:error\ndata:{\"request_id\":\"req-2\",\"code\":\"Throttling\",\"message\":\"Requests throttled\"}\n\n",
			expectedCode:   "Throttling",
			expectedMsg:    "Requests throttled",
			expectedReqID:  "req-2",
			expectedFormat: UpstreamErrorPayloadFormatSSE,
		},
		{
			name: "multi-line data joined into one payload",
			body: "event:error\n" +
				"data:{\"code\":\"InternalError\",\n" +
				"data:\"message\":\"upstream internal error\"}\n\n",
			expectedCode:   "InternalError",
			expectedMsg:    "upstream internal error",
			expectedFormat: UpstreamErrorPayloadFormatSSE,
		},
		{
			name:           "error payload in unnamed event",
			body:           "data:{\"code\":\"InvalidApiKey\",\"message\":\"Invalid API-key provided.\"}\n\n",
			expectedCode:   "InvalidApiKey",
			expectedMsg:    "Invalid API-key provided.",
			expectedFormat: UpstreamErrorPayloadFormatSSE,
		},
		{
			name: "numeric code is stringified",
			body: "event:error\ndata:{\"code\":429,\"message\":\"rate limited\"}\n\n",
			expectedCode:   "429",
			expectedMsg:    "rate limited",
			expectedFormat: UpstreamErrorPayloadFormatSSE,
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
			assert.Equal(t, tc.expectedFormat, detail.PayloadFormat)
		})
	}
}

func TestExtractUpstreamErrorFromSSEPrefersNamedErrorEventOverUnnamedPayload(t *testing.T) {
	t.Parallel()

	body := "data:{\"code\":\"FirstCode\",\"message\":\"first payload\"}\n\n" +
		"event:error\ndata:{\"code\":\"RealError\",\"message\":\"named error payload\"}\n\n"

	detail := ExtractUpstreamErrorFromSSE([]byte(body))

	require.NotNil(t, detail)
	assert.Equal(t, "RealError", detail.Code)
	assert.Equal(t, "named error payload", detail.Message)
}
