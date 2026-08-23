package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/console_setting"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

var completionRatioMetaOptionKeys = []string{
	"ModelPrice",
	"ModelRatio",
	"CompletionRatio",
	"CacheRatio",
	"CreateCacheRatio",
	"ImageRatio",
	"AudioRatio",
	"AudioCompletionRatio",
}

func isPaymentComplianceOptionKey(key string) bool {
	return strings.HasPrefix(key, "payment_setting.compliance_")
}

func isPositiveOptionValue(value string) bool {
	intValue, err := strconv.Atoi(strings.TrimSpace(value))
	if err == nil {
		return intValue > 0
	}
	floatValue, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return err == nil && floatValue > 0
}

func collectModelNamesFromOptionValue(raw string, modelNames map[string]struct{}) {
	if strings.TrimSpace(raw) == "" {
		return
	}

	var parsed map[string]any
	if err := common.UnmarshalJsonStr(raw, &parsed); err != nil {
		return
	}

	for modelName := range parsed {
		modelNames[modelName] = struct{}{}
	}
}

func buildCompletionRatioMetaValue(optionValues map[string]string) string {
	modelNames := make(map[string]struct{})
	for _, key := range completionRatioMetaOptionKeys {
		collectModelNamesFromOptionValue(optionValues[key], modelNames)
	}

	meta := make(map[string]ratio_setting.CompletionRatioInfo, len(modelNames))
	for modelName := range modelNames {
		meta[modelName] = ratio_setting.GetCompletionRatioInfo(modelName)
	}

	jsonBytes, err := common.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(jsonBytes)
}

func GetOptions(c *gin.Context) {
	var options []*model.Option
	optionValues := make(map[string]string)
	common.OptionMapRWMutex.Lock()
	for k, v := range common.OptionMap {
		if k == "theme.frontend" {
			continue
		}
		value := common.Interface2String(v)
		isSensitiveKey := strings.HasSuffix(k, "Token") ||
			strings.HasSuffix(k, "Secret") ||
			strings.HasSuffix(k, "Key") ||
			strings.HasSuffix(k, "secret") ||
			strings.HasSuffix(k, "api_key")
		if isSensitiveKey {
			continue
		}
		options = append(options, &model.Option{
			Key:   k,
			Value: value,
		})
		for _, optionKey := range completionRatioMetaOptionKeys {
			if optionKey == k {
				optionValues[k] = value
				break
			}
		}
	}
	common.OptionMapRWMutex.Unlock()
	options = append(options, &model.Option{
		Key:   "CompletionRatioMeta",
		Value: buildCompletionRatioMetaValue(optionValues),
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    options,
	})
}

type OptionUpdateRequest struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func UpdateOption(c *gin.Context) {
	var option OptionUpdateRequest
	err := common.DecodeJson(c.Request.Body, &option)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}
	switch option.Value.(type) {
	case bool:
		option.Value = common.Interface2String(option.Value.(bool))
	case float64:
		option.Value = common.Interface2String(option.Value.(float64))
	case int:
		option.Value = common.Interface2String(option.Value.(int))
	default:
		option.Value = fmt.Sprintf("%v", option.Value)
	}
	switch option.Key {
	case "QuotaForInviter", "QuotaForInvitee":
		if isPositiveOptionValue(option.Value.(string)) && !operation_setting.IsPaymentComplianceConfirmed() {
			common.ApiErrorI18n(c, i18n.MsgPaymentComplianceRequired)
			return
		}
	default:
		if isPaymentComplianceOptionKey(option.Key) {
			common.ApiErrorMsg(c, "合规确认字段不允许通过通用设置接口修改")
			return
		}
	}
	switch option.Key {
	case "GitHubOAuthEnabled":
		if option.Value == "true" && common.GitHubClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 GitHub OAuth，请先填入 GitHub Client Id 以及 GitHub Client Secret！",
			})
			return
		}
	case "discord.enabled":
		if option.Value == "true" && system_setting.GetDiscordSettings().ClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Discord OAuth，请先填入 Discord Client Id 以及 Discord Client Secret！",
			})
			return
		}
	case "oidc.enabled":
		if option.Value == "true" && system_setting.GetOIDCSettings().ClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 OIDC 登录，请先填入 OIDC Client Id 以及 OIDC Client Secret！",
			})
			return
		}
	case "LinuxDOOAuthEnabled":
		if option.Value == "true" && common.LinuxDOClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 LinuxDO OAuth，请先填入 LinuxDO Client Id 以及 LinuxDO Client Secret！",
			})
			return
		}
	case "EmailDomainRestrictionEnabled":
		if option.Value == "true" && len(common.EmailDomainWhitelist) == 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用邮箱域名限制，请先填入限制的邮箱域名！",
			})
			return
		}
	case "WeChatAuthEnabled":
		if option.Value == "true" && common.WeChatServerAddress == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用微信登录，请先填入微信登录相关配置信息！",
			})
			return
		}
	case "TurnstileCheckEnabled":
		if option.Value == "true" && common.TurnstileSiteKey == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Turnstile 校验，请先填入 Turnstile 校验相关配置信息！",
			})

			return
		}
	case "TelegramOAuthEnabled":
		if option.Value == "true" && common.TelegramBotToken == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Telegram OAuth，请先填入 Telegram Bot Token！",
			})
			return
		}
	case "theme.frontend":
		if option.Value != "default" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Classic 前端已移除，主题只能设置为 default",
			})
			return
		}
	case "GroupRatio":
		err = ratio_setting.CheckGroupRatio(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "gemini.safety_settings":
		err = model_setting.ValidateGeminiSafetySettings(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "claude.default_max_tokens":
		err = model_setting.ValidateClaudeDefaultMaxTokens(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "vision_relay.enabled", "vision_relay.target_models", "vision_relay.models",
		"vision_relay.timeout_sec", "vision_relay.base_url",
		"vision_relay.api_key", "vision_relay.sidecall_secret",
		"vision_relay.prompt", "vision_relay.structured",
		"vision_relay.structured_prompt", "vision_relay.disable_proxy_fetch",
		"vision_relay.cache_ttl_sec", "vision_relay.max_images",
		"vision_relay.request_concurrency", "vision_relay.max_description_bytes",
		"vision_relay.max_total_bytes", "vision_relay.default_max_tokens",
		"vision_relay.max_fallback_models":
		err = model_setting.ValidateVisionRelayWrite(option.Key, option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "cache_usage_aggregation.enabled", "cache_usage_aggregation.interval_minutes":
		err = model_setting.ValidateCacheUsageAggregationWrite(option.Key, option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case operation_setting.ToolPriceOptionKey:
		err = operation_setting.ValidateToolPricesJSON(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "ImageRatio":
		err = ratio_setting.UpdateImageRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "图片倍率设置失败: " + err.Error(),
			})
			return
		}
	case "AudioRatio":
		err = ratio_setting.UpdateAudioRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "音频倍率设置失败: " + err.Error(),
			})
			return
		}
	case "AudioCompletionRatio":
		err = ratio_setting.UpdateAudioCompletionRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "音频补全倍率设置失败: " + err.Error(),
			})
			return
		}
	case "CreateCacheRatio":
		err = ratio_setting.UpdateCreateCacheRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "缓存创建倍率设置失败: " + err.Error(),
			})
			return
		}
	case "ModelRequestRateLimitGroup":
		err = setting.CheckModelRequestRateLimitGroup(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "AutomaticDisableStatusCodes":
		_, err = operation_setting.ParseHTTPStatusCodeRanges(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "AutomaticRetryStatusCodes":
		_, err = operation_setting.ParseHTTPStatusCodeRanges(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.api_info":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "ApiInfo")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.announcements":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "Announcements")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.faq":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "FAQ")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.uptime_kuma_groups":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "UptimeKumaGroups")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	}
	err = model.UpdateOption(option.Key, option.Value.(string))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	// 出于安全考虑只记录被修改的配置项名称，不记录配置值（可能含密钥等敏感信息）。
	recordManageAudit(c, "option.update", map[string]interface{}{
		"key": option.Key,
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

// VisionRelayOptionUpdateRequest carries the full set of vision_relay.* option
// values in a single request body so the settings card can save them atomically.
// All fields are strings to match the DB option format. For api_key and
// sidecall_secret, an empty string means "keep the existing value" — the
// backend enforces this contract, so a direct API caller submitting an empty
// string for a secret cannot accidentally clear it.
type VisionRelayOptionUpdateRequest struct {
	Enabled           string `json:"enabled"`
	Structured        string `json:"structured"`
	StructuredPrompt  string `json:"structured_prompt"`
	TargetModels      string `json:"target_models"`
	Models            string `json:"models"`
	BaseURL           string `json:"base_url"`
	APIKey            string `json:"api_key"`
	Prompt            string `json:"prompt"`
	TimeoutSec        string `json:"timeout_sec"`
	SidecallSecret    string `json:"sidecall_secret"`
	DisableProxyFetch string `json:"disable_proxy_fetch"`
	// v0.4：每请求策略上限 + 缓存 TTL（字符串，与 DB option 格式一致）
	CacheTTLSeconds     string `json:"cache_ttl_sec"`
	MaxImages           string `json:"max_images"`
	RequestConcurrency  string `json:"request_concurrency"`
	MaxDescriptionBytes string `json:"max_description_bytes"`
	MaxTotalBytes       string `json:"max_total_bytes"`
	DefaultMaxTokens    string `json:"default_max_tokens"`
	MaxFallbackModels   string `json:"max_fallback_models"`
}

// UpdateVisionRelayOptions saves the full vision relay configuration in a single
// atomic transaction. It resolves secret keep-semantics (empty = keep existing),
// validates each key's format, validates the full prospective snapshot for
// cross-field consistency (e.g. enabled requires a complete endpoint), then
// writes only the changed keys via UpdateOptionsBulk. If any validation or DB
// write fails, no in-memory state is touched (transaction rollback).
func UpdateVisionRelayOptions(c *gin.Context) {
	var req VisionRelayOptionUpdateRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}

	updates := map[string]string{
		"vision_relay.enabled":               req.Enabled,
		"vision_relay.structured":            req.Structured,
		"vision_relay.structured_prompt":     req.StructuredPrompt,
		"vision_relay.target_models":         req.TargetModels,
		"vision_relay.models":                req.Models,
		"vision_relay.base_url":              req.BaseURL,
		"vision_relay.api_key":               req.APIKey,
		"vision_relay.prompt":                req.Prompt,
		"vision_relay.timeout_sec":           req.TimeoutSec,
		"vision_relay.sidecall_secret":       req.SidecallSecret,
		"vision_relay.disable_proxy_fetch":   req.DisableProxyFetch,
		"vision_relay.cache_ttl_sec":         req.CacheTTLSeconds,
		"vision_relay.max_images":            req.MaxImages,
		"vision_relay.request_concurrency":   req.RequestConcurrency,
		"vision_relay.max_description_bytes": req.MaxDescriptionBytes,
		"vision_relay.max_total_bytes":       req.MaxTotalBytes,
		"vision_relay.default_max_tokens":    req.DefaultMaxTokens,
		"vision_relay.max_fallback_models":   req.MaxFallbackModels,
	}

	// Read current OptionMap values for secret keep-semantics and change
	// detection. The snapshot reads happen under RLock to avoid racing with
	// concurrent option updates.
	common.OptionMapRWMutex.RLock()
	currentValues := make(map[string]string, len(model_setting.VisionRelayOptionKeys))
	for _, key := range model_setting.VisionRelayOptionKeys {
		currentValues[key] = common.OptionMap[key]
	}
	common.OptionMapRWMutex.RUnlock()

	// Resolve secret keep-semantics: empty api_key/sidecall_secret means "keep
	// the existing value", not "clear". This is the backend-enforced contract —
	// a direct API caller cannot clear a secret by submitting an empty string.
	resolved := make(map[string]string, len(updates))
	for key, value := range updates {
		if model_setting.IsVisionRelaySecretKey(key) && strings.TrimSpace(value) == "" {
			resolved[key] = currentValues[key]
		} else {
			resolved[key] = value
		}
	}

	// Per-key format validation. Skip enabled — it has cross-field dependencies
	// that the prospective snapshot validates against the resolved values, not
	// the stale OptionMap that ValidateVisionRelayWrite.enabled would read.
	for key, value := range updates {
		if key == "vision_relay.enabled" {
			continue
		}
		if model_setting.IsVisionRelaySecretKey(key) && strings.TrimSpace(value) == "" {
			continue
		}
		if err := model_setting.ValidateVisionRelayWrite(key, value); err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	}

	// Full prospective snapshot validation: the resulting state after applying
	// all updates together. This catches consistency errors that per-key
	// validation cannot — e.g. enabling vision relay in the same transaction
	// that sets base_url, where per-key enabled validation would read the old
	// (empty) base_url from OptionMap and reject the enable.
	if err := model_setting.ValidateVisionRelayBulkSnapshot(resolved); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	// Build the write map: only keys whose value actually changed, and never
	// include empty secrets (keep = no write). This keeps the DB write minimal
	// and the audit log meaningful.
	writeMap := make(map[string]string)
	for key, value := range updates {
		if model_setting.IsVisionRelaySecretKey(key) && strings.TrimSpace(value) == "" {
			continue
		}
		if value != currentValues[key] {
			writeMap[key] = value
		}
	}

	if len(writeMap) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "",
		})
		return
	}

	if err := model.UpdateOptionsBulk(writeMap); err != nil {
		common.ApiError(c, err)
		return
	}

	changedKeys := make([]string, 0, len(writeMap))
	for key := range writeMap {
		changedKeys = append(changedKeys, key)
	}
	recordManageAudit(c, "option.update_vision_relay", map[string]interface{}{
		"keys": changedKeys,
	})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}
