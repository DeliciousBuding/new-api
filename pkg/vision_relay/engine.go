package vision_relay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// 进程级并发闸（门槛四）：容量固定为常量（v0.2.1——不热调整）。
// 解码/压缩（内存大户）与旁路调用（网络）分闸：decode gate 容量 2，
// call gate 容量 8——图像处理内存峰值可控（v0.2.2 修复：CompressForVision
// 的 image.Decode/resize/JPEG encode 必须真正在 decode gate 内执行）。
type processGate struct {
	ch chan struct{}
}

var (
	globalDecodeGate = &processGate{ch: make(chan struct{}, GlobalDecodeSlots)}
	globalCallGate   = &processGate{ch: make(chan struct{}, GlobalCallSlots)}
)

func (g *processGate) acquire(ctx context.Context) (chan struct{}, error) {
	select {
	case g.ch <- struct{}{}:
		return g.ch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *processGate) release(ch chan struct{}) {
	<-ch
}

// Engine Vision Relay 编排核心（纯核心，无 NewAPI 依赖）
type Engine struct {
	Client  *VisionClient // 视觉端点客户端（nil 时构建时兜底）
	Fetcher ImageFetcher  // 远程图片有限流下载（NewAPI 适配注入；nil = URL 图失败占位）
	// SensitiveCheck 可选的敏感词检查（审核 P1-3/A6：注入前对描述过一遍原生
	// 敏感词检查，命中 → 该图 blocked 稳定占位，不注入原文）。nil = 不检查。
	// NewAPI 适配在 service 层注入 CheckSensitiveText 包装。
	SensitiveCheck func(text string) bool
	// Cache 跨请求识图描述缓存（纯优化，nil = 不启用）。命中后跳过该 digest
	// 的旁路调用；Get/Set 失败静默降级为主流程，绝不影响正确性。
	Cache DescriptionCache
	// CacheTTL 成功描述写缓存的过期时间（Cache 非 nil 时生效）。
	CacheTTL time.Duration
}

// Enhance 一次请求的完整增强（事务由调用方保证）：
//
//	Discover（协议路径扫描）→ Prepare（MaxImages 前置 + 解码/下载/校验）
//	→ digest 分组去重 → 有界并发（decode gate 内压缩 + call gate 内 HTTP）
//	→ 严格截断 → Apply（sjson 局部替换）
//
// 请求级总 deadline 由调用方通过 ctx 传入（v0.2.2：fetch/decode/call/fallback
// 全部继承同一 deadline）。返回增强 body（nil = 无图 no-op）。
// 图片级错误全部内部占位；返回 error = 基础设施错误（调用方 5xx）。
func (e *Engine) Enhance(ctx context.Context, raw []byte, format Format, cfg Config, stats *Stats) ([]byte, error) {
	start := time.Now()

	// 1. 路径感知扫描
	patches, err := Discover(raw, format)
	if err != nil {
		return nil, err
	}
	stats.Total = len(patches)
	if len(patches) == 0 {
		return nil, nil // 真 no-op：无图，原请求原样返回
	}

	// 2. prepare：**MaxImages 前置**（v0.2.2：第 MaxImages 张以后的图不下载、
	//    不解码、不计算 digest——直接 image_limit 占位，限制前置网络与内存消耗）
	images := make([]*PatchedImage, 0, min(len(patches), MaxImages))
	for i := range patches {
		if i >= MaxImages {
			img := &PatchedImage{Patch: patches[i], Err: ErrImageLimit}
			images = append(images, img)
			recordFailure(stats, EnumImageLimit, img)
			continue
		}
		gateCh, err := globalDecodeGate.acquire(ctx)
		if err != nil {
			// 闸等待失败（ctx 取消/超时）→ 占位；deadline 耗尽须如实记 timeout
			img := &PatchedImage{Patch: patches[i], Err: err}
			images = append(images, img)
			recordFailure(stats, gateErrEnum(err), img)
			continue
		}
		img := PrepareImage(ctx, patches[i], e.Fetcher, MaxDecodedBytes)
		globalDecodeGate.release(gateCh)
		images = append(images, img)
		if img.Err != nil {
			recordFailure(stats, enumFromErr(img.Err), img)
		}
	}

	// 3. digest 预分组去重（门槛六）：每唯一 digest 一个识图任务
	results := e.describeGrouped(ctx, images, cfg, stats)

	// 4. 严格截断（v0.2.2：单图/总量预算含边界文本与尾标；按图片原始顺序）
	truncateResults(images, results, stats)

	// 5. sjson 局部替换（A4：所有 image 块 → text 块/占位，零残留）
	enhanced, err := Apply(raw, images, results)
	if err != nil {
		return nil, err
	}
	stats.ElapsedMs = time.Since(start).Milliseconds()
	return enhanced, nil
}

// describeGrouped digest 分组 + 有界并发识图（门槛六 + A8 + v0.2.2 闸门拆分）。
// 每组在 decode gate 内完成压缩（DecodeConfig/Decode/resize/JPEG——内存大户），
// 然后在 call gate 内完成 HTTP 调用。
// 请求级熔断（审核 P0-2 §3）：首个 401/403（Abort）→ 尚未开始的任务全部
// 停止（不再发起 sidecall），未处理图由 Apply 以稳定占位兜底。
// 跨请求缓存（纯优化）：识别前按 digest 查缓存，命中跳过旁路调用；成功后
// 写缓存。缓存失败静默降级（Get 失败 = 未命中，Set 失败忽略）。
func (e *Engine) describeGrouped(ctx context.Context, images []*PatchedImage, cfg Config, stats *Stats) map[string]string {
	// 分组：每唯一 digest 一个任务（含 Err 的图不参与识别）
	groups := make(map[string][]*PatchedImage)
	for _, img := range images {
		if img.Err != nil || img.Digest == "" {
			continue // 已在 prepare 阶段计 Failed
		}
		groups[img.Digest] = append(groups[img.Digest], img)
	}
	stats.UniqueImages = len(groups)
	stats.CacheHits = stats.Total - stats.UniqueImages - stats.Failed
	if stats.CacheHits < 0 {
		stats.CacheHits = 0
	}
	if len(groups) == 0 {
		return map[string]string{}
	}
	client := e.Client
	if client == nil {
		client = &VisionClient{}
	}
	instruction := BuildInstruction(cfg)
	results := make(map[string]string, len(groups))
	var mu sync.Mutex
	sem := make(chan struct{}, RequestConcurrency)
	var wg sync.WaitGroup
	var abort atomic.Bool // 请求级熔断：401/403 后停止所有后续 sidecall
	for digest, group := range groups {
		if ctx.Err() != nil || abort.Load() {
			// P2-6：ctx 取消/熔断未调度的图计入 Failed（mu 保护——goroutine 并发写）
			mu.Lock()
			recordFailure(stats, EnumServiceUnavailable, group...)
			mu.Unlock()
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(d string, g []*PatchedImage) {
			defer wg.Done()
			defer func() { <-sem }()
			mu.Lock()
			if abort.Load() {
				recordFailure(stats, EnumServiceUnavailable, g...) // 在途队列中已被熔断
				mu.Unlock()
				return
			}
			mu.Unlock()
			// ⓪ 跨请求缓存：识别前按 cacheKey 查缓存，命中直接复用（跳过
			//    压缩与旁路调用）。cacheKey 同时绑定 digest 与 instruction
			//    （描述依赖识图指令，prompt 变更后旧缓存必须失效）。
			//    缓存是纯优化——命中仍需过敏感词检查（敏感词库可能热更新），
			//    命中敏感词则丢弃该缓存命中、走正常识图流程（重新识图后再过
			//    一次敏感词检查）。
			var cacheKey string
			if e.Cache != nil {
				cacheKey = descriptionCacheKey(d, instruction)
				if cached, ok := e.Cache.Get(ctx, cacheKey); ok && cached != "" {
					if e.SensitiveCheck == nil || !e.SensitiveCheck(cached) {
						mu.Lock()
						results[d] = cached
						stats.Success += len(g)
						stats.CacheServed++
						mu.Unlock()
						return
					}
				}
			}
			// ① decode gate（2）：完整解码/降采样/JPEG 编码——内存峰值闸门
			decodeGateCh, err := globalDecodeGate.acquire(ctx)
			if err != nil {
				// 审查 P2-2：ctx 取消/闸获取失败 → 与调度前置检查对称，补记 Failed
				mu.Lock()
				recordFailure(stats, gateErrEnum(err), g...)
				mu.Unlock()
				return
			}
			compressed, mediaType, err := CompressForVision(g[0].Data, g[0].Patch.Source.MediaType)
			globalDecodeGate.release(decodeGateCh)
			enum := ""
			desc := ""
			model := ""
			calls := 0
			fallbacks := 0
			if err != nil {
				enum = enumFromErr(err)
			}
			// ② call gate（8）：HTTP 旁路调用；闸后复查 abort（审查 P2-1：
			//    等待期间收到 401/403 → 不再发起 HTTP sidecall）
			if enum == "" && !abort.Load() {
				callGateCh, err := globalCallGate.acquire(ctx)
				if err == nil {
					r := client.DescribeOne(ctx, instruction, compressed, mediaType, cfg)
					globalCallGate.release(callGateCh)
					desc, enum, model = r.Desc, r.Enum, r.Model
					calls, fallbacks = r.HTTPCalls, r.Fallbacks
					if r.Abort {
						abort.Store(true) // 请求级熔断：后续任务不再发起 sidecall
					}
				} else {
					// 闸获取失败（ctx 取消/超时）→ 与 decode 闸失败对称，占位兜底
					enum = gateErrEnum(err)
				}
			}
			mu.Lock()
			stats.VisionCalls += calls // P2-6：实际 HTTP 次数（含 retry/fallback）
			stats.FallbackCount += fallbacks
			shouldCache := false
			if enum == "" && desc != "" {
				if e.SensitiveCheck != nil && e.SensitiveCheck(desc) {
					// 审核 P1-3（A6）：敏感词命中 → 该图 blocked 稳定占位，不注入原文
					recordFailure(stats, EnumBlocked, g...)
					mu.Unlock()
					return
				}
				results[d] = desc
				stats.Success += len(g) // 成功替换的图片块
				if stats.ModelsUsed == "" {
					stats.ModelsUsed = model
				}
				shouldCache = e.Cache != nil
			} else {
				// 失败：显式回写占位枚举，保留精确失败原因（timeout/auth_error/
				// blocked/size_limit 等），不再笼统 service_unavailable
				recordFailure(stats, enum, g...)
			}
			mu.Unlock()
			// ③ 跨请求缓存：成功后写入（纯优化，失败静默忽略）。移到锁外，
			//    避免 Redis 往返阻塞兄弟 goroutine；key 与查询一致（digest +
			//    instruction 绑定）。
			if shouldCache {
				_ = e.Cache.Set(ctx, cacheKey, desc, e.CacheTTL)
			}
		}(digest, group)
	}
	wg.Wait()
	return results
}

// recordFailure 统一登记失败图片块：回写显式占位枚举 + 累计 Failed + 失败
// 原因分布。图片块以变参传入（单图与整组共用同一入口）。必须在持有 stats
// 写锁时调用（describeGrouped 内 mu 保护）。enum 为空时兜底
// service_unavailable。
func recordFailure(stats *Stats, enum string, imgs ...*PatchedImage) {
	if enum == "" {
		enum = EnumServiceUnavailable
	}
	for _, im := range imgs {
		im.Enum = enum
	}
	stats.Failed += len(imgs)
	if stats.FailedReasons == nil {
		stats.FailedReasons = make(map[string]int)
	}
	stats.FailedReasons[enum] += len(imgs)
}

// gateErrEnum 闸获取失败（ctx 取消/超时）的占位枚举：deadline 耗尽 → timeout
// （与 classifyNetworkErr 对齐，不吞成 service_unavailable），其余 → 兜底。
func gateErrEnum(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return EnumTimeout
	}
	return EnumServiceUnavailable
}

// truncateResults 严格截断（v0.2.2）：
//   - 单图：预算 = MaxDescriptionBytes - len(TruncatedSuffix)，截断后加尾标，
//     最终长度 ≤ MaxDescriptionBytes
//   - 总量：预算含 wrap 边界文本（prefix/suffix/换行）与占位文本，按图片
//     原始顺序累计（确定性，非 map 随机序）
func truncateResults(images []*PatchedImage, results map[string]string, stats *Stats) {
	total := 0
	for _, img := range images {
		desc, ok := results[img.Digest]
		if !ok {
			// 占位文本（含边界），计入总量
			total += len(placeholderUnavailable(img.Patch, imageEnum(img), len(images)))
			continue
		}
		// wrap 后注入长度：prefix + \n + desc + \n + suffix
		base := len(fmt.Sprintf(ResultPrefix, img.Patch.Index, len(images))) +
			1 + len(ResultSuffix) + 1
		budget := MaxTotalBytes - total - base
		if budget < len(TruncatedSuffix)+16 {
			// 总量已耗尽：最小化——只留最短占位（不可空）
			results[img.Digest] = "[omitted]"
			total += len("[omitted]") + base
			continue
		}
		if len(desc) > MaxDescriptionBytes {
			desc = truncateUTF8(desc, MaxDescriptionBytes-len(TruncatedSuffix)) + TruncatedSuffix
			results[img.Digest] = desc
		}
		if len(desc) > budget {
			desc = truncateUTF8(desc, budget-len(TruncatedSuffix)) + TruncatedSuffix
			results[img.Digest] = desc
		}
		total += base + len(desc)
	}
	stats.DescriptionBytes = total
}

// truncateUTF8 按字节截断但保持 UTF-8 完整性
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
