package relayobserver

import (
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// This file implements the bounded canonical content normalizers (T2.2): the
// worker turns an already parsed request DTO into an ordered list of canonical
// items whose JSON payloads are stable, whitelist-only, and byte-capped.
//
// The whitelist follows the architecture SSOT (Incremental Conversation Model)
// and the forensics FINDINGS section 4: role, content type, text, tool call
// id/name/arguments, and tool result id/output are preserved; request options,
// tool definitions, auth data, provider headers, reasoning output, and user
// identifiers are stripped; inline media bytes, data URLs, file URLs, and
// unknown blocks become {kind, media_type, logical_bytes, hmac} metadata.
// The normalizer never stores authentication material, tool definitions,
// media bytes, URLs, or raw user identifiers.

// CanonicalKind is the frozen vocabulary of canonical item kinds consumed by
// the incremental storage phase (T2.3).
const (
	// CanonicalKindSystem marks the system prompt of a request (Responses
	// instructions or the Claude system field).
	CanonicalKindSystem = "system"
	// CanonicalKindMessage marks a role-bearing message with content parts.
	CanonicalKindMessage = "message"
	// CanonicalKindToolCall marks a standalone tool invocation item
	// (Responses function_call / custom_tool_call items).
	CanonicalKindToolCall = "tool_call"
	// CanonicalKindToolResult marks a standalone tool output item (Responses
	// function_call_output / custom_tool_call_output items).
	CanonicalKindToolResult = "tool_result"
	// CanonicalKindUnknown marks an unrecognized input item that is carried as
	// an explicit {kind, logical_bytes, hmac} gap instead of being forwarded
	// verbatim.
	CanonicalKindUnknown = "unknown"
	// CanonicalKindGap marks the truncated tail of an over-limit request. The
	// marker is data, not silent loss: it carries the dropped logical bytes and
	// the digest of the dropped tail start.
	CanonicalKindGap = "gap"
)

// partType is the frozen vocabulary of canonical content part types.
const (
	partTypeText       = "text"
	partTypeMedia      = "media"
	partTypeToolCall   = "tool_call"
	partTypeToolResult = "tool_result"
	partTypeUnknown    = "unknown"
)

// NormalizeOptions carries the per-event budget and the digest key into the
// normalizer. Reservation is the event's admission reservation (the request
// body size); MaxRequestBytes is the global RELAY_OBSERVER_MAX_REQUEST_BYTES
// cap. The effective canonical byte cap is min(reservation, maxRequestBytes).
// HMACKey is the observer key used for item digests; it must never be written
// into canonical output.
type NormalizeOptions struct {
	Reservation     int64
	MaxRequestBytes int64
	HMACKey         string
}

// NormalizeResult is the outcome of one normalization. ContentState reuses the
// frozen Event content states: ContentStateFull, ContentStateGap (unknown
// items or a truncated tail), or ContentStateMetadataOnly (fail-open: unknown
// format, nil request, or panic).
type NormalizeResult struct {
	Items          []CanonicalItem
	ContentState   string
	CanonicalBytes int64
	OmittedItems   int
}

// CanonicalItem is one ordered top-level message/item of a normalized request.
// Hmac is the keyed digest of the item's content layer (all fields except the
// digest itself), which is what T2.3 uses for content-object dedup.
type CanonicalItem struct {
	Kind         string          `json:"kind"`
	Role         string          `json:"role,omitempty"`
	ToolCallID   string          `json:"tool_call_id,omitempty"`
	Content      []CanonicalPart `json:"content,omitempty"`
	LogicalBytes int64           `json:"logical_bytes"`
	Hmac         string          `json:"hmac"`
	Truncated    bool            `json:"truncated,omitempty"`
}

// CanonicalPart is one whitelisted content part of a canonical item.
type CanonicalPart struct {
	Type         string         `json:"type"`
	Text         string         `json:"text,omitempty"`
	Media        *MediaRef      `json:"media,omitempty"`
	Call         *ToolCallRef   `json:"call,omitempty"`
	Result       *ToolResultRef `json:"result,omitempty"`
	LogicalBytes int64          `json:"logical_bytes,omitempty"`
	Hmac         string         `json:"hmac,omitempty"`
}

// MediaRef is the metadata replacement for inline media bytes, data URLs, and
// file URLs: kind, media type, the logical byte count of the original bytes,
// and the keyed digest of those bytes.
type MediaRef struct {
	Kind         string `json:"kind"`
	MediaType    string `json:"media_type,omitempty"`
	LogicalBytes int64  `json:"logical_bytes"`
	Hmac         string `json:"hmac"`
}

// ToolCallRef keeps exactly the whitelisted tool invocation fields:
// id, name, and arguments (arguments keeps its original JSON shape).
type ToolCallRef struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolResultRef keeps exactly the whitelisted tool output fields:
// the referenced tool call id and the output.
type ToolResultRef struct {
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
}

// NormalizeRequest normalizes one parsed request by relay format. It is
// strictly fail-open: unknown formats, nil requests, type mismatches, and
// panics degrade to a metadata-only result and never panic the worker.
func NormalizeRequest(relayFormat string, req dto.Request, opts NormalizeOptions) (res NormalizeResult) {
	defer func() {
		if r := recover(); r != nil {
			res = NormalizeResult{ContentState: ContentStateMetadataOnly}
		}
	}()
	if req == nil {
		return NormalizeResult{ContentState: ContentStateMetadataOnly}
	}

	var items []CanonicalItem
	hasGap := false
	switch relayFormat {
	case string(types.RelayFormatOpenAIResponses):
		if r, ok := req.(*dto.OpenAIResponsesRequest); ok {
			items, hasGap = normalizeResponses(r, opts)
		} else {
			return NormalizeResult{ContentState: ContentStateMetadataOnly}
		}
	case string(types.RelayFormatOpenAI):
		if r, ok := req.(*dto.GeneralOpenAIRequest); ok {
			items, hasGap = normalizeChat(r, opts)
		} else {
			return NormalizeResult{ContentState: ContentStateMetadataOnly}
		}
	case string(types.RelayFormatClaude):
		if r, ok := req.(*dto.ClaudeRequest); ok {
			items, hasGap = normalizeClaude(r, opts)
		} else {
			return NormalizeResult{ContentState: ContentStateMetadataOnly}
		}
	default:
		return NormalizeResult{ContentState: ContentStateMetadataOnly}
	}
	return finishNormalize(items, hasGap, opts)
}

// finishNormalize applies the canonical byte cap and assembles the result.
// The effective cap is min(reservation, maxRequestBytes); an over-limit tail
// is truncated and closed by an explicit gap marker that stays structurally
// valid JSON.
func finishNormalize(items []CanonicalItem, protocolGap bool, opts NormalizeOptions) NormalizeResult {
	limit := opts.Reservation
	if opts.MaxRequestBytes < limit {
		limit = opts.MaxRequestBytes
	}
	res := NormalizeResult{ContentState: ContentStateFull}
	var total int64
	var i int
	for ; i < len(items); i++ {
		payload, err := common.Marshal(items[i])
		if err != nil {
			return NormalizeResult{ContentState: ContentStateMetadataOnly}
		}
		if total+int64(len(payload)) > limit {
			break
		}
		res.Items = append(res.Items, items[i])
		total += int64(len(payload))
	}
	if i < len(items) {
		res.ContentState = ContentStateGap
		res.OmittedItems = len(items) - i
		var droppedLogical int64
		var droppedHmac string
		for j := i; j < len(items); j++ {
			droppedLogical += items[j].LogicalBytes
			if droppedHmac == "" {
				droppedHmac = items[j].Hmac
			}
		}
		gap := CanonicalItem{
			Kind:         CanonicalKindGap,
			LogicalBytes: droppedLogical,
			Hmac:         droppedHmac,
			Truncated:    true,
		}
		if gapPayload, err := common.Marshal(gap); err == nil && total+int64(len(gapPayload)) <= limit {
			res.Items = append(res.Items, gap)
			total += int64(len(gapPayload))
		}
	}
	if protocolGap {
		res.ContentState = ContentStateGap
	}
	res.CanonicalBytes = total
	return res
}

// normalizeResponses normalizes an OpenAI Responses request: instructions plus
// ordered input items. The input is json.RawMessage, so items are parsed with
// the map pattern of to_oai_chat_req.go; item and part types outside the
// whitelist degrade to explicit unknown gaps.
func normalizeResponses(req *dto.OpenAIResponsesRequest, opts NormalizeOptions) ([]CanonicalItem, bool) {
	items := make([]CanonicalItem, 0, 8)
	hasGap := false

	if len(req.Instructions) > 0 {
		if common.GetJsonType(req.Instructions) == "string" {
			var s string
			if err := common.Unmarshal(req.Instructions, &s); err == nil && s != "" {
				it := CanonicalItem{
					Kind:         CanonicalKindSystem,
					Content:      []CanonicalPart{{Type: partTypeText, Text: s}},
					LogicalBytes: int64(len(req.Instructions)),
				}
				items = append(items, withHmac(it, opts))
			} else {
				items = append(items, withHmac(unknownItem(req.Instructions), opts))
				hasGap = true
			}
		} else {
			items = append(items, withHmac(unknownItem(req.Instructions), opts))
			hasGap = true
		}
	}

	if len(req.Input) == 0 {
		return items, hasGap
	}
	switch common.GetJsonType(req.Input) {
	case "string":
		var s string
		if err := common.Unmarshal(req.Input, &s); err == nil && s != "" {
			it := CanonicalItem{
				Kind:         CanonicalKindMessage,
				Role:         "user",
				Content:      []CanonicalPart{{Type: partTypeText, Text: s}},
				LogicalBytes: int64(len(req.Input)),
			}
			items = append(items, withHmac(it, opts))
		} else {
			items = append(items, withHmac(unknownItem(req.Input), opts))
			hasGap = true
		}
	case "array":
		var rawItems []map[string]any
		if err := common.Unmarshal(req.Input, &rawItems); err != nil {
			items = append(items, withHmac(unknownItem(req.Input), opts))
			hasGap = true
			break
		}
		for _, item := range rawItems {
			it, gap := responsesItem(item, opts)
			items = append(items, it)
			if gap {
				hasGap = true
			}
		}
	default:
		items = append(items, withHmac(unknownItem(req.Input), opts))
		hasGap = true
	}
	return items, hasGap
}

// responsesItem normalizes one input item. Type names mirror
// to_oai_chat_req.go: function_call / custom_tool_call / function_call_output
// / custom_tool_call_output are tool items; an item without a type is a
// message; an item with an unknown type is carried as an explicit unknown gap
// instead of guessing its fields.
func responsesItem(item map[string]any, opts NormalizeOptions) (CanonicalItem, bool) {
	typ := strings.TrimSpace(str(item["type"]))
	switch typ {
	case "function_call":
		it := CanonicalItem{
			Kind: CanonicalKindToolCall,
			Content: []CanonicalPart{{
				Type: partTypeToolCall,
				Call: &ToolCallRef{ID: str(item["call_id"]), Name: str(item["name"]), Arguments: marshalRaw(item["arguments"])},
			}},
		}
		return finishItem(it, item, opts), false
	case "custom_tool_call":
		it := CanonicalItem{
			Kind: CanonicalKindToolCall,
			Content: []CanonicalPart{{
				Type: partTypeToolCall,
				Call: &ToolCallRef{ID: str(item["call_id"]), Name: str(item["name"]), Arguments: marshalRaw(item["input"])},
			}},
		}
		return finishItem(it, item, opts), false
	case "function_call_output", "custom_tool_call_output":
		it := CanonicalItem{
			Kind: CanonicalKindToolResult,
			Content: []CanonicalPart{{
				Type:   partTypeToolResult,
				Result: &ToolResultRef{ToolCallID: str(item["call_id"]), Output: marshalRaw(item["output"])},
			}},
		}
		return finishItem(it, item, opts), false
	}
	if typ != "" {
		// Unknown item type: carry it as an explicit gap instead of guessing
		// which fields are safe to forward.
		return withHmac(unknownItemFrom(item), opts), true
	}
	role := str(item["role"])
	if role == "" {
		role = "user"
	}
	it := CanonicalItem{Kind: CanonicalKindMessage, Role: role}
	hasGap := false
	switch c := item["content"].(type) {
	case nil:
	case string:
		if c != "" {
			it.Content = []CanonicalPart{{Type: partTypeText, Text: c}}
		}
	case []any:
		for _, p := range c {
			part, gap := responsesPart(p, opts)
			it.Content = append(it.Content, part)
			if gap {
				hasGap = true
			}
		}
	default:
		// Content of an unexpected shape (object, number, ...): explicit gap.
		rawBytes, err := common.Marshal(c)
		if err != nil {
			return withHmac(unknownItemFrom(item), opts), true
		}
		it.Content = []CanonicalPart{{Type: partTypeUnknown, LogicalBytes: int64(len(rawBytes)), Hmac: hmacDigest(rawBytes, opts.HMACKey)}}
		hasGap = true
	}
	return finishItem(it, item, opts), hasGap
}

// responsesPart normalizes one content part of a Responses item. Text parts
// keep their text; media parts become metadata; unknown part types become an
// explicit {type, logical_bytes, hmac} gap.
func responsesPart(p any, opts NormalizeOptions) (CanonicalPart, bool) {
	part, ok := p.(map[string]any)
	if !ok {
		rawBytes, err := common.Marshal(p)
		if err != nil {
			return CanonicalPart{Type: partTypeUnknown}, true
		}
		return CanonicalPart{Type: partTypeUnknown, LogicalBytes: int64(len(rawBytes)), Hmac: hmacDigest(rawBytes, opts.HMACKey)}, true
	}
	typ := strings.TrimSpace(str(part["type"]))
	switch typ {
	case "input_text", "output_text", "text":
		return CanonicalPart{Type: partTypeText, Text: str(part["text"])}, false
	case "input_image":
		rawBytes, mediaType := mediaFromResponsesImage(part)
		return mediaPart("input_image", rawBytes, mediaType, opts), false
	case "input_file":
		rawBytes, mediaType := mediaFromResponsesFile(part)
		return mediaPart("input_file", rawBytes, mediaType, opts), false
	case "input_audio":
		rawBytes, mediaType := mediaFromResponsesAudio(part)
		return mediaPart("input_audio", rawBytes, mediaType, opts), false
	case "input_video":
		rawBytes, mediaType := mediaFromResponsesVideo(part)
		return mediaPart("input_video", rawBytes, mediaType, opts), false
	default:
		rawBytes, err := common.Marshal(part)
		if err != nil {
			return CanonicalPart{Type: partTypeUnknown}, true
		}
		return CanonicalPart{Type: partTypeUnknown, LogicalBytes: int64(len(rawBytes)), Hmac: hmacDigest(rawBytes, opts.HMACKey)}, true
	}
}

// mediaFromResponsesImage extracts the original bytes and media type of an
// input_image part: image_url may be a string or an object with url.
func mediaFromResponsesImage(part map[string]any) (raw []byte, mediaType string) {
	if v, ok := part["image_url"]; ok {
		switch x := v.(type) {
		case string:
			return []byte(x), ""
		case map[string]any:
			return []byte(str(x["url"])), str(x["mime_type"])
		}
		return nil, ""
	}
	return []byte(str(part["url"])), str(part["mime_type"])
}

// mediaFromResponsesFile extracts the original bytes of an input_file part:
// file_url may be a string or an object with url.
func mediaFromResponsesFile(part map[string]any) (raw []byte, mediaType string) {
	if v, ok := part["file_url"]; ok {
		switch x := v.(type) {
		case string:
			return []byte(x), ""
		case map[string]any:
			return []byte(str(x["url"])), ""
		}
		return nil, ""
	}
	return []byte(str(part["file_url"])), ""
}

// mediaFromResponsesAudio extracts the original bytes and media type of an
// input_audio part from its data and format fields.
func mediaFromResponsesAudio(part map[string]any) (raw []byte, mediaType string) {
	if v, ok := part["input_audio"]; ok {
		if x, ok := v.(map[string]any); ok {
			return []byte(str(x["data"])), mediaTypeFromFormat(str(x["format"]))
		}
		return nil, ""
	}
	return nil, ""
}

// mediaFromResponsesVideo extracts the original bytes of an input_video part:
// video_url may be a string or an object with url.
func mediaFromResponsesVideo(part map[string]any) (raw []byte, mediaType string) {
	if v, ok := part["video_url"]; ok {
		switch x := v.(type) {
		case string:
			return []byte(x), ""
		case map[string]any:
			return []byte(str(x["url"])), ""
		}
		return nil, ""
	}
	return nil, ""
}

// normalizeChat normalizes an OpenAI Chat request: the ordered messages plus
// the tool calls attached to assistant messages. The DTO has a custom
// MarshalJSON, so the canonical objects are built field by field instead of
// marshaling the request or the message structs directly.
func normalizeChat(req *dto.GeneralOpenAIRequest, opts NormalizeOptions) ([]CanonicalItem, bool) {
	items := make([]CanonicalItem, 0, len(req.Messages))
	hasGap := false
	for _, msg := range req.Messages {
		parts, gap := chatMessageParts(msg, opts)
		it := CanonicalItem{
			Kind:         CanonicalKindMessage,
			Role:         msg.Role,
			ToolCallID:   msg.ToolCallId,
			Content:      parts,
			LogicalBytes: logicalBytesOf(msg),
		}
		items = append(items, withHmac(it, opts))
		if gap {
			hasGap = true
		}
	}
	return items, hasGap
}

// chatMessageParts builds the whitelisted content parts of one chat message:
// string content, media parts (struct or map form), tool calls, and explicit
// gaps for unexpected part shapes.
func chatMessageParts(msg dto.Message, opts NormalizeOptions) ([]CanonicalPart, bool) {
	parts := make([]CanonicalPart, 0, 4)
	hasGap := false
	switch content := msg.Content.(type) {
	case nil:
	case string:
		if content != "" {
			parts = append(parts, CanonicalPart{Type: partTypeText, Text: content})
		}
	case []any:
		for _, item := range content {
			switch v := item.(type) {
			case map[string]any:
				p, gap := chatPartFromMap(v, opts)
				parts = append(parts, p)
				if gap {
					hasGap = true
				}
			case dto.MediaContent:
				p, gap := chatPartFromMedia(v, opts)
				parts = append(parts, p)
				if gap {
					hasGap = true
				}
			default:
				rawBytes, err := common.Marshal(item)
				if err == nil {
					parts = append(parts, CanonicalPart{Type: partTypeUnknown, LogicalBytes: int64(len(rawBytes)), Hmac: hmacDigest(rawBytes, opts.HMACKey)})
					hasGap = true
				}
			}
		}
	case []dto.MediaContent:
		for _, v := range content {
			p, gap := chatPartFromMedia(v, opts)
			parts = append(parts, p)
			if gap {
				hasGap = true
			}
		}
	default:
		rawBytes, err := common.Marshal(content)
		if err == nil {
			parts = append(parts, CanonicalPart{Type: partTypeUnknown, LogicalBytes: int64(len(rawBytes)), Hmac: hmacDigest(rawBytes, opts.HMACKey)})
			hasGap = true
		}
	}
	if len(msg.ToolCalls) > 0 {
		var calls []dto.ToolCallRequest
		if err := common.Unmarshal(msg.ToolCalls, &calls); err == nil {
			for _, c := range calls {
				// Tool calls keep exactly id/name/arguments; description,
				// parameters, and custom payloads are dropped.
				parts = append(parts, CanonicalPart{
					Type: partTypeToolCall,
					Call: &ToolCallRef{ID: c.ID, Name: c.Function.Name, Arguments: rawString(c.Function.Arguments)},
				})
			}
		} else {
			rawBytes := msg.ToolCalls
			parts = append(parts, CanonicalPart{Type: partTypeUnknown, LogicalBytes: int64(len(rawBytes)), Hmac: hmacDigest(rawBytes, opts.HMACKey)})
			hasGap = true
		}
	}
	return parts, hasGap
}

// chatPartFromMap normalizes one media part in its map form, the shape the
// JSON decoder produces.
func chatPartFromMap(m map[string]any, opts NormalizeOptions) (CanonicalPart, bool) {
	switch str(m["type"]) {
	case partTypeText:
		return CanonicalPart{Type: partTypeText, Text: str(m["text"])}, false
	case dto.ContentTypeImageURL:
		rawBytes, mediaType := mediaFromChatImageURL(m["image_url"])
		return mediaPart(dto.ContentTypeImageURL, rawBytes, mediaType, opts), false
	case dto.ContentTypeInputAudio:
		rawBytes, mediaType := mediaFromChatInputAudio(m["input_audio"])
		return mediaPart(dto.ContentTypeInputAudio, rawBytes, mediaType, opts), false
	case dto.ContentTypeFile:
		rawBytes, mediaType := mediaFromChatFile(m["file"])
		return mediaPart(dto.ContentTypeFile, rawBytes, mediaType, opts), false
	case dto.ContentTypeVideoUrl:
		rawBytes, mediaType := mediaFromChatVideoURL(m["video_url"])
		return mediaPart(dto.ContentTypeVideoUrl, rawBytes, mediaType, opts), false
	default:
		rawBytes, err := common.Marshal(m)
		if err != nil {
			return CanonicalPart{Type: partTypeUnknown}, true
		}
		return CanonicalPart{Type: partTypeUnknown, LogicalBytes: int64(len(rawBytes)), Hmac: hmacDigest(rawBytes, opts.HMACKey)}, true
	}
}

// chatPartFromMedia normalizes one media part in its struct form, the shape
// produced by Message.SetMediaContent.
func chatPartFromMedia(m dto.MediaContent, opts NormalizeOptions) (CanonicalPart, bool) {
	switch m.Type {
	case partTypeText:
		return CanonicalPart{Type: partTypeText, Text: m.Text}, false
	case dto.ContentTypeImageURL:
		if img := m.GetImageMedia(); img != nil {
			return mediaPart(dto.ContentTypeImageURL, []byte(img.Url), img.MimeType, opts), false
		}
		return CanonicalPart{Type: partTypeUnknown}, true
	case dto.ContentTypeInputAudio:
		if a := m.GetInputAudio(); a != nil {
			return mediaPart(dto.ContentTypeInputAudio, []byte(a.Data), mediaTypeFromFormat(a.Format), opts), false
		}
		return CanonicalPart{Type: partTypeUnknown}, true
	case dto.ContentTypeFile:
		if f := m.GetFile(); f != nil {
			return mediaPart(dto.ContentTypeFile, []byte(f.FileData), "", opts), false
		}
		return CanonicalPart{Type: partTypeUnknown}, true
	case dto.ContentTypeVideoUrl:
		if v := m.GetVideoUrl(); v != nil {
			return mediaPart(dto.ContentTypeVideoUrl, []byte(v.Url), "", opts), false
		}
		return CanonicalPart{Type: partTypeUnknown}, true
	default:
		rawBytes, err := common.Marshal(m)
		if err != nil {
			return CanonicalPart{Type: partTypeUnknown}, true
		}
		return CanonicalPart{Type: partTypeUnknown, LogicalBytes: int64(len(rawBytes)), Hmac: hmacDigest(rawBytes, opts.HMACKey)}, true
	}
}

func mediaFromChatImageURL(v any) (raw []byte, mediaType string) {
	switch x := v.(type) {
	case string:
		return []byte(x), ""
	case map[string]any:
		return []byte(str(x["url"])), str(x["mime_type"])
	case *dto.MessageImageUrl:
		return []byte(x.Url), x.MimeType
	}
	return nil, ""
}

func mediaFromChatInputAudio(v any) (raw []byte, mediaType string) {
	switch x := v.(type) {
	case map[string]any:
		return []byte(str(x["data"])), mediaTypeFromFormat(str(x["format"]))
	case *dto.MessageInputAudio:
		return []byte(x.Data), mediaTypeFromFormat(x.Format)
	}
	return nil, ""
}

func mediaFromChatFile(v any) (raw []byte, mediaType string) {
	switch x := v.(type) {
	case map[string]any:
		return []byte(str(x["file_data"])), ""
	case *dto.MessageFile:
		return []byte(x.FileData), ""
	}
	return nil, ""
}

func mediaFromChatVideoURL(v any) (raw []byte, mediaType string) {
	switch x := v.(type) {
	case string:
		return []byte(x), ""
	case map[string]any:
		return []byte(str(x["url"])), ""
	case *dto.MessageVideoUrl:
		return []byte(x.Url), ""
	}
	return nil, ""
}

// normalizeClaude normalizes a Claude Messages request: system (string or
// blocks) plus ordered messages. Content blocks keep text and tool calls only
// (task T2.2); images become media metadata; thinking and unknown blocks are
// stripped. Metadata is never forwarded: it carries raw user identifiers.
func normalizeClaude(req *dto.ClaudeRequest, opts NormalizeOptions) ([]CanonicalItem, bool) {
	items := make([]CanonicalItem, 0, len(req.Messages)+1)
	hasGap := false

	if req.System != nil {
		parts, gap := claudeSystemParts(req.System, opts)
		if gap {
			hasGap = true
		}
		if len(parts) > 0 {
			it := CanonicalItem{Kind: CanonicalKindSystem, Content: parts, LogicalBytes: logicalBytesOf(req.System)}
			items = append(items, withHmac(it, opts))
		}
	}

	for _, msg := range req.Messages {
		it := CanonicalItem{Kind: CanonicalKindMessage, Role: msg.Role}
		switch content := msg.Content.(type) {
		case nil:
		case string:
			if content != "" {
				it.Content = []CanonicalPart{{Type: partTypeText, Text: content}}
			}
		case []any:
			for _, b := range content {
				part, gap := claudeBlockAny(b, opts)
				if gap {
					hasGap = true
				}
				if part.Type != "" {
					it.Content = append(it.Content, part)
				}
			}
		case []dto.ClaudeMediaMessage:
			for _, b := range content {
				part, gap := claudeBlockStruct(b, opts)
				if gap {
					hasGap = true
				}
				if part.Type != "" {
					it.Content = append(it.Content, part)
				}
			}
		}
		it.LogicalBytes = logicalBytesOf(msg)
		items = append(items, withHmac(it, opts))
	}
	return items, hasGap
}

// claudeSystemParts normalizes the Claude system field: a plain string, or a
// block list normalized with the same block rules as message content.
func claudeSystemParts(system any, opts NormalizeOptions) ([]CanonicalPart, bool) {
	parts := make([]CanonicalPart, 0, 2)
	hasGap := false
	switch s := system.(type) {
	case string:
		if s != "" {
			parts = append(parts, CanonicalPart{Type: partTypeText, Text: s})
		}
	case []any:
		for _, b := range s {
			part, gap := claudeBlockAny(b, opts)
			if gap {
				hasGap = true
			}
			if part.Type != "" {
				parts = append(parts, part)
			}
		}
	case []dto.ClaudeMediaMessage:
		for _, b := range s {
			part, gap := claudeBlockStruct(b, opts)
			if gap {
				hasGap = true
			}
			if part.Type != "" {
				parts = append(parts, part)
			}
		}
	}
	return parts, hasGap
}

// claudeBlockAny normalizes one Claude content block in either its map form
// (JSON decoder output) or its struct form.
func claudeBlockAny(b any, opts NormalizeOptions) (CanonicalPart, bool) {
	switch v := b.(type) {
	case map[string]any:
		return claudeBlock(v, opts)
	case dto.ClaudeMediaMessage:
		return claudeBlockStruct(v, opts)
	}
	return CanonicalPart{}, false
}

// claudeBlock normalizes one Claude content block in map form. Content blocks
// keep text and tool calls only; images become media metadata; thinking and
// unknown blocks are stripped.
func claudeBlock(m map[string]any, opts NormalizeOptions) (CanonicalPart, bool) {
	switch str(m["type"]) {
	case "text":
		return CanonicalPart{Type: partTypeText, Text: str(m["text"])}, false
	case "tool_use":
		return CanonicalPart{
			Type: partTypeToolCall,
			Call: &ToolCallRef{ID: str(m["id"]), Name: str(m["name"]), Arguments: marshalRaw(m["input"])},
		}, false
	case "tool_result":
		return CanonicalPart{
			Type:   partTypeToolResult,
			Result: &ToolResultRef{ToolCallID: str(m["tool_use_id"]), Output: claudeToolResultOutput(m["content"], opts)},
		}, false
	case "image":
		src, _ := m["source"].(map[string]any)
		if src == nil {
			return CanonicalPart{}, false
		}
		var rawBytes []byte
		if d := str(src["data"]); d != "" {
			rawBytes = []byte(d)
		} else {
			rawBytes = []byte(str(src["url"]))
		}
		if len(rawBytes) == 0 {
			return CanonicalPart{}, false
		}
		return mediaPart("image", rawBytes, str(src["media_type"]), opts), false
	default:
		// thinking and unknown blocks are not whitelisted for Claude.
		return CanonicalPart{}, false
	}
}

// claudeBlockStruct normalizes one Claude content block in struct form.
func claudeBlockStruct(b dto.ClaudeMediaMessage, opts NormalizeOptions) (CanonicalPart, bool) {
	switch b.Type {
	case "text":
		return CanonicalPart{Type: partTypeText, Text: b.GetText()}, false
	case "tool_use":
		return CanonicalPart{
			Type: partTypeToolCall,
			Call: &ToolCallRef{ID: b.Id, Name: b.Name, Arguments: marshalRaw(b.Input)},
		}, false
	case "tool_result":
		return CanonicalPart{
			Type:   partTypeToolResult,
			Result: &ToolResultRef{ToolCallID: b.ToolUseId, Output: claudeToolResultOutput(b.Content, opts)},
		}, false
	case "image":
		if b.Source == nil {
			return CanonicalPart{}, false
		}
		var rawBytes []byte
		if d := str(b.Source.Data); d != "" {
			rawBytes = []byte(d)
		} else {
			rawBytes = []byte(b.Source.Url)
		}
		if len(rawBytes) == 0 {
			return CanonicalPart{}, false
		}
		return mediaPart("image", rawBytes, b.Source.MediaType, opts), false
	default:
		return CanonicalPart{}, false
	}
}

// claudeToolResultOutput whitelists a tool_result content payload: a plain
// string passes through, a block list is normalized with the same rules as
// message content, anything else is dropped.
func claudeToolResultOutput(v any, opts NormalizeOptions) json.RawMessage {
	switch c := v.(type) {
	case nil:
		return nil
	case string:
		return rawString(c)
	case []any:
		parts := make([]CanonicalPart, 0, len(c))
		for _, b := range c {
			part, _ := claudeBlockAny(b, opts)
			if part.Type != "" {
				parts = append(parts, part)
			}
		}
		return marshalRaw(parts)
	case []dto.ClaudeMediaMessage:
		parts := make([]CanonicalPart, 0, len(c))
		for _, b := range c {
			part, _ := claudeBlockStruct(b, opts)
			if part.Type != "" {
				parts = append(parts, part)
			}
		}
		return marshalRaw(parts)
	}
	return nil
}

// mediaPart builds the {type: media, media: {kind, media_type, logical_bytes,
// hmac}} part for original bytes.
func mediaPart(kind string, rawBytes []byte, mediaType string, opts NormalizeOptions) CanonicalPart {
	return CanonicalPart{
		Type: partTypeMedia,
		Media: &MediaRef{
			Kind:         kind,
			MediaType:    mediaType,
			LogicalBytes: int64(len(rawBytes)),
			Hmac:         hmacDigest(rawBytes, opts.HMACKey),
		},
	}
}

// mediaTypeFromFormat maps a media format string to a MIME type.
func mediaTypeFromFormat(format string) string {
	if format == "" {
		return ""
	}
	return "audio/" + format
}

// withHmac computes the item digest over the item's content layer (the
// marshaled item with the digest field cleared) and returns the item with the
// digest set.
func withHmac(it CanonicalItem, opts NormalizeOptions) CanonicalItem {
	payload, err := common.Marshal(it)
	if err != nil {
		return it
	}
	it.Hmac = hmacDigest(payload, opts.HMACKey)
	return it
}

// finishItem sets the item's logical bytes and digest from its original map
// representation, which keeps the byte accounting independent of how the JSON
// input happened to be formatted.
func finishItem(it CanonicalItem, original map[string]any, opts NormalizeOptions) CanonicalItem {
	it.LogicalBytes = logicalBytesOf(original)
	return withHmac(it, opts)
}

// unknownItemFrom carries an unrecognized input item as an explicit gap; the
// digest is computed by withHmac over the canonical payload like every other
// item, while LogicalBytes keeps the original input size.
func unknownItemFrom(item map[string]any) CanonicalItem {
	rawBytes, err := common.Marshal(item)
	if err != nil {
		return CanonicalItem{Kind: CanonicalKindUnknown}
	}
	return CanonicalItem{Kind: CanonicalKindUnknown, LogicalBytes: int64(len(rawBytes))}
}

// unknownItem carries unrecognized raw input as an explicit gap.
func unknownItem(rawBytes []byte) CanonicalItem {
	return CanonicalItem{Kind: CanonicalKindUnknown, LogicalBytes: int64(len(rawBytes))}
}

// hmacDigest returns the hex keyed digest of raw; an absent key yields the
// empty digest instead of panicking (fail-open for the missing-key config).
func hmacDigest(rawBytes []byte, key string) string {
	if key == "" {
		return ""
	}
	return common.HmacSha256(string(rawBytes), key)
}

// logicalBytesOf reports the marshaled byte length of an original DTO value,
// used for gap accounting and truncation markers.
func logicalBytesOf(v any) int64 {
	b, err := common.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// marshalRaw marshals v into a raw JSON message, keeping its original type
// shape (a string stays a quoted string, an object stays an object).
func marshalRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := common.Marshal(v)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// rawString marshals a string into a quoted raw JSON message.
func rawString(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	b, err := common.Marshal(s)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// str extracts a string from a decoded JSON value; non-strings read as "".
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
