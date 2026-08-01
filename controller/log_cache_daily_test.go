package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type cacheStatDailyResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Items []model.CacheUsageDailyStat `json:"items"`
	} `json:"data"`
}

func setupCacheDailyTestDB(t *testing.T) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	// 两天前的 UTC 日桶 1000/86400=0，昨天的 172800/86400=2
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenName:    "k1",
		PromptTokens: 1000,
		Other:        `{"cache_tokens":9000,"cache_creation_tokens":500}`,
		CreatedAt:    172800,
	}).Error)
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenName:    "k2",
		PromptTokens: 500,
		Other:        `{"cache_tokens":1500,"cache_creation_tokens":100}`,
		CreatedAt:    173000,
	}).Error)
	// 更早的桶（超出请求窗口）
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenName:    "k1",
		PromptTokens: 100,
		Other:        `{"cache_tokens":100,"cache_creation_tokens":10}`,
		CreatedAt:    1000,
	}).Error)
	// 错误日志（type=1）不计入
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeError,
		TokenName:    "k1",
		PromptTokens: 999,
		Other:        `{"cache_tokens":9999,"cache_creation_tokens":999}`,
		CreatedAt:    172900,
	}).Error)
}

func postCacheDaily(t *testing.T, body string) cacheStatDailyResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/log/stat/cache/daily",
		strings.NewReader(body),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	GetLogsCacheStatDaily(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload cacheStatDailyResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success, payload.Message)
	return payload
}

func TestGetLogsCacheStatDailyGroupsByDay(t *testing.T) {
	setupCacheDailyTestDB(t)

	payload := postCacheDaily(t, `{"start_timestamp":100000,"end_timestamp":200000}`)

	require.Len(t, payload.Data.Items, 1)
	day := payload.Data.Items[0]
	require.Equal(t, int64(2), day.Day)
	require.Equal(t, int64(1500), day.PromptTokens)
	require.Equal(t, int64(10500), day.CacheReadTokens)
	require.Equal(t, int64(600), day.CacheCreationTokens)
}

func TestGetLogsCacheStatDailyFiltersTokenNames(t *testing.T) {
	setupCacheDailyTestDB(t)

	payload := postCacheDaily(t, `{"token_names":["k2"],"start_timestamp":100000,"end_timestamp":200000}`)

	require.Len(t, payload.Data.Items, 1)
	require.Equal(t, int64(500), payload.Data.Items[0].PromptTokens)
	require.Equal(t, int64(1500), payload.Data.Items[0].CacheReadTokens)
}

func TestGetLogsCacheStatDailyEmptyWhenNoLogs(t *testing.T) {
	setupCacheDailyTestDB(t)

	payload := postCacheDaily(t, `{"start_timestamp":100000,"end_timestamp":150000}`)

	require.Empty(t, payload.Data.Items)
}

func TestGetLogsCacheStatDailyDefaultWindowIsSevenDays(t *testing.T) {
	setupCacheDailyTestDB(t)

	// 不传窗口：end 默认 now（远大于 200000），start = now-7d。
	// 数据 created_at 都远小于 now-7d？不——172800 是 1970-01-03，
	// now-7d 远大于它，所以窗口不包含数据，返回空。
	payload := postCacheDaily(t, `{}`)

	require.Empty(t, payload.Data.Items)
}
