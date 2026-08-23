package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupCacheUsageAggregationControllerTestDB(t *testing.T) func() {
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.CacheUsageAggregationMeta{}, &model.SystemTask{}, &model.TokenCacheUsageHourly{}))
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)

	logDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, logDB.AutoMigrate(&model.Log{}))
	model.LOG_DB = logDB
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)

	return func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
	}
}

func performCacheUsageAggregationStatusRequest(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/cache-usage-aggregation/status", nil)
	GetCacheUsageAggregationStatus(c)
	return w
}

func TestGetCacheUsageAggregationStatus(t *testing.T) {
	cleanup := setupCacheUsageAggregationControllerTestDB(t)
	defer cleanup()

	// 未运行过：默认值 + latest_task 为空
	w := performCacheUsageAggregationStatusRequest(t)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Enabled         bool        `json:"enabled"`
			IntervalMinutes int         `json:"interval_minutes"`
			CoveredFromHour int64       `json:"covered_from_hour"`
			ReadyHour       int64       `json:"ready_hour"`
			LastRunAt       int64       `json:"last_run_at"`
			LatestTask      interface{} `json:"latest_task"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Success)
	assert.False(t, resp.Data.Enabled)
	assert.Equal(t, 15, resp.Data.IntervalMinutes)
	assert.Equal(t, int64(0), resp.Data.CoveredFromHour)
	assert.Equal(t, int64(0), resp.Data.ReadyHour)
	assert.Nil(t, resp.Data.LatestTask)

	// 运行过：水位回读 + 最近任务摘要（错误截断）
	require.NoError(t, model.SaveCacheUsageAggregationMeta(&model.CacheUsageAggregationMeta{
		CoveredFromHour: 1900,
		ReadyHour:       1999,
		LastRunAt:       12345,
	}))
	longError := strings.Repeat("x", 800)
	require.NoError(t, model.DB.Create(&model.SystemTask{
		TaskID:   "systask_test",
		Type:     model.SystemTaskTypeCacheUsageAggregation,
		Status:   model.SystemTaskStatusFailed,
		Error:    longError,
		Result:   `{"buckets":42}`,
		Payload:  "",
		State:    "",
		LockedBy: "",
	}).Error)

	w = performCacheUsageAggregationStatusRequest(t)
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, int64(1900), resp.Data.CoveredFromHour)
	assert.Equal(t, int64(1999), resp.Data.ReadyHour)
	assert.Equal(t, int64(12345), resp.Data.LastRunAt)

	task, ok := resp.Data.LatestTask.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, string(model.SystemTaskStatusFailed), task["status"])
	errText, _ := task["error"].(string)
	assert.LessOrEqual(t, len(errText), cacheUsageAggregationStatusErrorMaxLen+len("…"))
	assert.True(t, strings.HasSuffix(errText, "…"))
}

func TestTruncateCacheUsageAggregationError(t *testing.T) {
	assert.Equal(t, "", truncateCacheUsageAggregationError(""))
	assert.Equal(t, "short", truncateCacheUsageAggregationError("short"))

	long := strings.Repeat("y", 600)
	truncated := truncateCacheUsageAggregationError(long)
	assert.Len(t, truncated, cacheUsageAggregationStatusErrorMaxLen+len("…"))
	assert.True(t, strings.HasSuffix(truncated, "…"))
}

func TestCacheUsageAggregationHandlerContract(t *testing.T) {
	handler := cacheUsageAggregationHandler{}
	assert.Equal(t, model.SystemTaskTypeCacheUsageAggregation, handler.Type())
	assert.Nil(t, handler.NewPayload())

	// 快照钳制下界：interval 恒在 [5, 60] 分钟（快照默认 15，此断言防未来改坏）
	interval := handler.Interval()
	assert.True(t, interval >= 5*time.Minute && interval <= 60*time.Minute)
}

// insertControllerConsumeLog 向日志库插入一条 type=2 消费日志。
func insertControllerConsumeLog(t *testing.T, tokenId int64, createdAt int64, promptTokens int64, otherJSON string) {
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

// setControllerOption 锁内写入 OptionMap 键（快照读取路径）。
func setControllerOption(t *testing.T, key string, value string) {
	t.Helper()
	common.OptionMapRWMutex.Lock()
	defer common.OptionMapRWMutex.Unlock()
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMap[key] = value
}

// seedSegmentedQueryFixture 构造三段对拍环境：logs 覆盖 [baseHour-2, baseHour+2]，
// 聚合表覆盖到 ReadyHour=baseHour（含），窗口 start 非整小时（baseHour-1 桶中间）。
// 返回全实时基准结果与窗口参数。
func seedSegmentedQueryFixture(t *testing.T, baseHour int64) (start, end int64) {
	// logs：两个 token 跨 5 个小时，含 Anthropic/OpenAI 混合语义
	insertControllerConsumeLog(t, 1, (baseHour-2)*3600+10, 100, `{"cache_tokens":10,"cache_creation_tokens":5}`)
	insertControllerConsumeLog(t, 1, (baseHour-1)*3600+2000, 200, `{"usage_semantic":"anthropic","cache_tokens":20,"cache_creation_tokens":8}`)
	insertControllerConsumeLog(t, 1, (baseHour-1)*3600+3000, 300, `{"cache_tokens":30}`)
	insertControllerConsumeLog(t, 1, baseHour*3600+50, 400, `{"cache_tokens":40}`)
	insertControllerConsumeLog(t, 1, (baseHour+1)*3600+60, 500, `{"cache_tokens":50}`)
	insertControllerConsumeLog(t, 1, (baseHour+2)*3600+70, 600, `{"cache_tokens":60}`)
	insertControllerConsumeLog(t, 2, (baseHour-1)*3600+100, 700, `{"cache_tokens":70}`)
	insertControllerConsumeLog(t, 2, (baseHour+1)*3600+100, 800, `{"cache_tokens":80}`)

	// 任务已聚合到 baseHour（含）：CoveredFromHour=baseHour-2、ReadyHour=baseHour
	require.NoError(t, model.SaveCacheUsageAggregationMeta(&model.CacheUsageAggregationMeta{
		CoveredFromHour: baseHour - 2,
		ReadyHour:       baseHour,
	}))
	rows, err := model.AggregateCacheUsageHour(t.Context(), baseHour-2, baseHour)
	require.NoError(t, err)
	require.NoError(t, model.UpsertCacheUsageHourly(rows))

	// 窗口：start = (baseHour-1) 桶中间（非整小时），end = (baseHour+1) 桶开头后一点
	return (baseHour-1)*3600 + 1800, (baseHour + 1) * 3600 + 100
}

func TestSumCacheUsageByTokenIdsSegmentedMatchesRealtime(t *testing.T) {
	cleanup := setupCacheUsageAggregationControllerTestDB(t)
	defer cleanup()

	baseHour := int64(25000)
	start, end := seedSegmentedQueryFixture(t, baseHour)

	setControllerOption(t, "cache_usage_aggregation.enabled", "true")
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		delete(common.OptionMap, "cache_usage_aggregation.enabled")
		common.OptionMapRWMutex.Unlock()
	})

	got, err := sumCacheUsageByTokenIdsSegmented([]int64{1, 2}, start, end)
	require.NoError(t, err)
	want, err := model.SumCacheUsageByTokenIds([]int64{1, 2}, start, end)
	require.NoError(t, err)

	require.Equal(t, want, got)
	// 非空且覆盖两个 token（防对拍双双为空假绿）
	require.Len(t, got, 2)
	assert.Equal(t, int64(1400), got[1].PromptTokens)
}

func TestSumCacheUsageDailySegmentedMatchesRealtime(t *testing.T) {
	cleanup := setupCacheUsageAggregationControllerTestDB(t)
	defer cleanup()

	baseHour := int64(25000) // 25000/24 = 1041.67 → 跨天桶场景
	start, end := seedSegmentedQueryFixture(t, baseHour)

	setControllerOption(t, "cache_usage_aggregation.enabled", "true")
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		delete(common.OptionMap, "cache_usage_aggregation.enabled")
		common.OptionMapRWMutex.Unlock()
	})

	got, err := sumCacheUsageDailySegmented([]int64{1, 2}, start, end)
	require.NoError(t, err)
	want, err := model.SumCacheUsageDaily([]int64{1, 2}, start, end)
	require.NoError(t, err)

	require.Equal(t, want, got)
	require.NotEmpty(t, got)
}

func TestCacheUsageAggregationSegmentsFailSafe(t *testing.T) {
	cleanup := setupCacheUsageAggregationControllerTestDB(t)
	defer cleanup()

	baseHour := int64(25000)
	start, end := seedSegmentedQueryFixture(t, baseHour)

	requireNoSegments := func(t *testing.T) {
		t.Helper()
		_, ok := cacheUsageAggregationSegments(start, end)
		assert.False(t, ok)
	}

	// 未启用（无 option）→ 走全实时
	t.Run("disabled by default", requireNoSegments)

	// 已启用但水位未就绪（meta 全零）→ 走全实时
	t.Run("not ready", func(t *testing.T) {
		setControllerOption(t, "cache_usage_aggregation.enabled", "true")
		require.NoError(t, model.DB.Where("1 = 1").Delete(&model.CacheUsageAggregationMeta{}).Error)
		requireNoSegments(t)
	})

	// 已就绪但窗口起点超出覆盖下界 → 走全实时
	t.Run("window before covered range", func(t *testing.T) {
		setControllerOption(t, "cache_usage_aggregation.enabled", "true")
		require.NoError(t, model.SaveCacheUsageAggregationMeta(&model.CacheUsageAggregationMeta{
			CoveredFromHour: baseHour + 5, // 覆盖下界晚于窗口起点
			ReadyHour:       baseHour + 6,
		}))
		requireNoSegments(t)
	})

	// 全实时降级时结果仍正确（与基准一致）
	t.Run("fallback result correct", func(t *testing.T) {
		got, err := sumCacheUsageByTokenIdsSegmented([]int64{1, 2}, start, end)
		require.NoError(t, err)
		want, err := model.SumCacheUsageByTokenIds([]int64{1, 2}, start, end)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		delete(common.OptionMap, "cache_usage_aggregation.enabled")
		common.OptionMapRWMutex.Unlock()
	})
}
