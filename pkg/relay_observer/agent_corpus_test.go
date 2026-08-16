package relayobserver

// This file is the automated agent-corpus harness (P0-B boundary work, PR
// #15): it walks every fixture under testdata/agent-corpus/, drives it
// through the normalizer at the fixture's own capture limit, and computes the
// capture metrics the corpus contract defines — raw body bytes, unbounded and
// selected canonical bytes, selected item kinds, omitted count, gap position,
// session identity scope, tool pair integrity, and determinism — asserting
// the _meta contract and the P0-B boundary invariants on every sample. The
// samples themselves stay static (same policy as the golden normalizer
// corpus): all ids are synthetic fx-* fixtures and the _meta fields are the
// only per-sample contract.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// corpusFixture is one agent-corpus sample: the _meta contract block, the
// request headers, and the raw request body.
type corpusFixture struct {
	Meta struct {
		Name         string   `json:"name"`
		Desc         string   `json:"desc"`
		RawBodyBytes int64    `json:"raw_body_bytes"`
		CaptureLimit int64    `json:"capture_limit"`
		ScopeExpect  string   `json:"scope_expect"`
		Sources      []string `json:"sources_expect"`
		Format       string   `json:"format"`
	} `json:"_meta"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// loadCorpusFixtures walks testdata/agent-corpus/ and returns every .json
// sample with a _meta.name, sorted by name for stable output.
func loadCorpusFixtures(t *testing.T) []corpusFixture {
	t.Helper()
	dir := filepath.Join("testdata", "agent-corpus")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []corpusFixture
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		var f corpusFixture
		require.NoError(t, common.Unmarshal(data, &f), "fixture %s must parse", e.Name())
		require.NotEmpty(t, f.Meta.Name, "fixture %s must carry a _meta.name", e.Name())
		out = append(out, f)
	}
	require.NotEmpty(t, out, "the agent corpus must not be empty")
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// corpusRelayFormat resolves the relay format of a fixture: an explicit
// _meta.format wins; otherwise the body shape decides (responses bodies carry
// an input/instructions field, claude bodies a system field, chat is the
// fallback).
func corpusRelayFormat(f corpusFixture) string {
	if f.Meta.Format != "" {
		return f.Meta.Format
	}
	var body map[string]any
	if common.Unmarshal(f.Body, &body) != nil {
		return ""
	}
	if _, ok := body["input"]; ok {
		return string(types.RelayFormatOpenAIResponses)
	}
	if _, ok := body["instructions"]; ok {
		return string(types.RelayFormatOpenAIResponses)
	}
	if _, ok := body["system"]; ok {
		return string(types.RelayFormatClaude)
	}
	return string(types.RelayFormatOpenAI)
}

// parseCorpusBody parses the fixture body into the DTO of its relay format.
func parseCorpusBody(format string, body []byte) (dto.Request, error) {
	switch format {
	case string(types.RelayFormatOpenAIResponses):
		var r dto.OpenAIResponsesRequest
		if err := common.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case string(types.RelayFormatOpenAI):
		var r dto.GeneralOpenAIRequest
		if err := common.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case string(types.RelayFormatClaude):
		var r dto.ClaudeRequest
		if err := common.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		return &r, nil
	}
	return nil, fmt.Errorf("relayobserver: unknown corpus relay format %q", format)
}

// corpusCanonicalBytes sums the serialized payload sizes of the items.
func corpusCanonicalBytes(items []CanonicalItem) (int64, error) {
	var sum int64
	for _, it := range items {
		p, err := common.Marshal(it)
		if err != nil {
			return 0, err
		}
		sum += int64(len(p))
	}
	return sum, nil
}

// corpusItemKinds returns the ordered kind vocabulary of a normalization.
func corpusItemKinds(items []CanonicalItem) []string {
	kinds := make([]string, len(items))
	for i, it := range items {
		kinds[i] = it.Kind
	}
	return kinds
}

// corpusGapPosition reports the position of the closing truncation marker:
// the last item when it is a gap marker, -1 otherwise. Unknown-item gaps
// inside the stream are not truncation markers.
func corpusGapPosition(items []CanonicalItem) int {
	if len(items) > 0 && items[len(items)-1].Kind == CanonicalKindGap {
		return len(items) - 1
	}
	return -1
}

// corpusToolCalls extracts the tool_call ids of a normalized capture (message
// parts and standalone tool_call items).
func corpusToolCalls(items []CanonicalItem) map[string]bool {
	calls := make(map[string]bool)
	for _, it := range items {
		switch it.Kind {
		case CanonicalKindToolCall:
			if len(it.Content) > 0 && it.Content[0].Call != nil {
				calls[it.Content[0].Call.ID] = true
			}
		case CanonicalKindMessage:
			for _, p := range it.Content {
				if p.Type == partTypeToolCall && p.Call != nil && p.Call.ID != "" {
					calls[p.Call.ID] = true
				}
			}
		}
	}
	return calls
}

// corpusToolResults returns the tool_call ids referenced by tool_result parts.
func corpusToolResults(items []CanonicalItem) []string {
	var ids []string
	for _, it := range items {
		switch it.Kind {
		case CanonicalKindToolResult:
			if len(it.Content) > 0 && it.Content[0].Result != nil {
				ids = append(ids, it.Content[0].Result.ToolCallID)
			}
		case CanonicalKindMessage:
			for _, p := range it.Content {
				if p.Type == partTypeToolResult && p.Result != nil {
					ids = append(ids, p.Result.ToolCallID)
				}
			}
		}
	}
	return ids
}

// corpusHeaders converts the fixture header map into canonical net/http
// header form (identity resolution reads the canonical form).
func corpusHeaders(h map[string]string) http.Header {
	out := http.Header{}
	for k, v := range h {
		out.Set(k, v)
	}
	return out
}

// corpusResolvedSources returns the sorted source set of a resolved identity.
func corpusResolvedSources(idRes IdentityResult) []string {
	set := make(map[string]bool, 1+len(idRes.Auxiliary))
	if idRes.Primary.Digest != "" {
		set[string(idRes.Primary.Source)] = true
	}
	for _, a := range idRes.Auxiliary {
		set[string(a.Source)] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// TestAgentCorpusHarness runs every agent-corpus fixture through the
// normalizer at its own capture limit and asserts the corpus contract: the
// _meta fields (raw body size, scope, identity sources) must match the
// fixture content, and the P0-B boundary invariants must hold — a capture
// that fits the cap is kept whole byte for byte (the full-fit contract the
// fixed-envelope algorithm violated); an over-limit capture truncates with
// an exact-size gap marker when one fits; the output is deterministic,
// structurally valid JSON; and tool pairs stay intact in full captures.
func TestAgentCorpusHarness(t *testing.T) {
	for _, f := range loadCorpusFixtures(t) {
		t.Run(f.Meta.Name, func(t *testing.T) {
			runCorpusFixture(t, f)
		})
	}
}

func runCorpusFixture(t *testing.T, f corpusFixture) {
	t.Helper()

	// The raw body size contract: _meta.raw_body_bytes is the compact
	// serialized body length (README regeneration policy).
	var bodyCompact bytes.Buffer
	require.NoError(t, json.Compact(&bodyCompact, f.Body))
	rawBodyBytes := int64(bodyCompact.Len())
	assert.Equal(t, f.Meta.RawBodyBytes, rawBodyBytes, "raw_body_bytes must equal the compact body length")

	format := corpusRelayFormat(f)
	require.NotEmpty(t, format, "fixture relay format must be resolvable")
	req, err := parseCorpusBody(format, bodyCompact.Bytes())
	require.NoError(t, err)
	require.NotNil(t, req)

	limit := f.Meta.CaptureLimit
	require.Greater(t, limit, int64(0), "capture_limit must be positive")
	atLimitOpts := NormalizeOptions{CaptureLimit: limit, MaxRequestBytes: limit, HMACKey: testHMACKey}

	// The unbounded baseline: the full canonical capture without a byte cap.
	unbounded := NormalizeRequest(format, req, NormalizeOptions{CaptureLimit: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey})
	require.Equal(t, ContentStateFull, unbounded.ContentState, "the unbounded baseline must be full")
	unboundedBytes, err := corpusCanonicalBytes(unbounded.Items)
	require.NoError(t, err)

	// The capture at the fixture's own limit, twice for determinism.
	first := NormalizeRequest(format, req, atLimitOpts)
	second := NormalizeRequest(format, req, atLimitOpts)
	assert.Equal(t, first.ContentState, second.ContentState, "normalization must be deterministic")
	assert.Equal(t, first.OmittedItems, second.OmittedItems, "omitted counts must be deterministic")
	firstLines, err := canonicalLines(first.Items)
	require.NoError(t, err)
	secondLines, err := canonicalLines(second.Items)
	require.NoError(t, err)
	assert.Equal(t, firstLines, secondLines, "canonical output must be deterministic")

	selectedBytes, err := corpusCanonicalBytes(first.Items)
	require.NoError(t, err)
	assert.Equal(t, selectedBytes, first.CanonicalBytes, "CanonicalBytes must equal the sum of the item payloads")
	gapIdx := corpusGapPosition(first.Items)
	kinds := corpusItemKinds(first.Items)

	// P0-B boundary invariants. Full fit: a capture that fits the cap is
	// never truncated — this is the contract the old fixed-envelope
	// algorithm violated for the (limit - envelope, limit] band.
	if unboundedBytes <= limit {
		assert.Equal(t, ContentStateFull, first.ContentState, "a capture that fits the cap must stay full")
		assert.Zero(t, first.OmittedItems, "a capture that fits the cap must not omit items")
		assert.Equal(t, -1, gapIdx, "a capture that fits the cap must not carry a truncation marker")
		assert.Equal(t, unboundedBytes, selectedBytes, "a capture that fits the cap must keep every byte")
		unboundedLines, err := canonicalLines(unbounded.Items)
		require.NoError(t, err)
		assert.Equal(t, unboundedLines, firstLines, "a capture that fits the cap must be kept byte for byte")
	} else {
		assert.Equal(t, ContentStateGap, first.ContentState, "an over-limit capture must be marked gap")
		assert.Greater(t, first.OmittedItems, 0, "an over-limit capture must omit at least one item")
		assert.LessOrEqual(t, selectedBytes, limit, "selected bytes must stay inside the capture limit")
		if gapIdx >= 0 {
			assert.Equal(t, len(first.Items)-1, gapIdx, "the truncation marker must close the capture")
			last := first.Items[gapIdx]
			assert.True(t, last.Truncated)
			assert.Greater(t, last.LogicalBytes, int64(0))
			assert.NotEmpty(t, last.Hmac)
		}
	}

	// Structural validity: the capture must marshal and every item must
	// round-trip as JSON (a gap marker stays structurally valid JSON too).
	captureJSON, err := common.Marshal(first.Items)
	require.NoError(t, err)
	require.NotEmpty(t, captureJSON)
	for _, it := range first.Items {
		p, err := common.Marshal(it)
		require.NoError(t, err)
		var round CanonicalItem
		require.NoError(t, common.Unmarshal(p, &round))
	}

	// Session identity contract: the request path supplies the already resolved
	// scope; this package must not re-parse the fixture headers. Extra
	// auxiliary fallbacks are allowed (for example the codex prompt_cache_key
	// chain), so the expected sources are a required subset, and the primary
	// must be the first expected source.
	headers := corpusHeaders(f.Headers)
	scope := SessionScope(f.Meta.ScopeExpect)
	idRes, err := ResolveIdentity(IdentityInput{Scope: scope, Headers: headers, Body: bodyCompact.Bytes()}, KeyMaterial{CurrentKey: testHMACKey, CurrentVersion: 1})
	require.NoError(t, err)
	assert.Equal(t, scope, idRes.Scope, "the supplied request-path scope must be retained")
	if len(f.Meta.Sources) == 0 {
		assert.Empty(t, idRes.Primary.Digest, "no expected sources, but a primary alias resolved")
		assert.Empty(t, idRes.Auxiliary, "no expected sources, but auxiliary aliases resolved")
	} else {
		assert.Equal(t, f.Meta.Sources[0], string(idRes.Primary.Source), "the primary source must be the first expected source")
		resolved := make(map[string]bool, 1+len(idRes.Auxiliary))
		resolved[string(idRes.Primary.Source)] = true
		for _, a := range idRes.Auxiliary {
			resolved[string(a.Source)] = true
		}
		for _, src := range f.Meta.Sources {
			assert.True(t, resolved[src], "expected source %s must resolve", src)
		}
	}

	// Tool pair integrity on the unbounded capture: every tool_result must
	// reference a tool_call kept in the same capture.
	calls := corpusToolCalls(unbounded.Items)
	for _, rid := range corpusToolResults(unbounded.Items) {
		assert.True(t, calls[rid], "tool_result references missing tool_call %q", rid)
	}

	t.Logf("corpus %s: raw=%d limit=%d unbounded=%d selected=%d state=%s kinds=%v omitted=%d gapIdx=%d scope=%s sources=%v",
		f.Meta.Name, rawBodyBytes, limit, unboundedBytes, selectedBytes, first.ContentState, kinds, first.OmittedItems, gapIdx, scope, corpusResolvedSources(idRes))
}
