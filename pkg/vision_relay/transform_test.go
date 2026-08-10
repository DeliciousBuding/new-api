package vision_relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// 配置：Claude 请求——未知字段保留、tool_use.input 误命中防护、
// tool_result 递归、cache_control 平移、零 image 块残留（验收 8/23/20）
func TestDiscoverApplyClaudeGolden(t *testing.T) {
	raw := `{
		"model":"deepseek-v4-flash",
		"system":[{"type":"text","text":"sys"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"U1NZUw=="}}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"describe"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"},"cache_control":{"type":"ephemeral"}}
			]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu1","name":"Read","input":{"type":"image","source":{"type":"base64","data":"Tk9UX01BVENI"}}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu1","is_error":false,"content":[
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}
				],"vendor_field":{"keep":"me"}}
			]}
		],
		"vendor_top":{"a":1}
	}`
	patches, err := Discover([]byte(raw), FormatClaude)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	// 4 张图：system[1] + content[1] + content[2] + tool_result 嵌套 content[0]
	if len(patches) != 4 {
		t.Fatalf("expected 4 patches, got %d", len(patches))
	}
	// 路径验证
	paths := []string{"system.1", "messages.0.content.1", "messages.0.content.2", "messages.2.content.0.content.0"}
	for i, want := range paths {
		if patches[i].Path != want {
			t.Fatalf("patch %d path: expected %q, got %q", i, want, patches[i].Path)
		}
	}
	// cache_control 捕获
	if len(patches[2].CacheControl) == 0 {
		t.Fatal("image with cache_control should capture it")
	}
	// 同图 digest 一致（base64 解码后）
	imgs := make([]*PatchedImage, len(patches))
	for i, p := range patches {
		imgs[i] = PrepareImage(t.Context(), p, nil, MaxDecodedBytes)
	}
	if imgs[1].Digest != imgs[2].Digest || imgs[1].Digest != imgs[3].Digest {
		t.Fatalf("same image should share digest: %s %s %s", imgs[1].Digest, imgs[2].Digest, imgs[3].Digest)
	}
	results := map[string]string{imgs[0].Digest: "系统图", imgs[1].Digest: "内容图"}
	enhanced, err := Apply([]byte(raw), imgs, results)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	out := string(enhanced)

	// ① tool_use.input 中的 image 不命中（路径感知，验收 23）
	if !strings.Contains(out, "Tk9UX01BVENI") {
		t.Error("tool_use.input image must NOT be replaced")
	}
	// ② 未知字段保留（验收 8）
	if !strings.Contains(out, `"vendor_field":{"keep":"me"}`) {
		t.Error("tool_result vendor_field lost")
	}
	if !strings.Contains(out, `"is_error":false`) {
		t.Error("tool_result.is_error lost")
	}
	if !strings.Contains(out, `"vendor_top":{"a":1}`) {
		t.Error("top-level vendor field lost")
	}
	// ③ cache_control 平移到替换后的 text 块（验收 20）
	if !strings.Contains(out, `"cache_control":{"type":"ephemeral"}`) {
		t.Error("cache_control not preserved")
	}
	// ④ 协议路径上的 image 块全部替换（A4）——content 块 base64 data 消失
	if strings.Contains(out, "QUJD") || strings.Contains(out, "U1NZUw==") {
		t.Error("image block data remains after Apply")
	}
	// ⑤ 替换后的 text 块非空（400 bug 防护）
	if strings.Contains(out, `"text":""`) {
		t.Error("empty text block produced")
	}
	// ⑥ 输出仍是合法 JSON
	var v any
	if err := json.Unmarshal(enhanced, &v); err != nil {
		t.Fatalf("enhanced body invalid JSON: %v", err)
	}
}

// OpenAI：image_url 替换 + 字符串内容消息跳过 + 未知字段保留
func TestDiscoverApplyOpenAIGolden(t *testing.T) {
	raw := `{
		"model":"deepseek-v4-flash",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"user","content":[
				{"type":"text","text":"see this"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"},"vendor_extra":42}
			]},
			{"role":"user","content":"plain string"},
			{"role":"tool","content":[
				{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
			]}
		],
		"x_unknown":true
	}`
	patches, err := Discover([]byte(raw), FormatOpenAI)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches, got %d", len(patches))
	}
	imgs := make([]*PatchedImage, len(patches))
	for i, p := range patches {
		imgs[i] = PrepareImage(t.Context(), p, nil, MaxDecodedBytes)
	}
	if imgs[0].Digest != imgs[1].Digest {
		t.Fatal("same data should share digest")
	}
	enhanced, err := Apply([]byte(raw), imgs, map[string]string{imgs[0].Digest: "图片描述"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	out := string(enhanced)
	if strings.Contains(out, `"image_url"`) {
		t.Error("image_url block remains")
	}
	if !strings.Contains(out, `"vendor_extra":42`) {
		t.Error("vendor field lost")
	}
	if !strings.Contains(out, `"x_unknown":true`) {
		t.Error("top-level unknown field lost")
	}
	if !strings.Contains(out, `"content":"plain string"`) {
		t.Error("string content message mutated")
	}
}

// 失败块 → 占位（不残留 image 块；URL 不泄露）
func TestApplyFailurePlaceholder(t *testing.T) {
	raw := `{"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},
		{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}
	]}]}`
	patches, _ := Discover([]byte(raw), FormatClaude)
	imgs := make([]*PatchedImage, len(patches))
	for i, p := range patches {
		imgs[i] = PrepareImage(t.Context(), p, nil, MaxDecodedBytes)
	}
	// url 图无 fetcher → ErrDownload
	if imgs[1].Err == nil {
		t.Fatal("url image without fetcher should fail")
	}
	enhanced, _ := Apply([]byte(raw), imgs, map[string]string{})
	out := string(enhanced)
	if strings.Contains(out, `"type":"image"`) {
		t.Error("image block remains for failed image")
	}
	if !strings.Contains(out, "unavailable:") {
		t.Error("placeholder text missing")
	}
	if strings.Contains(out, "example.com") {
		t.Error("URL leaked into placeholder (privacy)")
	}
}

// 占位隐私：不含 URL/key/内部路径（验收 16）
func TestPlaceholderPrivacy(t *testing.T) {
	p := Patch{Index: 1, Source: ImageSource{MediaType: "image/png", URL: "https://internal.example.com/secret.png"}}
	text := placeholderUnavailable(p, EnumServiceUnavailable, 2)
	for _, banned := range []string{"internal.example.com", "secret", "messages.0"} {
		if strings.Contains(text, banned) {
			t.Errorf("placeholder leaks %q: %s", banned, text)
		}
	}
	if !strings.Contains(text, "image/png") {
		t.Errorf("placeholder should keep media_type: %s", text)
	}
}

// fuzz：任意 JSON 输入不 panic、输出合法 JSON
func FuzzDiscoverApply(f *testing.F) {
	seeds := []string{
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QUJD"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`,
		`{"messages":[]}`,
		`{}`,
		`not json`,
		`{"messages":[{"role":"user","content":123}]}`,
		`{"system":[{"type":"image"}],"messages":null}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if !gjson.ValidBytes([]byte(raw)) {
			return // 非法 JSON 允许失败（不 panic 即可）
		}
		for _, format := range []Format{FormatClaude, FormatOpenAI} {
			patches, err := Discover([]byte(raw), format)
			if err != nil {
				continue
			}
			imgs := make([]*PatchedImage, len(patches))
			for i, p := range patches {
				imgs[i] = PrepareImage(t.Context(), p, nil, MaxDecodedBytes)
			}
			out, err := Apply([]byte(raw), imgs, map[string]string{})
			if err != nil {
				continue
			}
			if !gjson.ValidBytes(out) {
				t.Fatalf("enhanced body invalid JSON: %s", out)
			}
		}
	})
}
