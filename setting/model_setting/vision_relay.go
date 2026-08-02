package model_setting

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/setting/config"
)

// VisionRelaySettings 定义 Vision Relay（网关层图片识图替换）配置。
// 注册名 "vision_relay" → DB option keys 形如 vision_relay.enabled。
// 阶段 1 无 UI，通过 options API 写入；api_key 以 _key 结尾会被现有
// GetOptions 敏感字段过滤自动隐藏。
// 安全限制（图片数/像素/字节/并发等）是包内常量，不进 DB 配置面（v0.2.1）。
type VisionRelaySettings struct {
	Enabled      bool     `json:"enabled"`       // 总开关，默认关闭
	TargetModels []string `json:"target_models"` // 目标模型 allowlist（glob，JSON 数组），默认空=不处理任何模型
	Models       []string `json:"models"`        // 视觉模型 fallback 链（JSON 数组）
	BaseURL      string   `json:"base_url"`      // 视觉端点（OpenAI 兼容）
	APIKey       string   `json:"api_key"`       // 视觉端点鉴权
	Prompt       string   `json:"prompt"`        // 识图指令模板（空=默认保真基线）
	TimeoutSec   int      `json:"timeout_sec"`   // 每请求识图总预算（秒）
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

// 全局实例 + 快照读锁（配置由 ConfigManager 热更新，读侧拷贝防撕裂）
var (
	visionRelaySettings = defaultVisionRelaySettings
	visionRelayMu       sync.RWMutex
)

func init() {
	config.GlobalConfig.Register("vision_relay", &visionRelaySettings)
}

// GetVisionRelaySnapshot 返回不可变配置快照（审核 v0.2.1）：
// 锁内复制 + 深拷贝 []string + Trim + 默认值填充。每个请求开始取一次，
// 整个 Enhance 流程使用该副本，防热更新读到半套参数。
func GetVisionRelaySnapshot() VisionRelaySnapshot {
	visionRelayMu.RLock()
	defer visionRelayMu.RUnlock()
	cfg := visionRelaySettings
	cfg.TargetModels = append([]string(nil), visionRelaySettings.TargetModels...)
	cfg.Models = append([]string(nil), visionRelaySettings.Models...)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.Prompt = strings.TrimSpace(cfg.Prompt)
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = defaultVisionRelaySettings.TimeoutSec
	}
	if len(cfg.Models) == 0 {
		cfg.Models = append([]string(nil), defaultVisionRelaySettings.Models...)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultVisionRelaySettings.BaseURL
	}
	return cfg
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

// Validate 校验最终生效配置（service 层每次请求开始时调用）：
// 非法配置 → 记录并放行原请求（策略不生效，fail-safe 不阻塞主链路）。
func (v *VisionRelaySettings) Validate() error {
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
	if _, err := v.TargetModelPatterns(); err != nil {
		return err
	}
	return nil
}
