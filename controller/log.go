package controller

import (
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/model_setting"

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
	TokenIds       []int64 `json:"token_ids"`
	StartTimestamp int64   `json:"start_timestamp"`
	EndTimestamp   int64   `json:"end_timestamp"`
}

// cacheStatMaxWindowSeconds 限制缓存聚合窗口最大为 90 天，
// 防止全站聚合（空 token_ids）对日志库做超范围扫描。
const cacheStatMaxWindowSeconds = 90 * 24 * 3600

// cacheStatItem 是单个 token 的缓存用量响应（含缓存命中率，一位小数）。
type cacheStatItem struct {
	TokenId             int64   `json:"token_id"`
	PromptTokens        int64   `json:"prompt_tokens"`
	InputTokens         int64   `json:"input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheRate           float64 `json:"cache_rate"`
}

// cacheRatePct 计算缓存命中率百分比（一位小数）。inputTokens 已由模型层
// 按日志语义规范化，避免 OpenAI/Anthropic 混合流量重复或漏算缓存输入。
func cacheRatePct(cacheReadTokens int64, inputTokens int64) float64 {
	if inputTokens <= 0 {
		if cacheReadTokens > 0 {
			return 100
		}
		return 0
	}
	rate := float64(cacheReadTokens) / float64(inputTokens) * 100
	if rate > 100 {
		return 100
	}
	return math.Round(rate*10) / 10
}

// cacheUsageAggregationWindow 是预聚合查询的三段边界（全部为秒级时间戳或
// 小时桶）。头/尾段走 logs 实时，中间段走聚合表。
type cacheUsageAggregationWindow struct {
	headEnd   int64 // 头段结束（含）：[start, headEnd]；start 恰为整点时 < start，头段跳过
	aggStart  int64 // 聚合段起始小时桶（含）
	aggEnd    int64 // 聚合段结束小时桶（含）= ReadyHour-1
	tailStart int64 // 尾段开始（含）：[tailStart, end]；ready*3600 > end 时尾段跳过
}

// ceilHour 向上取整到 UTC 整小时（秒 → 小时桶）。
func ceilHour(timestamp int64) int64 {
	return (timestamp + 3599) / 3600
}

// cacheUsageAggregationSegments 依据快照与水位计算三段边界。ok=false 表示
// 预聚合不可用（未启用/未就绪/窗口起点超出覆盖下界），调用方必须走全实时
// 路径——fail-safe：慢但结果永远正确，不存在聚合缺失导致的静默错数。
func cacheUsageAggregationSegments(start int64, end int64) (cacheUsageAggregationWindow, bool) {
	var w cacheUsageAggregationWindow
	snap := model_setting.GetCacheUsageAggregationSnapshot()
	if !snap.Enabled {
		return w, false
	}
	meta, err := model.GetCacheUsageAggregationMeta()
	if err != nil {
		common.SysLog("cache usage aggregation meta read failed: " + err.Error())
		return w, false
	}
	if meta.CoveredFromHour <= 0 || meta.ReadyHour <= 0 {
		return w, false
	}
	if start < meta.CoveredFromHour*3600 {
		return w, false
	}
	w.headEnd = min(end, ceilHour(start)*3600-1)
	w.aggStart = ceilHour(start)
	w.aggEnd = meta.ReadyHour - 1
	w.tailStart = meta.ReadyHour * 3600
	return w, true
}

func mergeCacheUsageStats(dst map[int64]model.CacheUsageStat, src map[int64]model.CacheUsageStat) {
	for id, st := range src {
		cur := dst[id]
		cur.TokenId = id
		cur.PromptTokens += st.PromptTokens
		cur.InputTokens += st.InputTokens
		cur.CacheReadTokens += st.CacheReadTokens
		cur.CacheCreationTokens += st.CacheCreationTokens
		dst[id] = cur
	}
}

func mergeCacheUsageHourlyRows(dst map[int64]model.CacheUsageStat, rows []model.TokenCacheUsageHourly) {
	for _, r := range rows {
		cur := dst[r.TokenId]
		cur.TokenId = r.TokenId
		cur.PromptTokens += r.PromptTokens
		cur.InputTokens += r.InputTokens
		cur.CacheReadTokens += r.CacheReadTokens
		cur.CacheCreationTokens += r.CacheCreationTokens
		dst[r.TokenId] = cur
	}
}

// sumCacheUsageByTokenIdsSegmented 返回多个 token 在窗口内的缓存用量聚合：
// 预聚合启用且水位覆盖窗口时走「头/尾实时段 + 聚合段」三段合并（头段修正
// 非整小时 start 的边界偏差），否则走全实时原路径。
func sumCacheUsageByTokenIdsSegmented(tokenIds []int64, start int64, end int64) (map[int64]model.CacheUsageStat, error) {
	segments, ok := cacheUsageAggregationSegments(start, end)
	if !ok {
		return model.SumCacheUsageByTokenIds(tokenIds, start, end)
	}

	stats := make(map[int64]model.CacheUsageStat)

	if start <= segments.headEnd {
		part, err := model.SumCacheUsageByTokenIds(tokenIds, start, segments.headEnd)
		if err != nil {
			return nil, err
		}
		mergeCacheUsageStats(stats, part)
	}
	if segments.tailStart <= end {
		part, err := model.SumCacheUsageByTokenIds(tokenIds, segments.tailStart, end)
		if err != nil {
			return nil, err
		}
		mergeCacheUsageStats(stats, part)
	}
	if segments.aggStart <= segments.aggEnd {
		rows, err := model.GetCacheUsageHourly(tokenIds, segments.aggStart, segments.aggEnd)
		if err != nil {
			return nil, err
		}
		mergeCacheUsageHourlyRows(stats, rows)
	}
	return stats, nil
}

// sumCacheUsageDailySegmented 返回窗口内按天分桶的缓存用量聚合（与 batch
// 同款三段切分）。聚合段的小时行在 Go 侧按 hour/24 归并成天桶，避开
// 「主库方言 vs 日志库方言」的第二套 SQL 分桶表达式。
func sumCacheUsageDailySegmented(tokenIds []int64, start int64, end int64) ([]model.CacheUsageDailyStat, error) {
	segments, ok := cacheUsageAggregationSegments(start, end)
	if !ok {
		return model.SumCacheUsageDaily(tokenIds, start, end)
	}

	byDay := make(map[int64]model.CacheUsageDailyStat)
	mergeDayRows := func(rows []model.CacheUsageDailyStat) {
		for _, r := range rows {
			cur := byDay[r.Day]
			cur.Day = r.Day
			cur.PromptTokens += r.PromptTokens
			cur.InputTokens += r.InputTokens
			cur.CacheReadTokens += r.CacheReadTokens
			cur.CacheCreationTokens += r.CacheCreationTokens
			byDay[r.Day] = cur
		}
	}

	if start <= segments.headEnd {
		rows, err := model.SumCacheUsageDaily(tokenIds, start, segments.headEnd)
		if err != nil {
			return nil, err
		}
		mergeDayRows(rows)
	}
	if segments.tailStart <= end {
		rows, err := model.SumCacheUsageDaily(tokenIds, segments.tailStart, end)
		if err != nil {
			return nil, err
		}
		mergeDayRows(rows)
	}
	if segments.aggStart <= segments.aggEnd {
		hourly, err := model.GetCacheUsageHourly(tokenIds, segments.aggStart, segments.aggEnd)
		if err != nil {
			return nil, err
		}
		for _, r := range hourly {
			day := r.HourBucket / 24
			cur := byDay[day]
			cur.Day = day
			cur.PromptTokens += r.PromptTokens
			cur.InputTokens += r.InputTokens
			cur.CacheReadTokens += r.CacheReadTokens
			cur.CacheCreationTokens += r.CacheCreationTokens
			byDay[day] = cur
		}
	}

	days := make([]int64, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	slices.Sort(days)
	rows := make([]model.CacheUsageDailyStat, 0, len(days))
	for _, day := range days {
		rows = append(rows, byDay[day])
	}
	return rows, nil
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
	if start < 0 || end <= start || end-start > cacheStatMaxWindowSeconds {
		common.ApiErrorMsg(c, "invalid time range")
		return
	}
	stats, err := sumCacheUsageByTokenIdsSegmented(req.TokenIds, start, end)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	items := make([]cacheStatItem, 0, len(req.TokenIds))
	for _, id := range req.TokenIds {
		st, ok := stats[id]
		if !ok {
			continue
		}
		items = append(items, cacheStatItem{
			TokenId:             st.TokenId,
			PromptTokens:        st.PromptTokens,
			InputTokens:         st.InputTokens,
			CacheReadTokens:     st.CacheReadTokens,
			CacheCreationTokens: st.CacheCreationTokens,
			CacheRate:           cacheRatePct(st.CacheReadTokens, st.InputTokens),
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
	InputTokens         int64   `json:"input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheRate           float64 `json:"cache_rate"`
}

// GetLogsCacheStatDaily 返回窗口内按天分桶的缓存用量聚合
// （dashboard 缓存效率趋势）。token_ids 为空表示全站；默认窗口最近 7 天。
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
	if start < 0 || end <= start || end-start > cacheStatMaxWindowSeconds {
		common.ApiErrorMsg(c, "invalid time range")
		return
	}
	rows, err := sumCacheUsageDailySegmented(req.TokenIds, start, end)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	items := make([]cacheStatDailyItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, cacheStatDailyItem{
			Day:                 r.Day,
			PromptTokens:        r.PromptTokens,
			InputTokens:         r.InputTokens,
			CacheReadTokens:     r.CacheReadTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			CacheRate:           cacheRatePct(r.CacheReadTokens, r.InputTokens),
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
