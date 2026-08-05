package vision_relay

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 本文件：协议路径感知的 JSON 变换（v0.2.1：gjson/sjson patch，不手写 JSON parser）。
//
// 只扫描协议允许的位置：
//   Claude:  messages[i].content[j] / messages[i].content[j].content[k]（仅 tool_result）/ system[j]
//   OpenAI:  messages[i].content[j]
// 绝对不做全局递归搜 {"type":"image"}——tool_use.input、普通用户 JSON、代码示例
// 里同名字段都不会被误改。未修改部分不经任何 DTO 往返，字节保留。

// Format 请求协议格式
type Format int

const (
	FormatClaude Format = iota
	FormatOpenAI
)

// Patch 协议路径上的图片块定位
type Patch struct {
	Path         string          // sjson 路径，如 "messages.0.content.1"
	Source       ImageSource     // 图片来源（base64 data 或 URL）
	Index        int             // 图序号（1 起，用于占位/边界文本）
	CacheControl json.RawMessage // 原块 cache_control（替换时平移到 text 块）
}

// PatchedImage 识别前的图片单元（engine 组装：prepare 后填 Digest/Err）
type PatchedImage struct {
	Patch  Patch
	Digest string
	Data   []byte // 解码后原始字节（base64 源；URL 源下载后回填）
	Err    error  // 提取/下载/校验失败（非 nil → 占位替换）
}

// Discover 路径感知扫描：收集协议路径上的图片块 Patch（只读）。
func Discover(raw []byte, format Format) ([]Patch, error) {
	switch format {
	case FormatClaude:
		return discoverClaude(raw)
	case FormatOpenAI:
		return discoverOpenAI(raw)
	}
	return nil, fmt.Errorf("unknown format %d", format)
}

func discoverClaude(raw []byte) ([]Patch, error) {
	var patches []Patch
	// system 数组（可含 image 块）
	if sys := gjson.GetBytes(raw, "system"); sys.IsArray() {
		sys.ForEach(func(key, value gjson.Result) bool {
			if !isClaudeImageBlock(value) {
				return true
			}
			patches = append(patches, Patch{
				Path:         fmt.Sprintf("system.%d", key.Int()),
				Source:       imageSourceFromClaudeBlock(value),
				Index:        len(patches) + 1,
				CacheControl: blockCacheControl(value),
			})
			return true
		})
	}
	// messages[].content[]
	msgs := gjson.GetBytes(raw, "messages")
	if !msgs.IsArray() {
		return patches, nil
	}
	msgs.ForEach(func(mk, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(ck, block gjson.Result) bool {
			path := fmt.Sprintf("messages.%d.content.%d", mk.Int(), ck.Int())
			if isClaudeImageBlock(block) {
				patches = append(patches, Patch{
					Path:         path,
					Source:       imageSourceFromClaudeBlock(block),
					Index:        len(patches) + 1,
					CacheControl: blockCacheControl(block),
				})
				return true
			}
			// tool_result 嵌套 content（仅此一层递归）
			if block.Get("type").String() == "tool_result" {
				if inner := block.Get("content"); inner.IsArray() {
					inner.ForEach(func(ik, iblk gjson.Result) bool {
						if !isClaudeImageBlock(iblk) {
							return true
						}
						patches = append(patches, Patch{
							Path:         fmt.Sprintf("%s.content.%d", path, ik.Int()),
							Source:       imageSourceFromClaudeBlock(iblk),
							Index:        len(patches) + 1,
							CacheControl: blockCacheControl(iblk),
						})
						return true
					})
				}
			}
			return true
		})
		return true
	})
	return patches, nil
}

func discoverOpenAI(raw []byte) ([]Patch, error) {
	var patches []Patch
	msgs := gjson.GetBytes(raw, "messages")
	if !msgs.IsArray() {
		return patches, nil
	}
	msgs.ForEach(func(mk, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(ck, block gjson.Result) bool {
			if !isOpenAIImageBlock(block) {
				return true
			}
			patches = append(patches, Patch{
				Path:         fmt.Sprintf("messages.%d.content.%d", mk.Int(), ck.Int()),
				Source:       imageSourceFromOpenAIBlock(block),
				Index:        len(patches) + 1,
				CacheControl: blockCacheControl(block),
			})
			return true
		})
		return true
	})
	return patches, nil
}

func isClaudeImageBlock(block gjson.Result) bool {
	return block.Get("type").String() == "image" && block.Get("source").Exists()
}

func isOpenAIImageBlock(block gjson.Result) bool {
	return block.Get("type").String() == "image_url" && block.Get("image_url").Exists()
}

func imageSourceFromClaudeBlock(block gjson.Result) ImageSource {
	source := block.Get("source")
	src := ImageSource{MediaType: source.Get("media_type").String()}
	switch source.Get("type").String() {
	case "url":
		src.URL = source.Get("url").String()
	default: // base64
		src.Data = source.Get("data").String()
		if src.MediaType == "" {
			src.MediaType = "image/png"
		}
	}
	return src
}

func imageSourceFromOpenAIBlock(block gjson.Result) ImageSource {
	url := block.Get("image_url.url").String()
	if len(url) > 5 && url[:5] == "data:" {
		mime, data, _ := parseDataURL(url)
		return ImageSource{Data: data, MediaType: mime}
	}
	return ImageSource{URL: url, MediaType: "image/png"}
}

// blockCacheControl 原块 cache_control（有则平移，无则 nil）
func blockCacheControl(block gjson.Result) json.RawMessage {
	cc := block.Get("cache_control")
	if !cc.Exists() {
		return nil
	}
	raw := []byte(cc.Raw)
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// Apply 识图完成后 sjson 局部替换：每个 patch 位置 → text 块。
// 每块必须替换（A4：策略命中后最终请求不允许残留 image 块）——
// 无结果（识别失败/未处理）用对应枚举占位文本兜底。
// 未知字段保留（验收 8）：除 type/text/source/image_url/detail 外的原块字段
// （含 cache_control）按原顺序平移到新块。
func Apply(raw []byte, images []*PatchedImage, results map[string]string) ([]byte, error) {
	body := raw
	for _, img := range images {
		desc, ok := results[img.Digest]
		if !ok {
			desc = placeholderUnavailable(img.Patch, enumFromErr(img.Err), len(images))
		} else {
			desc = wrapResult(img.Patch.Index, len(images), desc)
		}
		replacement, err := replacementBlock(body, img.Patch.Path, desc)
		if err != nil {
			return nil, err
		}
		body, err = sjson.SetRawBytes(body, img.Patch.Path, replacement)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// replacementBlock 构造替换文本块：type=text + 描述 + 原块未知字段
// （含 cache_control）按原顺序保留。块 JSON 从当前 body 路径读取
// （前面 patch 已替换过的块结构不变，路径仍有效）。
func replacementBlock(body []byte, path, desc string) ([]byte, error) {
	block := gjson.GetBytes(body, path)
	if !block.Exists() || !block.IsObject() {
		return nil, fmt.Errorf("patch path %q not found or not an object", path)
	}
	var buf bytes.Buffer
	buf.WriteString(`{"type":"text","text":`)
	descJSON, err := common.Marshal(desc)
	if err != nil {
		return nil, err
	}
	buf.Write(descJSON)
	block.ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		switch k {
		case "type", "text", "source", "image_url", "detail":
			return true // 被替换/删除的字段
		}
		keyJSON, err := common.Marshal(k)
		if err != nil {
			return false
		}
		buf.WriteString(`,"`)
		buf.Write(keyJSON[1 : len(keyJSON)-1])
		buf.WriteString(`":`)
		buf.WriteString(value.Raw)
		return true
	})
	buf.WriteString(`}`)
	return buf.Bytes(), nil
}
