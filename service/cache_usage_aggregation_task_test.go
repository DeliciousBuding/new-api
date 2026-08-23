package service

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupCacheUsageAggregationTaskTestDB 初始化主库（聚合表/水位表/任务表）与
// 日志库（logs 表）；返回恢复函数。skipLogs 用于模拟日志库故障场景。
func setupCacheUsageAggregationTaskTestDB(t *testing.T, skipLogs bool) func() {
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()

	mainDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, mainDB.AutoMigrate(&model.TokenCacheUsageHourly{}, &model.CacheUsageAggregationMeta{}, &model.SystemTask{}, &model.SystemTaskLock{}))
	model.DB = mainDB

	logDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	if !skipLogs {
		require.NoError(t, logDB.AutoMigrate(&model.Log{}))
	}
	model.LOG_DB = logDB

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)

	return func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
	}
}

// insertConsumeLogForTask 向日志库插入一条 type=2 消费日志。
func insertConsumeLogForTask(t *testing.T, tokenId int64, createdAt int64, promptTokens int64, otherJSON string) {
	t.Helper()
	require.NoError(t, model.LOG_DB.Create(&model.Log{
		UserId:       1,
		Type:         model.LogTypeConsume,
		TokenId:      int(tokenId),
		CreatedAt:    createdAt,
		PromptTokens: int(promptTokens),
		Other:        otherJSON,
	}).Error)
}

func TestRunCacheUsageAggregationIncremental(t *testing.T) {
	cleanup := setupCacheUsageAggregationTaskTestDB(t, false)
	defer cleanup()

	originalPacing := cacheUsageAggregationBatchPacing
	cacheUsageAggregationBatchPacing = 0
	defer func() { cacheUsageAggregationBatchPacing = originalPacing }()

	nowHour := common.GetTimestamp() / 3600
	require.NoError(t, model.SaveCacheUsageAggregationMeta(&model.CacheUsageAggregationMeta{
		CoveredFromHour: nowHour - 5,
		ReadyHour:       nowHour - 3, // 待增量 [nowHour-2, nowHour-1] 两个整小时
	}))
	// 增量区间内的数据（nowHour-1 小时）
	insertConsumeLogForTask(t, 7, (nowHour-1)*3600+10, 400, `{"cache_tokens":40}`)
	// 已聚合区间外的数据（nowHour-4 小时），不应重复聚合
	insertConsumeLogForTask(t, 7, (nowHour-4)*3600+10, 999, `{"cache_tokens":99}`)

	_, err := RunCacheUsageAggregationTask(context.Background(), &model.SystemTask{TaskID: "systask_test"}, "test-runner")
	require.NoError(t, err)

	meta, err := model.GetCacheUsageAggregationMeta()
	require.NoError(t, err)
	assert.Equal(t, nowHour-5, meta.CoveredFromHour)
	assert.Equal(t, nowHour-1, meta.ReadyHour) // 推进到上一个整小时
	assert.NotZero(t, meta.LastRunAt)

	stats, err := model.SumCacheUsageHourly([]int64{7}, nowHour-3, nowHour-1)
	require.NoError(t, err)
	require.Len(t, stats, 1)
	assert.Equal(t, int64(400), stats[7].PromptTokens)
	assert.Equal(t, int64(40), stats[7].CacheReadTokens)
}

func TestRunCacheUsageAggregationResumesBackfill(t *testing.T) {
	cleanup := setupCacheUsageAggregationTaskTestDB(t, false)
	defer cleanup()

	originalPacing := cacheUsageAggregationBatchPacing
	cacheUsageAggregationBatchPacing = 0
	defer func() { cacheUsageAggregationBatchPacing = originalPacing }()

	nowHour := common.GetTimestamp() / 3600
	// 回填中断点：覆盖下界已落、ReadyHour=0（如进程在回填中途崩溃）
	require.NoError(t, model.SaveCacheUsageAggregationMeta(&model.CacheUsageAggregationMeta{
		CoveredFromHour: nowHour - 3,
		ReadyHour:       0,
	}))

	result, err := RunCacheUsageAggregationTask(context.Background(), &model.SystemTask{TaskID: "systask_test"}, "test-runner")
	require.NoError(t, err)
	// 从覆盖下界续跑 3 个整小时
	assert.Equal(t, int64(3), result.AggregatedHours)

	meta, err := model.GetCacheUsageAggregationMeta()
	require.NoError(t, err)
	assert.Equal(t, nowHour-3, meta.CoveredFromHour)
	assert.Equal(t, nowHour-1, meta.ReadyHour)
}

func TestRunCacheUsageAggregationFirstEnable(t *testing.T) {
	cleanup := setupCacheUsageAggregationTaskTestDB(t, false)
	defer cleanup()

	originalPacing := cacheUsageAggregationBatchPacing
	cacheUsageAggregationBatchPacing = 0
	defer func() { cacheUsageAggregationBatchPacing = originalPacing }()

	nowHour := common.GetTimestamp() / 3600
	result, err := RunCacheUsageAggregationTask(context.Background(), &model.SystemTask{TaskID: "systask_test"}, "test-runner")
	require.NoError(t, err)

	// 首次启用：覆盖下界 = 30 天前，回填全部整小时
	expectedCover := nowHour - model.CacheUsageAggregationCoverDays*24
	assert.Equal(t, expectedCover, result.CoveredFromHour)
	assert.Equal(t, (nowHour-1)-expectedCover+1, result.AggregatedHours)
	assert.Equal(t, nowHour-1, result.ReadyHour)

	meta, err := model.GetCacheUsageAggregationMeta()
	require.NoError(t, err)
	assert.Equal(t, expectedCover, meta.CoveredFromHour)
	assert.Equal(t, nowHour-1, meta.ReadyHour)
}

func TestRunCacheUsageAggregationFailureKeepsWatermark(t *testing.T) {
	cleanup := setupCacheUsageAggregationTaskTestDB(t, true) // 不建 logs 表 → 聚合必然失败
	defer cleanup()

	originalPacing := cacheUsageAggregationBatchPacing
	cacheUsageAggregationBatchPacing = 0
	defer func() { cacheUsageAggregationBatchPacing = originalPacing }()

	nowHour := common.GetTimestamp() / 3600
	require.NoError(t, model.SaveCacheUsageAggregationMeta(&model.CacheUsageAggregationMeta{
		CoveredFromHour: nowHour - 5,
		ReadyHour:       nowHour - 3,
	}))

	_, err := RunCacheUsageAggregationTask(context.Background(), &model.SystemTask{TaskID: "systask_test"}, "test-runner")
	require.Error(t, err)

	// 失败不推水位：ReadyHour 保持原值
	meta, err := model.GetCacheUsageAggregationMeta()
	require.NoError(t, err)
	assert.Equal(t, nowHour-3, meta.ReadyHour)
}

func TestRunCacheUsageAggregationNoPendingHours(t *testing.T) {
	cleanup := setupCacheUsageAggregationTaskTestDB(t, false)
	defer cleanup()

	originalPacing := cacheUsageAggregationBatchPacing
	cacheUsageAggregationBatchPacing = 0
	defer func() { cacheUsageAggregationBatchPacing = originalPacing }()

	nowHour := common.GetTimestamp() / 3600
	require.NoError(t, model.SaveCacheUsageAggregationMeta(&model.CacheUsageAggregationMeta{
		CoveredFromHour: nowHour - 10,
		ReadyHour:       nowHour - 1, // 已追平
	}))

	result, err := RunCacheUsageAggregationTask(context.Background(), &model.SystemTask{TaskID: "systask_test"}, "test-runner")
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.AggregatedHours)
	assert.Equal(t, nowHour-1, result.ReadyHour)

	// 保留清理：90 天前的旧行被删除
	require.NoError(t, model.UpsertCacheUsageHourly([]model.TokenCacheUsageHourly{
		{TokenId: 1, HourBucket: nowHour - model.CacheUsageAggregationRetentionHours - 1, PromptTokens: 1},
		{TokenId: 2, HourBucket: nowHour - 10, PromptTokens: 2},
	}))
	_, err = RunCacheUsageAggregationTask(context.Background(), &model.SystemTask{TaskID: "systask_test"}, "test-runner")
	require.NoError(t, err)

	var count int64
	require.NoError(t, model.DB.Model(&model.TokenCacheUsageHourly{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)

	var remaining model.TokenCacheUsageHourly
	require.NoError(t, model.DB.First(&remaining).Error)
	assert.Equal(t, int64(2), remaining.TokenId)
}
