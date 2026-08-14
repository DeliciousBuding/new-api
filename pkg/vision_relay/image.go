package vision_relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // 注册 webp 解码（x/image 已依赖）
)

// 图片级错误哨兵（占位枚举映射见 enumFromErr）
var (
	ErrSizeLimit   = errors.New("image exceeds vision size limits")
	ErrUnsupported = errors.New("image format unsupported or undecodable")
	ErrDownload    = errors.New("image download failed")
	ErrExtract     = errors.New("image block extraction failed")
	ErrImageLimit  = errors.New("image limit exceeded")
)

// PrepareImage 从 Patch 提取/下载图片数据并计算 digest。
// base64 源：无分配预检查 → 解码 → 字节校验 → digest；
// URL 源：通过 fetcher 有限流下载（调用方传入，nil 时该图按下载失败占位）。
// 失败返回带 Err 的单元（不返回错误——图片级错误占位处理）。
func PrepareImage(ctx context.Context, p Patch, fetcher ImageFetcher, maxBytes int64) *PatchedImage {
	img := &PatchedImage{Patch: p}
	if p.Source.Data != "" {
		// 内嵌 base64：编码长度预检查（无分配），超限直接拒绝不解码
		if int64(len(p.Source.Data)) > ((maxBytes+2)/3)*4 {
			img.Err = ErrSizeLimit
			return img
		}
		decoded, err := base64.StdEncoding.DecodeString(p.Source.Data)
		if err != nil {
			img.Err = ErrUnsupported
			return img
		}
		if int64(len(decoded)) > maxBytes {
			img.Err = ErrSizeLimit
			return img
		}
		img.Data = decoded
	} else if p.Source.URL != "" {
		if fetcher == nil {
			img.Err = fmt.Errorf("%w: no fetcher configured", ErrDownload)
			return img
		}
		data, mediaType, err := fetcher.Fetch(ctx, p.Source.URL, maxBytes)
		if err != nil {
			img.Err = err
			return img
		}
		img.Data = data
		if mediaType != "" {
			// 回填真实 Content-Type：OpenAI URL 块默认 image/png（transform.go），
			// 实际可能是 JPEG/WebP——小图透传分支按真实 mime 发给视觉端点。
			// 必须写到 img.Patch（存储的副本）而非局部参数 p：p 是值拷贝，
			// 改 p.Source 会让下游 CompressForVision 永远看到默认 image/png。
			img.Patch.Source.MediaType = mediaType
		}
	} else {
		img.Err = fmt.Errorf("%w: block has neither data nor url", ErrExtract)
		return img
	}
	img.Digest = DigestBytes(img.Data)
	return img
}

// DigestBytes 图片内容指纹（原始解码字节，压缩前——同图跨请求稳定）
func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// descriptionCacheKey 跨请求描述缓存的 key：digest 与识图指令（instruction）
// 的绑定哈希。描述是"图片内容 × 识图指令"的函数——同一张图配不同 prompt
// 应产出不同描述，所以 key 必须同时绑定两者，prompt 变更后旧缓存自然失效。
func descriptionCacheKey(digest, instruction string) string {
	sum := sha256.Sum256([]byte(digest + "\x00" + instruction))
	return hex.EncodeToString(sum[:])
}

// CompressForVision 像素校验 + 压缩（必须在解码并发闸内调用）：
//  1. DecodeConfig 只读头校验（宽/高/像素，超限拒绝——解压炸弹防线，
//     不触发完整 Decode）
//  2. 小图原样发送（≤2000px 且 ≤1.5MB；小 PNG 无损保留）
//  3. 大图降采样/转 JPEG（对齐客户端压缩策略 2000px/1.5MB）
//
// 不处理 EXIF 旋转（审核 A7：标准库不自动转正，阶段 1 明确不处理）。
func CompressForVision(data []byte, mediaType string) ([]byte, string, error) {
	if len(data) == 0 {
		return nil, "", ErrUnsupported
	}
	// ① 只读头：像素/尺寸校验（完整 Decode 之前）
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", ErrUnsupported
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, "", ErrSizeLimit
	}
	if cfg.Width > MaxDimension || cfg.Height > MaxDimension {
		return nil, "", ErrSizeLimit
	}
	// ② 小图直接原样（压缩目标与 Claude Code 客户端一致：2000px / 1.5MB；
	//    小 PNG 保留无损）
	const targetPx = 2000
	const targetEnc = 1500 * 1024
	if int64(cfg.Width) <= targetPx && int64(cfg.Height) <= targetPx &&
		int64(len(data)) <= targetEnc && !(mediaType == "image/png" && int64(len(data)) > 300*1024) {
		return data, mediaType, nil
	}
	// ③ 完整解码 + 降采样 + JPEG（质量阶梯 85→30，目标 ≤1.5MB）
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", ErrUnsupported
	}
	width, height := cfg.Width, cfg.Height
	if width > targetPx || height > targetPx {
		scale := float64(targetPx) / float64(maxInt(width, height))
		width = maxInt(1, int(float64(width)*scale))
		height = maxInt(1, int(float64(height)*scale))
		dst := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
		img = dst
	}
	var buf bytes.Buffer
	quality := 85
	for {
		buf.Reset()
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, "", ErrUnsupported
		}
		if buf.Len() <= targetEnc || quality <= 30 {
			break
		}
		quality -= 10
	}
	return buf.Bytes(), "image/jpeg", nil
}

// parseDataURL 解析 "data:<mime>;base64,<data>" 形式
func parseDataURL(raw string) (string, string, error) {
	const prefix = "data:"
	if !strings.HasPrefix(raw, prefix) {
		return "", "", fmt.Errorf("not a data URL")
	}
	rest := raw[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", fmt.Errorf("data URL missing comma")
	}
	meta := rest[:comma]
	b64 := rest[comma+1:]
	mime := "image/png"
	for _, part := range strings.Split(meta, ";") {
		if strings.HasPrefix(part, "base64") {
			continue
		}
		if strings.Contains(part, "/") {
			mime = part
		}
	}
	return mime, b64, nil
}

// LimitedFetch 有限流下载实现（纯核心默认版；NewAPI 适配层可注入
// 带 SSRF 保护的实现覆盖）。io.LimitReader 在 maxBytes+1 处立即停止。
func LimitedFetch(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrDownload, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrDownload, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%w: HTTP %d", ErrDownload, resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrDownload, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", ErrSizeLimit
	}
	mediaType := resp.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "image/png"
	}
	return data, mediaType, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
