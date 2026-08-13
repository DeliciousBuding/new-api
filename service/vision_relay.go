package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/pkg/vision_relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/gin-gonic/gin"
)

// 递归保护头：旁路请求带该头，本层检测到直接跳过（审核 §8.2，
// 防 VisionBaseURL 误配本实例导致无限递归）
const relayRequestHeader = "X-NewAPI-Vision-Relay"

// visionRelayFetcher ImageFetcher 的 NewAPI 适配：SSRF 保护客户端 + 有限流下载。
// 纯核心包不感知 SSRF 客户端/下载策略（v0.2.1 边界）。
// SSRF 保护客户端不可用时**返回错误**（该图 service_unavailable 占位），
// 绝不 fallback 到无保护的 http.DefaultClient（v0.2.2 安全修复）。
type visionRelayFetcher struct{}

func (visionRelayFetcher) Fetch(ctx context.Context, url string, maxBytes int64) ([]byte, string, error) {
	// 用户提供的图片 URL 永远走受保护客户端，不随全局 EnableSSRFProtection
	// 开关降级为 general client（general client 无拨号校验）。全局开关关闭时
	// GetSSRFProtectedHTTPClientForUserInput 返回 nil → 拒绝抓取，fail closed。
	client := GetSSRFProtectedHTTPClientForUserInput()
	if client == nil {
		return nil, "", fmt.Errorf("%w: protected HTTP client unavailable", vision_relay.ErrDownload)
	}
	return vision_relay.LimitedFetch(ctx, client, url, maxBytes)
}

// PrepareVisionRelayRequest 预扣费成功后、retry 循环前调用（controller 唯一钩子）。
//
// 职责（v0.2.1 service 边界 + v0.2.2 修复）：
//  1. 检查递归头
//  2. 获取配置快照（OptionMap 同步读取，不可变）
//  3. 检查 enabled + target model（OriginModelName，映射前）——未命中 → no-op
//  4. **命中后**校验端点配置（ValidateEndpoint）——失败 = 5xx，绝不 fail-open
//  5. 从 BodyStorage 读取原始 body
//  6. 创建请求级总 deadline（TimeoutSec 全局预算，核心全部继承）
//  7. 调 pkg/vision_relay.Engine.Enhance
//  8. 验证增强 body 能反序列化为对应 DTO
//  9. 创建新 BodyStorage
//  10. 最后一次性原子提交：relayInfo.Request / Gin BodyStorage / Request.Body / ContentLength
//  11. 关闭旧 BodyStorage（防 fd/临时文件泄漏）
//  12. 记录 Stats
//
// 允许 no-op：enabled=false / target_models 空 / 模型未命中 / 协议不支持 /
// 请求无图片 / 递归保护头。以下必须 5xx：命中后端点配置非法、内部 JSON 变换失败、
// 新 BodyStorage 创建失败、增强 DTO 验证失败。
func PrepareVisionRelayRequest(c *gin.Context, relayInfo *relaycommon.RelayInfo) *types.NewAPIError {
	// 1. 配置快照（OptionMap 同步读取）——递归 marker 校验需要共享 secret
	cfg, err := model_setting.GetVisionRelaySnapshot()
	if err != nil {
		return visionRelayFail(relayInfo, "snapshot", &cfg, nil, fmt.Errorf("vision relay snapshot: %w", err))
	}
	// 2. 递归保护（审核 P0-2）：仅认证 marker（HMAC 校验通过）才允许 bypass；
	//    外部伪造的任意值（含旧字面 "1"）被忽略，继续正常执行 Vision Relay
	if h := c.GetHeader(relayRequestHeader); h != "" &&
		vision_relay.ValidateMarker(cfg.SidecallSecret, h, time.Now()) {
		return nil
	}
	// 3. 策略：allowlist 命中才处理（未启用/未命中 → no-op）
	if !cfg.Enabled {
		return nil
	}
	patterns, err := cfg.TargetModelPatterns()
	if err != nil {
		return visionRelayFail(relayInfo, "target_models", &cfg, nil, fmt.Errorf("vision relay target models: %w", err))
	}
	if !visionRelayMatchPatterns(patterns, relayInfo.OriginModelName) {
		return nil
	}
	// 4. 格式判定（Claude/OpenAI/Responses；未知格式不处理）——先于端点必填校验：
	//    本来就不处理的协议不得因缺 key 被误报 5xx（审核 P0-2 §4）
	format, ok := visionRelayFormat(relayInfo.RelayFormat)
	if !ok {
		return nil
	}
	// 5. 命中模型后校验端点配置（v0.2.2：配置损坏不再 fail-open 发原图）
	if err := cfg.ValidateEndpoint(); err != nil {
		return visionRelayFail(relayInfo, "endpoint", &cfg, nil, fmt.Errorf("vision relay endpoint config: %w", err))
	}

	// 5. 只读原始 body
	originalStorage, err := common.GetBodyStorage(c)
	if err != nil {
		return visionRelayFail(relayInfo, "body_storage", &cfg, nil, fmt.Errorf("get body storage: %w", err))
	}
	if _, err := originalStorage.Seek(0, io.SeekStart); err != nil {
		return visionRelayFail(relayInfo, "seek_body", &cfg, nil, fmt.Errorf("seek body storage: %w", err))
	}
	rawBody, err := io.ReadAll(originalStorage)
	if err != nil {
		return visionRelayFail(relayInfo, "read_body", &cfg, nil, fmt.Errorf("read body: %w", err))
	}

	// 6. 请求级总 deadline（v0.2.2：单请求全局预算，非每图各自 15s）
	enhanceCtx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()

	// 7. 核心引擎增强（图片级错误内部占位；返回 error = 基础设施错误）
	engine := &vision_relay.Engine{
		Client: &vision_relay.VisionClient{
			HTTPClient: GetHttpClient(), // 显式注入（继承 NewAPI 连接池/TLS 配置）
		},
		Fetcher: visionRelayFetcher{},
		// 审核 P1-3（A6）：识图描述注入前过原生敏感词检查（命中 → blocked 占位）
		SensitiveCheck: func(desc string) bool {
			ok, _ := CheckSensitiveText(desc)
			return ok
		},
	}
	coreCfg := vision_relay.Config{
		Enabled:        cfg.Enabled,
		TargetModels:   cfg.TargetModels,
		Models:         cfg.Models,
		BaseURL:        cfg.BaseURL,
		APIKey:         cfg.APIKey,
		Prompt:         cfg.Prompt,
		TimeoutSec:     cfg.TimeoutSec,
		SidecallSecret: cfg.SidecallSecret, // 审查 P1-2：出站旁路请求必须携带认证 marker（自回环递归防护）
	}
	var stats vision_relay.Stats
	enhanced, err := engine.Enhance(enhanceCtx, rawBody, format, coreCfg, &stats)
	if err != nil {
		// 上游识别链/内部变换失败（基础设施错误）→ 5xx + 结构化日志（audit-2026-08 #47）
		return visionRelayFail(relayInfo, "enhance", &cfg, &stats, err)
	}
	if enhanced == nil {
		// 真 no-op：无图。上面的 io.ReadAll 已把共享 BodyStorage 的偏移
		// 推到 EOF；回绕到 0 才能兑现"原始状态完全不动"的契约，否则后续
		// 读取 c.Request.Body 的消费者会拿到空 body。
		if _, err := originalStorage.Seek(0, io.SeekStart); err != nil {
			return visionRelayFail(relayInfo, "seek_body", &cfg, nil, fmt.Errorf("rewind body storage: %w", err))
		}
		return nil
	}

	// 8. 先验证增强 JSON 能反序列化为正确请求 DTO（提交前验证，防半提交）
	enhancedRequest, err := visionRelayDecodeRequest(enhanced, relayInfo.Request)
	if err != nil {
		return visionRelayFail(relayInfo, "decode", &cfg, &stats, fmt.Errorf("decode enhanced request: %w", err))
	}

	// 9. 创建新存储
	newStorage, err := common.CreateBodyStorage(enhanced)
	if err != nil {
		return visionRelayFail(relayInfo, "create_storage", &cfg, &stats, fmt.Errorf("create enhanced body storage: %w", err))
	}

	// 10. 原子提交（最后一步才动共享状态）
	relayInfo.Request = enhancedRequest
	c.Set(common.KeyBodyStorage, newStorage)
	c.Set(common.KeyRequestBody, nil)
	c.Request.Body = io.NopCloser(newStorage)
	c.Request.ContentLength = int64(len(enhanced))

	// 11. 提交成功后关闭旧存储
	if originalStorage != nil && originalStorage != newStorage {
		_ = originalStorage.Close()
	}

	// 12. 结构化统计日志（A12）
	logger.LogInfo(c, fmt.Sprintf(
		"vision: target_model=%s images_total=%d images_success=%d images_failed=%d unique=%d cache_hits=%d vision_calls=%d fallback_count=%d elapsed_ms=%d models_used=%s description_bytes=%d",
		relayInfo.OriginModelName, stats.Total, stats.Success, stats.Failed, stats.UniqueImages,
		stats.CacheHits, stats.VisionCalls, stats.FallbackCount, stats.ElapsedMs,
		stats.ModelsUsed, stats.DescriptionBytes))
	// 12b. 上游识别链劣化（5xx/超时/解析失败 → 图片级占位，请求本身未 5xx）——
	//      系统级 SysLog，运营无需翻请求日志即可感知（audit-2026-08 #47）
	if stats.Failed > 0 {
		visionRelayLogChainFailure(relayInfo, &cfg, &stats)
	}
	return nil
}

// visionRelayMatchPatterns 编译好的 allowlist 匹配
func visionRelayMatchPatterns(patterns []*regexp.Regexp, model string) bool {
	if model == "" {
		return false
	}
	for _, re := range patterns {
		if re.MatchString(model) {
			return true
		}
	}
	return false
}

// visionRelayFormat 协议格式映射（未知格式返回 ok=false）
func visionRelayFormat(format types.RelayFormat) (vision_relay.Format, bool) {
	switch format {
	case types.RelayFormatClaude:
		return vision_relay.FormatClaude, true
	case types.RelayFormatOpenAI:
		return vision_relay.FormatOpenAI, true
	case types.RelayFormatOpenAIResponses:
		return vision_relay.FormatResponses, true
	}
	return 0, false
}

// visionRelayDecodeRequest 从增强 JSON 反序列化为正确请求 DTO（类型与原始一致）
func visionRelayDecodeRequest(enhanced []byte, original dto.Request) (dto.Request, error) {
	switch original.(type) {
	case *dto.ClaudeRequest:
		var req dto.ClaudeRequest
		if err := common.Unmarshal(enhanced, &req); err != nil {
			return nil, err
		}
		return &req, nil
	case *dto.GeneralOpenAIRequest:
		var req dto.GeneralOpenAIRequest
		if err := common.Unmarshal(enhanced, &req); err != nil {
			return nil, err
		}
		return &req, nil
	case *dto.OpenAIResponsesRequest:
		var req dto.OpenAIResponsesRequest
		if err := common.Unmarshal(enhanced, &req); err != nil {
			return nil, err
		}
		return &req, nil
	default:
		return nil, fmt.Errorf("unsupported request type %T", original)
	}
}

// visionRelayInfraError 增强基础设施错误 → 5xx（绝不 fail-open）
func visionRelayInfraError(err error) *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		fmt.Errorf("vision relay: %w", err),
		types.ErrorCodeBadRequestBody,
		http.StatusInternalServerError,
		types.ErrOptionWithSkipRetry(),
	)
}

// visionRelayFail 记录 5xx 失败路径的结构化日志（audit-2026-08 #47）并返回 5xx。
// 字段：阶段（stage）、目标模型、识别链模型、状态码、耗时、images 数、错误摘要。
// 绝不打印 api_key/sidecall_secret 明文；请求级统计在增强后才有意义（stats 为 nil 时记 0）。
func visionRelayFail(relayInfo *relaycommon.RelayInfo, stage string, cfg *model_setting.VisionRelaySnapshot, stats *vision_relay.Stats, err error) *types.NewAPIError {
	imagesTotal, imagesFailed := 0, 0
	if stats != nil {
		imagesTotal = stats.Total
		imagesFailed = stats.Failed
	}
	elapsedMs := int64(0)
	if !relayInfo.StartTime.IsZero() {
		elapsedMs = time.Since(relayInfo.StartTime).Milliseconds()
	}
	common.SysError(fmt.Sprintf(
		"vision relay 5xx: stage=%s request_id=%s target_model=%s chain_models=%s status=%d elapsed_ms=%d images_total=%d images_failed=%d error=%s",
		stage, relayInfo.RequestId, relayInfo.OriginModelName, strings.Join(cfg.Models, ","),
		http.StatusInternalServerError, elapsedMs, imagesTotal, imagesFailed, err.Error()))
	return visionRelayInfraError(err)
}

// visionRelayLogChainFailure 上游识别链失败（5xx/超时/解析失败 → 图片级占位）的
// 结构化日志（audit-2026-08 #47）：请求未 5xx（占位继续）但运营需感知链路劣化。
// 只记模型名/计数/耗时，绝不打印 api_key/sidecall_secret/请求体明文。
func visionRelayLogChainFailure(relayInfo *relaycommon.RelayInfo, cfg *model_setting.VisionRelaySnapshot, stats *vision_relay.Stats) {
	common.SysLog(fmt.Sprintf(
		"vision relay chain degraded: request_id=%s target_model=%s chain_models=%s status=%d images_total=%d images_success=%d images_failed=%d unique=%d cache_hits=%d vision_calls=%d fallback_count=%d elapsed_ms=%d models_used=%s",
		relayInfo.RequestId, relayInfo.OriginModelName, strings.Join(cfg.Models, ","),
		http.StatusOK, stats.Total, stats.Success, stats.Failed, stats.UniqueImages,
		stats.CacheHits, stats.VisionCalls, stats.FallbackCount, stats.ElapsedMs, stats.ModelsUsed))
}
