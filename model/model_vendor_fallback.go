package model

import "strings"

// 本文件是 fork 二开（rankings 厂商/logo 兜底）的独立承载文件：
// 不修改任何官方文件，追上游时此文件不参与官方 merge。
// 相关：service/rankings_vendor_fallback.go

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
	for pattern, vendor := range defaultVendorRules {
		if strings.Contains(modelLower, pattern) {
			return vendor, getDefaultVendorIcon(vendor)
		}
	}
	return "", ""
}
