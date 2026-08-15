package vision_relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onePixelWebP 是最小 1x1 VP8L lossless webp（RIFF+WEBP+VP8L），复现生产
// Cerebras 渠道报的 `400 unsupported image format: 'image/webp'` 场景。
const onePixelWebP = "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA"

// 端到端：webp 图经完整 Enhance 流程后，视觉端点收到的是 image/jpeg 而非
// image/webp——锁住 webp 修复的完整链路契约（而非只测 CompressForVision 单点）。
func TestEngineEnhanceWebPConvertedToJPEG(t *testing.T) {
	var receivedMime atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content []map[string]any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad vision request body: %v", err)
			return
		}
		for _, msg := range body.Messages {
			for _, blk := range msg.Content {
				if blk["type"] != "image_url" {
					continue
				}
				iu, _ := blk["image_url"].(map[string]any)
				url, _ := iu["url"].(string)
				// 形如 "data:image/jpeg;base64,..."，提取 media type。
				mime := strings.TrimPrefix(url, "data:")
				if idx := strings.IndexByte(mime, ';'); idx >= 0 {
					mime = mime[:idx]
				}
				receivedMime.Store(mime)
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"这是描述"}}]}`))
	}))
	defer ts.Close()

	webpBytes, err := base64.StdEncoding.DecodeString(onePixelWebP)
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(webpBytes)
	raw := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"` + b64 + `"}}]}]}`

	cfg := testConfig(ts.URL)
	engine := &Engine{Client: &VisionClient{HTTPClient: ts.Client()}}
	stats := &Stats{}
	_, err = engine.Enhance(context.Background(), []byte(raw), FormatClaude, cfg, stats)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", receivedMime.Load(), "视觉端点应收到 image/jpeg 而非 image/webp")
	assert.Equal(t, 1, stats.Success, "webp 图应被成功识别而非占位")
	assert.Equal(t, 0, stats.Failed, "webp 图不应产生占位")
}
