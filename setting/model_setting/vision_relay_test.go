package model_setting

import (
	"strings"
	"testing"
)

func TestVisionRelayValidate(t *testing.T) {
	valid := defaultVisionRelaySettings
	valid.Enabled = true
	valid.APIKey = "sk-test"
	valid.TargetModels = []string{"deepseek*"}

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
	if snap.BaseURL != "https://api.tokendancelab.com" {
		t.Fatalf("default base url expected, got %q", snap.BaseURL)
	}
	// 快照是值对象：修改不影响下次读取
	snap.Enabled = true
	snap2, _ := GetVisionRelaySnapshot()
	if snap2.Enabled {
		t.Fatal("snapshot mutation leaked")
	}
}
