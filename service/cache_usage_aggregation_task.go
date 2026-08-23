package service

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// 常量与结构见 controller/system_task_handlers.go 的 cacheUsageAggregationHandler
// （handler 注册在 controller 包，对齐 channelTestHandler 惯例）；本文件只承载
// 任务执行逻辑。调度、DB 租约锁、心跳续租、失败留行重试、多实例去重全部由
// system task runner 框架提供（见 service/system_task.go）。

const (
	// cacheUsageAggregationBatchTimeout 是单小时批次的 ctx 预算：防连接池满时
	// 任务在 GORM 连接获取上静默挂死（挂死比失败更隐蔽——失败至少留任务行）。
	cacheUsageAggregationBatchTimeout = 30 * time.Second
)

// cacheUsageAggregationBatchPacing 是批间 sleep：回填（720 批）时把 CPU 压在
// 低规格 PG 可承受水平，与本就敏感的热路径写错峰。包级变量供测试归零。
var cacheUsageAggregationBatchPacing = 300 * time.Millisecond

// CacheUsageAggregationResult 是任务结果摘要（写入任务行 result 字段）。
type CacheUsageAggregationResult struct {
	AggregatedHours int64 `json:"aggregated_hours"`
	ReadyHour       int64 `json:"ready_hour"`
	CoveredFromHour int64 `json:"covered_from_hour"`
}

// RunCacheUsageAggregationTask 执行一轮聚合：
//  1. 首次启用（CoveredFromHour==0）先落覆盖下界并回填 CacheUsageAggregationCoverDays 天；
//  2. 增量聚合 [ReadyHour+1, nowHour-1]（不碰当前小时——半截小时留待下一轮）；
//  3. 逐批推进 ReadyHour（失败重跑从断点继续，不重聚合旧小时）；
//  4. 顺带清理保留窗口外的聚合行。
//
// 回填期间查询路径因 ReadyHour 未达 now-1h 而自动走全实时（fail-safe），
// 无静默缺数窗口；回填完成前逐批推进让实时段逐步收缩。
func RunCacheUsageAggregationTask(ctx context.Context, task *model.SystemTask, runnerID string) (*CacheUsageAggregationResult, error) {
	reportProgress := NewSystemTaskProgressReporter(task, runnerID)

	meta, err := model.GetCacheUsageAggregationMeta()
	if err != nil {
		return nil, err
	}

	now := common.GetTimestamp()
	nowHour := now / 3600
	result := &CacheUsageAggregationResult{CoveredFromHour: meta.CoveredFromHour}

	if meta.CoveredFromHour == 0 {
		// 首次启用/聚合表被清空：落覆盖下界（回填起点 = 30 天前整点），
		// ReadyHour 保持 0 → 下方循环从覆盖下界开始逐批回填。
		meta.CoveredFromHour = nowHour - model.CacheUsageAggregationCoverDays*24
		if err := model.SaveCacheUsageAggregationMeta(meta); err != nil {
			return nil, err
		}
		result.CoveredFromHour = meta.CoveredFromHour
	}

	startHour := meta.ReadyHour + 1
	if meta.ReadyHour == 0 {
		startHour = meta.CoveredFromHour
	}
	endHour := nowHour - 1

	if startHour > endHour {
		// 无增量：仅做保留清理。
		if err := runCacheUsageAggregationCleanup(nowHour); err != nil {
			return nil, err
		}
		result.ReadyHour = meta.ReadyHour
		result.CoveredFromHour = meta.CoveredFromHour
		return result, nil
	}

	totalHours := endHour - startHour + 1
	for hour := startHour; hour <= endHour; hour++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("cache usage aggregation canceled: %w", err)
		}
		batchCtx, cancel := context.WithTimeout(ctx, cacheUsageAggregationBatchTimeout)
		err := aggregateCacheUsageHour(batchCtx, hour)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("aggregate hour %d: %w", hour, err)
		}

		// 逐批推进（hour 单调递增 = max 语义，天然防主时钟回跳回退水位）。
		meta.ReadyHour = hour
		meta.LastRunAt = common.GetTimestamp()
		if err := model.SaveCacheUsageAggregationMeta(meta); err != nil {
			return nil, err
		}
		result.AggregatedHours++
		reportProgress(int(hour-startHour+1), int(totalHours))

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cache usage aggregation canceled: %w", ctx.Err())
		case <-time.After(cacheUsageAggregationBatchPacing):
		}
	}

	if err := runCacheUsageAggregationCleanup(nowHour); err != nil {
		return nil, err
	}

	result.ReadyHour = meta.ReadyHour
	result.CoveredFromHour = meta.CoveredFromHour
	return result, nil
}

func aggregateCacheUsageHour(ctx context.Context, hour int64) error {
	rows, err := model.AggregateCacheUsageHour(ctx, hour, hour)
	if err != nil {
		return err
	}
	return model.UpsertCacheUsageHourly(rows)
}

func runCacheUsageAggregationCleanup(nowHour int64) error {
	return model.DeleteCacheUsageHourlyBefore(nowHour - model.CacheUsageAggregationRetentionHours)
}
