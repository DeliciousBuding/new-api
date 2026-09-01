package vision_relay

import (
	"strings"
	"sync"
)

// 主模型瞬态失败自适应熔断（请求级）：
//   - 同一请求内主模型（models[0]）连续瞬态失败达到 threshold 次后，
//     后续图片跳过主模型、直接从次模型开始识图，避免每张图都先撞一次
//     主模型失败延迟；
//   - 熔断开启后每 probeEvery 张图放行一次主模型探测，主模型恢复即自动复位；
//   - 状态仅在单次请求内有效，请求结束自然丢弃，不跨请求污染。
const (
	primaryBreakerThreshold  = 3 // 主模型连续瞬态失败次数阈值
	primaryBreakerProbeEvery = 8 // 熔断开启后每 N 张图放行一次主模型探测
)

type primaryBreaker struct {
	mu         sync.Mutex
	threshold  int
	probeEvery int
	failures   int
	open       bool
	sinceProbe int
}

func newPrimaryBreaker(threshold, probeEvery int) *primaryBreaker {
	if threshold <= 0 {
		threshold = primaryBreakerThreshold
	}
	if probeEvery <= 0 {
		probeEvery = primaryBreakerProbeEvery
	}
	return &primaryBreaker{threshold: threshold, probeEvery: probeEvery}
}

// normalizeModels 与 DescribeOne 的模型解析保持一致：trim 空白并丢弃空项。
// describeGrouped 在创建 breaker 前先归一化一次，保证 breaker 视角的
// “主模型名” 与 DescribeOne 实际返回的 r.Model（已 trim）一致。
func normalizeModels(in []string) []string {
	out := make([]string, 0, len(in))
	for _, m := range in {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// decide 返回本张图应使用的配置副本，以及该副本是否仍包含主模型。
// 熔断关闭时正常返回完整链；熔断开启时跳过主模型，但按 probeEvery 放行探测图。
func (b *primaryBreaker) decide(cfg Config) (Config, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(cfg.Models) < 2 || !b.open {
		return cfg, true
	}
	b.sinceProbe++
	if b.sinceProbe >= b.probeEvery {
		b.sinceProbe = 0
		return cfg, true // 半开探测：放行主模型
	}
	cfg.Models = cfg.Models[1:]
	return cfg, false
}

// observe 根据一次识图结果更新熔断状态。primary 为主模型名；includePrimary
// 表示本次识图是否实际尝试了主模型（探测图或熔断未开启时均为 true）。
func (b *primaryBreaker) observe(primary string, r DescribeResult, includePrimary bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !includePrimary {
		// 跳过主模型的识图：成功与否都不提供主模型健康信号，维持现有熔断状态。
		return
	}
	if r.Enum == "" {
		if r.Model == primary {
			b.resetLocked()
		} else {
			b.noteFailureLocked()
		}
		return
	}
	// 失败且发生了回退：说明主模型先瞬态失败过（非回退型失败如 blocked/auth/
	// size_limit/unsupported 不会把 Fallbacks 计入），记一次主模型失败。
	if r.Fallbacks > 0 {
		b.noteFailureLocked()
	}
}

func (b *primaryBreaker) noteFailureLocked() {
	if b.open {
		// 已熔断：探测图再次失败只重置探测窗口，失败计数封顶，不无界增长。
		b.sinceProbe = 0
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.open = true
		b.sinceProbe = 0
	}
}

func (b *primaryBreaker) resetLocked() {
	b.failures = 0
	b.open = false
	b.sinceProbe = 0
}
