package vision_relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(url string) Config {
	return Config{
		Enabled:    true,
		Models:     []string{"vision-model-a", "vision-model-b"},
		BaseURL:    url,
		APIKey:     "sk-test",
		TimeoutSec: 5,
	}
}

func testPatchedImage() *PatchedImage {
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return &PatchedImage{
		Patch:  Patch{Path: "messages.0.content.0", Index: 1, Source: ImageSource{MediaType: "image/png"}},
		Data:   buf.Bytes(),
		Digest: DigestBytes(buf.Bytes()),
	}
}

// 成功：断言递归保护头 + image_url data URL + 结果解析（验收 13 部分）
func TestDescribeOneSuccess(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("X-NewAPI-Vision-Relay") != "1" {
			t.Error("recursion protection header missing")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad request body: %v", err)
			return
		}
		msgs := body["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		foundImage := false
		for _, blk := range content {
			b := blk.(map[string]any)
			if b["type"] == "image_url" {
				foundImage = true
				iu := b["image_url"].(map[string]any)
				if !strings.HasPrefix(iu["url"].(string), "data:image/png;base64,") {
					t.Error("image_url should be data URL")
				}
			}
		}
		if !foundImage {
			t.Error("request body missing image_url block")
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"这是图片描述"}}]}`))
	}))
	defer ts.Close()
	client := &VisionClient{HTTPClient: ts.Client()}
	desc, enum, model := client.DescribeOne(context.Background(), "指令", testPatchedImage(), testConfig(ts.URL))
	if enum != "" || desc != "这是图片描述" || model != "vision-model-a" {
		t.Fatalf("unexpected: desc=%q enum=%q model=%s", desc, enum, model)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

// 错误矩阵（门槛七）：400→unsupported_format 停止；413→size_limit；
// 451→blocked；401/403→终止 fallback（只调一次，验收 25）
func TestDescribeOneErrorMatrix(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantEnum  string
		wantCalls int
	}{
		{"400 停止该图", http.StatusBadRequest, `{"error":{"message":"bad image"}}`, EnumUnsupportedFormat, 1},
		{"413 不 fallback", http.StatusRequestEntityTooLarge, `{"error":{"message":"too large"}}`, EnumSizeLimit, 1},
		{"451 审核阻断", http.StatusUnavailableForLegalReasons, `{"error":{"message":"nsfw"}}`, EnumBlocked, 1},
		{"401 终止 fallback", http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`, EnumServiceUnavailable, 1},
		{"403 终止 fallback", http.StatusForbidden, `{"error":{"message":"forbidden"}}`, EnumServiceUnavailable, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			client := &VisionClient{HTTPClient: ts.Client()}
			_, enum, _ := client.DescribeOne(context.Background(), "指令", testPatchedImage(), testConfig(ts.URL))
			if enum != tc.wantEnum {
				t.Fatalf("expected enum %s, got %s", tc.wantEnum, enum)
			}
			if n := atomic.LoadInt32(&calls); int(n) != tc.wantCalls {
				t.Fatalf("expected %d calls, got %d", tc.wantCalls, n)
			}
		})
	}
}

// 瞬时错误重试 1 次仍失败 → fallback 切下一模型成功
func TestDescribeOneFallback(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"no channel"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"fallback 成功"}}]}`))
	}))
	defer ts.Close()
	client := &VisionClient{HTTPClient: ts.Client()}
	desc, enum, model := client.DescribeOne(context.Background(), "指令", testPatchedImage(), testConfig(ts.URL))
	if enum != "" || desc != "fallback 成功" || model != "vision-model-b" {
		t.Fatalf("unexpected: desc=%q enum=%q model=%s", desc, enum, model)
	}
	// model-a 重试 1 次（2 次调用）失败 + model-b 成功 = 3 次
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("expected 3 calls (retry + fallback), got %d", calls)
	}
}

// 空 choices/空 content → 换下一模型
func TestDescribeOneEmptyResponse(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Write([]byte(`{"choices":[]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"第二次成功"}}]}`))
	}))
	defer ts.Close()
	client := &VisionClient{HTTPClient: ts.Client()}
	desc, enum, _ := client.DescribeOne(context.Background(), "指令", testPatchedImage(), testConfig(ts.URL))
	if enum != "" || desc != "第二次成功" {
		t.Fatalf("unexpected: desc=%q enum=%q", desc, enum)
	}
}

// 取消：ctx 取消 → 旁路调用快速失败（验收 15）
func TestDescribeOneCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // 永远不响应（ctx 50ms 取消）
	}))
	defer ts.Close()
	client := &VisionClient{HTTPClient: ts.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, enum, _ := client.DescribeOne(ctx, "指令", testPatchedImage(), testConfig(ts.URL))
	if enum == "" {
		t.Fatal("cancel should yield failure enum")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancel should be fast, took %v", elapsed)
	}
}

// 端到端编排：digest 分组去重——同一图出现两次只调一次视觉端点（验收 24）
func TestEngineEnhanceDedup(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"同图描述"}}]}`))
	}))
	defer ts.Close()
	pngData := makePNG(t, 20, 20)
	b64 := base64.StdEncoding.EncodeToString(pngData)
	raw := `{"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}
	]}]}`
	cfg := testConfig(ts.URL)
	engine := &Engine{Client: &VisionClient{HTTPClient: ts.Client()}}
	stats := &Stats{}
	enhanced, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, cfg, stats)
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if stats.Total != 2 {
		t.Fatalf("expected 2 images, got %d", stats.Total)
	}
	if stats.Success != 1 {
		t.Fatalf("expected 1 success (dedup), got %d", stats.Success)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("vision endpoint should be called once, got %d", calls)
	}
	out := string(enhanced)
	if strings.Contains(out, `"type":"image"`) {
		t.Error("image block remains")
	}
	if !strings.Contains(out, "同图描述") {
		t.Error("description missing in output")
	}
}

// 编排：无图请求真 no-op（原样返回）
func TestEngineEnhanceNoImages(t *testing.T) {
	raw := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"model":"deepseek"}`
	engine := &Engine{}
	stats := &Stats{}
	enhanced, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, Config{}, stats)
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if enhanced != nil {
		t.Fatal("no-image request should return nil (no-op)")
	}
	if stats.Total != 0 {
		t.Fatalf("expected 0 images, got %d", stats.Total)
	}
}

// 编排：允许不命中的策略在 service 层，这里验证 engine 对超限图片数量占位（A4）
func TestEngineEnhanceImageLimitPlaceholder(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"描述"}}]}`))
	}))
	defer ts.Close()
	// MaxImages=6，构造 8 张不同图（不同颜色 PNG）
	var blocks []string
	for i := 0; i < 8; i++ {
		img := image.NewRGBA(image.Rect(0, 0, 10, 10))
		img.Set(0, 0, color.RGBA{R: uint8(i * 30), A: 255})
		var buf bytes.Buffer
		_ = png.Encode(&buf, img)
		b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
		blocks = append(blocks, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+b64+`"}}`)
	}
	raw := `{"messages":[{"role":"user","content":[` + strings.Join(blocks, ",") + `]}]}`
	engine := &Engine{Client: &VisionClient{HTTPClient: ts.Client()}}
	stats := &Stats{}
	enhanced, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, testConfig(ts.URL), stats)
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if stats.Omitted != 2 {
		t.Fatalf("expected 2 omitted, got %d", stats.Omitted)
	}
	out := string(enhanced)
	if strings.Contains(out, `"type":"image"`) {
		t.Error("image block remains after limit")
	}
	if !strings.Contains(out, "image_limit") {
		t.Error("omitted images should have image_limit placeholder")
	}
}
