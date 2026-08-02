package vision_relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// 旁路错误哨兵（错误矩阵，门槛七）
var (
	ErrBlocked = errors.New("vision provider content blocked")   // 451
	ErrAuth    = errors.New("vision provider auth/config error") // 401/403 → 终止整条 fallback
)

// transportError 网络传输层错误（连接失败/超时）——唯一允许同模型重试的类型
// （v0.2.2：429/5xx/空响应等 provider 瞬时错误不重试，直接换模型）。
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// 默认识图指令（保真基线，与 vision-bridge 同款防幻觉前缀）
const defaultInstruction = "你是图片转述桥接器。你的输出会被原样注入给另一个看不到图片的文本模型，" +
	"它完全依赖你的描述来回答用户问题，所以保真度优先：能抄录的（文字、代码、报错、配置、表格数据）" +
	"尽量原样抄录，不概括、不改写；看不清或不确定的内容标注（看不清），绝不猜测编造。" +
	"表格转 Markdown；代码/报错保持原文；UI 按 顶部→中部→底部 说明布局和关键控件；" +
	"普通图描述主体、动作、背景、细节。只描述这一张图片，不评论、不解释。"

// enumFromErr 把图片级错误映射为稳定枚举（映射失败 → service_unavailable）
func enumFromErr(err error) string {
	switch {
	case err == nil:
		return EnumServiceUnavailable
	case errors.Is(err, ErrSizeLimit):
		return EnumSizeLimit
	case errors.Is(err, ErrUnsupported) || errors.Is(err, ErrExtract):
		return EnumUnsupportedFormat
	case errors.Is(err, ErrBlocked):
		return EnumBlocked
	case errors.Is(err, ErrImageLimit):
		return EnumImageLimit
	default:
		return EnumServiceUnavailable
	}
}

// placeholderUnavailable 构造占位文本（隐私安全：仅稳定枚举 + media_type，
// 不含 URL/key/模型名/provider 错误体——审核 §8.3）
func placeholderUnavailable(p Patch, enum string, total int) string {
	mt := p.Source.MediaType
	if mt == "" {
		mt = "image/unknown"
	}
	return fmt.Sprintf("[Image %d/%d unavailable: %s, original_media_type=%s]", p.Index, total, enum, mt)
}

// wrapResult 成功描述带 untrusted 边界（保持非空）
func wrapResult(index, total int, desc string) string {
	return fmt.Sprintf(ResultPrefix, index, total) + "\n" + desc + "\n" + ResultSuffix
}

// BuildInstruction 识图指令（配置自定义或默认保真基线）
func BuildInstruction(cfg Config) string {
	if p := strings.TrimSpace(cfg.Prompt); p != "" {
		return p
	}
	return defaultInstruction
}

// VisionClient 视觉端点客户端（纯核心；调用方提供 http.Client）
type VisionClient struct {
	HTTPClient *http.Client
}

// Call 单次旁路调用。错误矩阵（门槛七 + v0.2.2 语义）：
//
//	网络传输错误（transportError）：同模型重试 1 次（预算内）
//	429 / 5xx / 空 choices / 空 content / 非 JSON：provider 瞬时错误 → 换下一模型（不重试）
//	400 → ErrUnsupported（停止该图，不遍历其他模型）
//	401/403 → ErrAuth（终止整条 fallback，防 key 错误打三模型）
//	413 → ErrSizeLimit（不 fallback）
//	451 → ErrBlocked（当前图停止）
// Call 单次旁路调用。错误矩阵（门槛七 + v0.2.2 语义）：
//
//	网络传输错误（transportError）：同模型重试 1 次（预算内）
//	429 / 5xx / 空 choices / 空 content / 非 JSON：provider 瞬时错误 → 换下一模型（不重试）
//	400 → ErrUnsupported（停止该图，不遍历其他模型）
//	401/403 → ErrAuth（请求级熔断；该图不重试不 fallback）
//	413 → ErrSizeLimit（不 fallback）
//	451 → ErrBlocked（当前图停止）
//
// sidecallToken 非空时携带递归保护认证 marker（审核 P0-2：目标实例只有
// HMAC 校验通过才允许 bypass，外部伪造不可绕过）。
// 返回 (文本, 实际 HTTP 请求次数, 错误)——calls 含 transport retry（P2-6）。
func (c *VisionClient) Call(ctx context.Context, model, instruction string, data []byte, mediaType, baseURL, apiKey, sidecallToken string, timeout time.Duration, maxTokens int) (string, int, error) {
	body, err := buildVisionPayload(model, instruction, data, mediaType, maxTokens)
	if err != nil {
		return "", 0, err
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	var text string
	calls := 0
	for attempt := 0; attempt <= 1; attempt++ { // 仅传输错误重试 ≤1（预算内）
		text, err = c.doRequest(ctx, client, model, baseURL, apiKey, sidecallToken, body, timeout)
		calls++
		var te *transportError
		if err == nil || !errors.As(err, &te) {
			break
		}
	}
	return text, calls, err
}

func buildVisionPayload(model, instruction string, data []byte, mediaType string, maxTokens int) ([]byte, error) {
	maxTokensUint := uint(maxTokens)
	payload := map[string]any{
		"model":      model,
		"max_tokens": maxTokensUint,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": instruction},
					map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url": fmt.Sprintf("data:%s;base64,%s", mediaType, base64.StdEncoding.EncodeToString(data)),
						},
					},
				},
			},
		},
	}
	return common.Marshal(payload)
}

func (c *VisionClient) doRequest(ctx context.Context, client *http.Client, model, baseURL, apiKey, sidecallToken string, body []byte, timeout time.Duration) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// 递归保护（审核 P0-2）：认证 marker（HMAC），目标实例校验通过才跳过；
	// token 未配置则不携带（防递归由模型匹配天然保证）
	if sidecallToken != "" {
		req.Header.Set("X-NewAPI-Vision-Relay", BuildMarker(sidecallToken, time.Now()))
	}
	resp, err := client.Do(req)
	if err != nil {
		// 网络传输错误（连接失败/超时）——同模型重试 1 次
		return "", &transportError{err: err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		if readErr != nil {
			return "", &transportError{err: readErr}
		}
		var result struct {
			Choices []struct {
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := common.Unmarshal(respBody, &result); err != nil {
			return "", fmt.Errorf("invalid vision response: %v", err)
		}
		if len(result.Choices) == 0 {
			return "", fmt.Errorf("empty vision choices")
		}
		s := contentString(result.Choices[0].Message.Content)
		if strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("empty vision content")
		}
		return s, nil
	case resp.StatusCode == http.StatusBadRequest:
		return "", fmt.Errorf("%w: HTTP 400", ErrUnsupported)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%w: HTTP %d", ErrAuth, resp.StatusCode)
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return "", fmt.Errorf("%w: HTTP 413", ErrSizeLimit)
	case resp.StatusCode == http.StatusTooManyRequests:
		// 429：provider 瞬时错误 → 换下一模型（不重试；尊重 Retry-After 不 sleep 破预算）
		return "", fmt.Errorf("vision rate limited (retry_after=%s)", resp.Header.Get("Retry-After"))
	case resp.StatusCode == http.StatusUnavailableForLegalReasons:
		return "", fmt.Errorf("%w: HTTP 451", ErrBlocked)
	default:
		// 其他 5xx：截断仅内部记录，绝不进占位文本
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("vision provider error: HTTP %d", resp.StatusCode)
	}
}

// contentString OpenAI content 字段取文本（string 或 [{type:text,...}] 数组）
func contentString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if m["type"] == "text" {
					if s, ok := m["text"].(string); ok {
						sb.WriteString(s)
					}
				}
			}
		}
		return sb.String()
	}
	return ""
}

// DescribeResult 单图识图结果（结构化，审核 P0-2 §3 / P2-6：保留请求级熔断
// 标记与真实 HTTP/fallback 计数，不提前压成字符串枚举）
type DescribeResult struct {
	Desc   string // 纯描述（成功）
	Enum   string // 失败枚举（空=成功）
	Model  string // 使用模型
	Abort  bool   // 请求级熔断：401/403（鉴权/配置错误）→ 整次 Enhance 停止后续 sidecall
	HTTPCalls int // 实际 HTTP 请求次数（含 transport retry 与 fallback）
	Fallbacks int // 真实模型切换次数（换到下一个模型算一次）
}

// DescribeOne 单图旁路识图（调用方已在 decode gate 内完成压缩、call gate 内
// 调用本函数——v0.2.2 闸门拆分）。fallback 链最多 MaxFallbackModels 个模型，
// 总预算继承 ctx deadline（v0.2.2：请求级全局 deadline，非每图独立）。
// Abort=true 表示鉴权/配置错误——调用方必须停止本请求其余 sidecall（P0-2 §3）。
func (c *VisionClient) DescribeOne(ctx context.Context, instruction string, data []byte, mediaType string, cfg Config) DescribeResult {
	models := make([]string, 0, len(cfg.Models))
	for _, item := range cfg.Models {
		if item = strings.TrimSpace(item); item != "" {
			models = append(models, item)
		}
		if len(models) >= MaxFallbackModels {
			break // v0.2.2：fallback 模型硬限制
		}
	}
	if len(models) == 0 {
		return DescribeResult{Enum: EnumServiceUnavailable}
	}
	// 总预算：优先 ctx deadline（请求级全局），无 deadline 时退回 cfg.TimeoutSec
	var totalBudget time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		totalBudget = time.Until(deadline)
	} else {
		totalBudget = time.Duration(cfg.TimeoutSec) * time.Second
	}
	deadline := time.Now().Add(totalBudget)
	var result DescribeResult
	for _, model := range models {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return DescribeResult{Enum: EnumTimeout, HTTPCalls: result.HTTPCalls, Fallbacks: result.Fallbacks}
		}
		text, calls, err := c.Call(ctx, model, instruction, data, mediaType,
			cfg.BaseURL, cfg.APIKey, cfg.SidecallToken, remaining, DefaultMaxTokens)
		result.HTTPCalls += calls
		if err == nil {
			result.Desc, result.Model = text, model
			return result
		}
		switch {
		case errors.Is(err, ErrBlocked):
			return DescribeResult{Enum: EnumBlocked, Model: model, HTTPCalls: result.HTTPCalls, Fallbacks: result.Fallbacks}
		case errors.Is(err, ErrAuth):
			// 401/403：配置/鉴权错误 → 请求级熔断（Abort），不 retry 不 fallback
			return DescribeResult{Enum: EnumServiceUnavailable, Model: model, Abort: true,
				HTTPCalls: result.HTTPCalls, Fallbacks: result.Fallbacks}
		case errors.Is(err, ErrSizeLimit):
			return DescribeResult{Enum: EnumSizeLimit, Model: model, HTTPCalls: result.HTTPCalls, Fallbacks: result.Fallbacks}
		case errors.Is(err, ErrUnsupported):
			return DescribeResult{Enum: EnumUnsupportedFormat, Model: model, HTTPCalls: result.HTTPCalls, Fallbacks: result.Fallbacks}
		default:
			result.Fallbacks++ // provider 瞬时（429/5xx/空响应）或传输重试后仍失败 → 换下一模型
			continue
		}
	}
	return DescribeResult{Enum: EnumServiceUnavailable, HTTPCalls: result.HTTPCalls, Fallbacks: result.Fallbacks}
}
