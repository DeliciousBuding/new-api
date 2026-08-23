package controller

import (
	"context"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/gin-gonic/gin"
)

// cacheUsageAggregationStatusErrorMaxLen 限制状态接口回传的任务错误文本长度，
// 防 GORM/PG 实现细节（SQL 片段、内网 host:port）外泄到面板面。
const cacheUsageAggregationStatusErrorMaxLen = 500

// cacheUsageAggregationHandler runs the scheduled cache usage preaggregation
// job (Keys 页 Cache Rate 与 dashboard 趋势的加速层)。启用与间隔来自
// cache_usage_aggregation.* 设置；执行逻辑在
// service.RunCacheUsageAggregationTask。注册点在
// controller/system_task_handlers.go 的 RegisterScheduledSystemTasks。
type cacheUsageAggregationHandler struct{}

func (cacheUsageAggregationHandler) Type() string { return model.SystemTaskTypeCacheUsageAggregation }

func (cacheUsageAggregationHandler) Enabled() bool {
	return model_setting.GetCacheUsageAggregationSnapshot().Enabled
}

func (cacheUsageAggregationHandler) Interval() time.Duration {
	minutes := model_setting.GetCacheUsageAggregationSnapshot().IntervalMinutes
	if minutes < 5 {
		minutes = 5
	}
	return time.Duration(minutes) * time.Minute
}

func (cacheUsageAggregationHandler) NewPayload() any { return nil }

func (cacheUsageAggregationHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	result, err := service.RunCacheUsageAggregationTask(ctx, task, runnerID)
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, result, nil)
}

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
