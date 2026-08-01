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

type cacheStatBatchResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Items []cacheStatItem `json:"items"`
	} `json:"data"`
}

func setupCacheBatchTestDB(t *testing.T) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	// 两个不同用户有同名 token "default"（token_id 不同）——聚合必须按 id 分开。
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenId:      11,
		TokenName:    "default",
		PromptTokens: 100,
		Other:        `{"cache_tokens":900,"cache_creation_tokens":50}`,
		CreatedAt:    172800,
	}).Error)
	require.NoError(t, db.Create(&model.Log{
		Type:         model.LogTypeConsume,
		TokenId:      22,
		TokenName:    "default",
		PromptTokens: 400,
		Other:        `{"cache_tokens":100,"cache_creation_tokens":10}`,
		CreatedAt:    173000,
	}).Error)
}

func postCacheBatch(t *testing.T, body string) cacheStatBatchResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/log/stat/cache/batch",
		strings.NewReader(body),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	GetLogsCacheStatBatch(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload cacheStatBatchResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success, payload.Message)
	return payload
}

func TestGetLogsCacheStatBatchGroupsByTokenId(t *testing.T) {
	setupCacheBatchTestDB(t)

	payload := postCacheBatch(t, `{"token_ids":[11,22],"start_timestamp":100000,"end_timestamp":200000}`)

	require.Len(t, payload.Data.Items, 2)
	byId := make(map[int64]cacheStatItem)
	for _, item := range payload.Data.Items {
		byId[item.TokenId] = item
	}
	require.Equal(t, int64(900), byId[11].CacheReadTokens)
	require.Equal(t, int64(100), byId[11].PromptTokens)
	require.Equal(t, 100.0, byId[11].CacheRate)
	require.Equal(t, int64(100), byId[22].CacheReadTokens)
	require.Equal(t, int64(400), byId[22].PromptTokens)
	require.Equal(t, 25.0, byId[22].CacheRate)
	// 同名 token 不得合并
	require.Equal(t, "default", byId[11].TokenName)
	require.Equal(t, "default", byId[22].TokenName)
}

func TestGetLogsCacheStatBatchEmptyWhenNoIds(t *testing.T) {
	setupCacheBatchTestDB(t)

	payload := postCacheBatch(t, `{"token_ids":[],"start_timestamp":100000,"end_timestamp":200000}`)

	require.Empty(t, payload.Data.Items)
}

func TestGetLogsCacheStatBatchRejectsInvalidTimeRange(t *testing.T) {
	setupCacheBatchTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPost, "/api/log/stat/cache/batch",
		strings.NewReader(`{"token_ids":[11],"start_timestamp":200000,"end_timestamp":100000}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	GetLogsCacheStatBatch(ctx)
	var payload cacheStatBatchResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.False(t, payload.Success)
	require.Equal(t, "invalid time range", payload.Message)
}
