package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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
type visionRelayFetcher struct{}

func (visionRelayFetcher) Fetch(ctx context.Context, url string, maxBytes int64) ([]byte, string, error) {
	client := GetSSRFProtectedHTTPClient()
	if client == nil {
		client = http.DefaultClient // 防御：测试环境/未初始化时兜底
	}
	return vision_relay.LimitedFetch(ctx, client, url, maxBytes)
}

// PrepareVisionRelayRequest 预扣费成功后、retry 循环前调用（controller 唯一钩子）。
//
// 职责（v0.2.1 service 边界）：
//  1. 检查递归头
//  2. 获取配置 snapshot（不可变）
//  3. 检查 enabled + target model（OriginModelName，映射前）
//  4. 从 BodyStorage 读取原始 body
//  5. 调 pkg/vision_relay.Engine.Enhance
//  6. 验证增强 body 能反序列化为对应 DTO
//  7. 创建新 BodyStorage
//  8. 最后一次性原子提交：relayInfo.Request / Gin BodyStorage / Request.Body / ContentLength
//  9. 关闭旧 BodyStorage（防 fd/临时文件泄漏）
//  10. 记录 Stats
//
// 返回 *types.NewAPIError：nil = 继续原链路；非 nil = 增强基础设施错误（5xx，
// 绝不 fail-open 发原图）。图片级错误全部内部占位。
func PrepareVisionRelayRequest(c *gin.Context, relayInfo *relaycommon.RelayInfo) *types.NewAPIError {
	// 1. 递归保护：旁路请求直接跳过
	if c.GetHeader(relayRequestHeader) == "1" {
		return nil
	}
	// 2. 配置快照（请求全程使用不可变副本）
	cfg := model_setting.GetVisionRelaySnapshot()
	// 配置非法 → 策略不生效，原请求透传（fail-safe，不阻塞主链路）
	if err := cfg.Validate(); err != nil {
		logger.LogWarn(c, fmt.Sprintf("vision relay config invalid, relay disabled: %v", err))
		return nil
	}
	// 3. 策略：allowlist 命中才处理
	if !visionRelayTargetMatched(relayInfo.OriginModelName, cfg) {
		return nil
	}
	// 格式判定（Claude/OpenAI；未知格式不处理）
	format, ok := visionRelayFormat(relayInfo.RelayFormat)
	if !ok {
		return nil
	}

	// 4. 只读原始 body
	originalStorage, err := common.GetBodyStorage(c)
	if err != nil {
		return visionRelayInfraError(fmt.Errorf("get body storage: %w", err))
	}
	if _, err := originalStorage.Seek(0, io.SeekStart); err != nil {
		return visionRelayInfraError(fmt.Errorf("seek body storage: %w", err))
	}
	rawBody, err := io.ReadAll(originalStorage)
	if err != nil {
		return visionRelayInfraError(fmt.Errorf("read body: %w", err))
	}

	// 5. 核心引擎增强（图片级错误内部占位；返回 error = 基础设施错误）
	engine := &vision_relay.Engine{
		Client:  &vision_relay.VisionClient{},
		Fetcher: visionRelayFetcher{},
	}
	coreCfg := vision_relay.Config{
		Enabled:      cfg.Enabled,
		TargetModels: cfg.TargetModels,
		Models:       cfg.Models,
		BaseURL:      cfg.BaseURL,
		APIKey:       cfg.APIKey,
		Prompt:       cfg.Prompt,
		TimeoutSec:   cfg.TimeoutSec,
	}
	var stats vision_relay.Stats
	enhanced, err := engine.Enhance(c.Request.Context(), rawBody, format, coreCfg, &stats)
	if err != nil {
		return visionRelayInfraError(err)
	}
	if enhanced == nil {
		return nil // 真 no-op：无图，原始状态完全不动
	}

	// 6. 先验证增强 JSON 能反序列化为正确请求 DTO（提交前验证，防半提交）
	enhancedRequest, err := visionRelayDecodeRequest(enhanced, relayInfo.Request)
	if err != nil {
		return visionRelayInfraError(fmt.Errorf("decode enhanced request: %w", err))
	}

	// 7. 创建新存储
	newStorage, err := common.CreateBodyStorage(enhanced)
	if err != nil {
		return visionRelayInfraError(fmt.Errorf("create enhanced body storage: %w", err))
	}

	// 8. 原子提交（最后一步才动共享状态）
	relayInfo.Request = enhancedRequest
	c.Set(common.KeyBodyStorage, newStorage)
	c.Set(common.KeyRequestBody, nil)
	c.Request.Body = io.NopCloser(newStorage)
	c.Request.ContentLength = int64(len(enhanced))

	// 9. 提交成功后关闭旧存储
	if originalStorage != nil && originalStorage != newStorage {
		_ = originalStorage.Close()
	}

	// 10. 结构化统计日志（A12）
	logger.LogInfo(c, fmt.Sprintf(
		"vision: target_model=%s images_total=%d images_success=%d images_failed=%d images_omitted=%d elapsed_ms=%d models_used=%s fallback_count=%d description_bytes=%d",
		relayInfo.OriginModelName, stats.Total, stats.Success, stats.Failed, stats.Omitted,
		stats.ElapsedMs, stats.ModelsUsed, stats.FallbackCount, stats.DescriptionBytes))
	return nil
}

// visionRelayTargetMatched allowlist 匹配（glob，OriginModelName 映射前）
func visionRelayTargetMatched(model string, cfg model_setting.VisionRelaySnapshot) bool {
	if !cfg.Enabled || model == "" {
		return false
	}
	patterns, err := cfg.TargetModelPatterns()
	if err != nil {
		return false // 配置非法 → 不命中（策略不生效，原请求透传）
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
	}
	return 0, false
}

// visionRelayDecodeRequest 从增强 JSON 反序列化为正确请求 DTO（类型与原始一致）
func visionRelayDecodeRequest(enhanced []byte, original dto.Request) (dto.Request, error) {
	switch original.(type) {
	case *dto.ClaudeRequest:
		var req dto.ClaudeRequest
		if err := json.Unmarshal(enhanced, &req); err != nil {
			return nil, err
		}
		return &req, nil
	case *dto.GeneralOpenAIRequest:
		var req dto.GeneralOpenAIRequest
		if err := json.Unmarshal(enhanced, &req); err != nil {
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
