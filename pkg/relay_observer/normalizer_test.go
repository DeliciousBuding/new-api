package relayobserver

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateGolden rewrites the golden fixture files in testdata/normalizer/. It is
// a fixture maintenance tool only: the default test run compares byte-for-byte
// against the committed files.
var updateGolden = flag.Bool("update", false, "rewrite testdata/normalizer golden files")

// testHMACKey is a synthetic key. Real observer keys live in configuration and
// must never appear in tests, fixtures, or output.
const testHMACKey = "observer-test-key"

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func ptrStr(s string) *string { return &s }

func ptrBool(b bool) *bool { return &b }

func ptrFloat(f float64) *float64 { return &f }

func ptrUint(u uint) *uint { return &u }

// canonicalLines renders a normalization result as one canonical JSON payload
// per line — the exact byte sequence T2.3 stores per content object.
func canonicalLines(items []CanonicalItem) (string, error) {
	var b strings.Builder
	for _, it := range items {
		p, err := common.Marshal(it)
		if err != nil {
			return "", err
		}
		b.Write(p)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

type fixtureCase struct {
	name   string
	format string
	req    dto.Request
	opts   NormalizeOptions
}

func roomyOpts() NormalizeOptions {
	return NormalizeOptions{CaptureLimit: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey}
}

func normalizerFixtures() []fixtureCase {
	// ---------- OpenAI Responses ----------
	responsesText := fixtureCase{
		name:   "responses_text",
		format: string(types.RelayFormatOpenAIResponses),
		req: &dto.OpenAIResponsesRequest{
			Model:        "gpt-5",
			Instructions: raw(`"Be brief."`),
			Input: raw(`[
				{"role":"user","content":[{"type":"input_text","text":"hello world"}]},
				{"role":"assistant","content":[{"type":"output_text","text":"hi there"}]}
			]`),
			Metadata:       raw(`{"user_id":"u-secret"}`),
			User:           raw(`"u-secret"`),
			Temperature:    ptrFloat(0.7),
			PromptCacheKey: raw(`"cache-9"`),
		},
		opts: roomyOpts(),
	}

	responsesTool := fixtureCase{
		name:   "responses_tool",
		format: string(types.RelayFormatOpenAIResponses),
		req: &dto.OpenAIResponsesRequest{
			Model: "gpt-5",
			Input: raw(`[
				{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"tokyo\"}"},
				{"type":"function_call_output","call_id":"call_1","output":"{\"temp\":22}"},
				{"type":"custom_tool_call","call_id":"call_2","name":"run_shell","input":{"cmd":"ls"}},
				{"type":"custom_tool_call_output","call_id":"call_2","output":"file1\n"},
				{"type":"computer_call","call_id":"call_9","action":"click"}
			]`),
			Tools: raw(`[{"type":"function","name":"get_weather","description":"weather lookup","parameters":{"type":"object"}}]`),
		},
		opts: roomyOpts(),
	}

	responsesMedia := fixtureCase{
		name:   "responses_media",
		format: string(types.RelayFormatOpenAIResponses),
		req: &dto.OpenAIResponsesRequest{
			Model: "gpt-5",
			Input: raw(`[
				{"role":"user","content":[
					{"type":"input_text","text":"describe this"},
					{"type":"input_image","image_url":{"url":"data:image/png;base64,aGVsbG8=","detail":"high"}},
					{"type":"input_file","file_url":"https://files.example.com/doc.pdf"},
					{"type":"input_audio","input_audio":{"data":"QUlGRkZGRg==","format":"wav"}},
					{"type":"input_video","video_url":{"url":"https://v.example.com/clip.mp4"}},
					{"type":"future_part","custom":"x"}
				]}
			]`),
		},
		opts: roomyOpts(),
	}

	responsesTruncation := fixtureCase{
		name:   "responses_truncation",
		format: string(types.RelayFormatOpenAIResponses),
		req: &dto.OpenAIResponsesRequest{
			Model: "gpt-5",
			Input: raw(`[
				{"role":"user","content":[{"type":"input_text","text":"alpha alpha alpha alpha"}]},
				{"role":"user","content":[{"type":"input_text","text":"bravo bravo bravo bravo"}]},
				{"role":"user","content":[{"type":"input_text","text":"charlie charlie charlie charlie"}]}
			]`),
		},
		opts: NormalizeOptions{CaptureLimit: 500, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey},
	}

	// ---------- OpenAI Chat ----------
	chatText := fixtureCase{
		name:   "chat_text",
		format: string(types.RelayFormatOpenAI),
		req: &dto.GeneralOpenAIRequest{
			Model: "gpt-5",
			Messages: []dto.Message{
				{Role: "system", Content: "you are a helpful bot"},
				{Role: "user", Content: "hello there"},
				{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "hi back"}}},
				{Role: "tool", ToolCallId: "call_1", Content: "42"},
			},
			User:        raw(`"u-secret"`),
			Metadata:    raw(`{"user_id":"u-secret"}`),
			Temperature: ptrFloat(0.7),
			MaxTokens:   ptrUint(1024),
		},
		opts: roomyOpts(),
	}

	chatTool := fixtureCase{
		name:   "chat_tool",
		format: string(types.RelayFormatOpenAI),
		req: &dto.GeneralOpenAIRequest{
			Model: "gpt-5",
			Messages: []dto.Message{
				{
					Role:      "assistant",
					ToolCalls: raw(`[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"tokyo\"}","description":"weather lookup","parameters":{"type":"object"}},"custom":{"secret":1}}]`),
				},
				{Role: "tool", ToolCallId: "call_1", Content: "22c"},
			},
			Tools: []dto.ToolCallRequest{{
				Type: "function",
				Function: dto.FunctionRequest{
					Name:        "get_weather",
					Description: "weather lookup",
					Parameters:  map[string]any{"type": "object"},
				},
			}},
		},
		opts: roomyOpts(),
	}

	chatMedia := fixtureCase{
		name:   "chat_media",
		format: string(types.RelayFormatOpenAI),
		req: &dto.GeneralOpenAIRequest{
			Model: "gpt-5",
			Messages: []dto.Message{
				{
					Role: "user",
					Content: []any{
						map[string]any{"type": "text", "text": "look"},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://img.example.com/a.png", "detail": "high"}},
						map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "QUlGRkZGRg==", "format": "wav"}},
						map[string]any{"type": "file", "file": map[string]any{"file_name": "a.txt", "file_data": "aGVsbG8=", "file_id": "file-1"}},
						map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://v.example.com/b.mp4"}},
					},
				},
			},
		},
		opts: roomyOpts(),
	}

	chatTruncation := fixtureCase{
		name:   "chat_truncation",
		format: string(types.RelayFormatOpenAI),
		req: &dto.GeneralOpenAIRequest{
			Model: "gpt-5",
			Messages: []dto.Message{
				{Role: "user", Content: "alpha alpha alpha alpha"},
				{Role: "user", Content: "bravo bravo bravo bravo"},
				{Role: "user", Content: "charlie charlie charlie charlie"},
			},
		},
		opts: NormalizeOptions{CaptureLimit: 500, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey},
	}

	// ---------- Claude Messages ----------
	claudeText := fixtureCase{
		name:   "claude_text",
		format: string(types.RelayFormatClaude),
		req: &dto.ClaudeRequest{
			Model:  "claude-opus-4",
			System: []any{map[string]any{"type": "text", "text": "system prompt"}},
			Messages: []dto.ClaudeMessage{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "hi"}}},
			},
			Metadata:    raw(`{"user_id":"u-secret","session_id":"s-9"}`),
			MaxTokens:   ptrUint(1024),
			Temperature: ptrFloat(0.7),
		},
		opts: roomyOpts(),
	}

	claudeTool := fixtureCase{
		name:   "claude_tool",
		format: string(types.RelayFormatClaude),
		req: &dto.ClaudeRequest{
			Model: "claude-opus-4",
			Messages: []dto.ClaudeMessage{
				{Role: "user", Content: []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content":     []any{map[string]any{"type": "text", "text": "result text"}},
				}}},
				{Role: "assistant", Content: []any{map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "get_weather",
					"input": map[string]any{"city": "tokyo"},
				}}},
			},
			Tools: []any{map[string]any{"name": "get_weather", "description": "weather lookup", "input_schema": map[string]any{"type": "object"}}},
		},
		opts: roomyOpts(),
	}

	claudeMedia := fixtureCase{
		name:   "claude_media",
		format: string(types.RelayFormatClaude),
		req: &dto.ClaudeRequest{
			Model: "claude-opus-4",
			Messages: []dto.ClaudeMessage{
				{Role: "user", Content: []any{
					map[string]any{"type": "text", "text": "what is this"},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": "LzlqLzRBQ=="}},
					map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://img.example.com/c.jpg"}},
				}},
			},
		},
		opts: roomyOpts(),
	}

	claudeTruncation := fixtureCase{
		name:   "claude_truncation",
		format: string(types.RelayFormatClaude),
		req: &dto.ClaudeRequest{
			Model: "claude-opus-4",
			Messages: []dto.ClaudeMessage{
				{Role: "user", Content: "alpha alpha alpha alpha"},
				{Role: "user", Content: "bravo bravo bravo bravo"},
				{Role: "user", Content: "charlie charlie charlie charlie"},
			},
		},
		opts: NormalizeOptions{CaptureLimit: 500, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey},
	}

	return []fixtureCase{
		responsesText, responsesTool, responsesMedia, responsesTruncation,
		chatText, chatTool, chatMedia, chatTruncation,
		claudeText, claudeTool, claudeMedia, claudeTruncation,
	}
}

// TestNormalizerGolden locks the canonical JSON byte output of the normalizer
// against committed fixtures: three protocols x text/tool/media/truncation
// shapes. A changed fixture or a changed normalizer both fail this test.
func TestNormalizerGolden(t *testing.T) {
	for _, tc := range normalizerFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			res := NormalizeRequest(tc.format, tc.req, tc.opts)
			got, err := canonicalLines(res.Items)
			require.NoError(t, err)

			goldenPath := filepath.Join("testdata", "normalizer", tc.name+".jsonl")
			if *updateGolden {
				require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
				require.NoError(t, os.WriteFile(goldenPath, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(goldenPath)
			require.NoError(t, err, "golden file missing; implementation must be reviewed before -update")
			// Fixtures are committed with LF line endings; git autocrlf may
			// present them as CRLF on Windows, so normalize before comparing.
			assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), got)
		})
	}
}

// TestNormalizerStable verifies the canonical output is byte-for-byte stable
// across repeated normalizations of the same parsed request.
func TestNormalizerStable(t *testing.T) {
	for _, tc := range normalizerFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			first := NormalizeRequest(tc.format, tc.req, tc.opts)
			second := NormalizeRequest(tc.format, tc.req, tc.opts)
			a, err := canonicalLines(first.Items)
			require.NoError(t, err)
			b, err := canonicalLines(second.Items)
			require.NoError(t, err)
			assert.Equal(t, a, b)
			assert.Equal(t, first.ContentState, second.ContentState)
		})
	}
}

// TestNormalizerTruncationBounds drives the canonical byte cap around its exact
// boundary: a limit that fits every item is full, one byte less truncates the
// tail and appends an explicit gap marker that still parses as JSON.
func TestNormalizerTruncationBounds(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{Role: "user", Content: "alpha"},
			{Role: "user", Content: "bravo"},
		},
	}
	opts := roomyOpts()
	full := NormalizeRequest(string(types.RelayFormatOpenAI), req, opts)
	require.Equal(t, ContentStateFull, full.ContentState)

	// Total canonical payload bytes including the trailing newline-equivalent
	// is not required: measure the payloads themselves.
	var total int64
	payloads := make([][]byte, 0, len(full.Items))
	for _, it := range full.Items {
		p, err := common.Marshal(it)
		require.NoError(t, err)
		total += int64(len(p))
		payloads = append(payloads, p)
	}
	require.Greater(t, int64(len(full.Items)), int64(1))

	cases := []struct {
		name      string
		limit     int64
		wantState string
		wantCount int
		wantGap   bool
	}{
		{name: "exact_fit", limit: total, wantState: ContentStateFull, wantCount: len(full.Items), wantGap: false},
		{name: "one_byte_short", limit: total - 1, wantState: ContentStateGap, wantCount: len(full.Items), wantGap: true},
		// The first item alone fits; the second does not, so the tail is
		// truncated even though no gap marker itself fits into the limit.
		{name: "first_item_only", limit: int64(len(payloads[0])), wantState: ContentStateGap, wantCount: 1, wantGap: false},
		{name: "below_first_item", limit: int64(len(payloads[0])) - 1, wantState: ContentStateGap, wantCount: 1, wantGap: true},
		{name: "zero_limit", limit: 0, wantState: ContentStateGap, wantCount: 0, wantGap: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := opts
			o.CaptureLimit = tc.limit
			o.MaxRequestBytes = tc.limit
			res := NormalizeRequest(string(types.RelayFormatOpenAI), req, o)
			assert.Equal(t, tc.wantState, res.ContentState)
			assert.Equal(t, tc.wantCount, len(res.Items))
			if tc.wantGap {
				require.NotEmpty(t, res.Items)
				last := res.Items[len(res.Items)-1]
				assert.Equal(t, CanonicalKindGap, last.Kind)
				assert.True(t, last.Truncated)
				assert.Equal(t, ContentStateGap, res.ContentState)
				// The gap marker is data, not silent loss: it carries the
				// original logical bytes of the dropped tail and a digest.
				assert.Greater(t, last.LogicalBytes, int64(0))
				assert.NotEmpty(t, last.Hmac)
				// Truncated output must stay structurally valid JSON.
				_, err := common.Marshal(res.Items)
				require.NoError(t, err)
				for _, it := range res.Items {
					var round CanonicalItem
					p, err := common.Marshal(it)
					require.NoError(t, err)
					require.NoError(t, common.Unmarshal(p, &round))
				}
			}
		})
	}
}

// TestNormalizerCanonicalLimitSources proves the effective cap is the minimum
// of the event reservation and MAX_REQUEST_BYTES: each bound alone must be able
// to truncate.
func TestNormalizerCanonicalLimitSources(t *testing.T) {
	req := &dto.ClaudeRequest{
		Model: "claude-opus-4",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "alpha alpha alpha alpha"},
			{Role: "user", Content: "bravo bravo bravo bravo"},
		},
	}
	small := int64(120)
	large := int64(1 << 20)

	byCaptureLimit := NormalizeRequest(string(types.RelayFormatClaude), req, NormalizeOptions{
		CaptureLimit: small, MaxRequestBytes: large, HMACKey: testHMACKey,
	})
	byMaxRequestBytes := NormalizeRequest(string(types.RelayFormatClaude), req, NormalizeOptions{
		CaptureLimit: large, MaxRequestBytes: small, HMACKey: testHMACKey,
	})
	both := NormalizeRequest(string(types.RelayFormatClaude), req, NormalizeOptions{
		CaptureLimit: small, MaxRequestBytes: small, HMACKey: testHMACKey,
	})

	assert.Equal(t, ContentStateGap, byCaptureLimit.ContentState)
	assert.Equal(t, ContentStateGap, byMaxRequestBytes.ContentState)
	assert.Equal(t, byCaptureLimit.Items, byMaxRequestBytes.Items)
	assert.Equal(t, byCaptureLimit.Items, both.Items)
	assert.Equal(t, byCaptureLimit.CanonicalBytes, byMaxRequestBytes.CanonicalBytes)
}

// TestNormalizerEnvelopeReservesGapMarker locks the P0-B envelope contract:
// the selection budget reserves the worst-case gap marker inside the capture
// limit. With an envelope, a truncating capture always closes with an
// explicit gap marker; without one, the first item can consume the whole
// budget and the marker silently disappears (the empty/gap-less failure mode
// the envelope closes).
func TestNormalizerEnvelopeReservesGapMarker(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{Role: "user", Content: strings.Repeat("alpha ", 40)},
			{Role: "user", Content: "bravo"},
		},
	}

	// Calibrate the budget on the actual canonical sizes: a limit that fits
	// the first item but not the item plus the gap marker.
	probe := NormalizeRequest(string(types.RelayFormatOpenAI), req, NormalizeOptions{
		CaptureLimit: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey,
	})
	require.Equal(t, ContentStateFull, probe.ContentState)
	firstBytes := len(mustMarshalForTest(t, probe.Items[0]))
	limit := int64(firstBytes) + 40 // item fits; item + marker (well over 40 B) does not

	// With the envelope the item is not selected, and the marker always
	// lands: the capture closes with data, not silent loss.
	withEnvelope := NormalizeOptions{
		CaptureLimit:            limit,
		MaxRequestBytes:         1 << 20,
		MinCaptureEnvelopeBytes: 256,
		HMACKey:                 testHMACKey,
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAI), req, withEnvelope)
	assert.Equal(t, ContentStateGap, res.ContentState)
	require.NotEmpty(t, res.Items, "the gap marker must always be written when truncation happens")
	last := res.Items[len(res.Items)-1]
	assert.Equal(t, CanonicalKindGap, last.Kind)
	assert.True(t, last.Truncated)
	assert.Greater(t, last.LogicalBytes, int64(0))
	assert.NotEmpty(t, last.Hmac)

	// Without an envelope, the first item eats the whole budget and the
	// marker does not fit: a truncation with no marker (silent loss).
	noEnvelope := NormalizeOptions{
		CaptureLimit:    limit,
		MaxRequestBytes: 1 << 20,
		HMACKey:         testHMACKey,
	}
	res2 := NormalizeRequest(string(types.RelayFormatOpenAI), req, noEnvelope)
	assert.Equal(t, ContentStateGap, res2.ContentState)
	require.NotEmpty(t, res2.Items, "the first item fits without an envelope")
	last2 := res2.Items[len(res2.Items)-1]
	assert.NotEqual(t, CanonicalKindGap, last2.Kind, "without an envelope the marker is not guaranteed")
}

// mustMarshalForTest marshals a canonical item for size calibration.
func mustMarshalForTest(t *testing.T, it CanonicalItem) []byte {
	t.Helper()
	p, err := common.Marshal(it)
	require.NoError(t, err)
	return p
}

// TestNormalizerWhitelist proves forbidden values never reach canonical
// output: user identifiers, metadata, tool definitions, request options,
// reasoning output, cache controls, and safety identifiers are stripped.
func TestNormalizerWhitelist(t *testing.T) {
	chat := &dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{Role: "user", Content: "hello", Name: ptrStr("alice"), Prefix: ptrBool(true), ReasoningContent: ptrStr("secret-reasoning-xyz")},
			{Role: "assistant", Content: "reply", Reasoning: ptrStr("secret-reasoning-xyz-2")},
		},
		Temperature:      ptrFloat(0.9),
		TopP:             ptrFloat(0.5),
		Tools:            []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "web_search", Description: "secret-desc-xyz", Parameters: map[string]any{"type": "object"}}}},
		ToolChoice:       "auto",
		User:             raw(`"alice-user-id-9"`),
		Metadata:         raw(`{"user_id":"user-id-9-secret"}`),
		SafetyIdentifier: raw(`"safety-id-9"`),
		Store:            raw(`true`),
		PromptCacheKey:   "cache-key-9",
		MaxTokens:        ptrUint(100),
		Seed:             ptrFloat(7),
	}
	responses := &dto.OpenAIResponsesRequest{
		Model:            "gpt-5",
		Input:            raw(`[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]`),
		Instructions:     raw(`"go"`),
		Metadata:         raw(`{"user_id":"user-id-9-secret"}`),
		ClientMetadata:   raw(`{"thread_id":"thread-id-9"}`),
		User:             raw(`"user-id-9-secret"`),
		PromptCacheKey:   raw(`"cache-key-9"`),
		SafetyIdentifier: raw(`"safety-id-9"`),
		Store:            raw(`true`),
		Temperature:      ptrFloat(0.9),
		Tools:            raw(`[{"type":"function","name":"web_search","description":"secret-desc-xyz","parameters":{"type":"object"}}]`),
		Text:             raw(`{"format":{"type":"text"}}`),
		Truncation:       raw(`"disabled"`),
	}
	claude := &dto.ClaudeRequest{
		Model:  "claude-opus-4",
		System: "system prompt",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: []any{map[string]any{"type": "thinking", "thinking": "secret-thought-xyz"}}},
		},
		Metadata:      raw(`{"user_id":"user-id-9-secret","session_id":"session-id-9"}`),
		Tools:         []any{map[string]any{"name": "web_search", "description": "secret-desc-xyz", "input_schema": map[string]any{"type": "object"}}},
		ToolChoice:    map[string]any{"type": "auto"},
		McpServers:    raw(`{"server-9-mcp":{"url":"https://mcp.example.com"}}`),
		MaxTokens:     ptrUint(1024),
		Temperature:   ptrFloat(0.7),
		TopP:          ptrFloat(0.5),
		CacheControl:  raw(`{"ephemeral":true}`),
		StopSequences: []string{"stop-seq-9"},
		Stream:        ptrBool(true),
	}

	opts := roomyOpts()
	cases := []struct {
		name string
		res  NormalizeResult
		// forbidden values that must never appear in canonical output; each is
		// unique and long enough that it cannot collide with an hmac hex
		forbidden []string
	}{
		{
			name: "chat",
			res:  NormalizeRequest(string(types.RelayFormatOpenAI), chat, opts),
			forbidden: []string{
				"alice-user-id-9", "user-id-9-secret", "safety-id-9", "cache-key-9",
				"secret-desc-xyz", "secret-reasoning-xyz", "secret-reasoning-xyz-2",
				"web_search",
			},
		},
		{
			name: "responses",
			res:  NormalizeRequest(string(types.RelayFormatOpenAIResponses), responses, opts),
			forbidden: []string{
				"user-id-9-secret", "thread-id-9", "safety-id-9", "cache-key-9",
				"secret-desc-xyz", "web_search",
			},
		},
		{
			name: "claude",
			res:  NormalizeRequest(string(types.RelayFormatClaude), claude, opts),
			forbidden: []string{
				"user-id-9-secret", "session-id-9", "secret-thought-xyz", "secret-desc-xyz",
				"web_search", "server-9-mcp", "mcp.example.com", "stop-seq-9",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := canonicalLines(tc.res.Items)
			require.NoError(t, err)
			for _, f := range tc.forbidden {
				assert.NotContains(t, out, f, "forbidden value leaked into canonical output")
			}
		})
	}
}

// TestNormalizerMediaRefs proves inline media bytes, data URLs, and file URLs
// are replaced by {kind, media_type, logical_bytes, hmac} metadata and never
// stored verbatim.
func TestNormalizerMediaRefs(t *testing.T) {
	const dataURL = "data:image/png;base64,aGVsbG8="
	const audioData = "QUlGRkZGRg=="
	const fileURL = "https://files.example.com/doc.pdf"
	req := &dto.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: raw(`[{"role":"user","content":[
			{"type":"input_image","image_url":"` + dataURL + `"},
			{"type":"input_audio","input_audio":{"data":"` + audioData + `","format":"wav"}},
			{"type":"input_file","file_url":"` + fileURL + `"}
		]}]`),
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAIResponses), req, roomyOpts())
	out, err := canonicalLines(res.Items)
	require.NoError(t, err)
	assert.NotContains(t, out, dataURL)
	assert.NotContains(t, out, audioData)
	assert.NotContains(t, out, fileURL)
	assert.Contains(t, out, `"kind":"input_image"`)
	assert.Contains(t, out, `"kind":"input_audio"`)
	assert.Contains(t, out, `"kind":"input_file"`)
	require.Len(t, res.Items, 1)
	require.Len(t, res.Items[0].Content, 3)
	for _, part := range res.Items[0].Content {
		require.NotNil(t, part.Media)
		assert.Equal(t, partTypeMedia, part.Type)
		assert.Greater(t, part.Media.LogicalBytes, int64(0))
		assert.NotEmpty(t, part.Media.Hmac)
	}
	// The hmac is the keyed digest of the exact original bytes.
	assert.Equal(t, common.HmacSha256(dataURL, testHMACKey), res.Items[0].Content[0].Media.Hmac)
	assert.Equal(t, common.HmacSha256(audioData, testHMACKey), res.Items[0].Content[1].Media.Hmac)
	assert.Equal(t, common.HmacSha256(fileURL, testHMACKey), res.Items[0].Content[2].Media.Hmac)
}

// TestNormalizerUnknownResponsesItem proves unknown Responses item and part
// types fall back to an explicit {kind, logical_bytes, hmac} gap instead of
// being forwarded verbatim.
func TestNormalizerUnknownResponsesItem(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: raw(`[
			{"type":"computer_call","call_id":"call_9","action":"click"},
			{"role":"user","content":[{"type":"future_part","custom":"x"}]}
		]`),
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAIResponses), req, roomyOpts())
	require.Len(t, res.Items, 2)

	item := res.Items[0]
	assert.Equal(t, CanonicalKindUnknown, item.Kind)
	assert.Empty(t, item.Content)
	assert.Greater(t, item.LogicalBytes, int64(0))
	assert.NotEmpty(t, item.Hmac)

	part := res.Items[1]
	assert.Equal(t, CanonicalKindMessage, part.Kind)
	require.Len(t, part.Content, 1)
	assert.Equal(t, partTypeUnknown, part.Content[0].Type)
	assert.Greater(t, part.Content[0].LogicalBytes, int64(0))
	assert.NotEmpty(t, part.Content[0].Hmac)

	// Explicit gap: the state tells the consumer the capture has a hole.
	assert.Equal(t, ContentStateGap, res.ContentState)

	// The output stays structurally valid JSON either way.
	_, err := common.Marshal(res.Items)
	require.NoError(t, err)
}

// TestNormalizerToolCallWhitelist proves tool calls keep exactly
// id/name/arguments (or id/output for results) and drop custom payloads,
// descriptions, and parameter schemas.
func TestNormalizerToolCallWhitelist(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{
				Role:      "assistant",
				ToolCalls: raw(`[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"tokyo\"}","description":"secret-desc-xyz","parameters":{"type":"object"}},"custom":{"secret-custom-xyz":1}}]`),
			},
			{Role: "tool", ToolCallId: "call_1", Content: "22c"},
		},
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAI), req, roomyOpts())
	out, err := canonicalLines(res.Items)
	require.NoError(t, err)
	assert.Contains(t, out, `"call":{"id":"call_1","name":"get_weather"`)
	assert.Contains(t, out, `"arguments":"{\"city\":\"tokyo\"}"`)
	assert.Contains(t, out, `"tool_call_id":"call_1"`)
	assert.NotContains(t, out, "secret-desc-xyz")
	assert.NotContains(t, out, "secret-custom-xyz")
	assert.NotContains(t, out, `"parameters"`)
	require.Len(t, res.Items, 2)
	require.Len(t, res.Items[0].Content, 1)
	require.NotNil(t, res.Items[0].Content[0].Call)
	assert.Equal(t, "call_1", res.Items[0].Content[0].Call.ID)
	assert.Equal(t, "get_weather", res.Items[0].Content[0].Call.Name)
}

// TestNormalizerClaudeStructBlocks proves the struct form of Claude system and
// content blocks (ClaudeMediaMessage values) normalizes exactly like the map
// form used by the JSON decoder.
func TestNormalizerClaudeStructBlocks(t *testing.T) {
	req := &dto.ClaudeRequest{
		Model:  "claude-opus-4",
		System: []dto.ClaudeMediaMessage{{Type: "text", Text: ptrStr("system prompt")}},
		Messages: []dto.ClaudeMessage{
			{
				Role: "user",
				Content: []dto.ClaudeMediaMessage{
					{Type: "text", Text: ptrStr("hello")},
					{Type: "image", Source: &dto.ClaudeMessageSource{Type: "base64", MediaType: "image/jpeg", Data: "LzlqLzRBQ=="}},
					{Type: "tool_use", Id: "toolu_1", Name: "get_weather", Input: map[string]any{"city": "tokyo"}},
					{Type: "tool_result", ToolUseId: "toolu_1", Content: "result"},
					{Type: "thinking", Thinking: ptrStr("secret-thought-xyz")},
				},
			},
		},
	}
	res := NormalizeRequest(string(types.RelayFormatClaude), req, roomyOpts())
	require.Len(t, res.Items, 2)
	assert.Equal(t, CanonicalKindSystem, res.Items[0].Kind)
	require.Len(t, res.Items[0].Content, 1)
	assert.Equal(t, "system prompt", res.Items[0].Content[0].Text)

	msg := res.Items[1]
	require.Len(t, msg.Content, 4) // text, media, tool_call, tool_result; thinking dropped
	assert.Equal(t, partTypeText, msg.Content[0].Type)
	assert.Equal(t, partTypeMedia, msg.Content[1].Type)
	assert.Equal(t, "image/jpeg", msg.Content[1].Media.MediaType)
	assert.Equal(t, partTypeToolCall, msg.Content[2].Type)
	assert.Equal(t, "toolu_1", msg.Content[2].Call.ID)
	assert.Equal(t, partTypeToolResult, msg.Content[3].Type)
	assert.Equal(t, "toolu_1", msg.Content[3].Result.ToolCallID)
	out, err := canonicalLines(res.Items)
	require.NoError(t, err)
	assert.NotContains(t, out, "secret-thought-xyz")
	assert.NotContains(t, out, "LzlqLzRBQ==")
}

// TestNormalizerChatMediaStructFull proves every struct-form chat media part
// (the shape produced by Message.SetMediaContent) normalizes to media
// metadata, and unknown chat part types become explicit gaps.
func TestNormalizerChatMediaStructFull(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{
				Role: "user",
				Content: []dto.MediaContent{
					{Type: "text", Text: "t"},
					{Type: "image_url", ImageUrl: &dto.MessageImageUrl{Url: "https://img.example.com/a.png", MimeType: "image/png"}},
					{Type: "input_audio", InputAudio: &dto.MessageInputAudio{Data: "QUlGRkZGRg==", Format: "wav"}},
					{Type: "file", File: &dto.MessageFile{FileName: "a.txt", FileData: "aGVsbG8=", FileId: "file-1"}},
					{Type: "video_url", VideoUrl: &dto.MessageVideoUrl{Url: "https://v.example.com/b.mp4"}},
					{Type: "mystery_part", Text: "x"},
				},
			},
		},
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAI), req, roomyOpts())
	require.Len(t, res.Items, 1)
	require.Len(t, res.Items[0].Content, 6)
	assert.Equal(t, partTypeText, res.Items[0].Content[0].Type)
	assert.Equal(t, "image/png", res.Items[0].Content[1].Media.MediaType)
	assert.Equal(t, "audio/wav", res.Items[0].Content[2].Media.MediaType)
	assert.Equal(t, int64(8), res.Items[0].Content[3].Media.LogicalBytes) // len("aGVsbG8=")
	assert.Equal(t, int64(27), res.Items[0].Content[4].Media.LogicalBytes)
	assert.Equal(t, partTypeUnknown, res.Items[0].Content[5].Type)
	assert.Equal(t, ContentStateGap, res.ContentState)
	out, err := canonicalLines(res.Items)
	require.NoError(t, err)
	assert.NotContains(t, out, "img.example.com")
	assert.NotContains(t, out, "v.example.com")
	assert.NotContains(t, out, "a.txt")
	assert.NotContains(t, out, "file-1")
}

// TestNormalizerResponsesInputShapes proves the remaining Responses input
// shapes: a plain string input, an unparseable input, an object-shaped input,
// an item with content of an unexpected shape, and a role-only item.
func TestNormalizerResponsesInputShapes(t *testing.T) {
	opts := roomyOpts()

	stringInput := &dto.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: raw(`"just a prompt"`),
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAIResponses), stringInput, opts)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "user", res.Items[0].Role)
	require.Len(t, res.Items[0].Content, 1)
	assert.Equal(t, "just a prompt", res.Items[0].Content[0].Text)
	assert.Equal(t, ContentStateFull, res.ContentState)

	// Unparseable input and object-shaped input degrade to explicit gaps.
	unparseable := &dto.OpenAIResponsesRequest{Model: "gpt-5", Input: raw(`[{"role":`)}
	res = NormalizeRequest(string(types.RelayFormatOpenAIResponses), unparseable, opts)
	require.Len(t, res.Items, 1)
	assert.Equal(t, CanonicalKindUnknown, res.Items[0].Kind)
	assert.Equal(t, ContentStateGap, res.ContentState)

	objectInput := &dto.OpenAIResponsesRequest{Model: "gpt-5", Input: raw(`{"type":"input_text","text":"x"}`)}
	res = NormalizeRequest(string(types.RelayFormatOpenAIResponses), objectInput, opts)
	require.Len(t, res.Items, 1)
	assert.Equal(t, CanonicalKindUnknown, res.Items[0].Kind)
	assert.Equal(t, ContentStateGap, res.ContentState)

	// A content value of an unexpected shape is an explicit part gap.
	weirdContent := &dto.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: raw(`[{"role":"user","content":{"weird":true}},{"role":"assistant"}]`),
	}
	res = NormalizeRequest(string(types.RelayFormatOpenAIResponses), weirdContent, opts)
	require.Len(t, res.Items, 2)
	assert.Equal(t, ContentStateGap, res.ContentState)
	require.Len(t, res.Items[0].Content, 1)
	assert.Equal(t, partTypeUnknown, res.Items[0].Content[0].Type)
	// The role-only item is an empty, valid message.
	assert.Empty(t, res.Items[1].Content)
	assert.Equal(t, "assistant", res.Items[1].Role)
}

// TestNormalizerResponsesMediaObjectForms proves file_url and video_url accept
// both their string and object forms.
func TestNormalizerResponsesMediaObjectForms(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: raw(`[{"role":"user","content":[
			{"type":"input_file","file_url":{"url":"https://files.example.com/doc.pdf"}},
			{"type":"input_video","video_url":{"url":"https://v.example.com/clip.mp4"}}
		]}]`),
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAIResponses), req, roomyOpts())
	require.Len(t, res.Items, 1)
	require.Len(t, res.Items[0].Content, 2)
	assert.Equal(t, int64(33), res.Items[0].Content[0].Media.LogicalBytes)
	assert.Equal(t, int64(30), res.Items[0].Content[1].Media.LogicalBytes)
	out, err := canonicalLines(res.Items)
	require.NoError(t, err)
	assert.NotContains(t, out, "files.example.com")
	assert.NotContains(t, out, "v.example.com")
}

// TestNormalizerClaudeStructNested proves struct-form Claude blocks nested in
// []any content and in tool_result outputs normalize like their map form.
func TestNormalizerClaudeStructNested(t *testing.T) {
	req := &dto.ClaudeRequest{
		Model: "claude-opus-4",
		Messages: []dto.ClaudeMessage{
			{
				Role: "user",
				Content: []any{
					dto.ClaudeMediaMessage{Type: "text", Text: ptrStr("hello")},
					dto.ClaudeMediaMessage{Type: "tool_result", ToolUseId: "toolu_1", Content: []dto.ClaudeMediaMessage{{Type: "text", Text: ptrStr("nested")}}},
				},
			},
			{Role: "assistant", Content: []any{dto.ClaudeMediaMessage{Type: "tool_use", Id: "toolu_1", Name: "get_weather", Input: map[string]any{"city": "tokyo"}}}},
		},
	}
	res := NormalizeRequest(string(types.RelayFormatClaude), req, roomyOpts())
	require.Len(t, res.Items, 2)
	user := res.Items[0]
	require.Len(t, user.Content, 2)
	assert.Equal(t, partTypeText, user.Content[0].Type)
	assert.Equal(t, partTypeToolResult, user.Content[1].Type)
	require.NotNil(t, user.Content[1].Result)
	assert.Equal(t, "toolu_1", user.Content[1].Result.ToolCallID)
	// The nested tool_result output is itself a whitelisted canonical part.
	assert.Contains(t, string(user.Content[1].Result.Output), `"text":"nested"`)
	assistant := res.Items[1]
	require.Len(t, assistant.Content, 1)
	assert.Equal(t, "get_weather", assistant.Content[0].Call.Name)
}

// TestNormalizerMalformedChatProves malformed tool calls and non-list content
// shapes degrade to explicit gaps instead of failing or leaking.
func TestNormalizerMalformedChat(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{Role: "assistant", ToolCalls: raw(`[{"id":`), Content: "reply"},
			{Role: "user", Content: map[string]any{"type": "text", "text": "map-shaped"}},
		},
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAI), req, roomyOpts())
	require.Len(t, res.Items, 2)
	assert.Equal(t, ContentStateGap, res.ContentState)
	require.Len(t, res.Items[0].Content, 2) // text part + unknown gap for the malformed tool calls
	assert.Equal(t, partTypeText, res.Items[0].Content[0].Type)
	assert.Equal(t, partTypeUnknown, res.Items[0].Content[0+1].Type)
	require.Len(t, res.Items[1].Content, 1)
	assert.Equal(t, partTypeUnknown, res.Items[1].Content[0].Type)
}

// TestNormalizerClaudeSystemForms proves the Claude system field normalizes in
// its string, block-map, and block-struct forms.
func TestNormalizerClaudeSystemForms(t *testing.T) {
	opts := roomyOpts()
	forms := []dto.Request{
		&dto.ClaudeRequest{Model: "claude-opus-4", System: "plain string", Messages: []dto.ClaudeMessage{{Role: "user", Content: "hi"}}},
		&dto.ClaudeRequest{Model: "claude-opus-4", System: []any{map[string]any{"type": "text", "text": "map block"}}, Messages: []dto.ClaudeMessage{{Role: "user", Content: "hi"}}},
		&dto.ClaudeRequest{Model: "claude-opus-4", System: []dto.ClaudeMediaMessage{{Type: "text", Text: ptrStr("struct block")}}, Messages: []dto.ClaudeMessage{{Role: "user", Content: "hi"}}},
	}
	for i, req := range forms {
		res := NormalizeRequest(string(types.RelayFormatClaude), req, opts)
		require.Len(t, res.Items, 2, "form %d", i)
		assert.Equal(t, CanonicalKindSystem, res.Items[0].Kind)
		require.Len(t, res.Items[0].Content, 1)
		assert.Equal(t, partTypeText, res.Items[0].Content[0].Type)
		assert.NotEmpty(t, res.Items[0].Content[0].Text)
	}
}

// TestNormalizerFailOpen proves every normalizer entry point degrades to a
// metadata-only result instead of panicking: unknown formats and nil requests
// never crash the worker.
func TestNormalizerFailOpen(t *testing.T) {
	opts := roomyOpts()
	for _, format := range []string{"bogus", "openai_image"} {
		t.Run("unknown_format_"+format, func(t *testing.T) {
			res := NormalizeRequest(format, &dto.GeneralOpenAIRequest{Model: "gpt-5"}, opts)
			assert.Equal(t, ContentStateMetadataOnly, res.ContentState)
			assert.Empty(t, res.Items)
		})
	}
	for _, format := range []string{"openai", "openai_responses", "claude"} {
		t.Run("nil_request_"+format, func(t *testing.T) {
			res := NormalizeRequest(format, nil, opts)
			assert.Equal(t, ContentStateMetadataOnly, res.ContentState)
			assert.Empty(t, res.Items)
		})
	}
}

// TestNormalizerEmptyInput proves an empty content request is a valid full
// capture with no items.
func TestNormalizerEmptyInput(t *testing.T) {
	opts := roomyOpts()
	reqs := []struct {
		format string
		req    dto.Request
	}{
		{string(types.RelayFormatOpenAI), &dto.GeneralOpenAIRequest{Model: "gpt-5"}},
		{string(types.RelayFormatOpenAIResponses), &dto.OpenAIResponsesRequest{Model: "gpt-5", Input: raw(`[]`)}},
		{string(types.RelayFormatClaude), &dto.ClaudeRequest{Model: "claude-opus-4"}},
	}
	for _, r := range reqs {
		res := NormalizeRequest(r.format, r.req, opts)
		assert.Equal(t, ContentStateFull, res.ContentState)
		assert.Empty(t, res.Items)
		assert.Zero(t, res.CanonicalBytes)
	}
}

// TestNormalizerResponsesInstructionsNonString proves non-string instructions
// degrade to an explicit unknown gap instead of being forwarded or dropped
// silently.
func TestNormalizerResponsesInstructionsNonString(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model:        "gpt-5",
		Instructions: raw(`{"format":{"type":"text"}}`),
		Input:        raw(`[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]`),
	}
	res := NormalizeRequest(string(types.RelayFormatOpenAIResponses), req, roomyOpts())
	require.Len(t, res.Items, 2)
	assert.Equal(t, CanonicalKindUnknown, res.Items[0].Kind)
	assert.Equal(t, ContentStateGap, res.ContentState)
}

// TestNormalizerItemHMACMatches proves every item digest is the keyed HMAC of
// its own canonical content layer — the digest T2.3 uses for content dedup.
// Gap markers carry their own content digest too (never a dropped item's
// digest, which would collide with that item's content object).
func TestNormalizerItemHMACMatches(t *testing.T) {
	for _, tc := range normalizerFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			res := NormalizeRequest(tc.format, tc.req, tc.opts)
			for _, it := range res.Items {
				// The digest covers the content layer, so the hmac field is
				// cleared before re-marshaling for the comparison.
				core := it
				core.Hmac = ""
				p, err := common.Marshal(core)
				require.NoError(t, err)
				assert.Equal(t, common.HmacSha256(string(p), testHMACKey), it.Hmac)
			}
		})
	}
}

// TestNormalizerKindContract locks the canonical kind vocabulary that T2.3
// consumes; adding a kind requires updating this map in the same change.
func TestNormalizerKindContract(t *testing.T) {
	expected := map[string]string{
		CanonicalKindSystem:     "system",
		CanonicalKindMessage:    "message",
		CanonicalKindToolCall:   "tool_call",
		CanonicalKindToolResult: "tool_result",
		CanonicalKindUnknown:    "unknown",
		CanonicalKindGap:        "gap",
	}
	for kind, want := range expected {
		assert.Equal(t, want, string(kind))
	}
	assert.Len(t, expected, 6)
}

// TestNormalizerCanonicalBytesAccounting proves CanonicalBytes reports the sum
// of the emitted item payloads, gap markers included.
func TestNormalizerCanonicalBytesAccounting(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: "gpt-5",
		Messages: []dto.Message{
			{Role: "user", Content: "alpha alpha alpha alpha"},
			{Role: "user", Content: "bravo bravo bravo bravo"},
		},
	}
	opts := roomyOpts()
	opts.CaptureLimit = 120
	res := NormalizeRequest(string(types.RelayFormatOpenAI), req, opts)
	assert.Equal(t, ContentStateGap, res.ContentState)
	var sum int64
	for _, it := range res.Items {
		p, err := common.Marshal(it)
		require.NoError(t, err)
		sum += int64(len(p))
	}
	assert.Equal(t, sum, res.CanonicalBytes)
	assert.LessOrEqual(t, res.CanonicalBytes, opts.CaptureLimit)

	full := NormalizeRequest(string(types.RelayFormatOpenAI), req, roomyOpts())
	require.NotEqual(t, len(full.Items), len(res.Items))
}

// TestNormalizerItemsCap proves the Runtime Limits per-request item cap (2048
// normalized items, hard 4096) is enforced on every protocol: an over-limit
// request truncates the tail into one explicit gap marker instead of building
// an unbounded item list.
func TestNormalizerItemsCap(t *testing.T) {
	// SSOT Runtime Limits: normalized items/request, default 2048.
	const maxItems = 2048
	messages := make([]dto.Message, 0, maxItems+2)
	for i := 0; i < maxItems+2; i++ {
		messages = append(messages, dto.Message{Role: "user", Content: "x"})
	}
	claudeMessages := make([]dto.ClaudeMessage, 0, maxItems+2)
	for i := 0; i < maxItems+2; i++ {
		claudeMessages = append(claudeMessages, dto.ClaudeMessage{Role: "user", Content: "x"})
	}
	responsesItems := `[{"role":"user","content":[{"type":"input_text","text":"x"}]},`
	responsesItems += strings.Repeat(`{"role":"user","content":[{"type":"input_text","text":"x"}]},`, maxItems)
	responsesItems = responsesItems[:len(responsesItems)-1] + "]"

	cases := []struct {
		name   string
		format string
		req    dto.Request
	}{
		{"chat", string(types.RelayFormatOpenAI), &dto.GeneralOpenAIRequest{Model: "gpt-5", Messages: messages}},
		{"claude", string(types.RelayFormatClaude), &dto.ClaudeRequest{Model: "claude-opus-4", Messages: claudeMessages}},
		{"responses", string(types.RelayFormatOpenAIResponses), &dto.OpenAIResponsesRequest{Model: "gpt-5", Input: raw(responsesItems)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := NormalizeRequest(tc.format, tc.req, roomyOpts())
			require.Equal(t, ContentStateGap, res.ContentState)
			// 2048 kept items plus at most one gap marker.
			require.LessOrEqual(t, len(res.Items), maxItems+1)
			last := res.Items[len(res.Items)-1]
			require.Equal(t, CanonicalKindGap, last.Kind)
			assert.True(t, last.Truncated)
			assert.Greater(t, last.LogicalBytes, int64(0))
			assert.NotEmpty(t, last.Hmac)
		})
	}
}

// TestNormalizerStabilityOfHmacKeyAbsence proves an empty HMAC key produces a
// deterministic empty digest instead of a panic (fail-open for the missing-key
// configuration).
func TestNormalizerHmacKeyAbsent(t *testing.T) {
	req := &dto.ClaudeRequest{Model: "claude-opus-4", Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}}}
	opts := roomyOpts()
	opts.HMACKey = ""
	res := NormalizeRequest(string(types.RelayFormatClaude), req, opts)
	require.Len(t, res.Items, 1)
	assert.Empty(t, res.Items[0].Hmac)
	assert.Equal(t, ContentStateFull, res.ContentState)
}
