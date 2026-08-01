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
		Items []cacheStatDailyItem `json:"items"`
	} `json:"data"`
}

func setupCacheDailyTestDB(t *testing.T) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	// 日桶 = created_at/86400：172800→桶 2，173000→桶 2，1000→桶 0
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenId:      11,
		TokenName:    "k1",
		PromptTokens: 1000,
		Other:        `{"usage_semantic":"anthropic","cache_tokens":9000,"cache_creation_tokens":500}`,
		CreatedAt:    172800,
	}).Error)
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenId:      22,
		TokenName:    "k2",
		PromptTokens: 500,
		Other:        `{"input_tokens_total":2000,"cache_tokens":1500,"cache_creation_tokens":100}`,
		CreatedAt:    173000,
	}).Error)
	// 更早的桶（超出请求窗口）
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenId:      11,
		TokenName:    "k1",
		PromptTokens: 100,
		Other:        `{"cache_tokens":100,"cache_creation_tokens":10}`,
		CreatedAt:    1000,
	}).Error)
	// 错误日志（type=LogTypeError）不计入
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeError,
		TokenId:      11,
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
	require.Equal(t, int64(12500), day.InputTokens)
	require.Equal(t, int64(10500), day.CacheReadTokens)
	require.Equal(t, int64(600), day.CacheCreationTokens)
	require.Equal(t, 84.0, day.CacheRate)
}

func TestGetLogsCacheStatDailyFiltersTokenIds(t *testing.T) {
	setupCacheDailyTestDB(t)

	payload := postCacheDaily(t, `{"token_ids":[22],"start_timestamp":100000,"end_timestamp":200000}`)

	require.Len(t, payload.Data.Items, 1)
	item := payload.Data.Items[0]
	require.Equal(t, int64(500), item.PromptTokens)
	require.Equal(t, int64(2000), item.InputTokens)
	require.Equal(t, int64(1500), item.CacheReadTokens)
	require.Equal(t, 75.0, item.CacheRate)
}

func TestGetLogsCacheStatDailyOrdersDaysAscending(t *testing.T) {
	setupCacheDailyTestDB(t)
	require.NoError(t, model.LOG_DB.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenId:      11,
		TokenName:    "k1",
		PromptTokens: 10,
		Other:        `{"cache_tokens":5}`,
		CreatedAt:    259200,
	}).Error)

	payload := postCacheDaily(t, `{"start_timestamp":100000,"end_timestamp":300000}`)

	require.Len(t, payload.Data.Items, 2)
	require.Equal(t, int64(2), payload.Data.Items[0].Day)
	require.Equal(t, int64(3), payload.Data.Items[1].Day)
}

func TestGetLogsCacheStatDailyEmptyWhenNoLogs(t *testing.T) {
	setupCacheDailyTestDB(t)

	payload := postCacheDaily(t, `{"start_timestamp":100000,"end_timestamp":150000}`)

	require.Empty(t, payload.Data.Items)
}

func TestGetLogsCacheStatDailyDefaultWindowIsSevenDays(t *testing.T) {
	setupCacheDailyTestDB(t)

	// 不传窗口：end 默认 now，start = now-7d；测试数据在 1970 年，必然在窗外。
	payload := postCacheDaily(t, `{}`)

	require.Empty(t, payload.Data.Items)
}

func TestGetLogsCacheStatDailyRejectsInvalidTimeRange(t *testing.T) {
	setupCacheDailyTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPost, "/api/log/stat/cache/daily",
		strings.NewReader(`{"start_timestamp":200000,"end_timestamp":100000}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	GetLogsCacheStatDaily(ctx)
	var payload cacheStatDailyResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.False(t, payload.Success)
	require.Equal(t, "invalid time range", payload.Message)
}

func TestCacheRatePctSemantics(t *testing.T) {
	cases := []struct {
		name  string
		read  int64
		input int64
		want  float64
	}{
		// 输入分母已在模型层按协议规范化。
		{"openai full cache", 33408, 33409, 100},
		{"partial cache", 100, 900, 11.1},
		{"mixed protocol aggregate", 10500, 12500, 84},
		{"no cache", 0, 500, 0},
		{"all cached no prompt", 500, 0, 100},
		{"no traffic", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, cacheRatePct(c.read, c.input))
		})
	}
}
