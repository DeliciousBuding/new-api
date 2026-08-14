package model_setting

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
)

func TestVisionRelayValidate(t *testing.T) {
	valid := defaultVisionRelaySettings
	valid.Enabled = true
	valid.APIKey = "sk-test"
	valid.TargetModels = []string{"deepseek*"}
	valid.BaseURL = "https://vision.example.com"
	valid.SidecallSecret = "test-sidecall-secret-123"

	cases := []struct {
		name    string
		mutate  func(*VisionRelaySettings)
		wantErr string // 空=期望通过
	}{
		{"默认配置（关闭）通过", func(s *VisionRelaySettings) {}, ""},
		{"启用+完整配置通过", func(s *VisionRelaySettings) { *s = valid }, ""},
		{"启用但缺 key", func(s *VisionRelaySettings) {
			*s = valid
			s.APIKey = ""
		}, "api_key"},
		{"启用但缺 models", func(s *VisionRelaySettings) {
			*s = valid
			s.Models = nil
		}, "models"},
		{"启用但 BaseURL 空", func(s *VisionRelaySettings) {
			*s = valid
			s.BaseURL = ""
		}, "base_url"},
		{"BaseURL 非 http/https", func(s *VisionRelaySettings) {
			*s = valid
			s.BaseURL = "ftp://example.com"
		}, "http"},
		{"负数/零超时", func(s *VisionRelaySettings) { s.TimeoutSec = 0 }, "timeout_sec"},
		{"glob 非法（未闭合字符类）", func(s *VisionRelaySettings) { s.TargetModels = []string{"deepseek*["} }, "target_models"},
		{"glob 合法含通配", func(s *VisionRelaySettings) { s.TargetModels = []string{"deepseek*", "qwen3-coder-*"} }, ""},
		{"glob 空项忽略", func(s *VisionRelaySettings) { s.TargetModels = []string{"", "deepseek*", " "} }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultVisionRelaySettings
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected pass, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestVisionRelayTargetModelPatterns(t *testing.T) {
	s := defaultVisionRelaySettings
	s.TargetModels = []string{"deepseek*", "qwen3-coder-*"}
	patterns, err := s.TargetModelPatterns()
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if len(patterns) != 2 {
		t.Fatalf("expected 2 patterns, got %d", len(patterns))
	}
	for _, model := range []string{"deepseek-v4-flash", "deepseek-r1", "qwen3-coder-30b"} {
		matched := false
		for _, re := range patterns {
			if re.MatchString(model) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("model %q should match allowlist", model)
		}
	}
	for _, model := range []string{"claude-sonnet-5", "gemini-3.5-flash"} {
		for _, re := range patterns {
			if re.MatchString(model) {
				t.Errorf("model %q should NOT match allowlist", model)
			}
		}
	}
}

func TestGetVisionRelaySnapshotDefaults(t *testing.T) {
	// OptionMap 未初始化（nil map 读安全）→ 返回默认配置
	snap, err := GetVisionRelaySnapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Enabled {
		t.Fatal("default should be disabled")
	}
	if len(snap.Models) == 0 || snap.Models[0] != "gemma-4-31b" {
		t.Fatalf("default models chain expected, got %v", snap.Models)
	}
	if snap.TimeoutSec != 15 {
		t.Fatalf("default timeout expected 15, got %d", snap.TimeoutSec)
	}
	if snap.BaseURL != "" {
		t.Fatalf("default base url expected empty, got %q", snap.BaseURL)
	}
	// 快照是值对象：修改不影响下次读取
	snap.Enabled = true
	snap2, _ := GetVisionRelaySnapshot()
	if snap2.Enabled {
		t.Fatal("snapshot mutation leaked")
	}
}

// 严格解析（审核 P0-2 §4）：key 存在但格式非法 → 明确错误，不静默 no-op
func TestGetVisionRelaySnapshotStrict(t *testing.T) {
	base := map[string]string{
		"vision_relay.enabled":       "true",
		"vision_relay.target_models": `["deepseek*"]`,
		"vision_relay.models":        `["gemma-4-31b"]`,
		"vision_relay.base_url":      "https://vision.example.com",
		"vision_relay.api_key":       "sk-test",
		"vision_relay.timeout_sec":   "15",
	}
	cases := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{"malformed target_models", "vision_relay.target_models", `["unclosed`, "target_models"},
		{"malformed models", "vision_relay.models", `{not-array`, "models"},
		{"timeout 非整数", "vision_relay.timeout_sec", "abc", "timeout_sec"},
		{"timeout 零", "vision_relay.timeout_sec", "0", "timeout_sec"},
		{"timeout 负", "vision_relay.timeout_sec", "-5", "timeout_sec"},
		{"base_url 无 scheme", "vision_relay.base_url", "ftp://example.com", "base_url"},
		{"base_url 无 host", "vision_relay.base_url", "http://", "base_url"},
		{"base_url 非法", "vision_relay.base_url", "://bad", "base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			common.OptionMapRWMutex.Lock()
			common.OptionMap = make(map[string]string, len(base)+1)
			for k, v := range base {
				common.OptionMap[k] = v
			}
			common.OptionMap[tc.key] = tc.value
			common.OptionMapRWMutex.Unlock()
			_, err := GetVisionRelaySnapshot()
			if err == nil {
				t.Fatalf("expected error for %q=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// enabled 键自身 malformed → 降级为 disabled（v0.2.3：残留坏配置不打全局 5xx；
// 与 disabled 语义一致 = 零行为，其余字段不解析）
func TestGetVisionRelaySnapshotMalformedEnabledDegradesToDisabled(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{
		"vision_relay.enabled":       "yes",
		"vision_relay.target_models": `["unclosed`, // 即便其他键也 malformed 也不报错
		"vision_relay.base_url":      "://bad",
	}
	common.OptionMapRWMutex.Unlock()
	snap, err := GetVisionRelaySnapshot()
	if err != nil {
		t.Fatalf("malformed enabled must degrade to disabled without error, got: %v", err)
	}
	if snap.Enabled {
		t.Fatal("snapshot must be disabled when enabled key is malformed")
	}
}

// 写时校验（controller/option.go 挂载）：格式非法在写入面拦截
func TestValidateVisionRelayWrite(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{
		"vision_relay.enabled":         "false",
		"vision_relay.target_models":   `["deepseek*"]`,
		"vision_relay.models":          `["grok-4.5"]`,
		"vision_relay.base_url":        "https://vision.example.com",
		"vision_relay.api_key":         "sk-test",
		"vision_relay.sidecall_secret": "abc123",
		"vision_relay.timeout_sec":     "15",
	}
	common.OptionMapRWMutex.Unlock()

	valid := []struct{ key, value string }{
		{"vision_relay.enabled", "false"},
		{"vision_relay.enabled", "true"},
		{"vision_relay.target_models", `["deepseek*", "qwen*"]`},
		{"vision_relay.target_models", ""},
		{"vision_relay.models", `["grok-4.5"]`},
		{"vision_relay.timeout_sec", "30"},
		{"vision_relay.base_url", "http://127.0.0.1:3000"},
		{"vision_relay.api_key", "sk-anything"}, // 自由格式键不校验
		{"vision_relay.disable_proxy_fetch", "true"},
		{"vision_relay.disable_proxy_fetch", "false"},
		{"vision_relay.disable_proxy_fetch", ""},
	}
	for _, tc := range valid {
		if err := ValidateVisionRelayWrite(tc.key, tc.value); err != nil {
			t.Errorf("ValidateVisionRelayWrite(%q, %q) unexpected error: %v", tc.key, tc.value, err)
		}
	}

	invalid := []struct{ key, value string }{
		{"vision_relay.enabled", "yes"},
		{"vision_relay.target_models", `["unclosed`},
		{"vision_relay.models", `{not-array`},
		{"vision_relay.timeout_sec", "0"},
		{"vision_relay.timeout_sec", "-1"},
		{"vision_relay.timeout_sec", "abc"},
		{"vision_relay.base_url", "ftp://example.com"},
		{"vision_relay.base_url", "://bad"},
		{"vision_relay.disable_proxy_fetch", "yes"},
	}
	for _, tc := range invalid {
		if err := ValidateVisionRelayWrite(tc.key, tc.value); err == nil {
			t.Errorf("ValidateVisionRelayWrite(%q, %q) expected error, got nil", tc.key, tc.value)
		}
	}
}

// 递归防线：enabled=true 且 sidecall_secret 为空 → 拒写（无条件要求，覆盖
// loopback 与网关自身公网域名两种自环；见 ValidateVisionRelayWrite）。
func TestValidateVisionRelayWriteEnabledRequiresSidecallSecret(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{
		"vision_relay.enabled":       "false",
		"vision_relay.target_models": `["deepseek*"]`,
		"vision_relay.models":        `["grok-4.5"]`,
		"vision_relay.base_url":      "http://127.0.0.1:3000",
		"vision_relay.api_key":       "sk-test",
		"vision_relay.timeout_sec":   "15",
	}
	common.OptionMapRWMutex.Unlock()

	if err := ValidateVisionRelayWrite("vision_relay.enabled", "true"); err == nil {
		t.Fatal("loopback without sidecall_secret must be rejected")
	}

	common.OptionMapRWMutex.Lock()
	common.OptionMap["vision_relay.sidecall_secret"] = "secret-48-hex"
	common.OptionMapRWMutex.Unlock()
	if err := ValidateVisionRelayWrite("vision_relay.enabled", "true"); err != nil {
		t.Fatalf("loopback with sidecall_secret must pass, got: %v", err)
	}
}

// 非 loopback 自环：base_url 指向网关自身公网域名，secret 为空同样必须拒写
// （旧逻辑只在 loopback 主机名时强制，会漏掉公网域名自环）。
func TestValidateVisionRelayWritePublicDomainRequiresSidecallSecret(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{
		"vision_relay.enabled":       "false",
		"vision_relay.target_models": `["deepseek*"]`,
		"vision_relay.models":        `["grok-4.5"]`,
		"vision_relay.base_url":      "https://api.example-relay.com/v1",
		"vision_relay.api_key":       "sk-test",
		"vision_relay.timeout_sec":   "15",
	}
	common.OptionMapRWMutex.Unlock()

	if err := ValidateVisionRelayWrite("vision_relay.enabled", "true"); err == nil {
		t.Fatal("non-loopback without sidecall_secret must be rejected")
	}
}

// enabled=true 但 base_url 缺失 → 默认空 → URL 解析失败（enabled 时端点必填，不硬编码默认端点）
func TestGetVisionRelaySnapshotMissingKeysUseDefaults(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{"vision_relay.enabled": "true"}
	common.OptionMapRWMutex.Unlock()
	_, err := GetVisionRelaySnapshot()
	if err == nil {
		t.Fatal("expected error when enabled=true but base_url missing (no hardcoded default endpoint)")
	}
}

// disabled + 残留 malformed 配置 → 零行为优先，不 5xx（enabled=true 才严格解析）
func TestGetVisionRelaySnapshotDisabledToleratesMalformed(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{
		"vision_relay.enabled":       "false",
		"vision_relay.target_models": `["unclosed`,
		"vision_relay.models":        `{not-array`,
		"vision_relay.base_url":      "http://",
		"vision_relay.timeout_sec":   "abc",
	}
	common.OptionMapRWMutex.Unlock()
	snap, err := GetVisionRelaySnapshot()
	if err != nil {
		t.Fatalf("disabled must tolerate malformed fields, got: %v", err)
	}
	if snap.Enabled {
		t.Fatal("must stay disabled")
	}
}

// B5：disable_proxy_fetch 字段解析（默认 false，显式 true 生效）。
func TestGetVisionRelaySnapshotParsesDisableProxyFetch(t *testing.T) {
	base := map[string]string{
		"vision_relay.enabled":             "true",
		"vision_relay.target_models":       `["deepseek*"]`,
		"vision_relay.models":              `["gemma-4-31b"]`,
		"vision_relay.base_url":            "https://vision.example.com",
		"vision_relay.api_key":             "sk-test",
		"vision_relay.sidecall_secret":     "test-sidecall-secret-123",
		"vision_relay.timeout_sec":         "15",
		"vision_relay.disable_proxy_fetch": "true",
	}
	common.OptionMapRWMutex.Lock()
	common.OptionMap = base
	common.OptionMapRWMutex.Unlock()

	snap, err := GetVisionRelaySnapshot()
	if err != nil {
		t.Fatalf("snapshot with disable_proxy_fetch must parse, got: %v", err)
	}
	if !snap.DisableProxyFetch {
		t.Fatal("disable_proxy_fetch=true must be reflected in snapshot")
	}

	// 缺省时默认 false
	delete(base, "vision_relay.disable_proxy_fetch")
	common.OptionMapRWMutex.Lock()
	common.OptionMap = base
	common.OptionMapRWMutex.Unlock()
	snap2, err := GetVisionRelaySnapshot()
	if err != nil {
		t.Fatalf("snapshot without disable_proxy_fetch must parse, got: %v", err)
	}
	if snap2.DisableProxyFetch {
		t.Fatal("disable_proxy_fetch must default to false")
	}
}
