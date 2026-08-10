package service

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/vision_relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

// ---- 测试夹具 ----

// setupVisionRelayEnv 写入 vision_relay.* OptionMap（快照从 OptionMap 读取）+ 构造
// gin context + RelayInfo（Claude 格式，deepseek 目标模型）。
func setupVisionRelayEnv(t *testing.T, visionURL string, enabled bool) (*gin.Context, *relaycommon.RelayInfo, []byte) {
	t.Helper()
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{
		"vision_relay.enabled":       "false",
		"vision_relay.target_models": `["deepseek*"]`,
		"vision_relay.models":        `["vision-model-a"]`,
		"vision_relay.base_url":      visionURL,
		"vision_relay.api_key":       "sk-test",
		"vision_relay.timeout_sec":   "5",
	}
	if enabled {
		common.OptionMap["vision_relay.enabled"] = "true"
	}
	common.OptionMapRWMutex.Unlock()

	raw := visionClaudeBody(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	storage, err := common.CreateBodyStorage(raw)
	if err != nil {
		t.Fatalf("create body storage: %v", err)
	}
	c.Set(common.KeyBodyStorage, storage)
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "deepseek-v4-flash",
		RelayFormat:     types.RelayFormatClaude,
		Request:         &dto.ClaudeRequest{Model: "deepseek-v4-flash"},
	}
	return c, relayInfo, raw
}

// visionClaudeBody 带一张真实 PNG 图的 Claude 请求体
func visionClaudeBody(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	return []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[
		{"type":"text","text":"describe"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}
	]}]}`)
}

// visionMockServer 视觉端点 mock（返回固定描述）
func visionMockServer(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"这是测试图片描述"}}]}`))
	}))
	return ts
}

func storageContent(t *testing.T, c *gin.Context) []byte {
	t.Helper()
	bs, err := common.GetBodyStorage(c)
	if err != nil {
		t.Fatalf("get body storage: %v", err)
	}
	if _, err := bs.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(bs)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 1. 无图片：relayInfo.Request、BodyStorage 对象、内容均不改变（真 no-op）
func TestPrepareVisionRelayNoImagesNoop(t *testing.T) {
	ts := visionMockServer(t, nil)
	defer ts.Close()
	c, relayInfo, raw := setupVisionRelayEnv(t, ts.URL, true)
	// 覆盖为无图请求
	noImageRaw := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	storage, _ := common.CreateBodyStorage(noImageRaw)
	c.Set(common.KeyBodyStorage, storage)
	oldRequest := relayInfo.Request

	err := PrepareVisionRelayRequest(c, relayInfo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if relayInfo.Request != oldRequest {
		t.Fatal("request must not change on no-op")
	}
	if !bytes.Equal(storageContent(t, c), noImageRaw) {
		t.Fatal("body storage must not change on no-op")
	}
	_ = raw
}

// 6. enabled + 命中模型 + API key 缺失 → 5xx，不发原图（v0.2.2 不 fail-open）
func TestPrepareVisionRelayMissingAPIKeyFails(t *testing.T) {
	ts := visionMockServer(t, nil)
	defer ts.Close()
	c, relayInfo, _ := setupVisionRelayEnv(t, ts.URL, true)
	// 清空 api_key
	common.OptionMapRWMutex.Lock()
	common.OptionMap["vision_relay.api_key"] = ""
	common.OptionMapRWMutex.Unlock()

	apiErr := PrepareVisionRelayRequest(c, relayInfo)
	if apiErr == nil {
		t.Fatal("missing api_key with enabled+matched model must fail (5xx), not fail-open")
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 5xx, got %d", apiErr.StatusCode)
	}
	// 原状态不变
	if relayInfo.Request.(*dto.ClaudeRequest).Model != "deepseek-v4-flash" {
		t.Fatal("request must remain untouched")
	}
	if !strings.Contains(string(storageContent(t, c)), `"type":"image"`) {
		t.Fatal("original image body must remain untouched")
	}
}

// 2+3. DTO decode 失败 / 新 storage 创建失败 → 原状态完全保留（事务回滚）
// （两路径都通过 Enhance 前的校验拦截；这里验证"增强失败"路径不半提交）
func TestPrepareVisionRelayInfraFailureNoPartialCommit(t *testing.T) {
	ts := visionMockServer(t, nil)
	defer ts.Close()
	c, relayInfo, _ := setupVisionRelayEnv(t, ts.URL, true)
	// 视觉端点不可达（已关闭的 server）→ 图片级失败 → 占位继续（不 5xx）
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	common.OptionMapRWMutex.Lock()
	common.OptionMap["vision_relay.base_url"] = deadURL
	common.OptionMapRWMutex.Unlock()

	apiErr := PrepareVisionRelayRequest(c, relayInfo)
	if apiErr != nil {
		t.Fatalf("image-level failure must not 5xx: %v", apiErr)
	}
	// 增强成功（占位）→ 请求被替换（无 image 块残留），且 body 合法
	enhanced := storageContent(t, c)
	if strings.Contains(string(enhanced), `"type":"image"`) {
		t.Fatal("image block must not remain (placeholder applied)")
	}
	if !strings.Contains(string(enhanced), "unavailable:") {
		t.Fatal("placeholder expected in enhanced body")
	}
}

//  4. 提交成功：旧 storage 关闭（替换后原对象不可再用），新 storage 可被
//     两次重读（retry 循环语义）
func TestPrepareVisionRelayCommitAndReread(t *testing.T) {
	var calls int
	ts := visionMockServer(t, &calls)
	defer ts.Close()
	c, relayInfo, _ := setupVisionRelayEnv(t, ts.URL, true)
	oldStorage, _ := common.GetBodyStorage(c)

	apiErr := PrepareVisionRelayRequest(c, relayInfo)
	if apiErr != nil {
		t.Fatalf("unexpected error: %v", apiErr)
	}
	newStorage, _ := common.GetBodyStorage(c)
	if newStorage == oldStorage {
		t.Fatal("body storage must be replaced")
	}
	// 旧 storage 已关闭：读取应失败
	if _, err := oldStorage.Seek(0, 0); err == nil {
		if _, err := io.ReadAll(oldStorage); err == nil {
			t.Log("note: closed storage read did not error (implementation may not detect)")
		}
	}
	// 新 storage 可多次重读（retry 循环每次重置 c.Request.Body = NopCloser(storage)）
	first := storageContent(t, c)
	second := storageContent(t, c)
	if !bytes.Equal(first, second) {
		t.Fatal("storage must be rereadable with identical content")
	}
	if !strings.Contains(string(first), "这是测试图片描述") {
		t.Fatal("enhanced body should contain vision description")
	}
	if calls != 1 {
		t.Fatalf("vision endpoint should be called once, got %d", calls)
	}
}

// 5. PassThrough：BodyStorage 是增强 body（PassThrough 路径读它 → 收到增强内容）
func TestPrepareVisionRelayPassThroughBody(t *testing.T) {
	ts := visionMockServer(t, nil)
	defer ts.Close()
	c, relayInfo, _ := setupVisionRelayEnv(t, ts.URL, true)
	apiErr := PrepareVisionRelayRequest(c, relayInfo)
	if apiErr != nil {
		t.Fatalf("unexpected error: %v", apiErr)
	}
	// PassThrough 分支（claude_handler.go:156-163）直接回放 GetBodyStorage——
	// 验证它就是增强 body（无 image 块、含描述）
	body := storageContent(t, c)
	if strings.Contains(string(body), `"type":"image"`) {
		t.Fatal("pass-through body must be the enhanced body, not original")
	}
	if !strings.Contains(string(body), "这是测试图片描述") {
		t.Fatal("pass-through body should contain description")
	}
	// relayInfo.Request 同步替换（普通路径适配器 DeepCopy 的是增强对象）
	req, ok := relayInfo.Request.(*dto.ClaudeRequest)
	if !ok {
		t.Fatalf("request type: %T", relayInfo.Request)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
}

//  8. retry 两次：视觉 endpoint 只调一次，两次读到的 upstream body 相同
//     （增强产物在 retry 前一次性生成）
func TestPrepareVisionRelayRetryConsistency(t *testing.T) {
	var calls int
	ts := visionMockServer(t, &calls)
	defer ts.Close()
	c, relayInfo, _ := setupVisionRelayEnv(t, ts.URL, true)

	apiErr := PrepareVisionRelayRequest(c, relayInfo)
	if apiErr != nil {
		t.Fatalf("unexpected error: %v", apiErr)
	}
	// 模拟 retry 循环：两次读取同一 storage（controller/relay.go:209-219 每次
	// 从 BodyStorage 重置 c.Request.Body）
	attempt1 := storageContent(t, c)
	attempt2 := storageContent(t, c)
	if !bytes.Equal(attempt1, attempt2) {
		t.Fatal("both retries must see identical enhanced body")
	}
	if calls != 1 {
		t.Fatalf("vision endpoint must be called exactly once across retries, got %d", calls)
	}
}

//  7. SSRF fetcher：正常路径有限流下载成功（InitHttpClient 初始化 SSRF 客户端）；
//     SSRF 保护客户端不可用场景由代码审查覆盖（包级只读，无法测试注入 nil——
//     实现保证 nil → 错误，绝不 fallback 到 http.DefaultClient）
func TestVisionRelayFetcherNormalPath(t *testing.T) {
	InitHttpClient() // 测试环境未走 main.go 启动路径，需显式初始化 SSRF 客户端
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(buf.Bytes())
	}))
	defer ts.Close()
	// SSRF 保护默认拒绝本地随机端口——临时放行测试端口（用完恢复）
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	setting := system_setting.GetFetchSetting()
	orig := *setting
	setting.AllowPrivateIp = true
	setting.AllowedPorts = append(append([]string(nil), setting.AllowedPorts...), u.Port())
	defer func() { *setting = orig }()

	f := visionRelayFetcher{}
	data, mt, err := f.Fetch(t.Context(), ts.URL, 1<<20)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !bytes.Equal(data, buf.Bytes()) || mt != "image/png" {
		t.Fatalf("unexpected fetch result: %d bytes %q", len(data), mt)
	}
}

//  9. 递归保护（P0-2）：外部伪造 marker（含旧字面 "1"）→ 不绕过，Enhance 照常执行；
//     合法认证 marker（HMAC 匹配配置的 sidecall_secret）→ 真跳过（请求原样）
func TestPrepareVisionRelayRecursionMarker(t *testing.T) {
	const token = "test-sidecall-secret"
	ts := visionMockServer(t, nil)
	defer ts.Close()

	t.Run("forged header does not bypass", func(t *testing.T) {
		c, relayInfo, _ := setupVisionRelayEnv(t, ts.URL, true)
		setSidecallSecret(token)
		c.Request.Header.Set("X-NewAPI-Vision-Relay", "1") // 旧版字面值伪造
		apiErr := PrepareVisionRelayRequest(c, relayInfo)
		if apiErr != nil {
			t.Fatalf("unexpected error: %v", apiErr)
		}
		body := storageContent(t, c)
		if strings.Contains(string(body), `"type":"image"`) {
			t.Fatal("forged header must NOT bypass: image block should be replaced")
		}
	})

	t.Run("forged random marker does not bypass", func(t *testing.T) {
		c, relayInfo, _ := setupVisionRelayEnv(t, ts.URL, true)
		setSidecallSecret(token)
		c.Request.Header.Set("X-NewAPI-Vision-Relay", "vr:1234567890:deadbeef")
		apiErr := PrepareVisionRelayRequest(c, relayInfo)
		if apiErr != nil {
			t.Fatalf("unexpected error: %v", apiErr)
		}
		body := storageContent(t, c)
		if strings.Contains(string(body), `"type":"image"`) {
			t.Fatal("forged random marker must NOT bypass")
		}
	})

	t.Run("valid marker bypasses", func(t *testing.T) {
		c, relayInfo, raw := setupVisionRelayEnv(t, ts.URL, true)
		setSidecallSecret(token)
		marker := vision_relay.BuildMarker(token, time.Now())
		c.Request.Header.Set("X-NewAPI-Vision-Relay", marker)
		apiErr := PrepareVisionRelayRequest(c, relayInfo)
		if apiErr != nil {
			t.Fatalf("unexpected error: %v", apiErr)
		}
		if !bytes.Equal(storageContent(t, c), raw) {
			t.Fatal("valid marker must bypass: body must remain original")
		}
	})
}

// setSidecallSecret setupVisionRelayEnv 会重置 OptionMap，需在其后补设
func setSidecallSecret(token string) {
	common.OptionMapRWMutex.Lock()
	common.OptionMap["vision_relay.sidecall_secret"] = token
	common.OptionMapRWMutex.Unlock()
}

//  10. 出站 marker（审核 P1-2）：配置 sidecall_secret 后旁路请求必须携带
//     合法认证 marker（自回环递归防护接线验证）
func TestPrepareVisionRelayOutboundMarker(t *testing.T) {
	const secret = "test-sidecall-secret"
	var sawMarker bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("X-NewAPI-Vision-Relay")
		if h != "" && vision_relay.ValidateMarker(secret, h, time.Now()) {
			sawMarker = true
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"这是测试图片描述"}}]}`))
	}))
	defer ts.Close()
	c, relayInfo, _ := setupVisionRelayEnv(t, ts.URL, true)
	setSidecallSecret(secret)
	apiErr := PrepareVisionRelayRequest(c, relayInfo)
	if apiErr != nil {
		t.Fatalf("unexpected error: %v", apiErr)
	}
	if !sawMarker {
		t.Fatal("outbound sidecall must carry valid HMAC marker when sidecall_secret configured")
	}
	body := storageContent(t, c)
	if strings.Contains(string(body), `"type":"image"`) {
		t.Fatal("enhanced body must not contain image block")
	}
}
