package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCacheHourBucketExprPerDatabase(t *testing.T) {
	t.Cleanup(func() {
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	})

	// MySQL 的 `/` 返回 decimal，必须用 DIV 才得到整数小时桶
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeMySQL)
	assert.Equal(t, "(created_at DIV 3600)", cacheHourBucketExpr())

	// ClickHouse 的 `/` 是浮点除法，必须用 intDiv
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeClickHouse)
	assert.Equal(t, "intDiv(created_at, 3600)", cacheHourBucketExpr())

	// PostgreSQL / SQLite 的 int/int 即为整除
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypePostgreSQL)
	assert.Equal(t, "(created_at / 3600)", cacheHourBucketExpr())

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	assert.Equal(t, "(created_at / 3600)", cacheHourBucketExpr())
}

// setupCacheUsageAggregationTestDB 用 SQLite 内存库初始化 DB 与 LOG_DB 并迁移
// 聚合表、水位表与 logs 表；返回恢复函数。
func setupCacheUsageAggregationTestDB(t *testing.T) func() {
	previousDB := DB
	previousLogDB := LOG_DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()

	mainDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, mainDB.AutoMigrate(&TokenCacheUsageHourly{}, &CacheUsageAggregationMeta{}))
	DB = mainDB

	logDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, logDB.AutoMigrate(&Log{}))
	LOG_DB = logDB

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)

	return func() {
		DB = previousDB
		LOG_DB = previousLogDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
	}
}

// insertConsumeLog 插入一条 type=2 消费日志。otherJSON 为 other 字段的原始 JSON。
func insertConsumeLog(t *testing.T, db *gorm.DB, tokenId int64, createdAt int64, promptTokens int64, otherJSON string) {
	t.Helper()
	require.NoError(t, db.Create(&Log{
		UserId:       1,
		Type:         LogTypeConsume,
		TokenId:      int(tokenId),
		CreatedAt:    createdAt,
		PromptTokens: int(promptTokens),
		Other:        otherJSON,
	}).Error)
}

func TestAggregateCacheUsageHour(t *testing.T) {
	cleanup := setupCacheUsageAggregationTestDB(t)
	defer cleanup()

	const hour = int64(2000) // 任意整小时桶
	// token 1，hour：一行 OpenAI 语义（input 回退 prompt）+ 一行 Anthropic 语义
	insertConsumeLog(t, LOG_DB, 1, hour*3600+10, 1000, `{"cache_tokens":100,"cache_creation_tokens":50}`)
	insertConsumeLog(t, LOG_DB, 1, hour*3600+20, 500, `{"usage_semantic":"anthropic","cache_tokens":200,"cache_creation_tokens":80}`)
	// token 2，hour：一行
	insertConsumeLog(t, LOG_DB, 2, hour*3600+30, 200, `{"cache_tokens":10}`)
	// token 1，hour+1：一行，验证桶分离
	insertConsumeLog(t, LOG_DB, 1, (hour+1)*3600+5, 300, `{}`)
	// 无关日志：type=3 管理日志、区间外时间戳
	require.NoError(t, LOG_DB.Create(&Log{UserId: 1, Type: LogTypeManage, TokenId: 1, CreatedAt: hour * 3600, PromptTokens: 999}).Error)
	insertConsumeLog(t, LOG_DB, 1, (hour-1)*3600+59, 777, `{"cache_tokens":1}`)
	insertConsumeLog(t, LOG_DB, 1, (hour+1)*3600+3599+1, 888, `{"cache_tokens":2}`)

	rows, err := AggregateCacheUsageHour(t.Context(), hour, hour+1)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	byKey := map[[2]int64]TokenCacheUsageHourly{}
	for _, r := range rows {
		byKey[[2]int64{r.TokenId, r.HourBucket}] = r
	}

	token1Hour := byKey[[2]int64{1, hour}]
	assert.Equal(t, int64(1500), token1Hour.PromptTokens)
	// Anthropic 行：input = prompt + cache_tokens + cache_creation_tokens = 500+200+80
	// OpenAI 行：无 input_tokens_total → 回退 prompt_tokens = 1000
	assert.Equal(t, int64(1780), token1Hour.InputTokens)
	assert.Equal(t, int64(300), token1Hour.CacheReadTokens)
	assert.Equal(t, int64(130), token1Hour.CacheCreationTokens)

	token2Hour := byKey[[2]int64{2, hour}]
	assert.Equal(t, int64(200), token2Hour.PromptTokens)
	assert.Equal(t, int64(200), token2Hour.InputTokens)
	assert.Equal(t, int64(10), token2Hour.CacheReadTokens)
	assert.Equal(t, int64(0), token2Hour.CacheCreationTokens)

	token1NextHour := byKey[[2]int64{1, hour + 1}]
	assert.Equal(t, int64(300), token1NextHour.PromptTokens)
	assert.Equal(t, int64(300), token1NextHour.InputTokens)
}

func TestUpsertCacheUsageHourlyOverwrites(t *testing.T) {
	cleanup := setupCacheUsageAggregationTestDB(t)
	defer cleanup()

	rows := []TokenCacheUsageHourly{
		{TokenId: 1, HourBucket: 100, PromptTokens: 10, InputTokens: 11, CacheReadTokens: 12, CacheCreationTokens: 13},
	}
	require.NoError(t, UpsertCacheUsageHourly(rows))

	// 覆盖写：同键不同值 → 结果等于第二次的值，而非累加
	rows = []TokenCacheUsageHourly{
		{TokenId: 1, HourBucket: 100, PromptTokens: 20, InputTokens: 21, CacheReadTokens: 22, CacheCreationTokens: 23},
	}
	require.NoError(t, UpsertCacheUsageHourly(rows))

	var stored TokenCacheUsageHourly
	require.NoError(t, DB.First(&stored).Error)
	assert.Equal(t, int64(20), stored.PromptTokens)
	assert.Equal(t, int64(21), stored.InputTokens)
	assert.Equal(t, int64(22), stored.CacheReadTokens)
	assert.Equal(t, int64(23), stored.CacheCreationTokens)
}

func TestCacheUsageAggregationMetaSaveAndGet(t *testing.T) {
	cleanup := setupCacheUsageAggregationTestDB(t)
	defer cleanup()

	// 未写入：返回全零 meta（CoveredFromHour=0 表示未回填）
	meta, err := GetCacheUsageAggregationMeta()
	require.NoError(t, err)
	assert.Equal(t, int64(1), meta.Id)
	assert.Equal(t, int64(0), meta.CoveredFromHour)
	assert.Equal(t, int64(0), meta.ReadyHour)

	require.NoError(t, SaveCacheUsageAggregationMeta(&CacheUsageAggregationMeta{
		CoveredFromHour: 1900,
		ReadyHour:       1999,
		LastRunAt:       12345,
	}))

	meta, err = GetCacheUsageAggregationMeta()
	require.NoError(t, err)
	assert.Equal(t, int64(1900), meta.CoveredFromHour)
	assert.Equal(t, int64(1999), meta.ReadyHour)
	assert.Equal(t, int64(12345), meta.LastRunAt)
	assert.NotZero(t, meta.UpdatedAt)

	// 覆盖写：单行不累积
	require.NoError(t, SaveCacheUsageAggregationMeta(&CacheUsageAggregationMeta{
		CoveredFromHour: 1910,
		ReadyHour:       2005,
	}))
	meta, err = GetCacheUsageAggregationMeta()
	require.NoError(t, err)
	assert.Equal(t, int64(1910), meta.CoveredFromHour)
	assert.Equal(t, int64(2005), meta.ReadyHour)
	assert.Equal(t, int64(0), meta.LastRunAt)

	var count int64
	require.NoError(t, DB.Model(&CacheUsageAggregationMeta{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestSumCacheUsageHourly(t *testing.T) {
	cleanup := setupCacheUsageAggregationTestDB(t)
	defer cleanup()

	rows := []TokenCacheUsageHourly{
		{TokenId: 1, HourBucket: 100, PromptTokens: 10, InputTokens: 11, CacheReadTokens: 12, CacheCreationTokens: 13},
		{TokenId: 1, HourBucket: 101, PromptTokens: 20, InputTokens: 21, CacheReadTokens: 22, CacheCreationTokens: 23},
		{TokenId: 1, HourBucket: 102, PromptTokens: 40, InputTokens: 41, CacheReadTokens: 42, CacheCreationTokens: 43}, // 超出区间，不计入
		{TokenId: 2, HourBucket: 100, PromptTokens: 5, InputTokens: 6, CacheReadTokens: 7, CacheCreationTokens: 8},
	}
	require.NoError(t, UpsertCacheUsageHourly(rows))

	stats, err := SumCacheUsageHourly([]int64{1, 2}, 100, 101)
	require.NoError(t, err)
	require.Len(t, stats, 2)

	assert.Equal(t, int64(30), stats[1].PromptTokens)
	assert.Equal(t, int64(32), stats[1].InputTokens)
	assert.Equal(t, int64(34), stats[1].CacheReadTokens)
	assert.Equal(t, int64(36), stats[1].CacheCreationTokens)

	assert.Equal(t, int64(5), stats[2].PromptTokens)
	assert.Equal(t, int64(6), stats[2].InputTokens)

	// 空 tokenIds / 非法区间 → 空 map 且不报错
	empty, err := SumCacheUsageHourly(nil, 100, 101)
	require.NoError(t, err)
	assert.Empty(t, empty)

	inverted, err := SumCacheUsageHourly([]int64{1}, 101, 100)
	require.NoError(t, err)
	assert.Empty(t, inverted)
}

func TestDeleteCacheUsageHourlyBefore(t *testing.T) {
	cleanup := setupCacheUsageAggregationTestDB(t)
	defer cleanup()

	rows := []TokenCacheUsageHourly{
		{TokenId: 1, HourBucket: 99, PromptTokens: 1},
		{TokenId: 1, HourBucket: 100, PromptTokens: 2},
		{TokenId: 2, HourBucket: 101, PromptTokens: 3},
	}
	require.NoError(t, UpsertCacheUsageHourly(rows))

	require.NoError(t, DeleteCacheUsageHourlyBefore(100))

	var remaining []TokenCacheUsageHourly
	require.NoError(t, DB.Order("hour_bucket ASC").Find(&remaining).Error)
	require.Len(t, remaining, 2)
	assert.Equal(t, int64(100), remaining[0].HourBucket)
	assert.Equal(t, int64(101), remaining[1].HourBucket)
}
