package service

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// Upstream error body formats recognized by ExtractUpstreamErrorFromSSE.
const (
	UpstreamErrorPayloadFormatJSON = "json"
	UpstreamErrorPayloadFormatSSE  = "sse"
)

// Some Anthropic-compatible upstreams (notably DashScope/Bailian) return
// non-2xx failures for streaming requests as SSE bodies, e.g.
//
//	event:error
//	data:{"request_id":"...","code":"Arrearage","message":"..."}
//
// instead of a standalone JSON object. RelayErrorHandler parses bodies as
// JSON, so those failures degrade into "bad response status code N" and the
// actionable upstream diagnostics are lost. This extractor recovers them for
// admin-facing logs and error records. It never changes the client-visible
// error message; whether SSE upstream errors should be normalized for clients
// is a separate protocol decision.

// sseEvent is one parsed Server-Sent Events block.
type sseEvent struct {
	name      string
	dataLines []string
}

// parseSSEEvents splits an SSE body into event blocks. Events are separated
// by blank lines; multi-line "data" fields are kept as separate lines (the
// SSE spec joins them with "\n" on dispatch), a later "event" line overrides
// an earlier one within the same block, and lines starting with ':' are
// comments (e.g. DashScope emits ":HTTP_STATUS/400"). The optional single
// space after the colon is trimmed, because DashScope emits "event:error"
// and "data:{...}" without spaces.
func parseSSEEvents(responseBody []byte) []sseEvent {
	var events []sseEvent
	var current *sseEvent
	flushCurrentEvent := func() {
		if current != nil && (current.name != "" || len(current.dataLines) > 0) {
			events = append(events, *current)
		}
		current = nil
	}
	for _, rawLine := range bytes.Split(responseBody, []byte{'\n'}) {
		line := bytes.TrimRight(rawLine, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			flushCurrentEvent()
			continue
		}
		if line[0] == ':' {
			continue
		}
		if current == nil {
			current = &sseEvent{}
		}
		field, value, hasValue := bytes.Cut(line, []byte{':'})
		fieldName := strings.ToLower(strings.TrimSpace(string(field)))
		fieldValue := ""
		if hasValue {
			fieldValue = string(bytes.TrimPrefix(value, []byte{' '}))
		}
		switch fieldName {
		case "event":
			current.name = fieldValue
		case "data":
			current.dataLines = append(current.dataLines, fieldValue)
		}
	}
	flushCurrentEvent()
	return events
}

// upstreamErrorPayload probes one SSE data payload for the error fields used
// by known upstreams. Message and Code stay `any` because non-error stream
// events reuse the same keys with object values (e.g. Anthropic
// "message_start" carries a message object); those events must decode without
// error and simply not qualify as error payloads.
type upstreamErrorPayload struct {
	Type      string          `json:"type"`
	Message   any             `json:"message"`
	Msg       string          `json:"msg"`
	Code      any             `json:"code"`
	RequestID string          `json:"request_id"`
	Error     json.RawMessage `json:"error"`
}

type nestedUpstreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// extractDetailFromErrorPayload parses a single SSE data payload into an
// UpstreamErrorDetail. It accepts both provider styles: an Anthropic/OpenAI
// nested error object ({"type":"error","error":{...}} or {"error":{...}}) and
// the DashScope flat style (top-level message/code/request_id). Payloads with
// neither a message nor a code are not error payloads.
func extractDetailFromErrorPayload(payload []byte) (*types.UpstreamErrorDetail, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var parsed upstreamErrorPayload
	if err := common.Unmarshal(trimmed, &parsed); err != nil {
		return nil, false
	}
	detail := &types.UpstreamErrorDetail{RequestID: parsed.RequestID}
	if len(parsed.Error) > 0 && common.GetJsonType(parsed.Error) == "object" {
		var nested nestedUpstreamError
		if err := common.Unmarshal(parsed.Error, &nested); err == nil {
			detail.Message = nested.Message
			detail.Type = nested.Type
			detail.Code = scalarToString(nested.Code)
		}
	}
	if detail.Message == "" {
		if message, ok := parsed.Message.(string); ok {
			detail.Message = message
		} else if parsed.Msg != "" {
			detail.Message = parsed.Msg
		}
	}
	if detail.Code == "" {
		detail.Code = scalarToString(parsed.Code)
	}
	// A top-level type of "error" only marks an Anthropic error event payload
	// and adds no diagnostic information; provider class names (e.g. DashScope
	// "Arrearage") are kept.
	if detail.Type == "" && parsed.Type != "" && parsed.Type != "error" {
		detail.Type = parsed.Type
	}
	if detail.Message == "" && detail.Code == "" {
		return nil, false
	}
	return detail, true
}

func scalarToString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

// ExtractUpstreamErrorFromSSE recovers a structured error from an SSE-encoded
// upstream error body. Events named "error" are checked first; if none
// carries an error payload, the first other event with an error-shaped data
// payload wins. Returns nil when the body is not SSE or contains no
// recognizable error payload, so a normal success stream never produces a
// false positive.
func ExtractUpstreamErrorFromSSE(responseBody []byte) *types.UpstreamErrorDetail {
	events := parseSSEEvents(responseBody)
	if len(events) == 0 {
		return nil
	}
	for _, event := range events {
		if len(event.dataLines) == 0 || !strings.EqualFold(event.name, "error") {
			continue
		}
		if detail, ok := extractDetailFromErrorPayload([]byte(strings.Join(event.dataLines, "\n"))); ok {
			detail.PayloadFormat = UpstreamErrorPayloadFormatSSE
			return detail
		}
	}
	for _, event := range events {
		if len(event.dataLines) == 0 || strings.EqualFold(event.name, "error") {
			continue
		}
		if detail, ok := extractDetailFromErrorPayload([]byte(strings.Join(event.dataLines, "\n"))); ok {
			detail.PayloadFormat = UpstreamErrorPayloadFormatSSE
			return detail
		}
	}
	return nil
}
