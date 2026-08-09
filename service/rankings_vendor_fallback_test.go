package service

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
)

// 本测试文件是 fork 二开（rankings 厂商/logo 三级兜底）的独立承载文件，
// 与 service/rankings_vendor_fallback.go 一一对应，追上游时不参与官方 merge。

// TestApplyRankingVendorFallbacks 端到端验证三层兜底整体行为：
// pricing 优先、models 表补缺、前缀规则最后兜底、已解析不覆盖。
func TestApplyRankingVendorFallbacks(t *testing.T) {
	vendorByID := map[int]model.PricingVendor{
		3:  {ID: 3, Name: "OpenAI", Icon: "OpenAI"},
		10: {ID: 10, Name: "DeepSeek", Icon: "DeepSeek.Color"},
		65: {ID: 65, Name: "StepFun", Icon: "Stepfun"},
		44: {ID: 44, Name: "xAI", Icon: "XAI"},
	}

	// meta 模拟 buildRankingModelMeta 的 pricing 层结果
	meta := map[string]rankingModelMeta{
		"gpt-5.6-sol":     {vendor: "OpenAI", vendorIcon: "OpenAI"}, // pricing 已有
		"deepseek-v4-pro": {vendor: "DeepSeek", vendorIcon: "DeepSeek.Color"},
		"gpt-5.6-terra":   {vendor: rankingUnknownVendor}, // 无 pricing，靠 models 表
		"deepseek-v3.2":   {vendor: rankingUnknownVendor}, // 无 pricing，靠 models 表
		"qwen3.7-max":     {vendor: rankingUnknownVendor}, // models 表无 vendor_id，靠前缀规则
	}

	// 模拟 model.GetModelVendorMap()：有 vendor_id 的目录行
	// （qwen3.7-max 故意不在其中 → 走前缀规则层）
	modelVendorMap := map[string]int{
		"gpt-5.6-terra": 3,
		"deepseek-v3.2": 10,
	}

	applyRankingVendorFallbacksWith(meta, vendorByID, modelVendorMap)

	// pricing 层不被动
	assert.Equal(t, rankingModelMeta{vendor: "OpenAI", vendorIcon: "OpenAI"}, meta["gpt-5.6-sol"])
	assert.Equal(t, rankingModelMeta{vendor: "DeepSeek", vendorIcon: "DeepSeek.Color"}, meta["deepseek-v4-pro"])

	// models 表层补全
	assert.Equal(t, rankingModelMeta{vendor: "OpenAI", vendorIcon: "OpenAI"}, meta["gpt-5.6-terra"])
	assert.Equal(t, rankingModelMeta{vendor: "DeepSeek", vendorIcon: "DeepSeek.Color"}, meta["deepseek-v3.2"])

	// 前缀规则层兜底（qwen* → 阿里巴巴，vendors 表无 阿里巴巴 → 用默认 icon Qwen.Color）
	assert.Equal(t, rankingModelMeta{vendor: "阿里巴巴", vendorIcon: "Qwen.Color"}, meta["qwen3.7-max"])
}

// TestAugmentRankingMetaWithModelsTable covers the models-table fallback:
// models that have historical traffic but are currently missing from the
// pricing cache should still resolve to their vendor from the models table.
func TestAugmentRankingMetaWithModelsTable(t *testing.T) {
	vendors := map[int]model.PricingVendor{
		3:  {ID: 3, Name: "OpenAI", Icon: "OpenAI"},
		10: {ID: 10, Name: "DeepSeek", Icon: "DeepSeek.Color"},
		65: {ID: 65, Name: "StepFun", Icon: "Stepfun"},
	}

	t.Run("pricing meta wins over fallback", func(t *testing.T) {
		meta := map[string]rankingModelMeta{
			"gpt-5.6-sol": {vendor: "OpenAI", vendorIcon: "OpenAI"},
		}
		augmentRankingMetaWithModelsTable(meta, vendors, map[string]int{
			"gpt-5.6-sol": 3,
		})
		assert.Equal(t, rankingModelMeta{vendor: "OpenAI", vendorIcon: "OpenAI"}, meta["gpt-5.6-sol"])
	})

	t.Run("models table fills models missing from pricing", func(t *testing.T) {
		meta := map[string]rankingModelMeta{}
		augmentRankingMetaWithModelsTable(meta, vendors, map[string]int{
			"deepseek-v4-flash": 10,
			"step-3.7-flash":    65,
		})
		assert.Equal(t, rankingModelMeta{vendor: "DeepSeek", vendorIcon: "DeepSeek.Color"}, meta["deepseek-v4-flash"])
		assert.Equal(t, rankingModelMeta{vendor: "StepFun", vendorIcon: "Stepfun"}, meta["step-3.7-flash"])
	})

	t.Run("unknown vendor id stays absent", func(t *testing.T) {
		meta := map[string]rankingModelMeta{}
		augmentRankingMetaWithModelsTable(meta, vendors, map[string]int{
			"mystery-model": 999,
		})
		_, exists := meta["mystery-model"]
		assert.False(t, exists, "model with dangling vendor_id must not be added")
	})

	t.Run("modelMeta returns Unknown when no meta present", func(t *testing.T) {
		got := modelMeta("mystery-model", map[string]rankingModelMeta{})
		assert.Equal(t, rankingModelMeta{vendor: rankingUnknownVendor}, got)
	})
}

func TestAugmentRankingMetaWithNameRules(t *testing.T) {
	vendorByName := map[string]model.PricingVendor{
		"OpenAI":   {ID: 3, Name: "OpenAI", Icon: "OpenAI"},
		"DeepSeek": {ID: 10, Name: "DeepSeek", Icon: "DeepSeek.Color"},
		"阿里巴巴":     {ID: 4, Name: "阿里巴巴", Icon: "Qwen.Color"},
		"智谱":       {ID: 7, Name: "智谱", Icon: "Zhipu.Color"},
		"xAI":      {ID: 44, Name: "xAI", Icon: "XAI"},
	}

	t.Run("prefix rules fill models with unknown vendor", func(t *testing.T) {
		meta := map[string]rankingModelMeta{
			"gpt-5.6-terra":    {vendor: rankingUnknownVendor},
			"deepseek-v3.2":    {vendor: rankingUnknownVendor},
			"qwen3.7-max":      {vendor: rankingUnknownVendor},
			"grok-4.5-console": {vendor: rankingUnknownVendor},
			"glm-5-turbo":      {vendor: rankingUnknownVendor},
		}
		augmentRankingMetaWithNameRules(meta, vendorByName)

		assert.Equal(t, rankingModelMeta{vendor: "OpenAI", vendorIcon: "OpenAI"}, meta["gpt-5.6-terra"])
		assert.Equal(t, rankingModelMeta{vendor: "DeepSeek", vendorIcon: "DeepSeek.Color"}, meta["deepseek-v3.2"])
		assert.Equal(t, rankingModelMeta{vendor: "阿里巴巴", vendorIcon: "Qwen.Color"}, meta["qwen3.7-max"])
		assert.Equal(t, rankingModelMeta{vendor: "xAI", vendorIcon: "XAI"}, meta["grok-4.5-console"])
		assert.Equal(t, rankingModelMeta{vendor: "智谱", vendorIcon: "Zhipu.Color"}, meta["glm-5-turbo"])
	})

	t.Run("already-resolved vendors are not overwritten", func(t *testing.T) {
		meta := map[string]rankingModelMeta{
			"gpt-5.5": {vendor: "OpenAI", vendorIcon: "OpenAI"},
		}
		augmentRankingMetaWithNameRules(meta, vendorByName)
		assert.Equal(t, rankingModelMeta{vendor: "OpenAI", vendorIcon: "OpenAI"}, meta["gpt-5.5"])
	})

	t.Run("unmatched model name stays unknown", func(t *testing.T) {
		meta := map[string]rankingModelMeta{
			"mystery-model": {vendor: rankingUnknownVendor},
		}
		augmentRankingMetaWithNameRules(meta, vendorByName)
		assert.Equal(t, rankingModelMeta{vendor: rankingUnknownVendor}, meta["mystery-model"])
	})

	t.Run("default icon used when vendor not in vendors list", func(t *testing.T) {
		meta := map[string]rankingModelMeta{
			"vidu-any": {vendor: rankingUnknownVendor},
		}
		augmentRankingMetaWithNameRules(meta, map[string]model.PricingVendor{})
		assert.Equal(t, rankingModelMeta{vendor: "Vidu", vendorIcon: "Vidu"}, meta["vidu-any"])
	})
}
