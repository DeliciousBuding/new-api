package model_setting

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

// VisionRelaySettings 定义 Vision Relay（网关层图片识图替换）配置。
// 注册名 "vision_relay" → DB option keys 形如 vision_relay.enabled。
// 8 字段（v0.2.3：含 sidecall_secret）；api_key/sidecall_secret 以敏感后缀
// 被 GetOptions 过滤自动隐藏（不回显，前端空值=不修改）。
// 安全限制（图片数/像素/字节/并发等）是包内常量，不进 DB 配置面（v0.2.1）。
type VisionRelaySettings struct {
	Enabled        bool     `json:"enabled"`         // 总开关，默认关闭
	TargetModels   []string `json:"target_models"`   // 目标模型 allowlist（glob，JSON 数组），默认空=不处理任何模型
	Models         []string `json:"models"`          // 视觉模型 fallback 链（JSON 数组）
	BaseURL        string   `json:"base_url"`        // 视觉端点（OpenAI 兼容）
	APIKey         string   `json:"api_key"`         // 视觉端点鉴权
	Prompt         string   `json:"prompt"`          // 识图指令模板（空=默认保真基线）
	TimeoutSec     int      `json:"timeout_sec"`     // 每请求识图总预算（秒）
	SidecallSecret string   `json:"sidecall_secret"` // 递归保护共享 secret（认证 marker HMAC 密钥，审核 P0-2；空=不携带/不信任任何递归头）
}

// VisionRelaySnapshot 不可变配置快照（值对象，深拷贝 slice；请求全程使用）
type VisionRelaySnapshot = VisionRelaySettings

// 默认配置
var defaultVisionRelaySettings = VisionRelaySettings{
	Enabled:      false,
	TargetModels: []string{},
	Models:       []string{"gemma-4-31b", "step-3.7-flash", "grok-4.5"},
	BaseURL:      "https://api.tokendancelab.com",
	APIKey:       "",
	Prompt:       "",
	TimeoutSec:   15,
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
	sidecallToken := common.OptionMap["vision_relay.sidecall_secret"]
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
	if err := parseVisionRelayFields(&snap, targetsRaw, modelsRaw, baseURL, apiKey, prompt, timeoutRaw, sidecallToken); err != nil {
		return VisionRelaySnapshot{}, err
	}
	return snap, nil
}

// parseVisionRelayFields 解析除 enabled 外的全部字段。GetVisionRelaySnapshot 在
// enabled=true 时调用；ValidateVisionRelayWrite 启用守卫也调用——守卫需要已存
// 字段的真实值做完整性校验，不受 disabled early-return 影响。
func parseVisionRelayFields(snap *VisionRelaySnapshot, targetsRaw, modelsRaw, baseURL, apiKey, prompt, timeoutRaw, sidecallToken string) error {
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
		return fmt.Errorf("vision_relay.base_url: invalid URL %q", snap.BaseURL)
	}
	snap.APIKey = strings.TrimSpace(apiKey)
	snap.Prompt = strings.TrimSpace(prompt)
	snap.SidecallSecret = strings.TrimSpace(sidecallToken)
	if timeoutRaw != "" {
		v, err := strconv.Atoi(timeoutRaw)
		if err != nil || v <= 0 {
			return fmt.Errorf("vision_relay.timeout_sec: must be positive integer, got %q", timeoutRaw)
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
	if v.TimeoutSec <= 0 {
		return fmt.Errorf("timeout_sec must be > 0, got %d", v.TimeoutSec)
	}
	if v.Enabled {
		baseURL := strings.TrimSpace(v.BaseURL)
		if baseURL == "" {
			return fmt.Errorf("base_url must not be empty when enabled")
		}
		if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
			return fmt.Errorf("base_url must start with http:// or https://, got %q", baseURL)
		}
		if strings.TrimSpace(v.APIKey) == "" {
			return fmt.Errorf("api_key must not be empty when enabled")
		}
		if len(v.Models) == 0 {
			return fmt.Errorf("models must not be empty when enabled")
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
//   - 自环场景（base_url 指向 loopback）强制 sidecall_secret 非空：
//     secret 空时 sidecall 不携带 marker，宽 allowlist 下会无界递归放大
//   - 数组/数字/URL 键做无状态格式校验（不依赖写入顺序）
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
		sidecallToken := common.OptionMap["vision_relay.sidecall_secret"]
		common.OptionMapRWMutex.RUnlock()
		snap := defaultVisionRelaySettings
		snap.Enabled = true
		if err := parseVisionRelayFields(&snap, targetsRaw, modelsRaw, baseURL, apiKey, prompt, timeoutRaw, sidecallToken); err != nil {
			return err
		}
		if err := snap.ValidateEndpoint(); err != nil {
			return err
		}
		u, err := url.Parse(strings.TrimSpace(snap.BaseURL))
		if err != nil {
			return err
		}
		host := u.Hostname()
		if (host == "127.0.0.1" || host == "::1" || host == "localhost") &&
			strings.TrimSpace(snap.SidecallSecret) == "" {
			return fmt.Errorf("vision_relay.sidecall_secret is required for self-loop base_url (recursion protection)")
		}
		return nil
	case "vision_relay.target_models", "vision_relay.models":
		if strings.TrimSpace(value) == "" {
			return nil
		}
		var arr []string
		if err := json.Unmarshal(common.StringToByteSlice(value), &arr); err != nil {
			return fmt.Errorf("%s: must be a JSON array of strings: %w", key, err)
		}
		return nil
	case "vision_relay.timeout_sec":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fmt.Errorf("vision_relay.timeout_sec: must be a positive integer, got %q", value)
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
	default:
		return nil
	}
}
