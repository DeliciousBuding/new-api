package model_setting

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setCacheUsageAggregationOption 在锁内写入/删除 OptionMap 键（快照读取走
// OptionMapRWMutex，测试写入必须同锁）。
func setCacheUsageAggregationOption(t *testing.T, key string, value string) {
	t.Helper()
	common.OptionMapRWMutex.Lock()
	defer common.OptionMapRWMutex.Unlock()
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	if value == "" {
		delete(common.OptionMap, key)
		return
	}
	common.OptionMap[key] = value
}

func TestCacheUsageAggregationSnapshot(t *testing.T) {
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		delete(common.OptionMap, "cache_usage_aggregation.enabled")
		delete(common.OptionMap, "cache_usage_aggregation.interval_minutes")
		common.OptionMapRWMutex.Unlock()
	})

	t.Run("missing keys use defaults", func(t *testing.T) {
		snap := GetCacheUsageAggregationSnapshot()
		assert.False(t, snap.Enabled)
		assert.Equal(t, 15, snap.IntervalMinutes)
	})

	t.Run("enabled true and custom interval", func(t *testing.T) {
		setCacheUsageAggregationOption(t, "cache_usage_aggregation.enabled", "true")
		setCacheUsageAggregationOption(t, "cache_usage_aggregation.interval_minutes", "30")
		snap := GetCacheUsageAggregationSnapshot()
		assert.True(t, snap.Enabled)
		assert.Equal(t, 30, snap.IntervalMinutes)
	})

	t.Run("malformed enabled degrades to disabled", func(t *testing.T) {
		setCacheUsageAggregationOption(t, "cache_usage_aggregation.enabled", "not-a-bool")
		snap := GetCacheUsageAggregationSnapshot()
		assert.False(t, snap.Enabled)
	})

	t.Run("out-of-range interval clamps", func(t *testing.T) {
		setCacheUsageAggregationOption(t, "cache_usage_aggregation.interval_minutes", "1")
		snap := GetCacheUsageAggregationSnapshot()
		assert.Equal(t, 5, snap.IntervalMinutes)

		setCacheUsageAggregationOption(t, "cache_usage_aggregation.interval_minutes", "999")
		snap = GetCacheUsageAggregationSnapshot()
		assert.Equal(t, 60, snap.IntervalMinutes)
	})

	t.Run("malformed interval falls back to default", func(t *testing.T) {
		setCacheUsageAggregationOption(t, "cache_usage_aggregation.interval_minutes", "abc")
		snap := GetCacheUsageAggregationSnapshot()
		assert.Equal(t, 15, snap.IntervalMinutes)
	})
}

func TestValidateCacheUsageAggregationWrite(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{name: "enabled true ok", key: "cache_usage_aggregation.enabled", value: "true", wantErr: false},
		{name: "enabled false ok", key: "cache_usage_aggregation.enabled", value: "false", wantErr: false},
		{name: "enabled malformed", key: "cache_usage_aggregation.enabled", value: "yes", wantErr: true},
		{name: "interval boundary low ok", key: "cache_usage_aggregation.interval_minutes", value: "5", wantErr: false},
		{name: "interval boundary high ok", key: "cache_usage_aggregation.interval_minutes", value: "60", wantErr: false},
		{name: "interval below range", key: "cache_usage_aggregation.interval_minutes", value: "4", wantErr: true},
		{name: "interval above range", key: "cache_usage_aggregation.interval_minutes", value: "61", wantErr: true},
		{name: "interval not a number", key: "cache_usage_aggregation.interval_minutes", value: "ten", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCacheUsageAggregationWrite(tt.key, tt.value)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
