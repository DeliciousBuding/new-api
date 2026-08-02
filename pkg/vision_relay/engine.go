package vision_relay

import (
	"context"
	"sync"
	"time"
	"unicode/utf8"
)

// 进程级并发闸（门槛四）：容量固定为常量（v0.2.1——不热调整）。
// 解码/压缩与旁路调用分槽（GlobalDecodeSlots / GlobalCallSlots），
// 图像处理内存峰值可控。
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
}

// Enhance 一次请求的完整增强（事务由调用方保证）：
//
//	Discover（协议路径扫描）→ Prepare（解码/下载/校验）→ digest 分组去重
//	→ 有界并发识图（解码闸 + 调用闸）→ 描述截断 → Apply（sjson 局部替换）
//
// 返回增强 body。图片级错误全部内部占位；返回 error = 基础设施错误
// （调用方应 5xx，绝不 fail-open 发原图）。
func (e *Engine) Enhance(ctx context.Context, raw []byte, format Format, cfg Config, stats *Stats) ([]byte, error) {
	start := time.Now()
	client := e.Client
	if client == nil {
		client = &VisionClient{}
	}

	// 1. 路径感知扫描
	patches, err := Discover(raw, format)
	if err != nil {
		return nil, err
	}
	stats.Total = len(patches)
	if len(patches) == 0 {
		return nil, nil // 真 no-op：无图，原请求原样返回
	}

	// 2. prepare：base64 解码 / URL 有限流下载（解码闸内）+ 数量上限标记
	images := make([]*PatchedImage, 0, len(patches))
	for i := range patches {
		gateCh, err := globalDecodeGate.acquire(ctx)
		if err != nil {
			images = append(images, &PatchedImage{Patch: patches[i], Err: err})
			continue
		}
		img := PrepareImage(ctx, patches[i], e.Fetcher, MaxDecodedBytes)
		globalDecodeGate.release(gateCh)
		images = append(images, img)
	}
	if len(images) > MaxImages {
		for i := MaxImages; i < len(images); i++ {
			if images[i].Err == nil {
				images[i].Err = ErrImageLimit
			}
		}
		stats.Omitted = len(images) - MaxImages
	}

	// 3. digest 预分组去重（门槛六）：每唯一 digest 一个识图任务
	results := e.describeGrouped(ctx, images, cfg, stats)

	// 4. 描述截断（P0-6：注入大小上限；保持非空）
	truncateResults(results, stats)

	// 5. sjson 局部替换（A4：所有 image 块 → text 块/占位，零残留）
	enhanced, err := Apply(raw, images, results)
	if err != nil {
		return nil, err
	}
	stats.ElapsedMs = time.Since(start).Milliseconds()
	return enhanced, nil
}

// describeGrouped digest 分组 + 有界并发识图（门槛六 + A8）
func (e *Engine) describeGrouped(ctx context.Context, images []*PatchedImage, cfg Config, stats *Stats) map[string]string {
	// 分组：每唯一 digest 一个任务（含 Err 的图不参与识别）
	groups := make(map[string][]*PatchedImage)
	for _, img := range images {
		if img.Err != nil || img.Digest == "" {
			stats.Failed++
			continue
		}
		groups[img.Digest] = append(groups[img.Digest], img)
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
	for digest, group := range groups {
		if ctx.Err() != nil {
			// 客户端断开 → 剩余图保持无结果 → 占位（验收 15）
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(d string, g []*PatchedImage) {
			defer wg.Done()
			defer func() { <-sem }()
			// 调用闸：decode/resize/encode + 旁路调用全在闸内（门槛四）
			callGateCh, err := globalCallGate.acquire(ctx)
			if err != nil {
				return // 闸等待被取消 → 该图无结果 → 占位
			}
			defer globalCallGate.release(callGateCh)
			desc, enum, model := client.DescribeOne(ctx, instruction, g[0], cfg)
			mu.Lock()
			defer mu.Unlock()
			if enum == "" {
				results[d] = desc
				stats.Success++
				if stats.ModelsUsed == "" {
					stats.ModelsUsed = model
				}
			} else {
				stats.Failed++
				stats.FallbackCount++
			}
		}(digest, group)
	}
	wg.Wait()
	return results
}

// truncateResults 描述注入上限（P0-6）：单图截断 + 总量截断；保持非空。
func truncateResults(results map[string]string, stats *Stats) {
	total := 0
	for d, desc := range results {
		if len(desc) > MaxDescriptionBytes {
			results[d] = truncateUTF8(desc, MaxDescriptionBytes) + "[truncated]"
		}
		total += len(results[d])
	}
	if total > MaxTotalBytes {
		remaining := MaxTotalBytes
		for d, desc := range results {
			if remaining <= 0 {
				results[d] = "[truncated]"
				continue
			}
			if len(desc) > remaining {
				results[d] = truncateUTF8(desc, remaining) + "[truncated]"
				remaining = 0
			} else {
				remaining -= len(desc)
			}
		}
	}
	stats.DescriptionBytes = total
}

// truncateUTF8 按字节截断但保持 UTF-8 完整性
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
