package model_setting

import (
	"fmt"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

// CacheUsageAggregationSettings 定义 Cache 用量预聚合（Keys 页 Cache Rate 与
// dashboard 趋势的加速层）配置。注册名 "cache_usage_aggregation" → DB option
// keys 形如 cache_usage_aggregation.enabled。默认关闭 = 零行为；任务与查询
// 路径均读快照，热更新无重启。
type CacheUsageAggregationSettings struct {
	Enabled         bool `json:"enabled"`          // 总开关，默认关闭
	IntervalMinutes int  `json:"interval_minutes"` // 增量任务间隔（分钟），钳制 [5,60]，默认 15
}

// 默认配置
var defaultCacheUsageAggregationSettings = CacheUsageAggregationSettings{
	Enabled:         false,
	IntervalMinutes: 15,
}

// 全局实例（配置注册/默认导出对象；运行时快照从 OptionMap 读取，见
// GetCacheUsageAggregationSnapshot——与 vision_relay 同款 data-race 安全模式）
var cacheUsageAggregationSettings = defaultCacheUsageAggregationSettings

func init() {
	config.GlobalConfig.Register("cache_usage_aggregation", &cacheUsageAggregationSettings)
}

// GetCacheUsageAggregationSnapshot 从 common.OptionMap 读取运行时快照。
// 缺失 key → 默认值；enabled 非法 → 降级关闭（disabled 语义 = 零行为，
// 残留坏配置不把查询/任务打进异常态）；interval 非法 → 默认值，越界 → 钳制。
func GetCacheUsageAggregationSnapshot() CacheUsageAggregationSettings {
	common.OptionMapRWMutex.RLock()
	enabledRaw := common.OptionMap["cache_usage_aggregation.enabled"]
	intervalRaw := common.OptionMap["cache_usage_aggregation.interval_minutes"]
	common.OptionMapRWMutex.RUnlock()

	snap := defaultCacheUsageAggregationSettings
	if enabledRaw != "" {
		if v, err := strconv.ParseBool(enabledRaw); err == nil {
			snap.Enabled = v
		}
	}
	if intervalRaw != "" {
		if v, err := strconv.Atoi(intervalRaw); err == nil {
			snap.IntervalMinutes = clampCacheUsageAggregationInterval(v)
		}
	}
	return snap
}

func clampCacheUsageAggregationInterval(v int) int {
	if v < 5 {
		return 5
	}
	if v > 60 {
		return 60
	}
	return v
}

// ValidateCacheUsageAggregationWrite 是 option 写侧校验（controller/option.go
// switch case 调用）：写入库前拦截非法值，读侧钳制只兜「改库绕过」的极端情况。
func ValidateCacheUsageAggregationWrite(key, value string) error {
	switch key {
	case "cache_usage_aggregation.enabled":
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("cache_usage_aggregation.enabled: %w", err)
		}
	case "cache_usage_aggregation.interval_minutes":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("cache_usage_aggregation.interval_minutes: %w", err)
		}
		if clampCacheUsageAggregationInterval(v) != v {
			return fmt.Errorf("cache_usage_aggregation.interval_minutes: 必须是 [5,60] 的整数")
		}
	}
	return nil
}
