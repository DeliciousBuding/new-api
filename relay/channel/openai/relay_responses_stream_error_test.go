package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runResponsesStreamBody drives OaiResponsesStreamHandler over a raw SSE body so
// a test controls exactly how the upstream stream ends (with or without
// [DONE]). It returns the client-visible recorder, the relay info, the reported
// usage and the returned error.
func runResponsesStreamBody(t *testing.T, body string) (*httptest.ResponseRecorder, *relaycommon.RelayInfo, *dto.Usage, *types.NewAPIError) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = oldTimeout
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "responses-stream-test")
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		DisablePing:     true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5.1",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	return w, info, usage, apiErr
}

// runResponsesStream frames each event as an SSE data line and terminates the
// stream with [DONE], which is how a well-formed upstream ends a Responses
// stream.
func runResponsesStream(t *testing.T, events ...string) (*httptest.ResponseRecorder, *relaycommon.RelayInfo, *dto.Usage, *types.NewAPIError) {
	t.Helper()
	var body strings.Builder
	for _, event := range events {
		body.WriteString("data: ")
		body.WriteString(event)
		body.WriteString("\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	return runResponsesStreamBody(t, body.String())
}

// A Bailian/DashScope style failure answers HTTP 200 and ends the stream with a
// flat error event. It must surface as a gateway error carrying the structured
// upstream detail, and the error frame must not be forwarded to the client, so
// the relay loop can retry another channel instead of recording a quota=0
// success consume log.
func TestOaiResponsesStreamHandlerReturnsInStreamErrorEvent(t *testing.T) {
	w, info, usage, apiErr := runResponsesStream(
		t,
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"error","code":"Model.AccessDenied","message":"Model access denied.","request_id":"req-1"}`,
		// A terminal event arriving after the failure must neither reach the
		// client nor turn the relay back into a success.
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`,
	)

	require.NotNil(t, apiErr)
	assert.Nil(t, usage)
	assert.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
	assert.Equal(t, "Model access denied.", apiErr.Error())
	assert.Equal(t, types.ErrorCode("Model.AccessDenied"), apiErr.GetErrorCode())

	detail := apiErr.GetUpstreamErrorDetail()
	require.NotNil(t, detail)
	assert.Equal(t, "Model.AccessDenied", detail.Code)
	assert.Equal(t, "req-1", detail.RequestID)

	body := w.Body.String()
	assert.Contains(t, body, "response.created")
	assert.NotContains(t, body, "Model access denied.")
	assert.NotContains(t, body, "response.completed")
	assert.True(t, info.StreamStatus.HasErrors())
	// EndReason is deliberately not asserted: the scanner goroutine ([DONE]) and
	// the data goroutine (Stop) race on a sync.Once, so "done" or "handler_stop"
	// can win. Only the recorded error and the suppressed frames are contract.
}

// A nested response.failed event is the other native shape for the same
// failure; the pending image generation count must be discarded, not billed.
func TestOaiResponsesStreamHandlerDiscardsImageOutputOnFailedEvent(t *testing.T) {
	_, info, _, apiErr := runResponsesStream(
		t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"img_1","status":"completed","result":"base64-a"}}`,
		`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"upstream boom"}}}`,
	)

	require.NotNil(t, apiErr)
	assert.Equal(t, "upstream boom", apiErr.Error())
	require.NotNil(t, info.ResponsesUsageInfo)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolImageGeneration)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

// A stream truncated before any terminal event with nothing billable in it is an
// upstream failure ("stream closed before response.completed"), not a free
// success.
func TestOaiResponsesStreamHandlerErrorsOnTruncatedStream(t *testing.T) {
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n"
	_, info, usage, apiErr := runResponsesStreamBody(t, body)

	require.NotNil(t, apiErr)
	assert.Nil(t, usage)
	assert.Contains(t, apiErr.Error(), "without a terminal event")
	assert.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	// 500 keeps the error inside the retryable status-code matrix and outside
	// ShouldDisableByStatusCode, so a truncated stream re-routes instead of
	// auto-banning the channel.
	assert.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
}

// The truncation guard must not fire on a stream that produced output: billing
// keeps falling back to the token estimate exactly as before.
func TestOaiResponsesStreamHandlerKeepsProductiveStreamWithoutTerminalEvent(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello world\"}\n\n"
	_, _, usage, apiErr := runResponsesStreamBody(t, body)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Greater(t, usage.CompletionTokens, 0)
}

// A terminal event keeps the stream successful even when the upstream reports no
// usage at all, which is the pre-existing zero-quota path.
func TestOaiResponsesStreamHandlerKeepsCompletedStreamWithoutUsage(t *testing.T) {
	_, _, usage, apiErr := runResponsesStream(
		t,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`,
	)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 0, usage.TotalTokens)
}
