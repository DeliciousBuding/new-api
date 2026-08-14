package vision_relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 本文件：协议路径感知的 JSON 变换（v0.2.1：gjson/sjson patch，不手写 JSON parser）。
//
// 只扫描协议允许的位置：
//   Claude:  messages[i].content[j] / messages[i].content[j].content[k]（仅 tool_result）/ system[j]
//   OpenAI:  messages[i].content[j]
//   Responses: input[i].content[j] / input[i].output[j]（仅 function_call_output）/
//              instructions[j]
// 绝对不做全局递归搜 {"type":"image"}——tool_use.input、普通用户 JSON、代码示例
// 里同名字段都不会被误改。未修改部分不经任何 DTO 往返，字节保留。

// Format 请求协议格式
type Format int

const (
	FormatClaude Format = iota
	FormatOpenAI
	FormatResponses
)

// Patch 协议路径上的图片块定位
type Patch struct {
	Path         string          // sjson 路径，如 "messages.0.content.1"
	Source       ImageSource     // 图片来源（base64 data 或 URL）
	Index        int             // 图序号（1 起，用于占位/边界文本）
	CacheControl json.RawMessage // 原块 cache_control（替换时平移到 text 块）
	TextType     string          // 替换块类型（Claude/OpenAI="text"；Responses="input_text"）
}

// PatchedImage 识别前的图片单元（engine 组装：prepare 后填 Digest/Err）
type PatchedImage struct {
	Patch  Patch
	Digest string
	Data   []byte // 解码后原始字节（base64 源；URL 源下载后回填）
	Err    error  // 提取/下载/校验/识图失败（非 nil → 占位替换）
	// Enum 显式占位枚举（prepare 阶段错误经 enumFromErr(Err) 推导；
	// describe 阶段失败由引擎回写 r.Enum）。空 = 用 enumFromErr(Err) 兜底。
	// 独立于 Err 的原因是：describe 阶段的失败原因是"字符串枚举"（timeout/
	// blocked/auth_error 等）而非 Go error，必须显式回写才能保留精确原因。
	Enum string
}

// imageEnum 返回图片块最终占位枚举：优先显式回写的 Enum，否则用 Err 映射。
func imageEnum(img *PatchedImage) string {
	if img.Enum != "" {
		return img.Enum
	}
	return enumFromErr(img.Err)
}

// Discover 路径感知扫描：收集协议路径上的图片块 Patch（只读）。
func Discover(raw []byte, format Format) ([]Patch, error) {
	switch format {
	case FormatClaude:
		return discoverClaude(raw)
	case FormatOpenAI:
		return discoverOpenAI(raw)
	case FormatResponses:
		return discoverResponses(raw)
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

func discoverResponses(raw []byte) ([]Patch, error) {
	var patches []Patch
	// instructions（数组形式，可含 input_image）
	if ins := gjson.GetBytes(raw, "instructions"); ins.IsArray() {
		patches = append(patches, discoverResponsesContent(ins, "instructions", len(patches))...)
	}
	// input[]：message 的 content 数组 + function_call_output 的 output 数组（一层递归）
	input := gjson.GetBytes(raw, "input")
	if !input.IsArray() {
		return patches, nil
	}
	input.ForEach(func(ik, item gjson.Result) bool {
		path := fmt.Sprintf("input.%d", ik.Int())
		if content := item.Get("content"); content.IsArray() {
			patches = append(patches, discoverResponsesContent(content, path+".content", len(patches))...)
			return true
		}
		if item.Get("type").String() == "function_call_output" {
			if out := item.Get("output"); out.IsArray() {
				patches = append(patches, discoverResponsesContent(out, path+".output", len(patches))...)
			}
		}
		return true
	})
	return patches, nil
}

// discoverResponsesContent 扫描 content/output 数组中的 input_image 块。
// 路径前缀如 "input.0.content"；startIndex 用于图序号续接（跨数组统一编号）。
func discoverResponsesContent(content gjson.Result, pathPrefix string, startIndex int) []Patch {
	var patches []Patch
	content.ForEach(func(ck, block gjson.Result) bool {
		if !isResponsesImageBlock(block) {
			return true
		}
		patches = append(patches, Patch{
			Path:     fmt.Sprintf("%s.%d", pathPrefix, ck.Int()),
			Source:   imageSourceFromResponsesBlock(block),
			Index:    startIndex + len(patches) + 1,
			TextType: "input_text",
		})
		return true
	})
	return patches
}

func isClaudeImageBlock(block gjson.Result) bool {
	return block.Get("type").String() == "image" && block.Get("source").Exists()
}

func isOpenAIImageBlock(block gjson.Result) bool {
	return block.Get("type").String() == "image_url" && block.Get("image_url").Exists()
}

func isResponsesImageBlock(block gjson.Result) bool {
	return block.Get("type").String() == "input_image" &&
		(block.Get("image_url").Exists() || block.Get("data").Exists())
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

// imageSourceFromResponsesBlock 支持两种 input_image 形态：
// image_url.url（http 或 data:）与 data+mime_type（原生 base64）。
func imageSourceFromResponsesBlock(block gjson.Result) ImageSource {
	if iu := block.Get("image_url"); iu.Exists() {
		url := iu.Get("url").String()
		if len(url) > 5 && url[:5] == "data:" {
			mime, data, _ := parseDataURL(url)
			return ImageSource{Data: data, MediaType: mime}
		}
		return ImageSource{URL: url, MediaType: "image/png"}
	}
	data := block.Get("data").String()
	if strings.HasPrefix(data, "data:") {
		// 实测 codex_cli 在 data 字段直接塞完整 data URI（data:image/png;base64,...），
		// 而 OpenAI Responses 规范是裸 base64——两种都容错（empirical validation）
		mime, raw, _ := parseDataURL(data)
		if raw != "" {
			return ImageSource{Data: raw, MediaType: mime}
		}
	}
	mime := block.Get("mime_type").String()
	if mime == "" {
		mime = "image/png"
	}
	return ImageSource{Data: data, MediaType: mime}
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
			desc = placeholderUnavailable(img.Patch, imageEnum(img), len(images))
		} else {
			desc = wrapResult(img.Patch.Index, len(images), desc)
		}
		replacement, err := replacementBlock(body, img.Patch, desc)
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

// replacementBlock 构造替换文本块：type=text/input_text + 描述 + 原块未知字段
// （含 cache_control）按原顺序保留。块 JSON 从当前 body 路径读取
// （前面 patch 已替换过的块结构不变，路径仍有效）。
func replacementBlock(body []byte, patch Patch, desc string) ([]byte, error) {
	block := gjson.GetBytes(body, patch.Path)
	if !block.Exists() || !block.IsObject() {
		return nil, fmt.Errorf("patch path %q not found or not an object", patch.Path)
	}
	textType := patch.TextType
	if textType == "" {
		textType = "text"
	}
	var buf bytes.Buffer
	buf.WriteString(`{"type":`)
	typeJSON, err := json.Marshal(textType)
	if err != nil {
		return nil, err
	}
	buf.Write(typeJSON)
	buf.WriteString(`,"text":`)
	descJSON, err := json.Marshal(desc)
	if err != nil {
		return nil, err
	}
	buf.Write(descJSON)
	block.ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		switch k {
		case "type", "text", "source", "image_url", "detail", "data", "mime_type":
			return true // 被替换/删除的字段
		}
		keyJSON, err := json.Marshal(k)
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
