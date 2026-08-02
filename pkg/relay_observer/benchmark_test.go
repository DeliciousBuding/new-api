package relayobserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// This file holds the T5.2 observer benchmarks over a fixed in-memory corpus
// (no database, no network, no disk writes). They run with -benchtime=200x
// per the fixed-corpus policy: b.N stays small and deterministic, so the
// numbers are comparable across runs and machines. Every benchmark asserts
// its own invariant inside the loop so a silently broken path fails the
// benchmark instead of timing an empty fast path.

// largeRequestItems is the message/item count of the "large request"
// benchmarks: well below maxNormalizedItems (2048) but a realistic hot-path
// size.
const largeRequestItems = 200

// benchmarkOpts is the shared per-event budget of the benchmark corpus.
var benchmarkOpts = NormalizeOptions{CaptureLimit: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey}

// BenchmarkNormalizer covers the worker-side normalization over the golden
// fixture corpus: three relay formats (OpenAI Responses, OpenAI Chat, Claude)
// at regular and large request sizes.
func BenchmarkNormalizer(b *testing.B) {
	fixtures := []struct {
		name   string
		format string
		req    dto.Request
	}{
		{name: "responses_regular", format: string(types.RelayFormatOpenAIResponses), req: benchmarkResponses(8)},
		{name: "responses_large", format: string(types.RelayFormatOpenAIResponses), req: benchmarkResponses(largeRequestItems)},
		{name: "chat_regular", format: string(types.RelayFormatOpenAI), req: benchmarkChat(8)},
		{name: "chat_large", format: string(types.RelayFormatOpenAI), req: benchmarkChat(largeRequestItems)},
		{name: "claude_regular", format: string(types.RelayFormatClaude), req: benchmarkClaude(8)},
		{name: "claude_large", format: string(types.RelayFormatClaude), req: benchmarkClaude(largeRequestItems)},
	}
	for _, f := range fixtures {
		b.Run(f.name, func(b *testing.B) {
			opts := benchmarkOpts
			for i := 0; i < b.N; i++ {
				res := NormalizeRequest(f.format, f.req, opts)
				if res.ContentState != ContentStateFull {
					b.Fatalf("normalization must complete, state=%s", res.ContentState)
				}
			}
		})
	}
}

// BenchmarkCodecEncodeDecode covers the content codec over a mid-size
// canonical item: the encode (marshal + zstd) and the decode with digest
// re-verification.
func BenchmarkCodecEncodeDecode(b *testing.B) {
	item := CanonicalItem{
		Kind:         CanonicalKindMessage,
		Role:         "user",
		Content:      []CanonicalPart{{Type: partTypeText, Text: strings.Repeat("benchmark corpus text ", 24)}},
		LogicalBytes: 1024,
	}
	item = withHmac(item, benchmarkOpts)
	payload, logical, err := encodeItem(item)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("encode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, _, err := encodeItem(item); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got, err := decodeItem(payload, item.Hmac, logical, testHMACKey)
			if err != nil {
				b.Fatal(err)
			}
			if got.Kind != item.Kind {
				b.Fatal("decode must round-trip the item kind")
			}
		}
	})
}

// BenchmarkDecodeItem isolates the per-object decode path of a turn context
// reconstruction (the P0-1 reworked path: one JSON parse, digest over the
// parsed item).
func BenchmarkDecodeItem(b *testing.B) {
	item := CanonicalItem{
		Kind:         CanonicalKindMessage,
		Role:         "assistant",
		Content:      []CanonicalPart{{Type: partTypeText, Text: strings.Repeat("reconstruction corpus ", 16)}},
		LogicalBytes: 512,
	}
	item = withHmac(item, benchmarkOpts)
	payload, logical, err := encodeItem(item)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := decodeItem(payload, item.Hmac, logical, testHMACKey); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEnqueue covers the request-path admission against a fake store:
// the byte reservation, the count registration, and the non-blocking channel
// send. The worker drains concurrently, so the queue stays bounded.
func BenchmarkEnqueue(b *testing.B) {
	cfg := DefaultConfig()
	cfg.QueueSize = MaxQueueSize
	cfg.QueueBytes = MaxQueueBytes
	d := NewDispatcher(cfg, &scriptedStore{})
	d.Start()
	defer func() { d.Stop(context.Background()) }()
	ev := sampleEventPtr()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !d.TryEnqueue(ev, 1) {
			b.Fatal("admission must succeed under the benchmark load")
		}
	}
	b.StopTimer()
	// The worker drains asynchronously; give it a bounded moment, then
	// assert the queue is empty so the benchmark never measures a leaked
	// reservation or a stranded event.
	for i := 0; i < 100 && d.pendingCount.Load() > 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if d.pendingBytes.Load() != 0 || d.pendingCount.Load() != 0 {
		b.Fatalf("the worker must drain the benchmark events, queue bytes=%d count=%d", d.pendingBytes.Load(), d.pendingCount.Load())
	}
}

// benchmarkResponses builds an OpenAI Responses request with n input items.
func benchmarkResponses(n int) *dto.OpenAIResponsesRequest {
	var items []string
	for i := 0; i < n; i++ {
		items = append(items, fmt.Sprintf(`{"role":"user","content":[{"type":"input_text","text":"benchmark message %d with padding text"}]}`, i))
	}
	return &dto.OpenAIResponsesRequest{
		Model:        "gpt-5",
		Instructions: raw(`"Be brief."`),
		Input:        raw("[" + strings.Join(items, ",") + "]"),
		Metadata:     raw(`{"user_id":"u-bench"}`),
	}
}

// benchmarkChat builds an OpenAI Chat request with n messages.
func benchmarkChat(n int) *dto.GeneralOpenAIRequest {
	messages := make([]dto.Message, 0, n)
	for i := 0; i < n; i++ {
		messages = append(messages, dto.Message{
			Role:    "user",
			Content: fmt.Sprintf("benchmark message %d with padding text", i),
		})
	}
	return &dto.GeneralOpenAIRequest{
		Model:    "gpt-5",
		Messages: messages,
		Metadata: raw(`{"user_id":"u-bench"}`),
	}
}

// benchmarkClaude builds a Claude Messages request with n messages.
func benchmarkClaude(n int) *dto.ClaudeRequest {
	messages := make([]dto.ClaudeMessage, 0, n)
	for i := 0; i < n; i++ {
		messages = append(messages, dto.ClaudeMessage{
			Role:    "user",
			Content: fmt.Sprintf("benchmark message %d with padding text", i),
		})
	}
	return &dto.ClaudeRequest{
		Model:    "claude-opus-4",
		System:   []any{map[string]any{"type": "text", "text": "system prompt"}},
		Messages: messages,
		Metadata: raw(`{"user_id":"u-bench"}`),
	}
}
