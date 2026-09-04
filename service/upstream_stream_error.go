package service

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// responsesStreamErrorEvents are the Responses SSE event types that report an
// upstream failure. "error" is the native flat error event; "response.error"
// and "response.failed" carry the failure inside response.error.
//
// DashScope/Bailian answer a streaming request with HTTP 200 and end the SSE
// stream with one of these events instead of a non-2xx status, so neither
// RelayErrorHandler nor ExtractUpstreamErrorFromSSE ever runs for them: the
// stream handler is the only place that observes the failure.
var responsesStreamErrorEvents = map[string]struct{}{
	"error":           {},
	"response.error":  {},
	"response.failed": {},
}

// NewResponsesStreamEventError builds the gateway error for a Responses stream
// event that reports an upstream failure inside an HTTP 200 response, and
// returns nil for every other event type so a stream handler can call it
// unconditionally on each frame.
//
// The structured detail is recovered from the raw event payload with the same
// extractor used for non-2xx SSE error bodies, then completed from the event's
// nested response.error object. Both error paths therefore report identical
// admin_info.upstream_error diagnostics (code/type/message/request_id) and feed
// the same account/auth-fatal classification.
//
// The status code is 500 because an in-stream failure carries no HTTP status of
// its own (the upstream answered 200), so retry policy stays on the existing
// status-code matrix instead of an invented mapping. The frame must not be
// forwarded to the client: the relay loop owns the client-visible error.
func NewResponsesStreamEventError(event *dto.ResponsesStreamResponse, payload string) *types.NewAPIError {
	if event == nil {
		return nil
	}
	if _, ok := responsesStreamErrorEvents[event.Type]; !ok {
		return nil
	}

	detail, _ := extractDetailFromErrorPayload([]byte(payload))
	if event.Response != nil {
		if nested := event.Response.GetOpenAIError(); nested != nil {
			if detail == nil {
				detail = &types.UpstreamErrorDetail{}
			}
			if detail.Message == "" {
				detail.Message = nested.Message
			}
			if detail.Code == "" {
				detail.Code = scalarToString(nested.Code)
			}
			if detail.Type == "" {
				detail.Type = nested.Type
			}
		}
	}

	message := ""
	code := ""
	errType := ""
	if detail != nil {
		message, code, errType = detail.Message, detail.Code, detail.Type
	}
	if message == "" {
		message = fmt.Sprintf("responses stream error: %s", event.Type)
	}
	if code == "" {
		code = string(types.ErrorCodeBadResponse)
	}

	apiErr := types.WithOpenAIError(types.OpenAIError{
		Message: message,
		Type:    errType,
		Code:    code,
	}, http.StatusInternalServerError)
	if detail != nil && (detail.Message != "" || detail.Code != "" || detail.RequestID != "") {
		apiErr.SetUpstreamErrorDetail(detail)
	}
	return apiErr
}
