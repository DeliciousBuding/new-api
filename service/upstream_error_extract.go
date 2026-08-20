package service

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// maxUpstreamErrorBodyBytes bounds how much of an upstream error body the
// extractor scans. Upstream error bodies are untrusted input and are only
// needed for admin diagnostics, so truncation loses nothing actionable while
// capping the per-request parsing cost against line-dense malicious bodies.
const maxUpstreamErrorBodyBytes = 64 << 10

// Field-level bounds for what gets persisted. Messages may be arbitrarily
// long; codes/types/request IDs go into bounded columns (request_id maps to a
// varchar(128) logs column, so an oversized value would drop the whole row on
// strict-mode MySQL/PostgreSQL).
const (
	maxUpstreamErrorMessageBytes = 2048
	maxUpstreamErrorFieldBytes   = 128
)

// upstreamErrorFallbackMessage is used when a payload carries an explicit
// error marker but no usable message (mirrors the channel-test fallback).
const upstreamErrorFallbackMessage = "upstream returned error payload"

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
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    any             `json:"code"`
	Error   json.RawMessage `json:"error"`
}

// extractDetailFromErrorPayload parses a single SSE data payload into an
// UpstreamErrorDetail. It accepts the provider styles seen in practice: an
// OpenAI/Anthropic nested error object ({"error":{...}}), an error string
// ({"error":"..."}), a doubly nested message ({"error":{"error":{...}}}), and
// the DashScope flat style (top-level message/code/request_id). A payload
// with neither a message nor a code is only accepted when it carries an
// explicit `error` marker.
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

	if len(parsed.Error) > 0 {
		switch common.GetJsonType(parsed.Error) {
		case "object":
			var nested nestedUpstreamError
			if err := common.Unmarshal(parsed.Error, &nested); err == nil {
				detail.Message = nested.Message
				detail.Type = nested.Type
				detail.Code = scalarToString(nested.Code)
				if detail.Message == "" && len(nested.Error) > 0 {
					detail.Message = nestedErrorMessage(nested.Error)
				}
			}
		case "string":
			var errMessage string
			if err := common.Unmarshal(parsed.Error, &errMessage); err == nil {
				detail.Message = errMessage
			}
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
	// An explicit `error` marker qualifies the payload even when no message or
	// code could be extracted.
	if len(parsed.Error) > 0 && detail.Message == "" && detail.Code == "" {
		detail.Message = upstreamErrorFallbackMessage
	}
	if detail.Message == "" && detail.Code == "" {
		return nil, false
	}
	detail.Message = truncateBytes(detail.Message, maxUpstreamErrorMessageBytes)
	detail.Code = truncateBytes(detail.Code, maxUpstreamErrorFieldBytes)
	detail.Type = truncateBytes(detail.Type, maxUpstreamErrorFieldBytes)
	detail.RequestID = truncateBytes(detail.RequestID, maxUpstreamErrorFieldBytes)
	return detail, true
}

func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// nestedErrorMessage recovers a message from a doubly nested error shape
// ({"error":{"error":{...}}} or {"error":{"error":"..."}}).
func nestedErrorMessage(nestedError json.RawMessage) string {
	switch common.GetJsonType(nestedError) {
	case "string":
		var message string
		if err := common.Unmarshal(nestedError, &message); err == nil {
			return message
		}
	case "object":
		var deeper struct {
			Message string `json:"message"`
		}
		if err := common.Unmarshal(nestedError, &deeper); err == nil {
			return deeper.Message
		}
	}
	return ""
}

// hasErrorEnvelope reports whether a data payload carries an explicit error
// marker (an `error` key or a top-level type of "error"). It gates the
// unnamed-event fallback so a normal success chunk (e.g. a bare `code` field)
// is never misclassified as an error.
func hasErrorEnvelope(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var probe struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := common.Unmarshal(trimmed, &probe); err != nil {
		return false
	}
	if len(probe.Error) > 0 && common.GetJsonType(probe.Error) != "null" {
		return true
	}
	return strings.EqualFold(probe.Type, "error")
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
// carries an error payload, the first other event whose data payload has an
// explicit error envelope wins. Returns nil when the body is not SSE or
// contains no recognizable error payload, so a normal success stream never
// produces a false positive.
func ExtractUpstreamErrorFromSSE(responseBody []byte) *types.UpstreamErrorDetail {
	if len(responseBody) > maxUpstreamErrorBodyBytes {
		responseBody = responseBody[:maxUpstreamErrorBodyBytes]
	}
	responseBody = bytes.TrimPrefix(responseBody, []byte{0xEF, 0xBB, 0xBF})

	events := parseSSEEvents(responseBody)
	if len(events) == 0 {
		return nil
	}
	for _, event := range events {
		if len(event.dataLines) == 0 || !strings.EqualFold(event.name, "error") {
			continue
		}
		if detail, ok := extractDetailFromErrorPayload([]byte(strings.Join(event.dataLines, "\n"))); ok {
			return detail
		}
	}
	for _, event := range events {
		if len(event.dataLines) == 0 || strings.EqualFold(event.name, "error") {
			continue
		}
		payload := []byte(strings.Join(event.dataLines, "\n"))
		if !hasErrorEnvelope(payload) {
			continue
		}
		if detail, ok := extractDetailFromErrorPayload(payload); ok {
			return detail
		}
	}
	return nil
}

// FormatUpstreamErrorDetail renders the detail as a single log-friendly line.
// Control characters are escaped so upstream-controlled text cannot forge log
// lines.
func FormatUpstreamErrorDetail(detail *types.UpstreamErrorDetail) string {
	if detail == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if detail.Code != "" {
		parts = append(parts, "code="+sanitizeLogField(detail.Code))
	}
	if detail.Type != "" && detail.Type != detail.Code {
		parts = append(parts, "type="+sanitizeLogField(detail.Type))
	}
	if detail.RequestID != "" {
		parts = append(parts, "request_id="+sanitizeLogField(detail.RequestID))
	}
	if detail.Message != "" {
		parts = append(parts, "message="+sanitizeLogField(detail.Message))
	}
	return strings.Join(parts, ", ")
}

func sanitizeLogField(value string) string {
	if !strings.ContainsAny(value, "\r\n") {
		return value
	}
	return strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(value)
}
