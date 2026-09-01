package model_setting

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

// VisionRelaySettings 定义 Vision Relay（网关层图片识图替换）配置。
// 注册名 "vision_relay" → DB option keys 形如 vision_relay.enabled。
// 15 字段（v0.4：每请求策略上限与缓存 TTL 迁入 DB 热更新）；api_key/
// sidecall_secret 以敏感后缀被 GetOptions 过滤自动隐藏（不回显，前端空值=
// 不修改）。
// 配置面分层（v0.4）：每请求策略（max_images/request_concurrency/描述字节/
// tokens/fallback 数/cache_ttl_sec）进 DB 热更新；进程级资源防线（解码字节/
// 像素/边长/全局并发闸）保持包内常量 + 启动环境变量，不进 DB（热改内存闸
// 会瞬时放大进程内存峰值，见 pkg/vision_relay/types.go）。
type VisionRelaySettings struct {
	Enabled           bool     `json:"enabled"`             // 总开关，默认关闭
	TargetModels      []string `json:"target_models"`       // 目标模型 allowlist（glob，JSON 数组），默认空=不处理任何模型
	Models            []string `json:"models"`              // 视觉模型 fallback 链（JSON 数组）
	BaseURL           string   `json:"base_url"`            // 视觉端点（OpenAI 兼容）
	APIKey            string   `json:"api_key"`             // 视觉端点鉴权
	Prompt            string   `json:"prompt"`              // 识图指令模板（空=默认保真基线）
	TimeoutSec        int      `json:"timeout_sec"`         // 每请求识图总预算（秒）
	Structured        bool     `json:"structured"`          // 结构化转写（v0.3）：SUMMARY/TRANSCRIPTION/LAYOUT/UNCERTAINTY 四小节证据结构；仅 Prompt 为空时生效；默认开启
	StructuredPrompt  string   `json:"structured_prompt"`   // 结构化转写指令模板（空=内置默认四小节指令）；仅 Structured=true 且 prompt 为空时生效
	SidecallSecret    string   `json:"sidecall_secret"`     // 递归保护共享 secret（认证 marker HMAC 密钥，审核 P0-2；空=不携带/不信任任何递归头）
	DisableProxyFetch bool     `json:"disable_proxy_fetch"` // 抓取用户图片 URL 时禁用环境代理（proxy-only 出口部署；默认 off=走环境代理）
	// v0.4：每请求策略上限 + 缓存 TTL（DB 热更新，写入面带范围校验，请求面
	// 越界钳制兜底）。默认值见 defaultVisionRelaySettings，与核心包默认常量
	// 保持一致；核心 withDefaults 是三层防御最内层（调用方手构 Config 时兜底）。
	CacheTTLSeconds     int `json:"cache_ttl_sec"`         // 跨请求描述缓存 TTL（秒）；0 = 禁用缓存
	MaxImages           int `json:"max_images"`            // 单请求最多处理图片数（fetch/decode 前生效）
	RequestConcurrency  int `json:"request_concurrency"`   // 每请求图片并发度（sidecall goroutine 数）
	MaxDescriptionBytes int `json:"max_description_bytes"` // 单图描述注入上限（截断后加尾标）
	MaxTotalBytes       int `json:"max_total_bytes"`       // 全部注入（含边界文本）总上限
	DefaultMaxTokens    int `json:"default_max_tokens"`    // 视觉模型输出上限（识图端点 max_tokens）
	MaxFallbackModels   int `json:"max_fallback_models"`   // fallback 链最多尝试模型数
}

// VisionRelaySnapshot 不可变配置快照（值对象，深拷贝 slice；请求全程使用）
type VisionRelaySnapshot = VisionRelaySettings

// 默认配置
var defaultVisionRelaySettings = VisionRelaySettings{
	Enabled:      false,
	TargetModels: []string{},
	Models:       []string{"gemma-4-31b", "step-3.7-flash", "grok-4.5"},
	BaseURL:      "",
	APIKey:       "",
	Prompt:       "",
	TimeoutSec:   15,
	// 结构化转写默认开启（v0.3.1）：识图是辅助能力非关键路径，四小节证据
	// 结构优于单段散文，且解析侧有散文降级兜底；可经 vision_relay.structured
	// 显式关闭。
	Structured: true,
	// v0.4：每请求策略默认值与核心包默认常量一致（pkg/vision_relay/types.go）。
	// MaxImages=20 配套 MaxTotalBytes=48k（agent 重发历史图的实测单图描述
	// ~1.3KB，20 图约 27KB，48k 留出结构化转写余量）；RequestConcurrency=4
	// 在 30s 预算内可转写更多图（跨请求缓存命中后并发几乎不产生成本）。
	CacheTTLSeconds:     86400, // 24h，与 v0.2.2 写死值一致；0=禁用
	MaxImages:           20,
	RequestConcurrency:  4,
	MaxDescriptionBytes: 8_000,
	MaxTotalBytes:       48_000,
	DefaultMaxTokens:    2000,
	MaxFallbackModels:   3,
}

// 全局实例（配置注册/默认导出对象；运行时快照从 OptionMap 读取，见
// GetVisionRelaySnapshot——不直接读本变量，避免与 ConfigManager 反射写入竞态）
var visionRelaySettings = defaultVisionRelaySettings

func init() {
	config.GlobalConfig.Register("vision_relay", &visionRelaySettings)
}

// GetVisionRelaySnapshot 从 common.OptionMap 读取运行时快照（v0.2.2 修复 data race）：
// ConfigManager 通过反射直接修改已注册 struct，自研锁无法覆盖该写入路径；
// 而 updateOptionMap 与读取方共用 OptionMapRWMutex——从 OptionMap 读取才是
// 真正同步的。读取在锁内复制 string，解析在锁外。每个请求开始取一次，
// 整个 Enhance 流程使用该副本（防热更新读到半套参数）。
//
// 严格解析（审核 P0-2 §4）：缺失 key → 默认值；key 存在但格式非法 →
// 明确配置错误（不静默修正——malformed allowlist 静默 no-op 会重新形成
// 原图透传风险）。enabled 用 strconv.ParseBool；数组 JSON 解析错误不吞；
// timeout 非正整数报错；base_url 用 net/url 完整解析（scheme http/https +
// host 非空）。
func GetVisionRelaySnapshot() (VisionRelaySnapshot, error) {
	common.OptionMapRWMutex.RLock()
	enabledRaw := common.OptionMap["vision_relay.enabled"]
	targetsRaw := common.OptionMap["vision_relay.target_models"]
	modelsRaw := common.OptionMap["vision_relay.models"]
	baseURL := common.OptionMap["vision_relay.base_url"]
	apiKey := common.OptionMap["vision_relay.api_key"]
	prompt := common.OptionMap["vision_relay.prompt"]
	timeoutRaw := common.OptionMap["vision_relay.timeout_sec"]
	structuredRaw := common.OptionMap["vision_relay.structured"]
	structuredPromptRaw := common.OptionMap["vision_relay.structured_prompt"]
	sidecallToken := common.OptionMap["vision_relay.sidecall_secret"]
	disableProxyRaw := common.OptionMap["vision_relay.disable_proxy_fetch"]
	// v0.4：每请求策略数值键（limits + 缓存 TTL）。逐键宽松解析（见
	// parseVisionRelayLimits），与其余键的严格解析刻意不对称。
	limitsRaw := map[string]string{
		"vision_relay.cache_ttl_sec":         common.OptionMap["vision_relay.cache_ttl_sec"],
		"vision_relay.max_images":            common.OptionMap["vision_relay.max_images"],
		"vision_relay.request_concurrency":   common.OptionMap["vision_relay.request_concurrency"],
		"vision_relay.max_description_bytes": common.OptionMap["vision_relay.max_description_bytes"],
		"vision_relay.max_total_bytes":       common.OptionMap["vision_relay.max_total_bytes"],
		"vision_relay.default_max_tokens":    common.OptionMap["vision_relay.default_max_tokens"],
		"vision_relay.max_fallback_models":   common.OptionMap["vision_relay.max_fallback_models"],
	}
	common.OptionMapRWMutex.RUnlock()

	snap := defaultVisionRelaySettings
	// enabled 是权威开关：格式非法 → 降级为 disabled（与"残留坏配置不把网关打成
	// 5xx"的自述目标一致；disabled 语义 = 零行为，其余字段不解析）
	if enabledRaw != "" {
		v, err := strconv.ParseBool(enabledRaw)
		if err != nil {
			return snap, nil
		}
		snap.Enabled = v
	}
	// 未启用 → 零行为优先：其余字段即使 malformed 也不阻断请求
	// （enabled=true 时才严格解析，防止残留坏配置把已关闭的功能打成 5xx）
	if !snap.Enabled {
		return snap, nil
	}
	if err := parseVisionRelayFields(&snap, targetsRaw, modelsRaw, baseURL, apiKey, prompt, timeoutRaw, structuredRaw, structuredPromptRaw, sidecallToken, disableProxyRaw); err != nil {
		return VisionRelaySnapshot{}, err
	}
	parseVisionRelayLimits(&snap, limitsRaw)
	return snap, nil
}

// parseVisionRelayLimits 解析 v0.4 数值键（每请求策略上限 + 缓存 TTL）。
// 与其余键的严格解析刻意不对称：limits 是资源/成本防线，坏值钳制到硬边界
// 回退默认防线即可——按严格解析把单个坏数值升级为全局 5xx 是更糟的故障
// 面（防线坏了不等于防线没了）。写时校验（ValidateVisionRelayWrite）已把
// 非法值拦截在入库面；这里只兜"直接改库绕过写校验"的残留。
// 生效值会出现在请求日志的 max_images/request_concurrency 字段，热更新
// 是否生效一眼可见。
func parseVisionRelayLimits(snap *VisionRelaySnapshot, raw map[string]string) {
	snap.CacheTTLSeconds = clampVisionRelayInt(raw["vision_relay.cache_ttl_sec"],
		snap.CacheTTLSeconds, visionRelayCacheTTLSecondsMin, visionRelayCacheTTLSecondsMax)
	snap.MaxImages = clampVisionRelayInt(raw["vision_relay.max_images"],
		snap.MaxImages, visionRelayMaxImagesMin, visionRelayMaxImagesMax)
	snap.RequestConcurrency = clampVisionRelayInt(raw["vision_relay.request_concurrency"],
		snap.RequestConcurrency, visionRelayRequestConcurrencyMin, visionRelayRequestConcurrencyMax)
	snap.MaxDescriptionBytes = clampVisionRelayInt(raw["vision_relay.max_description_bytes"],
		snap.MaxDescriptionBytes, visionRelayMaxDescriptionBytesMin, visionRelayMaxDescriptionBytesMax)
	snap.MaxTotalBytes = clampVisionRelayInt(raw["vision_relay.max_total_bytes"],
		snap.MaxTotalBytes, visionRelayMaxTotalBytesMin, visionRelayMaxTotalBytesMax)
	snap.DefaultMaxTokens = clampVisionRelayInt(raw["vision_relay.default_max_tokens"],
		snap.DefaultMaxTokens, visionRelayDefaultMaxTokensMin, visionRelayDefaultMaxTokensMax)
	snap.MaxFallbackModels = clampVisionRelayInt(raw["vision_relay.max_fallback_models"],
		snap.MaxFallbackModels, visionRelayMaxFallbackModelsMin, visionRelayMaxFallbackModelsMax)
}

// clampVisionRelayInt 整型配置钳制解析：空/非法 → 默认；越界 → 钳制到
// [min,max]。绝不返回越界值——核心包收到的 Limits 恒在硬边界内。
func clampVisionRelayInt(raw string, def, min, max int) int {
	if strings.TrimSpace(raw) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// parseVisionRelayFields 解析除 enabled 外的全部字段。GetVisionRelaySnapshot 在
// enabled=true 时调用；ValidateVisionRelayWrite 启用守卫也调用——守卫需要已存
// 字段的真实值做完整性校验，不受 disabled early-return 影响。
func parseVisionRelayFields(snap *VisionRelaySnapshot, targetsRaw, modelsRaw, baseURL, apiKey, prompt, timeoutRaw, structuredRaw, structuredPromptRaw, sidecallToken, disableProxyRaw string) error {
	if targetsRaw != "" {
		// 先解到全新 slice 再赋值（深拷贝）：避免 Unmarshal 对长度 ≤ 当前容量的
		// JSON 数组原地解码进 defaultVisionRelaySettings 共享 backing array，
		// 造成默认配置污染 + 并发读写竞态（审查 P1-1）
		var targets []string
		if err := common.UnmarshalJsonStr(targetsRaw, &targets); err != nil {
			return fmt.Errorf("vision_relay.target_models: %w", err)
		}
		snap.TargetModels = targets
	}
	if modelsRaw != "" {
		var models []string
		if err := common.UnmarshalJsonStr(modelsRaw, &models); err != nil {
			return fmt.Errorf("vision_relay.models: %w", err)
		}
		snap.Models = models
	}
	snap.BaseURL = strings.TrimSpace(baseURL)
	if snap.BaseURL == "" {
		snap.BaseURL = defaultVisionRelaySettings.BaseURL
	}
	if u, err := url.Parse(snap.BaseURL); err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// 不回显原始 baseURL：它可能带 userinfo 凭据，而该错误会经
		// visionRelayFail 记入后端日志（审核：凭据绝不进日志）。
		return fmt.Errorf("vision_relay.base_url: must be a valid http(s) URL with a host")
	}
	snap.APIKey = strings.TrimSpace(apiKey)
	snap.Prompt = strings.TrimSpace(prompt)
	snap.StructuredPrompt = strings.TrimSpace(structuredPromptRaw)
	snap.SidecallSecret = strings.TrimSpace(sidecallToken)
	if structuredRaw != "" {
		v, err := strconv.ParseBool(structuredRaw)
		if err != nil {
			return fmt.Errorf("vision_relay.structured: %w", err)
		}
		snap.Structured = v
	}
	if disableProxyRaw != "" {
		v, err := strconv.ParseBool(disableProxyRaw)
		if err != nil {
			return fmt.Errorf("vision_relay.disable_proxy_fetch: %w", err)
		}
		snap.DisableProxyFetch = v
	}
	if timeoutRaw != "" {
		v, err := strconv.Atoi(timeoutRaw)
		if err != nil || v < visionRelayTimeoutSecMin || v > visionRelayTimeoutSecMax {
			return fmt.Errorf("vision_relay.timeout_sec: must be in [%d, %d], got %q", visionRelayTimeoutSecMin, visionRelayTimeoutSecMax, timeoutRaw)
		}
		snap.TimeoutSec = v
	}
	if len(snap.Models) == 0 {
		snap.Models = append([]string(nil), defaultVisionRelaySettings.Models...)
	}
	return nil
}

// globToRegexp 把模型 glob 模式转为正则：
// '*' 匹配任意字符序列，'?' 匹配单个字符，'[...]' 字符类（'[!...]' 取反），
// 其余按字面。未闭合字符类返回错误（非法 glob）。
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var builder strings.Builder
	builder.WriteString("^")
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '*':
			builder.WriteString(".*")
		case '?':
			builder.WriteString(".")
		case '[':
			negate := false
			j := i + 1
			if j < len(runes) && runes[j] == '!' {
				negate = true
				j++
			}
			end := -1
			for k := j; k < len(runes); k++ {
				if runes[k] == ']' {
					end = k
					break
				}
			}
			if end == -1 {
				return nil, fmt.Errorf("unclosed character class in pattern %q", pattern)
			}
			builder.WriteString("[")
			if negate {
				builder.WriteString("^")
			}
			builder.WriteString(string(runes[j:end]))
			builder.WriteString("]")
			i = end
		default:
			builder.WriteString(regexp.QuoteMeta(string(runes[i])))
		}
	}
	builder.WriteString("$")
	return regexp.Compile(builder.String())
}

// TargetModelPatterns 解析并编译目标模型 allowlist（glob）。
// 供策略匹配复用；编译失败视为配置错误（Validate 时暴露）。
func (v *VisionRelaySettings) TargetModelPatterns() ([]*regexp.Regexp, error) {
	patterns := make([]*regexp.Regexp, 0, len(v.TargetModels))
	for _, item := range v.TargetModels {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		re, err := globToRegexp(item)
		if err != nil {
			return nil, fmt.Errorf("invalid target_models pattern %q: %w", item, err)
		}
		patterns = append(patterns, re)
	}
	return patterns, nil
}

// Validate 校验完整配置（含端点必填项）——测试/工具用；Service 路径用
// ValidateEndpoint（模型命中后才要求端点完整，v0.2.2 不再 fail-open）。
func (v *VisionRelaySettings) Validate() error {
	if err := v.ValidateEndpoint(); err != nil {
		return err
	}
	_, err := v.TargetModelPatterns()
	return err
}

// ValidateEndpoint 校验端点相关配置（enabled 且模型命中后调用）。
// 失败 = 配置错误 → Service 返回 5xx（绝不把原图发给纯文本模型）。
func (v *VisionRelaySettings) ValidateEndpoint() error {
	if v.TimeoutSec < visionRelayTimeoutSecMin || v.TimeoutSec > visionRelayTimeoutSecMax {
		return fmt.Errorf("timeout_sec must be in [%d, %d], got %d", visionRelayTimeoutSecMin, visionRelayTimeoutSecMax, v.TimeoutSec)
	}
	if v.Enabled {
		baseURL := strings.TrimSpace(v.BaseURL)
		if baseURL == "" {
			return fmt.Errorf("base_url must not be empty when enabled")
		}
		if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
			// 不回显原始 baseURL：它可能被误写成含凭据的形态（user:pass@host），
			// 该错误会被 visionRelayFail 记入后端日志，回显会把凭据带进日志。
			return fmt.Errorf("base_url must start with http:// or https://")
		}
		if strings.TrimSpace(v.APIKey) == "" {
			return fmt.Errorf("api_key must not be empty when enabled")
		}
		if len(v.Models) == 0 {
			return fmt.Errorf("models must not be empty when enabled")
		}
		// 递归保护必须在运行时也强制：写时校验（ValidateVisionRelayWrite）
		// 已要求 enabled 时 secret 非空，但 secret 可被后续写空值清空（敏感键
		// 空值放行）或直接改库。运行时这层是最终防线——空 secret 会让旁路请求
		// 不带认证 marker，base_url 自环（loopback 或自身公网域名）时无界递归
		// 放大。此处 fail-closed 拒绝，而不是无 marker 继续。
		if strings.TrimSpace(v.SidecallSecret) == "" {
			return fmt.Errorf("sidecall_secret must not be empty when enabled (recursion protection)")
		}
	}
	return nil
}

// ValidateVisionRelayWrite 写时校验（controller/option.go 调用，仿
// ValidateGeminiSafetySettings 模式，审核 P0：把配置错误拦截在写入面，
// 消除 malformed 值入库后在请求面爆炸的全局 5xx 风险面）。
//
// 语义：
//   - enabled=true 时校验已存端点配置完整性（设置页按依赖顺序先写字段、
//     最后写 enabled——见 vision-relay-settings-card.tsx 提交排序）
//   - 自环场景强制 sidecall_secret 非空（enabled 时无条件要求，覆盖
//     loopback 与网关自身公网域名两种自环）：secret 空时 sidecall 不携带
//     marker，宽 allowlist 下会无界递归放大
//   - 数组/数字/URL 键做无状态格式校验（不依赖写入顺序）
//   - api_key/sidecall_secret 敏感键做最小格式校验（audit-2026-08 #48）：
//     非空时禁止空白/控制字符 + 长度下限，防手滑/注入直进 DB
func ValidateVisionRelayWrite(key, value string) error {
	switch key {
	case "vision_relay.enabled":
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("vision_relay.enabled: %w", err)
		}
		if !enabled {
			return nil
		}
		// 读取已存字段真实值做完整性校验（GetVisionRelaySnapshot 在 disabled 时
		// 提前返回不解析字段——守卫必须看到已存 base_url/api_key/models）
		common.OptionMapRWMutex.RLock()
		targetsRaw := common.OptionMap["vision_relay.target_models"]
		modelsRaw := common.OptionMap["vision_relay.models"]
		baseURL := common.OptionMap["vision_relay.base_url"]
		apiKey := common.OptionMap["vision_relay.api_key"]
		prompt := common.OptionMap["vision_relay.prompt"]
		timeoutRaw := common.OptionMap["vision_relay.timeout_sec"]
		structuredRaw := common.OptionMap["vision_relay.structured"]
		structuredPromptRaw := common.OptionMap["vision_relay.structured_prompt"]
		sidecallToken := common.OptionMap["vision_relay.sidecall_secret"]
		disableProxyRaw := common.OptionMap["vision_relay.disable_proxy_fetch"]
		common.OptionMapRWMutex.RUnlock()
		snap := defaultVisionRelaySettings
		snap.Enabled = true
		if err := parseVisionRelayFields(&snap, targetsRaw, modelsRaw, baseURL, apiKey, prompt, timeoutRaw, structuredRaw, structuredPromptRaw, sidecallToken, disableProxyRaw); err != nil {
			return err
		}
		if err := snap.ValidateEndpoint(); err != nil {
			return err
		}
		// 递归保护：enabled 时无条件要求 sidecall_secret 非空。
		// 旧逻辑只在 base_url 为 loopback（127.0.0.1/::1/localhost）时强制，
		// 但 base_url 同样可指向网关自身公网域名（自环）或任意内网服务；空
		// secret 时旁路请求不带认证 marker，宽 allowlist 下会无界递归放大。
		// sidecall_secret 成本极低（一个 16+ 字符串），无条件要求覆盖所有自环场景。
		if strings.TrimSpace(snap.SidecallSecret) == "" {
			return fmt.Errorf("vision_relay.sidecall_secret is required when enabled (recursion protection)")
		}
		return nil
	case "vision_relay.api_key", "vision_relay.sidecall_secret":
		// 敏感键最小格式校验（audit-2026-08 #48）；空值放行（前端空值=不修改）
		if err := validateVisionRelaySensitiveValue(key, value); err != nil {
			return err
		}
		return nil
	case "vision_relay.target_models", "vision_relay.models":
		if strings.TrimSpace(value) == "" {
			return nil
		}
		var arr []string
		if err := common.UnmarshalJsonStr(value, &arr); err != nil {
			return fmt.Errorf("%s: must be a JSON array of strings: %w", key, err)
		}
		return nil
	case "vision_relay.timeout_sec":
		n, err := strconv.Atoi(value)
		if err != nil || n < visionRelayTimeoutSecMin || n > visionRelayTimeoutSecMax {
			return fmt.Errorf("vision_relay.timeout_sec: must be in [%d, %d], got %q", visionRelayTimeoutSecMin, visionRelayTimeoutSecMax, value)
		}
		return nil
	case "vision_relay.cache_ttl_sec", "vision_relay.max_images",
		"vision_relay.request_concurrency", "vision_relay.max_description_bytes",
		"vision_relay.max_total_bytes", "vision_relay.default_max_tokens",
		"vision_relay.max_fallback_models":
		// v0.4：数值键写时范围校验（硬边界表）。空值放行（前端空值=不修改；
		// 请求面按默认值钳制兜底）。
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return validateVisionRelayIntKey(key, value)
	case "vision_relay.disable_proxy_fetch", "vision_relay.structured":
		if strings.TrimSpace(value) == "" {
			return nil
		}
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return nil
	case "vision_relay.base_url":
		if strings.TrimSpace(value) == "" {
			return nil
		}
		u, err := url.Parse(strings.TrimSpace(value))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("vision_relay.base_url: must be an absolute http(s) URL, got %q", value)
		}
		return nil
	case "vision_relay.prompt", "vision_relay.structured_prompt":
		if utf8.RuneCountInString(value) > maxVisionRelayPromptLength {
			return fmt.Errorf("%s: exceeds maximum length of %d characters", key, maxVisionRelayPromptLength)
		}
		return nil
	default:
		return nil
	}
}

// 敏感字段写时校验的最小长度（audit-2026-08 #48：防手滑的最小集）。
// prompt/structured_prompt 的最大长度上限（防超长指令注入或误粘贴整段文档）。
const (
	minVisionRelayAPIKeyLength         = 8    // 常见 key 均远长于此；仅拦明显截断/占位符
	minVisionRelaySidecallSecretLength = 16   // HMAC 认证 marker 密钥，弱密钥可被暴力枚举伪造
	maxVisionRelayPromptLength         = 8192 // 识图指令模板上限；正常使用远低于此值
)

// v0.4 数值键硬边界（写时校验与请求时钳制共用）。上限留足调优余量但封顶
// 天文数字——limits 是成本/资源防线，钳制上限即防线。缓存 TTL 上限 7 天
// （TTL 过长会让陈旧描述滞留 Redis 且绕过识图指令变更后的自然失效）。
const (
	visionRelayCacheTTLSecondsMin, visionRelayCacheTTLSecondsMax         = 0, 604_800 // 0=禁用缓存
	visionRelayTimeoutSecMin, visionRelayTimeoutSecMax                   = 1, 600
	visionRelayMaxImagesMin, visionRelayMaxImagesMax                     = 1, 200
	visionRelayRequestConcurrencyMin, visionRelayRequestConcurrencyMax   = 1, 8
	visionRelayMaxDescriptionBytesMin, visionRelayMaxDescriptionBytesMax = 1_000, 32_000
	visionRelayMaxTotalBytesMin, visionRelayMaxTotalBytesMax             = 4_000, 2_000_000
	visionRelayDefaultMaxTokensMin, visionRelayDefaultMaxTokensMax       = 256, 16_384
	visionRelayMaxFallbackModelsMin, visionRelayMaxFallbackModelsMax     = 1, 8
)

// visionRelayIntKeyBounds 数值键 → [min,max] 表（写时校验用；请求时钳制
// 的 min/max 在 parseVisionRelayLimits 逐键硬编码同一组常量）。
var visionRelayIntKeyBounds = map[string][2]int{
	"vision_relay.cache_ttl_sec":         {visionRelayCacheTTLSecondsMin, visionRelayCacheTTLSecondsMax},
	"vision_relay.max_images":            {visionRelayMaxImagesMin, visionRelayMaxImagesMax},
	"vision_relay.request_concurrency":   {visionRelayRequestConcurrencyMin, visionRelayRequestConcurrencyMax},
	"vision_relay.max_description_bytes": {visionRelayMaxDescriptionBytesMin, visionRelayMaxDescriptionBytesMax},
	"vision_relay.max_total_bytes":       {visionRelayMaxTotalBytesMin, visionRelayMaxTotalBytesMax},
	"vision_relay.default_max_tokens":    {visionRelayDefaultMaxTokensMin, visionRelayDefaultMaxTokensMax},
	"vision_relay.max_fallback_models":   {visionRelayMaxFallbackModelsMin, visionRelayMaxFallbackModelsMax},
}

// validateVisionRelayIntKey 数值键写时校验：必须为整数且落在硬边界内。
// 非法值在入库面被拒，杜绝"天文数字直进 DB"（请求面钳制只是兜底，不替
// 代写时拦截——写时给操作者即时反馈，请求面静默钳制会让误配无感知）。
func validateVisionRelayIntKey(key, value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s: must be an integer, got %q", key, value)
	}
	bounds, ok := visionRelayIntKeyBounds[key]
	if !ok {
		return nil
	}
	if n < bounds[0] || n > bounds[1] {
		return fmt.Errorf("%s: out of allowed range [%d, %d], got %d", key, bounds[0], bounds[1], n)
	}
	return nil
}

// validateVisionRelaySensitiveValue api_key/sidecall_secret 写时最小格式校验
// （audit-2026-08 #48）：空值放行（前端空值=不修改语义）；非空时禁止空白/
// 控制字符（防换行/日志注入），且长度达下限。校验失败返回明确错误，写入面拦截。
func validateVisionRelaySensitiveValue(key, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return fmt.Errorf("%s: must not contain whitespace or control characters", key)
	}
	minLen := minVisionRelayAPIKeyLength
	if key == "vision_relay.sidecall_secret" {
		minLen = minVisionRelaySidecallSecretLength
	}
	if utf8.RuneCountInString(value) < minLen {
		return fmt.Errorf("%s: must be at least %d characters when set", key, minLen)
	}
	return nil
}

// VisionRelayOptionKeys is the full set of vision_relay.* DB option keys, in
// the order the settings card renders them. The controller bulk endpoint uses
// this to map request fields to option keys and to know which keys belong to
// the vision relay feature.
var VisionRelayOptionKeys = []string{
	"vision_relay.enabled",
	"vision_relay.structured",
	"vision_relay.structured_prompt",
	"vision_relay.target_models",
	"vision_relay.models",
	"vision_relay.base_url",
	"vision_relay.api_key",
	"vision_relay.prompt",
	"vision_relay.timeout_sec",
	"vision_relay.sidecall_secret",
	"vision_relay.disable_proxy_fetch",
	// v0.4：每请求策略上限 + 缓存 TTL（DB 热更新，写时范围校验）
	"vision_relay.cache_ttl_sec",
	"vision_relay.max_images",
	"vision_relay.request_concurrency",
	"vision_relay.max_description_bytes",
	"vision_relay.max_total_bytes",
	"vision_relay.default_max_tokens",
	"vision_relay.max_fallback_models",
}

// IsVisionRelaySecretKey reports whether the given option key is a write-only
// sensitive field (api_key / sidecall_secret). The bulk endpoint treats an
// empty string for these keys as "keep existing value" rather than "clear".
func IsVisionRelaySecretKey(key string) bool {
	return key == "vision_relay.api_key" || key == "vision_relay.sidecall_secret"
}

// ValidateVisionRelayBulkSnapshot validates the full prospective state that
// results from applying a bulk update. The caller must resolve secret
// keep-semantics first: for api_key/sidecall_secret, an empty string in the
// update means "keep existing" — the caller must fill in the current OptionMap
// value before calling this function so the snapshot reflects what the DB will
// actually contain after the write.
//
// This replaces per-key ValidateVisionRelayWrite for the bulk path because
// per-key validation of vision_relay.enabled reads the OLD OptionMap state,
// which is stale when multiple keys change in the same transaction (e.g.
// enabling and setting base_url together). The snapshot validates the
// resulting state directly, including format checks delegated to
// parseVisionRelayFields and consistency checks via ValidateEndpoint.
func ValidateVisionRelayBulkSnapshot(resolved map[string]string) error {
	enabledRaw, ok := resolved["vision_relay.enabled"]
	if !ok {
		return fmt.Errorf("vision_relay.enabled: missing from bulk update")
	}
	enabled, err := strconv.ParseBool(enabledRaw)
	if err != nil {
		return fmt.Errorf("vision_relay.enabled: %w", err)
	}
	snap := defaultVisionRelaySettings
	snap.Enabled = enabled
	if !enabled {
		return nil
	}
	if err := parseVisionRelayFields(&snap,
		resolved["vision_relay.target_models"],
		resolved["vision_relay.models"],
		resolved["vision_relay.base_url"],
		resolved["vision_relay.api_key"],
		resolved["vision_relay.prompt"],
		resolved["vision_relay.timeout_sec"],
		resolved["vision_relay.structured"],
		resolved["vision_relay.structured_prompt"],
		resolved["vision_relay.sidecall_secret"],
		resolved["vision_relay.disable_proxy_fetch"],
	); err != nil {
		return err
	}
	// v0.4：数值键同样进前瞻快照（宽松钳制，与请求面一致；写时范围校验由
	// 控制器的逐键 ValidateVisionRelayWrite 循环负责）。
	parseVisionRelayLimits(&snap, resolved)
	if err := snap.ValidateEndpoint(); err != nil {
		return err
	}
	if strings.TrimSpace(snap.SidecallSecret) == "" {
		return fmt.Errorf("vision_relay.sidecall_secret is required when enabled (recursion protection)")
	}
	return nil
}
