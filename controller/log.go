package controller

import (
	"math"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

func GetAllLogs(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	username := c.Query("username")
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	requestId := c.Query("request_id")
	upstreamRequestId := c.Query("upstream_request_id")
	logs, total, err := model.GetAllLogs(logType, startTimestamp, endTimestamp, modelName, username, tokenName, pageInfo.GetStartIdx(), pageInfo.GetPageSize(), channel, group, requestId, upstreamRequestId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(logs)
	common.ApiSuccess(c, pageInfo)
	return
}

func GetUserLogs(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	userId := c.GetInt("id")
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	group := c.Query("group")
	requestId := c.Query("request_id")
	upstreamRequestId := c.Query("upstream_request_id")
	logs, total, err := model.GetUserLogs(userId, logType, startTimestamp, endTimestamp, modelName, tokenName, pageInfo.GetStartIdx(), pageInfo.GetPageSize(), group, requestId, upstreamRequestId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(logs)
	common.ApiSuccess(c, pageInfo)
	return
}

// Deprecated: SearchAllLogs 已废弃，前端未使用该接口。
func SearchAllLogs(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": "该接口已废弃",
	})
}

// Deprecated: SearchUserLogs 已废弃，前端未使用该接口。
func SearchUserLogs(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": "该接口已废弃",
	})
}

func GetLogByKey(c *gin.Context) {
	tokenId := c.GetInt("token_id")
	if tokenId == 0 {
		c.JSON(200, gin.H{
			"success": false,
			"message": "无效的令牌",
		})
		return
	}
	logs, err := model.GetLogByTokenId(tokenId)
	if err != nil {
		c.JSON(200, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data":    logs,
	})
}

type cacheStatBatchRequest struct {
	TokenNames     []string `json:"token_names"`
	StartTimestamp int64    `json:"start_timestamp"`
	EndTimestamp   int64    `json:"end_timestamp"`
}

// cacheStatItem 是单个 token 的缓存用量响应（含缓存命中率，一位小数）。
type cacheStatItem struct {
	TokenName           string  `json:"token_name"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheRate           float64 `json:"cache_rate"`
}

// GetLogsCacheStatBatch 批量返回多个 token 在窗口内的缓存用量聚合
// （keys 页逐 key 缓存率展示）。默认窗口为最近 7 天；空 token 列表返回空。
func GetLogsCacheStatBatch(c *gin.Context) {
	var req cacheStatBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiError(c, err)
		return
	}
	end := req.EndTimestamp
	if end == 0 {
		end = common.GetTimestamp()
	}
	start := req.StartTimestamp
	if start == 0 {
		start = end - 7*24*3600
	}
	stats, err := model.SumCacheUsageByTokenNames(req.TokenNames, start, end)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	items := make([]cacheStatItem, 0, len(req.TokenNames))
	for _, name := range req.TokenNames {
		st, ok := stats[name]
		if !ok {
			continue
		}
		rate := 0.0
		total := st.CacheReadTokens + st.PromptTokens
		if total > 0 {
			rate = float64(st.CacheReadTokens) / float64(total) * 100
		}
		items = append(items, cacheStatItem{
			TokenName:           st.TokenName,
			PromptTokens:        st.PromptTokens,
			CacheReadTokens:     st.CacheReadTokens,
			CacheCreationTokens: st.CacheCreationTokens,
			CacheRate:           math.Round(rate*10) / 10,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    gin.H{"items": items},
	})
}

// cacheStatDailyItem 是单个天桶的缓存用量响应（含缓存命中率，一位小数）。
type cacheStatDailyItem struct {
	Day                 int64   `json:"day"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheRate           float64 `json:"cache_rate"`
}

// GetLogsCacheStatDaily 返回窗口内按天分桶的缓存用量聚合
// （dashboard 缓存效率趋势）。token_names 为空表示全站；默认窗口最近 7 天。
func GetLogsCacheStatDaily(c *gin.Context) {
	var req cacheStatBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiError(c, err)
		return
	}
	end := req.EndTimestamp
	if end == 0 {
		end = common.GetTimestamp()
	}
	start := req.StartTimestamp
	if start == 0 {
		start = end - 7*24*3600
	}
	rows, err := model.SumCacheUsageDaily(req.TokenNames, start, end)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	items := make([]cacheStatDailyItem, 0, len(rows))
	for _, r := range rows {
		rate := 0.0
		total := r.CacheReadTokens + r.PromptTokens
		if total > 0 {
			rate = float64(r.CacheReadTokens) / float64(total) * 100
		}
		items = append(items, cacheStatDailyItem{
			Day:                 r.Day,
			PromptTokens:        r.PromptTokens,
			CacheReadTokens:     r.CacheReadTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			CacheRate:           math.Round(rate*10) / 10,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    gin.H{"items": items},
	})
}

func GetLogsStat(c *gin.Context) {
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	username := c.Query("username")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	stat, err := model.SumUsedQuota(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, group)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	//tokenNum := model.SumUsedToken(logType, startTimestamp, endTimestamp, modelName, username, "")
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"quota": stat.Quota,
			"rpm":   stat.Rpm,
			"tpm":   stat.Tpm,
		},
	})
	return
}

func GetLogsSelfStat(c *gin.Context) {
	username := c.GetString("username")
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	quotaNum, err := model.SumUsedQuota(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, group)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	//tokenNum := model.SumUsedToken(logType, startTimestamp, endTimestamp, modelName, username, tokenName)
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"quota": quotaNum.Quota,
			"rpm":   quotaNum.Rpm,
			"tpm":   quotaNum.Tpm,
			//"token": tokenNum,
		},
	})
	return
}
