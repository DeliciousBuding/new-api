package vision_relay

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 结构化模式：模型按四小节输出 → DescribeOne 解析并渲染为分节 Markdown（v0.3）
func TestDescribeOneStructuredRender(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"[SUMMARY]\n接口报错截图\n[TRANSCRIPTION]\nError: connection refused\n[LAYOUT]\n顶部标题\n[UNCERTAINTY]\n无"}}]}`))
	}))
	defer ts.Close()
	cfg := testConfig(ts.URL)
	cfg.Structured = true
	client := &VisionClient{HTTPClient: ts.Client()}
	r := client.DescribeOne(context.Background(), BuildInstruction(cfg), testImageData(), "image/png", cfg)
	require.Equal(t, "", r.Enum, "结构化成功不应有失败枚举")
	assert.Contains(t, r.Desc, "[SUMMARY]")
	assert.Contains(t, r.Desc, "接口报错截图")
	assert.Contains(t, r.Desc, "[TRANSCRIPTION]")
	assert.Contains(t, r.Desc, "Error: connection refused")
	assert.Contains(t, r.Desc, "[LAYOUT]")
	// UNCERTAINTY 为 none → 渲染时跳过
	assert.NotContains(t, r.Desc, "[UNCERTAINTY]")
}

// 结构化模式 + 散文输出（无分节头）→ 整段退化为 SUMMARY，不丢信息（v0.3）
func TestDescribeOneStructuredProseFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"模型没按格式输出，只有一段散文描述"}}]}`))
	}))
	defer ts.Close()
	cfg := testConfig(ts.URL)
	cfg.Structured = true
	client := &VisionClient{HTTPClient: ts.Client()}
	r := client.DescribeOne(context.Background(), BuildInstruction(cfg), testImageData(), "image/png", cfg)
	require.Equal(t, "", r.Enum, "散文降级仍是成功")
	assert.Contains(t, r.Desc, "模型没按格式输出，只有一段散文描述")
	assert.True(t, len(r.Desc) > 0)
}

// 自定义 Prompt 优先：即使 Structured=true，自定义 Prompt 输出不做解析渲染
func TestDescribeOneStructuredWithCustomPrompt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"[SUMMARY]\n概要"}}]}`))
	}))
	defer ts.Close()
	cfg := testConfig(ts.URL)
	cfg.Structured = true
	cfg.Prompt = "自定义识图指令"
	client := &VisionClient{HTTPClient: ts.Client()}
	r := client.DescribeOne(context.Background(), BuildInstruction(cfg), testImageData(), "image/png", cfg)
	require.Equal(t, "", r.Enum)
	// 自定义 Prompt 下原样透传，不追加 [SUMMARY] 渲染头
	assert.Equal(t, "[SUMMARY]\n概要", r.Desc)
}

// 逐模型尝试明细：首选模型 503 → fallback 成功，Attempts 记录每次成败与枚举（v0.3）
func TestDescribeOneAttempts(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"no channel"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"fallback 成功"}}]}`))
	}))
	defer ts.Close()
	cfg := testConfig(ts.URL)
	client := &VisionClient{HTTPClient: ts.Client()}
	r := client.DescribeOne(context.Background(), "指令", testImageData(), "image/png", cfg)
	require.Equal(t, "", r.Enum)
	require.Len(t, r.Attempts, 2, "两次模型尝试都应记录")
	assert.Equal(t, "vision-model-a", r.Attempts[0].Model)
	assert.Equal(t, EnumServiceUnavailable, r.Attempts[0].Enum, "首选模型 503 → service_unavailable")
	assert.Equal(t, "vision-model-b", r.Attempts[1].Model)
	assert.Equal(t, "", r.Attempts[1].Enum, "成功尝试无枚举")
}

// 引擎端到端：attempts 从 DescribeOne 聚合到 Stats（v0.3）
func TestEngineAggregatesAttempts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"同图描述"}}]}`))
	}))
	defer ts.Close()
	cfg := testConfig(ts.URL)
	engine := &Engine{Client: &VisionClient{HTTPClient: ts.Client()}}
	stats := &Stats{}
	raw := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		base64.StdEncoding.EncodeToString(makePNG(t, 20, 20)) + `"}}]}]}`
	_, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, cfg, stats)
	require.NoError(t, err)
	require.Len(t, stats.Attempts, 1, "单图单模型应记录一次尝试")
	assert.Equal(t, "vision-model-a", stats.Attempts[0].Model)
	assert.Equal(t, "", stats.Attempts[0].Enum)
	assert.GreaterOrEqual(t, stats.Attempts[0].ElapsedMs, int64(0))
}
