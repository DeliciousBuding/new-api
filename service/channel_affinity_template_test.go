package service

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func buildChannelAffinityTemplateContextForTest(meta channelAffinityMeta) *gin.Context {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	setChannelAffinityContext(ctx, meta)
	return ctx
}

func TestApplyChannelAffinityOverrideTemplate_NoTemplate(t *testing.T) {
	ctx := buildChannelAffinityTemplateContextForTest(channelAffinityMeta{
		RuleName: "rule-no-template",
	})
	base := map[string]interface{}{
		"temperature": 0.7,
	}

	merged, applied := ApplyChannelAffinityOverrideTemplate(ctx, base)
	require.False(t, applied)
	require.Equal(t, base, merged)
}

func TestApplyChannelAffinityOverrideTemplate_MergeTemplate(t *testing.T) {
	ctx := buildChannelAffinityTemplateContextForTest(channelAffinityMeta{
		RuleName: "rule-with-template",
		ParamTemplate: map[string]interface{}{
			"temperature": 0.2,
			"top_p":       0.95,
		},
		UsingGroup:     "default",
		ModelName:      "gpt-4.1",
		RequestPath:    "/v1/responses",
		KeySourceType:  "gjson",
		KeySourcePath:  "prompt_cache_key",
		KeyHint:        "abcd...wxyz",
		KeyFingerprint: "abcd1234",
	})
	base := map[string]interface{}{
		"temperature": 0.7,
		"max_tokens":  2000,
	}

	merged, applied := ApplyChannelAffinityOverrideTemplate(ctx, base)
	require.True(t, applied)
	require.Equal(t, 0.7, merged["temperature"])
	require.Equal(t, 0.95, merged["top_p"])
	require.Equal(t, 2000, merged["max_tokens"])
	require.Equal(t, 0.7, base["temperature"])

	anyInfo, ok := ctx.Get(ginKeyChannelAffinityLogInfo)
	require.True(t, ok)
	info, ok := anyInfo.(map[string]interface{})
	require.True(t, ok)
	overrideInfoAny, ok := info["override_template"]
	require.True(t, ok)
	overrideInfo, ok := overrideInfoAny.(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, true, overrideInfo["applied"])
	require.Equal(t, "rule-with-template", overrideInfo["rule_name"])
	require.EqualValues(t, 2, overrideInfo["param_override_keys"])
}

func TestApplyChannelAffinityOverrideTemplate_MergeOperations(t *testing.T) {
	ctx := buildChannelAffinityTemplateContextForTest(channelAffinityMeta{
		RuleName: "rule-with-ops-template",
		ParamTemplate: map[string]interface{}{
			"operations": []map[string]interface{}{
				{
					"mode":  "pass_headers",
					"value": []string{"Originator"},
				},
			},
		},
	})
	base := map[string]interface{}{
		"temperature": 0.7,
		"operations": []map[string]interface{}{
			{
				"path":  "model",
				"mode":  "trim_prefix",
				"value": "openai/",
			},
		},
	}

	merged, applied := ApplyChannelAffinityOverrideTemplate(ctx, base)
	require.True(t, applied)
	require.Equal(t, 0.7, merged["temperature"])

	opsAny, ok := merged["operations"]
	require.True(t, ok)
	ops, ok := opsAny.([]interface{})
	require.True(t, ok)
	require.Len(t, ops, 2)

	firstOp, ok := ops[0].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "pass_headers", firstOp["mode"])

	secondOp, ok := ops[1].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "trim_prefix", secondOp["mode"])
}

func TestShouldSkipRetryAfterChannelAffinityFailure(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() *gin.Context
		want bool
	}{
		{
			name: "nil context",
			ctx: func() *gin.Context {
				return nil
			},
			want: false,
		},
		{
			name: "explicit skip retry flag in context",
			ctx: func() *gin.Context {
				ctx := buildChannelAffinityTemplateContextForTest(channelAffinityMeta{
					RuleName:   "rule-explicit-flag",
					SkipRetry:  false,
					UsingGroup: "default",
					ModelName:  "gpt-5",
				})
				ctx.Set(ginKeyChannelAffinitySkipRetry, true)
				return ctx
			},
			want: true,
		},
		{
			name: "fallback to matched rule meta",
			ctx: func() *gin.Context {
				return buildChannelAffinityTemplateContextForTest(channelAffinityMeta{
					RuleName:   "rule-skip-retry",
					SkipRetry:  true,
					UsingGroup: "default",
					ModelName:  "gpt-5",
				})
			},
			want: true,
		},
		{
			name: "no flag and no skip retry meta",
			ctx: func() *gin.Context {
				return buildChannelAffinityTemplateContextForTest(channelAffinityMeta{
					RuleName:   "rule-no-skip-retry",
					SkipRetry:  false,
					UsingGroup: "default",
					ModelName:  "gpt-5",
				})
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ShouldSkipRetryAfterChannelAffinityFailure(tt.ctx()))
		})
	}
}

func TestExtractChannelAffinityValue_RequestHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("X-Affinity-Key", " tenant-123 ")

	value := extractChannelAffinityValue(ctx, operation_setting.ChannelAffinityKeySource{
		Type: "request_header",
		Key:  "X-Affinity-Key",
	})

	require.Equal(t, "tenant-123", value)
}

func TestGetPreferredChannelByAffinity_RequestHeaderKeySource(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rule := operation_setting.ChannelAffinityRule{
		Name:       "header-affinity",
		ModelRegex: []string{"^gpt-.*$"},
		PathRegex:  []string{"/v1/responses"},
		KeySources: []operation_setting.ChannelAffinityKeySource{
			{Type: "request_header", Key: "X-Affinity-Key"},
		},
		IncludeRuleName:  true,
		IncludeModelName: true,
	}

	affinityValue := fmt.Sprintf("header-hit-%d", time.Now().UnixNano())
	cacheKeySuffix := buildChannelAffinityCacheKeySuffix(rule, "gpt-5", "default", affinityValue)

	cache := getChannelAffinityCache()
	require.NoError(t, cache.SetWithTTL(cacheKeySuffix, 9528, time.Minute))
	t.Cleanup(func() {
		_, _ = cache.DeleteMany([]string{cacheKeySuffix})
	})

	setting := operation_setting.GetChannelAffinitySetting()
	originalRules := setting.Rules
	setting.Rules = append([]operation_setting.ChannelAffinityRule{rule}, originalRules...)
	t.Cleanup(func() {
		setting.Rules = originalRules
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("X-Affinity-Key", affinityValue)

	channelID, found := GetPreferredChannelByAffinity(ctx, "gpt-5", "default")
	require.True(t, found)
	require.Equal(t, 9528, channelID)

	meta, ok := getChannelAffinityMeta(ctx)
	require.True(t, ok)
	require.Equal(t, "request_header", meta.KeySourceType)
	require.Equal(t, "X-Affinity-Key", meta.KeySourceKey)
	require.Equal(t, buildChannelAffinityKeyHint(affinityValue), meta.KeyHint)
}

func TestClearCurrentChannelAffinityCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cacheKeySuffix := fmt.Sprintf("codex cli trace:default:clear-current-%d", time.Now().UnixNano())
	cacheKeyFull := channelAffinityCacheNamespace + ":" + cacheKeySuffix
	cache := getChannelAffinityCache()
	require.NoError(t, cache.SetWithTTL(cacheKeySuffix, 9527, time.Minute))
	t.Cleanup(func() {
		_, _ = cache.DeleteMany([]string{cacheKeySuffix})
	})

	ctx := buildChannelAffinityTemplateContextForTest(channelAffinityMeta{
		CacheKey:   cacheKeyFull,
		TTLSeconds: 60,
		RuleName:   "codex cli trace",
		SkipRetry:  true,
	})
	require.True(t, ShouldSkipRetryAfterChannelAffinityFailure(ctx))

	deleted := ClearCurrentChannelAffinityCache(ctx)
	require.True(t, deleted)
	_, found, err := cache.Get(cacheKeySuffix)
	require.NoError(t, err)
	require.False(t, found)
	require.False(t, ShouldSkipRetryAfterChannelAffinityFailure(ctx))
}

func TestChannelAffinityHitCodexTemplatePassHeadersEffective(t *testing.T) {
	gin.SetMode(gin.TestMode)

	setting := operation_setting.GetChannelAffinitySetting()
	require.NotNil(t, setting)

	var codexRule *operation_setting.ChannelAffinityRule
	for i := range setting.Rules {
		rule := &setting.Rules[i]
		if strings.EqualFold(strings.TrimSpace(rule.Name), "codex cli trace") {
			codexRule = rule
			break
		}
	}
	require.NotNil(t, codexRule)

	affinityValue := fmt.Sprintf("pc-hit-%d", time.Now().UnixNano())
	cacheKeySuffix := buildChannelAffinityCacheKeySuffix(*codexRule, "gpt-5", "default", affinityValue)

	cache := getChannelAffinityCache()
	require.NoError(t, cache.SetWithTTL(cacheKeySuffix, 9527, time.Minute))
	t.Cleanup(func() {
		_, _ = cache.DeleteMany([]string{cacheKeySuffix})
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"prompt_cache_key":"%s"}`, affinityValue)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	channelID, found := GetPreferredChannelByAffinity(ctx, "gpt-5", "default")
	require.True(t, found)
	require.Equal(t, 9527, channelID)

	baseOverride := map[string]interface{}{
		"temperature": 0.2,
	}
	mergedOverride, applied := ApplyChannelAffinityOverrideTemplate(ctx, baseOverride)
	require.True(t, applied)
	require.Equal(t, 0.2, mergedOverride["temperature"])

	info := &relaycommon.RelayInfo{
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-123",
			"User-Agent": "codex-cli-test",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ParamOverride: mergedOverride,
			HeadersOverride: map[string]interface{}{
				"X-Static": "legacy-static",
			},
		},
	}

	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-5"}`), info)
	require.NoError(t, err)
	require.True(t, info.UseRuntimeHeadersOverride)

	require.Equal(t, "legacy-static", info.RuntimeHeadersOverride["x-static"])
	require.Equal(t, "Codex CLI", info.RuntimeHeadersOverride["originator"])
	require.Equal(t, "sess-123", info.RuntimeHeadersOverride["session_id"])
	require.Equal(t, "codex-cli-test", info.RuntimeHeadersOverride["user-agent"])

	_, exists := info.RuntimeHeadersOverride["x-codex-beta-features"]
	require.False(t, exists)
	_, exists = info.RuntimeHeadersOverride["x-codex-turn-metadata"]
	require.False(t, exists)
}

// testDiskBodyStorage is a minimal common.BodyStorage implementation that
// reports IsDisk()==true, so tests can exercise the bounded-prefix body paths
// without a real disk cache file.
type testDiskBodyStorage struct {
	data   []byte
	reader *bytes.Reader
}

func newTestDiskBodyStorage(data []byte) *testDiskBodyStorage {
	return &testDiskBodyStorage{data: data, reader: bytes.NewReader(data)}
}

func (s *testDiskBodyStorage) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *testDiskBodyStorage) Seek(offset int64, whence int) (int64, error) {
	return s.reader.Seek(offset, whence)
}
func (s *testDiskBodyStorage) Close() error                    { return nil }
func (s *testDiskBodyStorage) Bytes() ([]byte, error)          { return s.data, nil }
func (s *testDiskBodyStorage) Size() int64                     { return int64(len(s.data)) }
func (s *testDiskBodyStorage) IsDisk() bool                    { return true }
func (s *testDiskBodyStorage) NewReader() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

// --- issue #39: affinity 软失败解绑 ---

func newAffinitySoftFailureTestContext(t *testing.T, cacheKey string) *gin.Context {
	t.Helper()
	return buildChannelAffinityTemplateContextForTest(channelAffinityMeta{CacheKey: cacheKey})
}

func clearChannelAffinitySoftFailuresForTest() {
	channelAffinitySoftFailures.Range(func(k, _ any) bool {
		channelAffinitySoftFailures.Delete(k)
		return true
	})
}

func TestAffinitySoftFailureUnbindsAfterThreshold(t *testing.T) {
	t.Cleanup(clearChannelAffinitySoftFailuresForTest)
	key := "new-api:channel_affinity:v1:rule-soft:gpt-x:default:fp123"
	require.False(t, RecordChannelAffinitySoftFailure(newAffinitySoftFailureTestContext(t, key)))
	require.False(t, RecordChannelAffinitySoftFailure(newAffinitySoftFailureTestContext(t, key)))
	require.True(t, RecordChannelAffinitySoftFailure(newAffinitySoftFailureTestContext(t, key)))
	// The unbind clears the streak: the next failure starts a fresh streak.
	require.False(t, RecordChannelAffinitySoftFailure(newAffinitySoftFailureTestContext(t, key)))
}

func TestAffinitySoftFailureCountsOncePerRequest(t *testing.T) {
	t.Cleanup(clearChannelAffinitySoftFailuresForTest)
	ctx := newAffinitySoftFailureTestContext(t, "new-api:channel_affinity:v1:rule-soft:gpt-x:default:fp456")
	require.False(t, RecordChannelAffinitySoftFailure(ctx))
	// A multi-attempt request counts once: a second call on the same request
	// must not advance the streak.
	require.False(t, RecordChannelAffinitySoftFailure(ctx))
}

func TestAffinitySoftFailureResetBreaksStreak(t *testing.T) {
	t.Cleanup(clearChannelAffinitySoftFailuresForTest)
	key := "new-api:channel_affinity:v1:rule-soft:gpt-x:default:fp789"
	require.False(t, RecordChannelAffinitySoftFailure(newAffinitySoftFailureTestContext(t, key)))
	require.False(t, RecordChannelAffinitySoftFailure(newAffinitySoftFailureTestContext(t, key)))
	// One successful response breaks the streak.
	ResetChannelAffinitySoftFailures(newAffinitySoftFailureTestContext(t, key))
	require.False(t, RecordChannelAffinitySoftFailure(newAffinitySoftFailureTestContext(t, key)))
	require.False(t, RecordChannelAffinitySoftFailure(newAffinitySoftFailureTestContext(t, key)))
}

func TestAffinitySoftFailureNoopWithoutBinding(t *testing.T) {
	t.Cleanup(clearChannelAffinitySoftFailuresForTest)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.False(t, RecordChannelAffinitySoftFailure(ctx))
	require.NotPanics(t, func() { ResetChannelAffinitySoftFailures(ctx) })
}

// --- issue #43: affinity gjson 提取对磁盘体截断 ---

func TestAffinityGjsonExtractionOnDiskBodyPrefix(t *testing.T) {
	// Body larger than the 1 MiB prefix: a key near the start resolves, a key
	// behind the prefix is a miss (truncation hides it), and the storage
	// cursor is preserved for the request path's later body reads.
	head := []byte(`{"prompt_cache_key":"cache-early","model":"gpt-x","messages":[`)
	filler := bytes.Repeat([]byte{'a'}, channelAffinityGjsonPrefixBytes)
	body := append(append([]byte{}, head...), filler...)
	body = append(body, []byte(`],"tail_key":"tail-late"}`)...)

	storage := newTestDiskBodyStorage(body)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set(common.KeyBodyStorage, storage)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	earlySrc := operation_setting.ChannelAffinityKeySource{Type: "gjson", Path: "prompt_cache_key"}
	require.Equal(t, "cache-early", extractChannelAffinityValue(ctx, earlySrc))

	lateSrc := operation_setting.ChannelAffinityKeySource{Type: "gjson", Path: "tail_key"}
	require.Equal(t, "", extractChannelAffinityValue(ctx, lateSrc))

	// The prefix read restored the cursor (mirroring diskStorage.Bytes()).
	pos, err := storage.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	require.Equal(t, int64(0), pos)
}

func TestAffinityGjsonExtractionOnSmallDiskBody(t *testing.T) {
	// A disk body within the prefix budget is read fully: tail keys resolve.
	storage := newTestDiskBodyStorage([]byte(`{"tail_key":"tail-val"}`))
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set(common.KeyBodyStorage, storage)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	src := operation_setting.ChannelAffinityKeySource{Type: "gjson", Path: "tail_key"}
	require.Equal(t, "tail-val", extractChannelAffinityValue(ctx, src))
}
