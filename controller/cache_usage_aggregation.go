package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/gin-gonic/gin"
)

// cacheUsageAggregationStatusErrorMaxLen 限制状态接口回传的任务错误文本长度，
// 防 GORM/PG 实现细节（SQL 片段、内网 host:port）外泄到面板面。
const cacheUsageAggregationStatusErrorMaxLen = 500

// GetCacheUsageAggregationStatus 返回 Cache 用量预聚合的进度状态：
// 开关、覆盖水位与最近一次任务结果。系统设置卡片与人工诊断共用（RootAuth）。
func GetCacheUsageAggregationStatus(c *gin.Context) {
	snap := model_setting.GetCacheUsageAggregationSnapshot()

	meta, err := model.GetCacheUsageAggregationMeta()
	if err != nil {
		common.ApiError(c, err)
		return
	}

	status := gin.H{
		"enabled":           snap.Enabled,
		"interval_minutes":  snap.IntervalMinutes,
		"covered_from_hour": meta.CoveredFromHour,
		"ready_hour":        meta.ReadyHour,
		"last_run_at":       meta.LastRunAt,
		"latest_task":       nil,
	}

	latestTask, err := model.GetLatestSystemTask(model.SystemTaskTypeCacheUsageAggregation)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if latestTask != nil {
		status["latest_task"] = gin.H{
			"status": latestTask.Status,
			"error":  truncateCacheUsageAggregationError(latestTask.Error),
			"result": latestTask.Result,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    status,
	})
}

func truncateCacheUsageAggregationError(errText string) string {
	if len(errText) <= cacheUsageAggregationStatusErrorMaxLen {
		return errText
	}
	return errText[:cacheUsageAggregationStatusErrorMaxLen] + "…"
}
