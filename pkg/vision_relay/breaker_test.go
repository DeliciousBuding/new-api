package vision_relay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrimaryBreakerOpensAndSkipsPrimary(t *testing.T) {
	cfg := Config{Models: []string{"gemma", "deepseek"}}
	b := newPrimaryBreaker(2, 4)

	// 熔断前：完整链，包含主模型
	got, includePrimary := b.decide(cfg)
	assert.True(t, includePrimary)
	assert.Equal(t, []string{"gemma", "deepseek"}, got.Models)

	// 连续两次主模型瞬态失败（回退后失败或回退后成功）→ 打开熔断
	b.observe("gemma", DescribeResult{Enum: EnumServiceUnavailable, Fallbacks: 1}, true)
	b.observe("gemma", DescribeResult{Enum: "", Model: "deepseek", Fallbacks: 1}, true)

	got, includePrimary = b.decide(cfg)
	assert.False(t, includePrimary)
	require.Len(t, got.Models, 1)
	assert.Equal(t, "deepseek", got.Models[0])
}

func TestPrimaryBreakerResetsOnPrimarySuccess(t *testing.T) {
	cfg := Config{Models: []string{"gemma", "deepseek"}}
	b := newPrimaryBreaker(2, 4)

	b.observe("gemma", DescribeResult{Enum: EnumServiceUnavailable, Fallbacks: 1}, true)
	b.observe("gemma", DescribeResult{Enum: EnumServiceUnavailable, Fallbacks: 1}, true)

	_, includePrimary := b.decide(cfg)
	assert.False(t, includePrimary)

	// 主模型成功 → 复位
	b.observe("gemma", DescribeResult{Enum: "", Model: "gemma"}, true)
	got, includePrimary := b.decide(cfg)
	assert.True(t, includePrimary)
	assert.Equal(t, []string{"gemma", "deepseek"}, got.Models)
}

func TestPrimaryBreakerProbeRecovery(t *testing.T) {
	cfg := Config{Models: []string{"gemma", "deepseek"}}
	b := newPrimaryBreaker(1, 3)

	// 一次失败即打开
	b.observe("gemma", DescribeResult{Enum: EnumServiceUnavailable, Fallbacks: 1}, true)

	// 前 probeEvery-1 张跳过主模型
	for i := 0; i < 2; i++ {
		_, includePrimary := b.decide(cfg)
		assert.False(t, includePrimary)
	}

	// 第 probeEvery 张放行探测
	_, includePrimary := b.decide(cfg)
	assert.True(t, includePrimary)

	// 探测图主模型成功 → 复位
	b.observe("gemma", DescribeResult{Enum: "", Model: "gemma"}, true)
	got, includePrimary := b.decide(cfg)
	assert.True(t, includePrimary)
	assert.Equal(t, []string{"gemma", "deepseek"}, got.Models)
}

func TestPrimaryBreakerIgnoresTerminalFailures(t *testing.T) {
	cfg := Config{Models: []string{"gemma", "deepseek"}}
	b := newPrimaryBreaker(1, 4)

	// 非回退型失败（如 blocked/size_limit/auth/unsupported）不计入主模型失败
	b.observe("gemma", DescribeResult{Enum: EnumBlocked, Fallbacks: 0}, true)
	b.observe("gemma", DescribeResult{Enum: EnumSizeLimit, Fallbacks: 0}, true)

	_, includePrimary := b.decide(cfg)
	assert.True(t, includePrimary)
}

func TestPrimaryBreakerSingleModelNoop(t *testing.T) {
	cfg := Config{Models: []string{"only"}}
	b := newPrimaryBreaker(1, 1)

	got, includePrimary := b.decide(cfg)
	assert.True(t, includePrimary)
	assert.Equal(t, []string{"only"}, got.Models)
}

func TestNormalizeModels(t *testing.T) {
	assert.Equal(t, []string{"gemma", "deepseek"}, normalizeModels([]string{" gemma ", "", "deepseek"}))
	assert.Equal(t, []string{"gemma"}, normalizeModels([]string{"gemma", "  "}))
	assert.Empty(t, normalizeModels(nil))
	assert.Empty(t, normalizeModels([]string{"", "   "}))
}

func TestPrimaryBreakerSkippedCallDoesNotReset(t *testing.T) {
	cfg := Config{Models: []string{"gemma", "deepseek"}}
	b := newPrimaryBreaker(1, 4)

	// 打开熔断
	b.observe("gemma", DescribeResult{Enum: EnumServiceUnavailable, Fallbacks: 1}, true)
	_, includePrimary := b.decide(cfg)
	assert.False(t, includePrimary)

	// 跳过主模型的调用即使恰好返回主模型名（重复/别名配置），也不能复位熔断
	b.observe("gemma", DescribeResult{Enum: "", Model: "gemma"}, false)
	_, includePrimary = b.decide(cfg)
	assert.False(t, includePrimary)
}

func TestPrimaryBreakerFailedProbeStaysOpen(t *testing.T) {
	cfg := Config{Models: []string{"gemma", "deepseek"}}
	b := newPrimaryBreaker(1, 3)

	b.observe("gemma", DescribeResult{Enum: EnumServiceUnavailable, Fallbacks: 1}, true)
	_, includePrimary := b.decide(cfg)
	assert.False(t, includePrimary)

	// 探测图再次失败：保持熔断，且失败计数封顶不无界增长
	for i := 0; i < 20; i++ {
		b.observe("gemma", DescribeResult{Enum: EnumServiceUnavailable, Fallbacks: 1}, true)
	}
	_, includePrimary = b.decide(cfg)
	assert.False(t, includePrimary)
}
