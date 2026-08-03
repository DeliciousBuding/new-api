package vision_relay

import (
	"context"
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
			images = append(images, &PatchedImage{Patch: patches[i], Err: ErrImageLimit})
			stats.Failed++
			continue
		}
		gateCh, err := globalDecodeGate.acquire(ctx)
		if err != nil {
			// 闸等待被取消 → 保持无结果（Apply 按 service_unavailable 占位）
			images = append(images, &PatchedImage{Patch: patches[i], Err: err})
			stats.Failed++
			continue
		}
		img := PrepareImage(ctx, patches[i], e.Fetcher, MaxDecodedBytes)
		globalDecodeGate.release(gateCh)
		images = append(images, img)
		if img.Err != nil {
			stats.Failed++
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
			stats.Failed += len(group)
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
				stats.Failed += len(g) // 在途队列中已被熔断
				mu.Unlock()
				return
			}
			mu.Unlock()
			// ① decode gate（2）：完整解码/降采样/JPEG 编码——内存峰值闸门
			decodeGateCh, err := globalDecodeGate.acquire(ctx)
			if err != nil {
				// 审查 P2-2：ctx 取消/闸获取失败 → 与调度前置检查对称，补记 Failed
				mu.Lock()
				stats.Failed += len(g)
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
				}
			}
			mu.Lock()
			defer mu.Unlock()
			stats.VisionCalls += calls // P2-6：实际 HTTP 次数（含 retry/fallback）
			stats.FallbackCount += fallbacks
			if enum == "" && desc != "" {
				if e.SensitiveCheck != nil && e.SensitiveCheck(desc) {
					// 审核 P1-3（A6）：敏感词命中 → 该图 blocked 稳定占位，不注入原文
					for _, im := range g {
						im.Err = ErrBlocked
					}
					stats.Failed += len(g)
					return
				}
				results[d] = desc
				stats.Success += len(g) // 成功替换的图片块
				if stats.ModelsUsed == "" {
					stats.ModelsUsed = model
				}
			} else {
				stats.Failed += len(g)
			}
		}(digest, group)
	}
	wg.Wait()
	return results
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
			total += len(placeholderUnavailable(img.Patch, enumFromErr(img.Err), len(images)))
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
