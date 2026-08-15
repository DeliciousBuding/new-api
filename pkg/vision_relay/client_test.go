package vision_relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testConfig(url string) Config {
	return Config{
		Enabled:        true,
		Models:         []string{"vision-model-a", "vision-model-b"},
		BaseURL:        url,
		APIKey:         "sk-test",
		TimeoutSec:     5,
		SidecallSecret: "test-sidecall-secret",
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

// testImageData 测试用 PNG 数据（DescribeOne 直接收压缩后数据）
func testImageData() []byte {
	return testPatchedImage().Data
}

// 成功：断言认证 marker（HMAC 校验通过，P0-2）+ image_url data URL + 结果解析（验收 13 部分）
func TestDescribeOneSuccess(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if h := r.Header.Get("X-NewAPI-Vision-Relay"); h == "" ||
			!ValidateMarker("test-sidecall-secret", h, time.Now()) {
			t.Error("recursion protection marker missing or invalid")
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
	r := client.DescribeOne(context.Background(), "指令", testImageData(), "image/png", testConfig(ts.URL))
	if r.Enum != "" || r.Desc != "这是图片描述" || r.Model != "vision-model-a" {
		t.Fatalf("unexpected: desc=%q enum=%q model=%s", r.Desc, r.Enum, r.Model)
	}
	if r.Abort {
		t.Fatal("success must not abort")
	}
	if r.HTTPCalls != 1 || r.Fallbacks != 0 {
		t.Fatalf("expected 1 http call 0 fallbacks, got calls=%d fallbacks=%d", r.HTTPCalls, r.Fallbacks)
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
		{"401 终止 fallback", http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`, EnumAuthError, 1},
		{"403 终止 fallback", http.StatusForbidden, `{"error":{"message":"forbidden"}}`, EnumAuthError, 1},
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
			r := client.DescribeOne(context.Background(), "指令", testImageData(), "image/png", testConfig(ts.URL))
			if r.Enum != tc.wantEnum {
				t.Fatalf("expected enum %s, got %s", tc.wantEnum, r.Enum)
			}
			if r.HTTPCalls != tc.wantCalls {
				t.Fatalf("expected %d http calls, got %d", tc.wantCalls, r.HTTPCalls)
			}
			if n := atomic.LoadInt32(&calls); int(n) != tc.wantCalls {
				t.Fatalf("expected %d calls, got %d", tc.wantCalls, n)
			}
		})
	}
}

// provider 瞬时错误（503）不重试 → fallback 切下一模型成功（v0.2.2 语义）
func TestDescribeOneFallback(t *testing.T) {
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
	client := &VisionClient{HTTPClient: ts.Client()}
	r := client.DescribeOne(context.Background(), "指令", testImageData(), "image/png", testConfig(ts.URL))
	if r.Enum != "" || r.Desc != "fallback 成功" || r.Model != "vision-model-b" {
		t.Fatalf("unexpected: desc=%q enum=%q model=%s", r.Desc, r.Enum, r.Model)
	}
	// 503 不重试：model-a 1 次 + model-b 1 次 = 2 次；fallback 真实切换 1 次
	if r.HTTPCalls != 2 || r.Fallbacks != 1 {
		t.Fatalf("expected 2 http calls 1 fallback, got calls=%d fallbacks=%d", r.HTTPCalls, r.Fallbacks)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 calls (no retry, direct fallback), got %d", calls)
	}
}

// 传输错误（连接断开）→ 同模型重试 1 次（v0.2.2：仅传输层错误重试）
func TestDescribeOneTransportRetry(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// 模拟传输层失败：Hijack 后直接断开连接
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"重试成功"}}]}`))
	}))
	defer ts.Close()
	client := &VisionClient{HTTPClient: ts.Client()}
	r := client.DescribeOne(context.Background(), "指令", testImageData(), "image/png", testConfig(ts.URL))
	if r.Enum != "" || r.Desc != "重试成功" || r.Model != "vision-model-a" {
		t.Fatalf("unexpected: desc=%q enum=%q model=%s", r.Desc, r.Enum, r.Model)
	}
	// 同模型重试 1 次 = 2 次调用；传输错误不算 fallback
	if r.HTTPCalls != 2 || r.Fallbacks != 0 {
		t.Fatalf("expected 2 http calls 0 fallbacks, got calls=%d fallbacks=%d", r.HTTPCalls, r.Fallbacks)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 calls (transport retry), got %d", calls)
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
	r := client.DescribeOne(context.Background(), "指令", testImageData(), "image/png", testConfig(ts.URL))
	if r.Enum != "" || r.Desc != "第二次成功" {
		t.Fatalf("unexpected: desc=%q enum=%q", r.Desc, r.Enum)
	}
	// 空响应 → 换下一模型：2 次 HTTP、1 次 fallback
	if r.HTTPCalls != 2 || r.Fallbacks != 1 {
		t.Fatalf("expected 2 http calls 1 fallback, got calls=%d fallbacks=%d", r.HTTPCalls, r.Fallbacks)
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
	r := client.DescribeOne(ctx, "指令", testImageData(), "image/png", testConfig(ts.URL))
	if r.Enum == "" {
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
	if stats.Success != 2 {
		t.Fatalf("expected 2 success blocks (dedup: 2 blocks, 1 vision call), got %d", stats.Success)
	}
	if stats.UniqueImages != 1 || stats.CacheHits != 1 || stats.VisionCalls != 1 {
		t.Fatalf("expected unique=1 cache_hits=1 vision_calls=1, got unique=%d cache_hits=%d calls=%d",
			stats.UniqueImages, stats.CacheHits, stats.VisionCalls)
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
	// 第 7、8 张直接 image_limit 占位（不下载不解码不识别）
	if stats.Failed != 2 {
		t.Fatalf("expected 2 failed (image_limit), got %d", stats.Failed)
	}
	if stats.UniqueImages != 6 {
		t.Fatalf("expected 6 unique images processed, got %d", stats.UniqueImages)
	}
	out := string(enhanced)
	if strings.Contains(out, `"type":"image"`) {
		t.Error("image block remains after limit")
	}
	if !strings.Contains(out, "image_limit") {
		t.Error("omitted images should have image_limit placeholder")
	}
}

// 401/403 → Abort=true（请求级熔断标记），不 retry 不 fallback（P0-2 §3）
func TestDescribeOneAuthAbort(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("HTTP%d", status), func(t *testing.T) {
			var calls int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(status)
				w.Write([]byte(`{"error":{"message":"invalid key"}}`))
			}))
			defer ts.Close()
			client := &VisionClient{HTTPClient: ts.Client()}
			r := client.DescribeOne(context.Background(), "指令", testImageData(), "image/png", testConfig(ts.URL))
			if !r.Abort {
				t.Fatal("401/403 must set Abort (request-global auth failure)")
			}
			if r.Enum != EnumAuthError {
				t.Fatalf("expected auth_error enum, got %s", r.Enum)
			}
			if r.HTTPCalls != 1 || r.Fallbacks != 0 {
				t.Fatalf("auth failure must not retry/fallback: calls=%d fallbacks=%d", r.HTTPCalls, r.Fallbacks)
			}
			if atomic.LoadInt32(&calls) != 1 {
				t.Fatalf("expected exactly 1 http call, got %d", calls)
			}
		})
	}
}

// 请求级熔断（P0-2 §3 必测）：6 张唯一图 + 401 → 首个 401 后停止后续 sidecall，
// HTTP 总数 <= RequestConcurrency(2)；最终零 image 块（全部稳定占位）
func TestEngineEnhanceAuthCircuitBreak(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer ts.Close()
	var blocks []string
	for i := 0; i < 6; i++ { // 6 张唯一图（不同颜色 → 不同 digest）
		img := image.NewRGBA(image.Rect(0, 0, 10, 10))
		img.Set(0, 0, color.RGBA{R: uint8(10 + i*30), G: uint8(i * 5), A: 255})
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
	n := atomic.LoadInt32(&calls)
	if n > RequestConcurrency {
		t.Fatalf("sidecall must be bounded by RequestConcurrency=%d, got %d", RequestConcurrency, n)
	}
	if n == 0 {
		t.Fatal("at least one sidecall must have happened")
	}
	out := string(enhanced)
	if strings.Contains(out, `"type":"image"`) {
		t.Error("image block remains after auth circuit break")
	}
	if stats.Failed != 6 {
		t.Fatalf("all 6 images must fail (placeholder), got %d", stats.Failed)
	}
	if stats.VisionCalls > RequestConcurrency {
		t.Fatalf("stats.VisionCalls=%d must be <= RequestConcurrency", stats.VisionCalls)
	}
}

// 敏感词检查（审核 P1-3/A6）：SensitiveCheck 命中 → blocked 稳定占位，不注入原文
func TestEngineEnhanceSensitiveCheck(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"敏感内容描述"}}]}`))
	}))
	defer ts.Close()
	engine := &Engine{
		Client: &VisionClient{HTTPClient: ts.Client()},
		SensitiveCheck: func(desc string) bool {
			return strings.Contains(desc, "敏感")
		},
	}
	stats := &Stats{}
	enhanced, err := engine.Enhance(context.Background(),
		[]byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+base64.StdEncoding.EncodeToString(testImageData())+`"}}]}]}`),
		FormatClaude, testConfig(ts.URL), stats)
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}
	out := string(enhanced)
	if strings.Contains(out, "敏感内容描述") {
		t.Fatal("sensitive description must not be injected")
	}
	if !strings.Contains(out, "blocked") {
		t.Fatal("sensitive image must be replaced with blocked placeholder")
	}
	if stats.Success != 0 || stats.Failed != 1 {
		t.Fatalf("expected failed=1 success=0, got success=%d failed=%d", stats.Success, stats.Failed)
	}
}

// fakeDescriptionCache 测试用缓存：Get 命中注入描述，Set/Delete 记录调用。
type fakeDescriptionCache struct {
	hits    map[string]string
	sets    []string
	deletes []string
}

func (f *fakeDescriptionCache) Get(ctx context.Context, key string) (string, bool) {
	if f.hits == nil {
		return "", false
	}
	v, ok := f.hits[key]
	return v, ok
}

func (f *fakeDescriptionCache) Set(ctx context.Context, key, value string) error {
	f.sets = append(f.sets, value)
	return nil
}

func (f *fakeDescriptionCache) Delete(ctx context.Context, key string) error {
	f.deletes = append(f.deletes, key)
	return nil
}

// 网络层错误分类：deadline 耗尽 → ErrTimeout（不重试）；其余 → transportError（可重试）
func TestClassifyNetworkErr(t *testing.T) {
	timeout := classifyNetworkErr(context.DeadlineExceeded)
	if !errors.Is(timeout, ErrTimeout) {
		t.Fatalf("context.DeadlineExceeded should classify to ErrTimeout, got %v", timeout)
	}
	var te *transportError
	if errors.As(timeout, &te) {
		t.Fatal("timeout must not classify to transportError (no same-model retry)")
	}
	conn := classifyNetworkErr(io.ErrUnexpectedEOF)
	if !errors.As(conn, &te) {
		t.Fatalf("generic network error should classify to transportError, got %v", conn)
	}
	if errors.Is(conn, ErrTimeout) {
		t.Fatal("generic network error must not classify to ErrTimeout")
	}
}

// 取消 → 原样返回（不归为可重试 transportError、不包成 ErrTimeout）：客户端
// 已断连，重试与换模型都无意义，DescribeOne 的 ctx.Err() 检查会提前终止链。
func TestClassifyNetworkErrCanceled(t *testing.T) {
	got := classifyNetworkErr(context.Canceled)
	require.Equal(t, context.Canceled, got, "context.Canceled must be returned as-is")
	var te *transportError
	require.False(t, errors.As(got, &te), "context.Canceled must not classify to retryable transportError")
	require.False(t, errors.Is(got, ErrTimeout), "context.Canceled must not classify to ErrTimeout")
}

// 图片级错误 → 稳定枚举：超时/鉴权有独立枚举，不与 provider 瞬时错误混淆
func TestEnumFromErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"timeout", ErrTimeout, EnumTimeout},
		{"auth", ErrAuth, EnumAuthError},
		{"size", ErrSizeLimit, EnumSizeLimit},
		{"unsupported", ErrUnsupported, EnumUnsupportedFormat},
		{"extract", ErrExtract, EnumUnsupportedFormat},
		{"blocked", ErrBlocked, EnumBlocked},
		{"image limit", ErrImageLimit, EnumImageLimit},
		{"unknown → service_unavailable", errors.New("boom"), EnumServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := enumFromErr(tc.err); got != tc.want {
				t.Fatalf("enumFromErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// 闸获取失败（ctx 取消/超时）的占位枚举：deadline 耗尽 → timeout（不吞成
// service_unavailable），其余（含客户端取消）→ 兜底 service_unavailable。
func TestGateErrEnum(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline exhausted → timeout", context.DeadlineExceeded, EnumTimeout},
		{"client canceled → service_unavailable", context.Canceled, EnumServiceUnavailable},
		{"other error → service_unavailable", errors.New("boom"), EnumServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, gateErrEnum(tc.err))
		})
	}
}

// 超时（ctx deadline 耗尽）→ 如实上报 timeout 枚举（不吞成 service_unavailable）
func TestDescribeOneTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // 不响应，直到客户端 deadline 耗尽
	}))
	defer ts.Close()
	client := &VisionClient{HTTPClient: ts.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := client.DescribeOne(ctx, "指令", testImageData(), "image/png", testConfig(ts.URL))
	if r.Enum != EnumTimeout {
		t.Fatalf("deadline exceeded must surface timeout enum, got %q", r.Enum)
	}
}

// 已取消的 ctx → DescribeOne 立即停止链，不发任何 sidecall（0 次 HTTP、0 fallback）
func TestDescribeOneCanceledStopsChain(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"不应被调用"}}]}`))
	}))
	defer ts.Close()
	client := &VisionClient{HTTPClient: ts.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := client.DescribeOne(ctx, "指令", testImageData(), "image/png", testConfig(ts.URL))
	require.Equal(t, EnumServiceUnavailable, r.Enum)
	require.Zero(t, r.HTTPCalls)
	require.Zero(t, r.Fallbacks)
	require.Zero(t, atomic.LoadInt32(&calls), "canceled ctx must not issue any sidecall")
}

// budgetProbeTransport 记录两次尝试的请求 deadline；第一次返回传输错误触发
// 重试，第二次成功——用于断言重试共享同一 deadline（不重新给完整 timeout）。
type budgetProbeTransport struct {
	firstDeadline  time.Time
	secondDeadline time.Time
}

func (t *budgetProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, _ := req.Context().Deadline()
	if t.firstDeadline.IsZero() {
		t.firstDeadline = deadline
		time.Sleep(200 * time.Millisecond) // 消耗预算，让重试的预算递减可观察
		return nil, errors.New("connection reset by peer")
	}
	t.secondDeadline = deadline
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
	}, nil
}

// 传输错误重试不得重新拿到完整 timeout：两次尝试共享同一 deadline，否则
// 单模型总耗时会随重试翻倍（回归保护）。
func TestCallRetrySharesBudget(t *testing.T) {
	probe := &budgetProbeTransport{}
	client := &VisionClient{HTTPClient: &http.Client{Transport: probe}}
	text, calls, err := client.Call(context.Background(), "model", "指令",
		testImageData(), "image/png", "http://unused", "sk", "", 500*time.Millisecond, DefaultMaxTokens)
	require.NoError(t, err)
	require.Equal(t, "ok", text)
	require.Equal(t, 2, calls, "transport error must retry exactly once")
	// 第二次尝试的 deadline 应与第一次几乎相同（共享同一预算），而不是
	// 第一次 deadline + 一个完整 timeout。
	require.WithinDuration(t, probe.firstDeadline, probe.secondDeadline, 50*time.Millisecond,
		"retry must share the first attempt's deadline, not get a fresh full timeout")
}

// 跨请求缓存命中 → 跳过旁路调用（CacheServed 计数 + 描述直接复用）
func TestEngineEnhanceCacheHit(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"不应被调用"}}]}`))
	}))
	defer ts.Close()
	pngData := testImageData()
	cfg := testConfig(ts.URL)
	cache := &fakeDescriptionCache{hits: map[string]string{
		descriptionCacheKey(DigestBytes(pngData), BuildInstruction(cfg)): "缓存描述",
	}}
	engine := &Engine{Client: &VisionClient{HTTPClient: ts.Client()}, Cache: cache}
	stats := &Stats{}
	raw := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(pngData) + `"}}]}]}`
	enhanced, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, cfg, stats)
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if stats.Success != 1 || stats.CacheServed != 1 || stats.VisionCalls != 0 {
		t.Fatalf("cache hit: success=%d cache_served=%d vision_calls=%d", stats.Success, stats.CacheServed, stats.VisionCalls)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("cache hit must skip vision sidecall, got %d calls", calls)
	}
	if !strings.Contains(string(enhanced), "缓存描述") {
		t.Fatal("cached description should be injected")
	}
}

// 缓存未命中 → 正常识图，成功后写缓存（Set 记录描述值）
func TestEngineEnhanceCacheWrite(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"新识图描述"}}]}`))
	}))
	defer ts.Close()
	cache := &fakeDescriptionCache{}
	engine := &Engine{Client: &VisionClient{HTTPClient: ts.Client()}, Cache: cache}
	stats := &Stats{}
	raw := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(testImageData()) + `"}}]}]}`
	if _, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, testConfig(ts.URL), stats); err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("cache miss must call vision sidecall once, got %d", calls)
	}
	if len(cache.sets) != 1 || cache.sets[0] != "新识图描述" {
		t.Fatalf("successful description must be cached, got %v", cache.sets)
	}
	if stats.CacheServed != 0 {
		t.Fatalf("cache miss must not count CacheServed, got %d", stats.CacheServed)
	}
}

// 缓存命中但命中敏感词（词库热更新）→ 丢弃缓存走正常识图
func TestEngineEnhanceCacheSensitiveDiscard(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"正常描述"}}]}`))
	}))
	defer ts.Close()
	pngData := testImageData()
	cfg := testConfig(ts.URL)
	cache := &fakeDescriptionCache{hits: map[string]string{
		descriptionCacheKey(DigestBytes(pngData), BuildInstruction(cfg)): "敏感缓存描述",
	}}
	engine := &Engine{
		Client:         &VisionClient{HTTPClient: ts.Client()},
		Cache:          cache,
		SensitiveCheck: func(desc string) bool { return strings.Contains(desc, "敏感") },
	}
	stats := &Stats{}
	raw := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(pngData) + `"}}]}]}`
	enhanced, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, cfg, stats)
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("sensitive cached value must be discarded and re-described, got %d calls", calls)
	}
	if !strings.Contains(string(enhanced), "正常描述") {
		t.Fatal("re-described description should be injected")
	}
	if stats.CacheServed != 0 {
		t.Fatalf("discarded cache hit must not count CacheServed, got %d", stats.CacheServed)
	}
}

// 缓存命中但命中敏感词（词库热更新）→ 删除污染 key 并走正常识图：
// 污染的缓存值不得注入，key 必须被 Delete 清除，视觉端点被调用一次。
func TestEngineEnhanceCacheSensitiveEvicts(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"重新识图描述"}}]}`))
	}))
	defer ts.Close()
	pngData := testImageData()
	cfg := testConfig(ts.URL)
	cacheKey := descriptionCacheKey(DigestBytes(pngData), BuildInstruction(cfg))
	cache := &fakeDescriptionCache{hits: map[string]string{
		cacheKey: "敏感缓存描述",
	}}
	engine := &Engine{
		Client:         &VisionClient{HTTPClient: ts.Client()},
		Cache:          cache,
		SensitiveCheck: func(desc string) bool { return strings.Contains(desc, "敏感") },
	}
	stats := &Stats{}
	raw := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(pngData) + `"}}]}]}`
	enhanced, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, cfg, stats)
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}
	// (a) 污染的缓存值不得注入，注入的是重新识图得到的描述
	out := string(enhanced)
	if strings.Contains(out, "敏感缓存描述") {
		t.Error("poisoned cached value must not be injected")
	}
	if !strings.Contains(out, "重新识图描述") {
		t.Error("freshly re-described value should be injected")
	}
	// (b) 污染的 key 必须被删除（后续请求不再命中污染值）
	if len(cache.deletes) != 1 || cache.deletes[0] != cacheKey {
		t.Fatalf("poisoned cache key must be deleted once, got %v", cache.deletes)
	}
	// (c) 丢弃缓存后必须重新识图：视觉端点被调用一次
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("vision endpoint should be hit once after eviction, got %d", calls)
	}
	if stats.CacheServed != 0 {
		t.Fatalf("discarded cache hit must not count CacheServed, got %d", stats.CacheServed)
	}
}

// 失败原因分布：image_limit（前置）与 unsupported_format（识图）按枚举精确计数
func TestEngineEnhanceFailedReasons(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 400 → unsupported_format
		w.Write([]byte(`{"error":{"message":"bad image"}}`))
	}))
	defer ts.Close()
	// 8 张唯一图（MaxImages=6）→ 2 张 image_limit + 6 张 unsupported_format
	var blocks []string
	for i := 0; i < 8; i++ {
		img := image.NewRGBA(image.Rect(0, 0, 10, 10))
		img.Set(0, 0, color.RGBA{R: uint8(i * 30), A: 255})
		var buf bytes.Buffer
		_ = png.Encode(&buf, img)
		blocks = append(blocks, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+base64.StdEncoding.EncodeToString(buf.Bytes())+`"}}`)
	}
	raw := `{"messages":[{"role":"user","content":[` + strings.Join(blocks, ",") + `]}]}`
	engine := &Engine{Client: &VisionClient{HTTPClient: ts.Client()}}
	stats := &Stats{}
	if _, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, testConfig(ts.URL), stats); err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if stats.Failed != 8 {
		t.Fatalf("expected 8 failed, got %d", stats.Failed)
	}
	if stats.FailedReasons[EnumImageLimit] != 2 {
		t.Fatalf("expected 2 image_limit, got %d", stats.FailedReasons[EnumImageLimit])
	}
	if stats.FailedReasons[EnumUnsupportedFormat] != 6 {
		t.Fatalf("expected 6 unsupported_format, got %d", stats.FailedReasons[EnumUnsupportedFormat])
	}
}

// 单图 401 → auth_error 计入失败原因分布（不 fallback 不 retry）
func TestEngineEnhanceFailedReasonsAuthError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer ts.Close()
	raw := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(testImageData()) + `"}}]}]}`
	engine := &Engine{Client: &VisionClient{HTTPClient: ts.Client()}}
	stats := &Stats{}
	if _, err := engine.Enhance(context.Background(), []byte(raw), FormatClaude, testConfig(ts.URL), stats); err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("expected 1 failed, got %d", stats.Failed)
	}
	if stats.FailedReasons[EnumAuthError] != 1 {
		t.Fatalf("expected 1 auth_error, got %d", stats.FailedReasons[EnumAuthError])
	}
}
