package service

import "github.com/QuantumNous/new-api/model"

// Fork-added file: rankings vendor/logo three-tier fallback.
// 官方 service/rankings.go 只在 buildRankingModelMeta 里留一个调用点
// （applyRankingVendorFallbacks），追上游 merge 时官方文件 diff 面仅 1 行。
// 相关：model/model_vendor_fallback.go

// 厂商/logo 解析优先级（前两层为官方原生语义，后两层为 fork 兜底）：
//  1. pricing（官方）：启用渠道 + status=1 的模型，走 VendorID → vendors 表
//  2. OwnerBy（官方）：pricing 模型无有效 VendorID 时的厂商名回退
//  3. models 表（fork）：有 vendor_id 归属但当前不在 pricing 的模型
//     （无启用渠道 / 目录行被禁用），排行榜统计历史用量，应显示正确厂商
//  4. 前缀规则（fork）：models 表也无归属的历史流量模型名（已删目录行/
//     别名），按 gpt-* → OpenAI、deepseek-* → DeepSeek 等推断基础厂商
//
// 已解析出厂商的模型不会被后层覆盖；前层优先。

func applyRankingVendorFallbacks(meta map[string]rankingModelMeta, vendorByID map[int]model.PricingVendor) {
	applyRankingVendorFallbacksWith(meta, vendorByID, model.GetModelVendorMap())
}

// applyRankingVendorFallbacksWith 可注入 modelVendorMap，便于单测覆盖整体链路。
func applyRankingVendorFallbacksWith(meta map[string]rankingModelMeta, vendorByID map[int]model.PricingVendor, modelVendorMap map[string]int) {
	vendorByName := make(map[string]model.PricingVendor)
	for _, vendor := range vendorByID {
		vendorByName[vendor.Name] = vendor
	}

	augmentRankingMetaWithModelsTable(meta, vendorByID, modelVendorMap)
	augmentRankingMetaWithNameRules(meta, vendorByName)
}

// augmentRankingMetaWithModelsTable 第三层：models 目录里挂了 vendor_id 的
// 模型补全 meta。pricing 里已有的模型不覆盖。
func augmentRankingMetaWithModelsTable(meta map[string]rankingModelMeta, vendorByID map[int]model.PricingVendor, modelVendorMap map[string]int) {
	for modelName, vendorID := range modelVendorMap {
		if _, exists := meta[modelName]; exists {
			continue
		}
		if vendor, ok := vendorByID[vendorID]; ok {
			meta[modelName] = rankingModelMeta{vendor: vendor.Name, vendorIcon: vendor.Icon}
		}
	}
}

// augmentRankingMetaWithNameRules 第四层：models 表也无归属时按模型名前缀
// 规则推断基础厂商。已解析出厂商的模型不覆盖。
func augmentRankingMetaWithNameRules(meta map[string]rankingModelMeta, vendorByName map[string]model.PricingVendor) {
	for modelName, item := range meta {
		if item.vendor != "" && item.vendor != rankingUnknownVendor {
			continue
		}
		vendorName, defaultIcon := model.GetDefaultVendorForModel(modelName)
		if vendorName == "" {
			continue
		}
		icon := defaultIcon
		if vendor, ok := vendorByName[vendorName]; ok && vendor.Icon != "" {
			icon = vendor.Icon
		}
		meta[modelName] = rankingModelMeta{vendor: vendorName, vendorIcon: icon}
	}
}
