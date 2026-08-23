package model

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CacheUsageAggregationCoverDays 是聚合表覆盖窗口（天）：回填起点 = 该值前整点。
// 前端 dashboard 预设最大 29 天，30 天覆盖保证默认查询窗口恒命中聚合段。
// 与 controller 的 cacheStatMaxWindowSeconds（90 天窗口钳制）无耦合——超出覆盖
// 区间的查询由查询路径 fail-safe 自动回退全实时。
const CacheUsageAggregationCoverDays = 30

// CacheUsageAggregationRetentionHours 是聚合表保留窗口（小时，90 天），
// 与查询面 90 天最大窗口钳制对齐。
const CacheUsageAggregationRetentionHours = 90 * 24

// TokenCacheUsageHourly 是主库的小时粒度缓存用量聚合表（Keys 页 Cache Rate
// 与 dashboard 趋势的加速表）。纯衍生数据：由 system task 从 logs 增量聚合，
// 可随时清空重建。小时桶 = UTC 整点（created_at / 3600）。
type TokenCacheUsageHourly struct {
	TokenId             int64 `gorm:"primaryKey"`
	HourBucket          int64 `gorm:"primaryKey"`
	PromptTokens        int64
	InputTokens         int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

func (TokenCacheUsageHourly) TableName() string {
	return "token_cache_usage_hourly"
}

// CacheUsageAggregationMeta 是聚合进度的单行双水位（主库）：
//   - CoveredFromHour：覆盖下界（= 回填起点，0 = 从未回填），查询 fail-safe 判据；
//   - ReadyHour：覆盖上界 + 回填游标。例行增量逐小时推进；回填逐批推进——失败重跑
//     从 ReadyHour+1 继续，不重聚合旧小时（避免 log_cleanup 删旧行后覆盖写抹零）。
type CacheUsageAggregationMeta struct {
	Id              int64 `gorm:"primaryKey"`
	CoveredFromHour int64
	ReadyHour       int64
	LastRunAt       int64
	UpdatedAt       int64
}

func (CacheUsageAggregationMeta) TableName() string {
	return "cache_usage_aggregation_meta"
}

// cacheHourBucketExpr 返回按 UTC 整小时分桶的表达式。整除语义与
// cacheDayBucketExpr 同源：MySQL 的 `/` 返回 decimal 需 DIV，ClickHouse 需
// intDiv，PostgreSQL / SQLite 的 int/int 即为整除。
func cacheHourBucketExpr() string {
	switch {
	case common.UsingLogDatabase(common.DatabaseTypeMySQL):
		return "(created_at DIV 3600)"
	case common.UsingLogDatabase(common.DatabaseTypeClickHouse):
		return "intDiv(created_at, 3600)"
	default:
		return "(created_at / 3600)"
	}
}

// AggregateCacheUsageHour 从 logs 聚合 [startHour, endHour]（含两端整小时）内
// type=2 消费日志的缓存用量，按 (token_id, hour_bucket) 分组。输入/缓存提取
// 复用 cacheRateInputExpr / cacheJsonExtractExpr 的跨协议语义与多库表达式。
// ctx 经 WithContext 传入（任务侧给每批预算防连接池满时静默挂死）。
// logs 只 INSERT 不 UPDATE（不变式见 model/log.go createLog），同一小时区间
// 重复聚合结果一致。
func AggregateCacheUsageHour(ctx context.Context, startHour int64, endHour int64) ([]TokenCacheUsageHourly, error) {
	rows := []TokenCacheUsageHourly{}
	err := LOG_DB.WithContext(ctx).Table("logs").
		Select("token_id, "+cacheHourBucketExpr()+" AS hour_bucket, "+
			"COALESCE(SUM(prompt_tokens), 0) prompt_tokens, "+
			"COALESCE(SUM("+cacheRateInputExpr()+"), 0) input_tokens, "+
			"SUM("+cacheJsonExtractExpr("cache_tokens")+") cache_read_tokens, "+
			"SUM("+cacheJsonExtractExpr("cache_creation_tokens")+") cache_creation_tokens").
		Where("type = ?", LogTypeConsume).
		Where("created_at >= ?", startHour*3600).
		Where("created_at <= ?", endHour*3600+3599).
		Group("token_id, hour_bucket").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// UpsertCacheUsageHourly 以覆盖语义写入聚合表（logs 不可变，每小时重算后整体
// 替换，而非累加）。冲突键 (token_id, hour_bucket)，DoUpdates 用
// AssignmentColumns 生成 `SET col = excluded.col`（三库方言由 GORM 处理）。
func UpsertCacheUsageHourly(rows []TokenCacheUsageHourly) error {
	if len(rows) == 0 {
		return nil
	}
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "token_id"},
			{Name: "hour_bucket"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"prompt_tokens",
			"input_tokens",
			"cache_read_tokens",
			"cache_creation_tokens",
		}),
	}).Create(&rows).Error
}

// GetCacheUsageHourly 返回聚合表在 [startHour, endHour] 桶区间内的逐小时行
// （不聚合）。查询路径在 Go 侧归并（batch 按 token、daily 按天桶）——避开
// 「主库方言 vs 日志库方言」的第二套 SQL 分桶表达式；量级 = token 数 × 小时数
// （7 天窗口 36 token ≈ 6000 行），主键顺序扫描毫秒级。空 tokenIds 返回空。
func GetCacheUsageHourly(tokenIds []int64, startHour int64, endHour int64) ([]TokenCacheUsageHourly, error) {
	rows := []TokenCacheUsageHourly{}
	if len(tokenIds) == 0 || startHour > endHour {
		return rows, nil
	}
	err := DB.Where("token_id IN ?", tokenIds).
		Where("hour_bucket >= ?", startHour).
		Where("hour_bucket <= ?", endHour).
		Order("hour_bucket ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetCacheUsageAggregationMeta 读取单行水位；从未写入时返回全零 meta
// （CoveredFromHour=0 = 未回填，由任务自检触发回填）。
func GetCacheUsageAggregationMeta() (*CacheUsageAggregationMeta, error) {
	meta := CacheUsageAggregationMeta{Id: 1}
	err := DB.First(&meta).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &CacheUsageAggregationMeta{Id: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// SaveCacheUsageAggregationMeta 以覆盖语义写入单行水位。调用方推进水位时需用
// max(旧值, 新值) 防主时钟回跳导致水位回退。
func SaveCacheUsageAggregationMeta(meta *CacheUsageAggregationMeta) error {
	if meta == nil {
		return nil
	}
	meta.Id = 1
	meta.UpdatedAt = common.GetTimestamp()
	return DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"covered_from_hour", "ready_hour", "last_run_at", "updated_at"}),
	}).Create(meta).Error
}

// DeleteCacheUsageHourlyBefore 清理保留窗口之外的聚合行。单条 DELETE 即可
// （90 天 ≈ 21.6 万行，B1ms 上毫秒级）；刻意不用 LIMIT 分批——GORM v1 的
// postgres driver 会静默丢弃 DELETE 的 LIMIT，伪分批反而误导。
func DeleteCacheUsageHourlyBefore(cutoffHour int64) error {
	return DB.Where("hour_bucket < ?", cutoffHour).Delete(&TokenCacheUsageHourly{}).Error
}
