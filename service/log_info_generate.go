package service

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// attachQuotaSaturationToOther nests a quota saturation marker under
// other.admin_info.quota_saturation. Nesting under admin_info makes it
// admin-only for free, since model.formatUserLogs strips the whole admin_info
// object for non-admin viewers. Creates admin_info if absent. No-op when the
// clamp is nil (the common case: no saturation happened).
func attachQuotaSaturationToOther(other map[string]interface{}, clamp *common.QuotaClamp) {
	if clamp == nil || other == nil {
		return
	}
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	if !ok || adminInfo == nil {
		adminInfo = map[string]interface{}{}
		other["admin_info"] = adminInfo
	}
	adminInfo["quota_saturation"] = clamp.AuditMap()
}

// attachQuotaSaturation records the request's quota clamp (if any) onto the
// consume log's other.admin_info and emits a request-correlated backend audit
// line. Called right before RecordConsumeLog on the text/audio/wss paths.
func attachQuotaSaturation(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if relayInfo == nil {
		return
	}
	clamp := relayInfo.QuotaClamp
	if clamp == nil {
		return
	}
	attachQuotaSaturationToOther(other, clamp)
	logger.LogWarn(ctx, fmt.Sprintf("quota saturation on consume log: op=%s kind=%s original=%g clamped=%d user=%d model=%s",
		clamp.Op, clamp.Kind, clamp.Original, clamp.Clamped, relayInfo.UserId, relayInfo.OriginModelName))
}

func appendRequestPath(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if other == nil {
		return
	}
	if ctx != nil && ctx.Request != nil && ctx.Request.URL != nil {
		if path := ctx.Request.URL.Path; path != "" {
			other["request_path"] = path
			return
		}
	}
	if relayInfo != nil && relayInfo.RequestURLPath != "" {
		path := relayInfo.RequestURLPath
		if idx := strings.Index(path, "?"); idx != -1 {
			path = path[:idx]
		}
		other["request_path"] = path
	}
}

// DetectClientProfile 按官方渠道亲和性同源的头清单识别客户端，返回细粒度档位：
//   - codex_cli / codex_desktop / codex_app / codex_vscode / codex_browser（Originator 前缀 + X-Codex-* 头 + UA 兜底）
//   - claude_cli / claude_desktop / claude_plugin / claude_app（X-App 值 + claude-cli UA 细分）
//   - claude_sdk / openai_sdk（Anthropic-Version / SDK UA / X-Stainless-* 头）
//   - gemini_cli / gemini_sdk（Gemini-CLI UA / google-genai-sdk、genai-py UA）
//   - 品牌客户端（IDE/chat/agent/平台）：cherry_studio、trae、qoder、cursor、windsurf、
//     cline、roo_code、continue、zed、copilot、gemini_cli、perplexity、poe、openrouter、
//     groq、grok、ollama、kimi、qwen、doubao、zhipu、deepseek、chatgpt、minis、opencode、
//     hermes_agent、workbuddy、openclaw、rikkahub、sub2api（UA 特异性词，见函数内矩阵）
//   - LLM 框架与自动化：langchain、llama_index、mcp_sdk、automation（n8n/zapier/make.com）
//   - 通用工具：gohttp、cliproxyapi、http_client（curl/requests/httpx/urllib/okhttp/axios 等）
//   - chat（兜底）
//
// 识别依据优先序：特征头（Originator / X-App / Anthropic-Version）> 特异性 UA 词 >
// 通用框架头（X-Stainless-*，可被客户端覆盖，仅作 openai_sdk 兜底）。
// 每个分支的 UA 字符串均有实证来源（官方源码 / 逆向文档 / nginx 流量），见分支内注释。
//
// 结果仅作审计展示 hint，不参与鉴权/计费/路由；调用方可伪造。
func DetectClientProfile(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	h := c.Request.Header
	if v := h.Get("Originator"); v != "" {
		lv := strings.ToLower(v)
		switch {
		case strings.HasPrefix(lv, "codex_cli"):
			return "codex_cli"
		case strings.HasPrefix(lv, "codex-tui"):
			// CLIProxyAPI 的 Codex 上游路径（codex-tui/… 伪装 UA + Originator: codex-tui）
			return "codex_cli"
		case strings.HasPrefix(lv, "codex_desktop"):
			return "codex_desktop"
		case strings.HasPrefix(lv, "codex desktop"):
			// OpenAI 官方 desktop app 发送的 Originator 为 "Codex Desktop"（带空格，
			// openai/codex #31481 实证），与 codex_desktop 下划线变体等价。
			return "codex_desktop"
		case strings.HasPrefix(lv, "codex"):
			return "codex_app"
		}
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Codex-Window-Id", "X-OpenAI-Subagent"} {
		if h.Get(name) != "" {
			ua := strings.ToLower(c.Request.UserAgent())
			if strings.Contains(ua, "desktop") {
				return "codex_desktop"
			}
			return "codex_cli"
		}
	}
	if v := h.Get("X-App"); v != "" {
		lv := strings.ToLower(v)
		switch {
		case strings.Contains(lv, "cli"):
			return "claude_cli"
		case strings.Contains(lv, "desktop"):
			return "claude_desktop"
		case strings.Contains(lv, "vscode") || strings.Contains(lv, "jetbrains") || strings.Contains(lv, "intellij") || strings.Contains(lv, "cursor"):
			return "claude_plugin"
		default:
			return "claude_app"
		}
	}
	if h.Get("Anthropic-Version") != "" {
		// claude-cli 等客户端同样携带 Anthropic-Version——先排除已可识别
		// 的 Claude 客户端 UA，纯 SDK 调用才归 claude_sdk。
		ua := strings.ToLower(c.Request.UserAgent())
		if !strings.Contains(ua, "claude-cli/") && !strings.Contains(ua, "claude-desktop-3p") {
			return "claude_sdk"
		}
		// 落入下方 Claude 族 UA 细分
	}
	// Anthropic SDK UA 优先于通用 X-Stainless 头：anthropic 官方 SDK
	// （TS/Python/Go，均为 stainless 生成）同时携带 X-Stainless-* 头，若先判
	// X-Stainless 会被误归 openai_sdk。UA 特异性词比通用框架头更可信——
	// X-Stainless-* 可被客户端 default_headers 覆盖/删除（UA 才是稳定信号）。
	// 已实证格式：Anthropic/JS <ver>（TS）、Anthropic/Python <ver>、
	// Anthropic/Go <ver>；anthropic-sdk- 前缀兜底其余语言变体。
	ua := strings.ToLower(c.Request.UserAgent())
	if strings.Contains(ua, "anthropic/js") || strings.Contains(ua, "anthropic/python") ||
		strings.Contains(ua, "anthropic/go") || strings.Contains(ua, "anthropic-sdk-") {
		return "claude_sdk"
	}
	if h.Get("X-Stainless-Lang") != "" || h.Get("X-Stainless-Runtime") != "" {
		return "openai_sdk"
	}
	// UA 兜底：通用工具/中转代理/品牌客户端（在特征头之后、chat 之前）。
	// 匹配顺序 = 特异性降序：子串越泛化越靠后，避免互相覆盖。
	// 品牌词都足够特异（正常请求 UA 不会含这些子串），Contains 误伤可控；
	// 泛化形态（裸浏览器等）不做识别，落 chat 兜底。
	ua = strings.ToLower(c.Request.UserAgent())
	switch {
	// 通用 HTTP 客户端（Go 默认 UA / 中转代理）
	case strings.Contains(ua, "go-http-client"):
		return "gohttp"
	case strings.Contains(ua, "cli-proxy-openai-compat"), strings.Contains(ua, "cliproxyapi"):
		// CLIProxyAPI（router-for-me/CLIProxyAPI）openai-compat 路径硬编码
		// User-Agent: cli-proxy-openai-compat（不透传下游 UA）；Kimi 路径为
		// CLIProxyAPI/<version>。Go 默认 UA 为 Go-http-client/1.1 或 /2.0
		// （由连接协议版本决定）。
		return "cliproxyapi"
	case strings.Contains(ua, "hermesagent"):
		// HermesAgent/<version>（自有运维 agent 源码实证）
		return "hermes_agent"
	case strings.Contains(ua, "workbuddy"):
		return "workbuddy"
	case strings.Contains(ua, "openclaw"):
		return "openclaw"
	case strings.Contains(ua, "cherry studio"), strings.Contains(ua, "cherry-studio"), strings.Contains(ua, "cherrystudio"):
		// Cherry Studio 官方 UA 为 "Cherry Studio"（无版本号）
		return "cherry_studio"
	case strings.Contains(ua, "rikkahub"):
		return "rikkahub"
	case strings.Contains(ua, "sub2api"):
		// Sub2API-Discovery/1.0 探活（nginx 流量实证）
		return "sub2api"
	// IDE / 编码 agent
	case strings.Contains(ua, "windsurf"):
		return "windsurf"
	case strings.Contains(ua, "cline"):
		return "cline"
	case strings.Contains(ua, "roo code"), strings.Contains(ua, "roo-code"), strings.Contains(ua, "roocode"):
		return "roo_code"
	case strings.Contains(ua, "trae"):
		return "trae"
	case strings.Contains(ua, "cursor"):
		return "cursor"
	case strings.Contains(ua, "opencode"):
		// opencode/<version>（nginx 流量实证）。注意：opencode 含子串 "codex"，
		// 必须保持在本分支先于下方 codex 族分支，否则会被 codex_cli 吞掉。
		return "opencode"
	case strings.Contains(ua, "continue/"):
		return "continue"
	case strings.Contains(ua, "zed/"):
		return "zed"
	case strings.Contains(ua, "copilot"):
		return "copilot"
	// Codex 族（UA 层兜底；Originator 头已在上层细分）
	case strings.Contains(ua, "codex desktop"):
		return "codex_desktop"
	case strings.Contains(ua, "codex_vscode"):
		return "codex_vscode"
	case strings.Contains(ua, "codex-browser-use"):
		return "codex_browser"
	case strings.Contains(ua, "codex"):
		return "codex_cli"
	// Claude 族（UA 层兜底；X-App 头已在上层细分）
	case strings.Contains(ua, "claude-cli/"):
		switch {
		case strings.Contains(ua, "desktop"):
			return "claude_desktop"
		case strings.Contains(ua, "vscode"):
			return "claude_plugin"
		default:
			return "claude_cli"
		}
	case strings.Contains(ua, "claude-desktop-3p"):
		return "claude_desktop"
	case strings.Contains(ua, "claude/") && (strings.Contains(ua, "electron") || strings.Contains(ua, "msix")):
		return "claude_desktop"
	// 官方 SDK / 平台客户端
	case strings.Contains(ua, "openai/python"), strings.Contains(ua, "openai-python"), strings.Contains(ua, "openai/js"):
		return "openai_sdk"
	case strings.Contains(ua, "gemini-cli"), strings.Contains(ua, "gemini cli"):
		return "gemini_cli"
	case strings.Contains(ua, "google-genai-sdk"), strings.Contains(ua, "genai-py"):
		// google-genai（新版）与 google-generativeai（旧版 genai-py）SDK UA 实证
		return "gemini_sdk"
	case strings.Contains(ua, "qoder"):
		// Qoder / Qoder Work（阿里，千问办公前台）。Qoder-Cli UA 为 GitHub issue 实证
		return "qoder"
	case strings.Contains(ua, "perplexity"):
		return "perplexity"
	case strings.Contains(ua, "poe/"):
		return "poe"
	case strings.Contains(ua, "openrouter"):
		return "openrouter"
	case strings.Contains(ua, "groq"):
		return "groq"
	case strings.Contains(ua, "ollama"):
		return "ollama"
	case strings.Contains(ua, "moonshot"), strings.Contains(ua, "kimi"):
		return "kimi"
	case strings.Contains(ua, "qwen"):
		return "qwen"
	case strings.Contains(ua, "doubao"), strings.Contains(ua, "volcengine"):
		return "doubao"
	case strings.Contains(ua, "chatglm"), strings.Contains(ua, "zhipu"):
		return "zhipu"
	case strings.Contains(ua, "deepseek"):
		return "deepseek"
	case strings.Contains(ua, "chatgpt"):
		return "chatgpt"
	// xAI Grok（Grok app/CLI 与 Groq 公司为不同实体；Grok-User/Grok/ 为
	// agenstry caller-labels 流量实证模式）
	case strings.Contains(ua, "grok-user"), strings.Contains(ua, "grok/"):
		return "grok"
	case strings.Contains(ua, "minis/"):
		// Minis/<version> 安卓客户端（nginx 流量实证）
		return "minis"
	// LLM 框架（agenstry caller-labels 实证：langchain 系列嵌入 UA 不带锚定）
	case strings.Contains(ua, "langchain"):
		return "langchain"
	case strings.Contains(ua, "llama_index"), strings.Contains(ua, "llama-index"):
		return "llama_index"
	// MCP SDK（agenstry 实证：mcp-python-sdk / mcp-typescript-sdk / @modelcontextprotocol/sdk）
	case strings.Contains(ua, "mcp-python-sdk"), strings.Contains(ua, "mcp-typescript-sdk"), strings.Contains(ua, "modelcontextprotocol"):
		return "mcp_sdk"
	// 自动化工作流（agenstry 实证：n8n / Zapier / Make.com 作为客户端调用 LLM API）
	case strings.Contains(ua, "n8n"), strings.Contains(ua, "zapier"), strings.Contains(ua, "make.com"):
		return "automation"
	// 裸 HTTP 客户端（curl/wget/requests/urllib/okhttp/axios 等）
	case strings.Contains(ua, "curl/"), strings.Contains(ua, "wget/"), strings.Contains(ua, "python-requests"),
		strings.Contains(ua, "python-httpx"), strings.Contains(ua, "urllib"), strings.Contains(ua, "urllib3"),
		strings.Contains(ua, "node-fetch"), strings.Contains(ua, "reqwest"), strings.Contains(ua, "okhttp"),
		strings.Contains(ua, "axios"):
		return "http_client"
	}
	return "chat"
}

func GenerateTextOtherInfo(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, modelRatio, groupRatio, completionRatio float64,
	cacheTokens int, cacheRatio float64, modelPrice float64, userGroupRatio float64) map[string]interface{} {
	other := make(map[string]interface{})
	other["model_ratio"] = modelRatio
	other["group_ratio"] = groupRatio
	other["completion_ratio"] = completionRatio
	other["cache_tokens"] = cacheTokens
	other["cache_ratio"] = cacheRatio
	other["model_price"] = modelPrice
	other["user_group_ratio"] = userGroupRatio
	other["frt"] = float64(relayInfo.FirstResponseTime.UnixMilli() - relayInfo.StartTime.UnixMilli())
	if relayInfo.ReasoningEffort != "" {
		other["reasoning_effort"] = relayInfo.ReasoningEffort
	}
	if relayInfo.IsModelMapped {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = relayInfo.UpstreamModelName
	}

	isSystemPromptOverwritten := common.GetContextKeyBool(ctx, constant.ContextKeySystemPromptOverride)
	if isSystemPromptOverwritten {
		other["is_system_prompt_overwritten"] = true
	}

	adminInfo := make(map[string]interface{})
	adminInfo["use_channel"] = ctx.GetStringSlice("use_channel")
	adminInfo["client_profile"] = DetectClientProfile(ctx)
	// 原始 UA 字符串随识别结果一并落盘，便于管理员核对识别依据；
	// 仅管理员可见（admin_info 整体对非管理员剥离）。
	if ua := ctx.Request.UserAgent(); ua != "" {
		adminInfo["client_ua"] = ua
	}
	isMultiKey := common.GetContextKeyBool(ctx, constant.ContextKeyChannelIsMultiKey)
	if isMultiKey {
		adminInfo["is_multi_key"] = true
		adminInfo["multi_key_index"] = common.GetContextKeyInt(ctx, constant.ContextKeyChannelMultiKeyIndex)
	}

	isLocalCountTokens := common.GetContextKeyBool(ctx, constant.ContextKeyLocalCountTokens)
	if isLocalCountTokens {
		adminInfo["local_count_tokens"] = isLocalCountTokens
	}

	AppendChannelAffinityAdminInfo(ctx, adminInfo)

	other["admin_info"] = adminInfo
	appendRequestPath(ctx, relayInfo, other)
	appendRequestConversionChain(relayInfo, other)
	appendFinalRequestFormat(relayInfo, other)
	appendBillingInfo(relayInfo, other)
	appendParamOverrideInfo(relayInfo, other)
	appendStreamStatus(relayInfo, other)
	return other
}

func appendParamOverrideInfo(relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if relayInfo == nil || other == nil || len(relayInfo.ParamOverrideAudit) == 0 {
		return
	}
	other["po"] = relayInfo.ParamOverrideAudit
}

func appendStreamStatus(relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if relayInfo == nil || other == nil || !relayInfo.IsStream || relayInfo.StreamStatus == nil {
		return
	}
	ss := relayInfo.StreamStatus
	status := "ok"
	if !ss.IsNormalEnd() || ss.HasErrors() {
		status = "error"
	}
	streamInfo := map[string]interface{}{
		"status":     status,
		"end_reason": string(ss.EndReason),
	}
	if ss.EndError != nil {
		streamInfo["end_error"] = ss.EndError.Error()
	}
	if ss.ErrorCount > 0 {
		streamInfo["error_count"] = ss.ErrorCount
		messages := make([]string, 0, len(ss.Errors))
		for _, e := range ss.Errors {
			messages = append(messages, e.Message)
		}
		streamInfo["errors"] = messages
	}
	other["stream_status"] = streamInfo
}

func appendBillingInfo(relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if relayInfo == nil || other == nil {
		return
	}
	// billing_source: "wallet" or "subscription"
	if relayInfo.BillingSource != "" {
		other["billing_source"] = relayInfo.BillingSource
	}
	if relayInfo.UserSetting.BillingPreference != "" {
		other["billing_preference"] = relayInfo.UserSetting.BillingPreference
	}
	if relayInfo.BillingSource == "subscription" {
		if relayInfo.SubscriptionId != 0 {
			other["subscription_id"] = relayInfo.SubscriptionId
		}
		if relayInfo.SubscriptionPreConsumed > 0 {
			other["subscription_pre_consumed"] = relayInfo.SubscriptionPreConsumed
		}
		// post_delta: settlement delta applied after actual usage is known (can be negative for refund)
		if relayInfo.SubscriptionPostDelta != 0 {
			other["subscription_post_delta"] = relayInfo.SubscriptionPostDelta
		}
		if relayInfo.SubscriptionPlanId != 0 {
			other["subscription_plan_id"] = relayInfo.SubscriptionPlanId
		}
		if relayInfo.SubscriptionPlanTitle != "" {
			other["subscription_plan_title"] = relayInfo.SubscriptionPlanTitle
		}
		// Compute "this request" subscription consumed + remaining
		consumed := relayInfo.SubscriptionPreConsumed + relayInfo.SubscriptionPostDelta
		usedFinal := relayInfo.SubscriptionAmountUsedAfterPreConsume + relayInfo.SubscriptionPostDelta
		if consumed < 0 {
			consumed = 0
		}
		if usedFinal < 0 {
			usedFinal = 0
		}
		if relayInfo.SubscriptionAmountTotal > 0 {
			remain := relayInfo.SubscriptionAmountTotal - usedFinal
			if remain < 0 {
				remain = 0
			}
			other["subscription_total"] = relayInfo.SubscriptionAmountTotal
			other["subscription_used"] = usedFinal
			other["subscription_remain"] = remain
		}
		if consumed > 0 {
			other["subscription_consumed"] = consumed
		}
		// Wallet quota is not deducted when billed from subscription.
		other["wallet_quota_deducted"] = 0
	}
}

func appendRequestConversionChain(relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if relayInfo == nil || other == nil {
		return
	}
	if len(relayInfo.RequestConversionChain) == 0 {
		return
	}
	chain := make([]string, 0, len(relayInfo.RequestConversionChain))
	for _, f := range relayInfo.RequestConversionChain {
		switch f {
		case types.RelayFormatOpenAI:
			chain = append(chain, "OpenAI Compatible")
		case types.RelayFormatClaude:
			chain = append(chain, "Claude Messages")
		case types.RelayFormatGemini:
			chain = append(chain, "Google Gemini")
		case types.RelayFormatOpenAIResponses:
			chain = append(chain, "OpenAI Responses")
		default:
			chain = append(chain, string(f))
		}
	}
	if len(chain) == 0 {
		return
	}
	other["request_conversion"] = chain
}

func appendFinalRequestFormat(relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if relayInfo == nil || other == nil {
		return
	}
	if relayInfo.GetFinalRequestRelayFormat() == types.RelayFormatClaude {
		// claude indicates the final upstream request format is Claude Messages.
		// Frontend log rendering uses this to keep the original Claude input display.
		other["claude"] = true
	}
}

func GenerateWssOtherInfo(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, usage *dto.RealtimeUsage, modelRatio, groupRatio, completionRatio, audioRatio, audioCompletionRatio, modelPrice, userGroupRatio float64) map[string]interface{} {
	info := GenerateTextOtherInfo(ctx, relayInfo, modelRatio, groupRatio, completionRatio, 0, 0.0, modelPrice, userGroupRatio)
	info["ws"] = true
	info["audio_input"] = usage.InputTokenDetails.AudioTokens
	info["audio_output"] = usage.OutputTokenDetails.AudioTokens
	info["text_input"] = usage.InputTokenDetails.TextTokens
	info["text_output"] = usage.OutputTokenDetails.TextTokens
	info["audio_ratio"] = audioRatio
	info["audio_completion_ratio"] = audioCompletionRatio
	return info
}

func GenerateAudioOtherInfo(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, usage *dto.Usage, modelRatio, groupRatio, completionRatio, audioRatio, audioCompletionRatio, modelPrice, userGroupRatio float64) map[string]interface{} {
	info := GenerateTextOtherInfo(ctx, relayInfo, modelRatio, groupRatio, completionRatio, 0, 0.0, modelPrice, userGroupRatio)
	info["audio"] = true
	info["audio_input"] = usage.PromptTokensDetails.AudioTokens
	info["audio_output"] = usage.CompletionTokenDetails.AudioTokens
	info["text_input"] = usage.PromptTokensDetails.TextTokens
	info["text_output"] = usage.CompletionTokenDetails.TextTokens
	info["audio_ratio"] = audioRatio
	info["audio_completion_ratio"] = audioCompletionRatio
	return info
}

func GenerateClaudeOtherInfo(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, modelRatio, groupRatio, completionRatio float64,
	cacheTokens int, cacheRatio float64,
	cacheCreationTokens int, cacheCreationRatio float64,
	cacheCreationTokens5m int, cacheCreationRatio5m float64,
	cacheCreationTokens1h int, cacheCreationRatio1h float64,
	modelPrice float64, userGroupRatio float64) map[string]interface{} {
	info := GenerateTextOtherInfo(ctx, relayInfo, modelRatio, groupRatio, completionRatio, cacheTokens, cacheRatio, modelPrice, userGroupRatio)
	info["claude"] = true
	info["cache_creation_tokens"] = cacheCreationTokens
	info["cache_creation_ratio"] = cacheCreationRatio
	if cacheCreationTokens5m != 0 {
		info["cache_creation_tokens_5m"] = cacheCreationTokens5m
		info["cache_creation_ratio_5m"] = cacheCreationRatio5m
	}
	if cacheCreationTokens1h != 0 {
		info["cache_creation_tokens_1h"] = cacheCreationTokens1h
		info["cache_creation_ratio_1h"] = cacheCreationRatio1h
	}
	return info
}

func GenerateMjOtherInfo(relayInfo *relaycommon.RelayInfo, priceData hosttypes.PriceData) map[string]interface{} {
	other := make(map[string]interface{})
	other["model_price"] = priceData.ModelPrice
	other["group_ratio"] = priceData.GroupRatioInfo.GroupRatio
	if priceData.GroupRatioInfo.HasSpecialRatio {
		other["user_group_ratio"] = priceData.GroupRatioInfo.GroupSpecialRatio
	}
	appendRequestPath(nil, relayInfo, other)
	return other
}

// InjectTieredBillingInfo overlays tiered billing fields onto an existing
// module-specific other map. Call this after GenerateTextOtherInfo /
// GenerateClaudeOtherInfo / etc. when the request used tiered_expr billing.
func InjectTieredBillingInfo(other map[string]interface{}, relayInfo *relaycommon.RelayInfo, result *billingexpr.TieredResult) {
	if relayInfo == nil || other == nil {
		return
	}
	snap := relayInfo.TieredBillingSnapshot
	if snap == nil {
		return
	}
	other["billing_mode"] = "tiered_expr"
	other["expr_b64"] = base64.StdEncoding.EncodeToString([]byte(snap.ExprString))
	if result != nil {
		other["matched_tier"] = result.MatchedTier
	}
}
