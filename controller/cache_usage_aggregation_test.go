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
	previousMainType := common.MainDatabaseType()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.CacheUsageAggregationMeta{}, &model.SystemTask{}))
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)

	return func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousMainType)
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
