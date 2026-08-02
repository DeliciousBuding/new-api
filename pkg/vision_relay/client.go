package vision_relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 旁路错误哨兵（错误矩阵，门槛七）
var (
	ErrBlocked = errors.New("vision provider content blocked")   // 451
	ErrAuth    = errors.New("vision provider auth/config error") // 401/403 → 终止整条 fallback
)

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

// Call 单次旁路调用（一个模型一次尝试；传输错误预算内重试 1 次）。
// 错误矩阵（门槛七）：
//
//	400 → ErrUnsupported（停止该图，不遍历其他模型）
//	401/403 → ErrAuth（终止整条 fallback，防 key 错误打三模型）
//	413 → ErrSizeLimit（不 fallback）
//	429 → 瞬时错误（fallback 切下一模型；尊重 Retry-After 但不 sleep 突破总预算）
//	451 → ErrBlocked（当前图停止）
//	2xx 但 choices/content 为空 → 瞬时错误（允许换下一模型）
//	其他 5xx/非 JSON 错误体 → 瞬时错误（截断仅内部记录，绝不进占位文本）
func (c *VisionClient) Call(ctx context.Context, model, instruction string, data []byte, mediaType, baseURL, apiKey string, timeout time.Duration, maxTokens int) (string, error) {
	maxTokensUint := uint(maxTokens)
	payload := map[string]any{
		"model":     model,
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
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	var text string
	for attempt := 0; attempt <= 1; attempt++ { // 传输错误重试 ≤1（预算内）
		text, err = c.doRequest(ctx, client, model, baseURL, apiKey, body, timeout)
		if err == nil || !isTransientError(err) {
			break
		}
	}
	return text, err
}

func (c *VisionClient) doRequest(ctx context.Context, client *http.Client, model, baseURL, apiKey string, body []byte, timeout time.Duration) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// 递归保护（审核 §8.2）：旁路请求带标记，service 层检测到即跳过
	req.Header.Set("X-NewAPI-Vision-Relay", "1")
	resp, err := client.Do(req)
	if err != nil {
		return "", err // 传输错误（超时/连接失败）
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		if readErr != nil {
			return "", readErr
		}
		var result struct {
			Choices []struct {
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
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

// isTransientError 瞬时错误（可重试/换模型）；鉴权/格式/尺寸/审核错误除外
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBlocked) || errors.Is(err, ErrAuth) ||
		errors.Is(err, ErrSizeLimit) || errors.Is(err, ErrUnsupported) {
		return false
	}
	return true
}

// DescribeOne 单图识图（调用方已在解码/调用并发闸内；压缩也在闸内执行）。
// 返回 (纯描述, 枚举, 使用模型)。枚举非空 = 失败（对应占位）。
func (c *VisionClient) DescribeOne(ctx context.Context, instruction string, img *PatchedImage, cfg Config) (string, string, string) {
	data, mediaType, err := CompressForVision(img.Data, img.Patch.Source.MediaType)
	if err != nil {
		return "", enumFromErr(err), ""
	}
	models := make([]string, 0, len(cfg.Models))
	for _, item := range cfg.Models {
		if item = strings.TrimSpace(item); item != "" {
			models = append(models, item)
		}
	}
	if len(models) == 0 {
		return "", EnumServiceUnavailable, ""
	}
	totalBudget := time.Duration(cfg.TimeoutSec) * time.Second
	deadline := time.Now().Add(totalBudget)
	for _, model := range models {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", EnumTimeout, ""
		}
		text, err := c.Call(ctx, model, instruction, data, mediaType,
			cfg.BaseURL, cfg.APIKey, remaining, DefaultMaxTokens)
		if err == nil {
			return text, "", model
		}
		switch {
		case errors.Is(err, ErrBlocked):
			return "", EnumBlocked, model
		case errors.Is(err, ErrAuth):
			// 401/403：配置错误，终止整条 fallback
			return "", EnumServiceUnavailable, model
		case errors.Is(err, ErrSizeLimit):
			return "", EnumSizeLimit, model
		case errors.Is(err, ErrUnsupported):
			return "", EnumUnsupportedFormat, model
		default:
			continue // 瞬时（超时/5xx/429/空响应）→ 换下一模型
		}
	}
	return "", EnumServiceUnavailable, ""
}
