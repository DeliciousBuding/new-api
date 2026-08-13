package model

import (
	"sort"
	"strings"
)

// Fork-added file: rankings vendor/logo fallback. Not modified by upstream merges.
// See also: service/rankings_vendor_fallback.go

// GetModelVendorMap 返回非软删且挂了 vendor_id 的模型目录行（model_name → vendor_id）。
// 排行榜用其作为第二层兜底数据源：模型有历史流量但当前不在 pricing
// （无启用渠道 / 目录行被禁用）时，仍能从 models 表拿到厂商归属。
// 排行榜读路径保持轻量：单次索引扫描，无 join。
func GetModelVendorMap() map[string]int {
	result := make(map[string]int)
	var rows []struct {
		ModelName string
		VendorID  int
	}
	if err := DB.Model(&Model{}).
		Where("vendor_id > 0").
		Select("model_name, vendor_id").
		Scan(&rows).Error; err != nil {
		return result
	}
	for _, r := range rows {
		if _, exists := result[r.ModelName]; !exists {
			result[r.ModelName] = r.VendorID
		}
	}
	return result
}

// GetDefaultVendorForModel 按模型名前缀规则推断基础厂商（gpt-* → OpenAI、
// deepseek-* → DeepSeek、qwen* → 阿里巴巴…）。排行榜第三层兜底：models 表
// 也无 vendor 归属的历史流量模型名（已删目录行/别名）也能显示基础厂商。
// 规则表复用官方 pricing_default.go 的 defaultVendorRules/defaultVendorIcons。
// 返回空串表示无规则命中。
func GetDefaultVendorForModel(modelName string) (vendorName string, icon string) {
	modelLower := strings.ToLower(modelName)
	for _, pattern := range sortedVendorRuleKeys() {
		if strings.Contains(modelLower, pattern) {
			vendor := defaultVendorRules[pattern]
			return vendor, getDefaultVendorIcon(vendor)
		}
	}
	return "", ""
}

// sortedVendorRuleKeys 返回 defaultVendorRules 的 key，按确定顺序排序：
// 先按 key 长度降序（更具体的模式优先），再按字典序（完全确定性）。
// 官方 defaultVendorRules 是 map，直接 `range` 迭代顺序随机，会导致跨厂商
// 重叠的模型名（如 "360gpt2-o" 同时含 "360" 与 "gpt"、"@cf/meta/llama-*"
// 同时含 "@cf/" 与 "llama"）在不同快照间随机归属不同厂商，排行榜厂商列
// 与历史聚合随之抖动。排序后归属稳定、可复现。
func sortedVendorRuleKeys() []string {
	keys := make([]string, 0, len(defaultVendorRules))
	for k := range defaultVendorRules {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}
