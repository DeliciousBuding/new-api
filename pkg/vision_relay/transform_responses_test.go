package vision_relay

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Responses 格式 golden：input_image 双形态（data+mime_type / image_url.data:）、
// instructions 数组、function_call_output.output 一层递归、function_call.arguments
// 防误命中、纯字符串 input 项不碰、未知字段保留、替换块 type=input_text。
func TestDiscoverApplyResponsesGolden(t *testing.T) {
	raw := `{
		"model":"deepseek-v4-flash",
		"instructions":[
			{"type":"input_text","text":"sys"},
			{"type":"input_image","data":"QUJD","mime_type":"image/png","vendor_ins":1}
		],
		"input":[
			{"role":"user","type":"message","content":[
				{"type":"input_text","text":"describe"},
				{"type":"input_image","image_url":{"url":"data:image/png;base64,QUJD"},"vendor_extra":42}
			]},
			{"type":"function_call","name":"Read","arguments":"{\"type\":\"input_image\",\"image_url\":{\"url\":\"http://evil/x.png\"}}"},
			{"type":"function_call_output","call_id":"fc1","output":[
				{"type":"output_text","text":"ok"},
				{"type":"input_image","image_url":{"url":"https://example.com/a.png"}}
			]},
			"plain string item"
		],
		"vendor_top":{"a":1}
	}`
	patches, err := Discover([]byte(raw), FormatResponses)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	// 3 张图：instructions[1] + input[0].content[1] + input[2].output[1]
	if len(patches) != 3 {
		t.Fatalf("expected 3 patches, got %d", len(patches))
	}
	wantPaths := []string{"instructions.1", "input.0.content.1", "input.2.output.1"}
	for i, want := range wantPaths {
		if patches[i].Path != want {
			t.Fatalf("patch %d path: expected %q, got %q", i, want, patches[i].Path)
		}
		if patches[i].TextType != "input_text" {
			t.Fatalf("patch %d TextType: expected input_text, got %q", i, patches[i].TextType)
		}
	}
	// 同图 digest 一致（data 与 data: URL 两种形态解码后相同）
	imgs := make([]*PatchedImage, len(patches))
	for i, p := range patches {
		imgs[i] = PrepareImage(t.Context(), p, nil, MaxDecodedBytes)
	}
	if imgs[0].Digest != imgs[1].Digest {
		t.Fatal("data-form and data:-URL form should share digest")
	}
	if imgs[2].Err == nil {
		t.Fatal("http URL image without fetcher should fail")
	}
	// Apply：两张 data 图有结果，URL 图走占位
	enhanced, err := Apply([]byte(raw), imgs, map[string]string{imgs[0].Digest: "一张红色图片"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	out := string(enhanced)
	if strings.Contains(out, `"type":"input_image"`) {
		t.Error("input_image block remains")
	}
	if !strings.Contains(out, `"type":"input_text"`) {
		t.Error("replacement block should be input_text")
	}
	if !strings.Contains(out, "一张红色图片") {
		t.Error("description missing from replacement")
	}
	if !strings.Contains(out, "unavailable:") {
		t.Error("placeholder missing for failed URL image")
	}
	if !strings.Contains(out, `"vendor_extra":42`) || !strings.Contains(out, `"vendor_ins":1`) {
		t.Error("vendor fields lost")
	}
	if !strings.Contains(out, `"vendor_top":{"a":1}`) {
		t.Error("top-level unknown field lost")
	}
	if !strings.Contains(out, `"plain string item"`) {
		t.Error("plain string input item mutated")
	}
	if !strings.Contains(out, `"arguments":"{\"type\":\"input_image\"`) {
		t.Error("function_call.arguments should never be scanned (path-aware)")
	}
	// 占位块本身不泄露 URL（隐私）；arguments 里的 "http://evil" 是
	// 有意保留的原始字符串（路径感知防误改），不算泄露
	replaced := gjson.GetBytes(enhanced, "input.2.output.1")
	if strings.Contains(replaced.String(), "example.com") {
		t.Error("URL leaked into placeholder (privacy)")
	}
	if replaced.Get("type").String() != "input_text" {
		t.Errorf("replaced block type: expected input_text, got %q", replaced.Get("type").String())
	}
}

// Responses 无图请求 → 零 patch（no-op 前提）
func TestDiscoverResponsesNoImage(t *testing.T) {
	raw := `{
		"model":"deepseek-v4-flash",
		"instructions":"be concise",
		"input":[
			{"role":"user","type":"message","content":"hello"},
			{"type":"function_call","name":"Read","arguments":"{\"path\":\"/a.png\"}"},
			{"type":"function_call_output","call_id":"fc1","output":"done"}
		]
	}`
	patches, err := Discover([]byte(raw), FormatResponses)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(patches) != 0 {
		t.Fatalf("expected 0 patches, got %d", len(patches))
	}
}

// codex_cli 实测形态：data 字段塞完整 data URI（data:image/png;base64,...）。
// 必须容错解析，且与裸 base64 形态 digest 一致（empirical validation）。
func TestDiscoverResponsesDataURIPrefix(t *testing.T) {
	raw := `{
		"model":"deepseek-v4-flash",
		"input":[
			{"role":"user","content":[
				{"type":"input_image","data":"data:image/png;base64,QUJD","mime_type":"image/png"}
			]}
		]
	}`
	patches, err := Discover([]byte(raw), FormatResponses)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	img := PrepareImage(t.Context(), patches[0], nil, MaxDecodedBytes)
	if img.Err != nil {
		t.Fatalf("data URI prefix should be tolerated, got: %v", img.Err)
	}
	// 与裸 base64 形态 digest 一致
	rawPlain := `{
		"model":"deepseek-v4-flash",
		"input":[
			{"role":"user","content":[
				{"type":"input_image","data":"QUJD","mime_type":"image/png"}
			]}
		]
	}`
	patchesPlain, err := Discover([]byte(rawPlain), FormatResponses)
	if err != nil {
		t.Fatalf("discover plain: %v", err)
	}
	imgPlain := PrepareImage(t.Context(), patchesPlain[0], nil, MaxDecodedBytes)
	if img.Digest != imgPlain.Digest {
		t.Fatal("data URI prefix form and plain base64 form should share digest")
	}
}

// Responses 规范形态：input_image.image_url 为裸字符串（http URL），
// 而非对象 {url:...}。修复前 imageSourceFromResponsesBlock 按对象读取 .url
// 会得到空串，最终退化为 unsupported_format。字符串 http URL 应解析为
// 可下载的 URL 源（无 fetcher 时由 PrepareImage 报 ErrDownload，而非空源）。
func TestDiscoverResponsesImageURLString(t *testing.T) {
	raw := `{
		"model":"deepseek-v4-flash",
		"input":[
			{"role":"user","content":[
				{"type":"input_image","image_url":"https://example.com/a.png"}
			]}
		]
	}`
	patches, err := Discover([]byte(raw), FormatResponses)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	if patches[0].Source.URL != "https://example.com/a.png" {
		t.Fatalf("expected URL source, got %+v", patches[0].Source)
	}
	if patches[0].Source.Data != "" {
		t.Fatalf("string image_url should not be parsed as data, got data=%q", patches[0].Source.Data)
	}
	// 无 fetcher 时 URL 源应在 PrepareImage 报 ErrDownload（不是空源 ErrExtract）
	img := PrepareImage(t.Context(), patches[0], nil, MaxDecodedBytes)
	if img.Err == nil {
		t.Fatal("URL image without fetcher should fail")
	}
	if enumFromErr(img.Err) == EnumUnsupportedFormat {
		t.Fatalf("string URL should map to download/service error, not unsupported_format; got %q", enumFromErr(img.Err))
	}
}

// Responses 规范形态：input_image.image_url 为裸 data URL 字符串
// （data:image/png;base64,...）。应解码为 base64 data 源，与对象形态 digest 一致。
func TestDiscoverResponsesImageURLDataString(t *testing.T) {
	raw := `{
		"model":"deepseek-v4-flash",
		"input":[
			{"role":"user","content":[
				{"type":"input_image","image_url":"data:image/png;base64,QUJD"}
			]}
		]
	}`
	patches, err := Discover([]byte(raw), FormatResponses)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	if patches[0].Source.Data != "QUJD" {
		t.Fatalf("expected base64 data, got %+v", patches[0].Source)
	}
	img := PrepareImage(t.Context(), patches[0], nil, MaxDecodedBytes)
	if img.Err != nil {
		t.Fatalf("data URL string should decode, got %v", img.Err)
	}
	// 与对象形态 {url:"data:..."} digest 一致
	rawObj := `{
		"model":"deepseek-v4-flash",
		"input":[
			{"role":"user","content":[
				{"type":"input_image","image_url":{"url":"data:image/png;base64,QUJD"}}
			]}
		]
	}`
	patchesObj, err := Discover([]byte(rawObj), FormatResponses)
	if err != nil {
		t.Fatalf("discover obj: %v", err)
	}
	imgObj := PrepareImage(t.Context(), patchesObj[0], nil, MaxDecodedBytes)
	if img.Digest != imgObj.Digest {
		t.Fatal("string data-URL form and object form should share digest")
	}
}

// Responses file_id 形态：input_image.file_id（OpenAI 托管文件）无下载能力，
// 应被识别为图片块并在 Apply 时替换为 unsupported_format 占位，
// 且替换后的文本块不残留 file_id 字段。
func TestDiscoverResponsesFileID(t *testing.T) {
	raw := `{
		"model":"deepseek-v4-flash",
		"input":[
			{"role":"user","content":[
				{"type":"input_image","file_id":"file-abc123"}
			]}
		]
	}`
	patches, err := Discover([]byte(raw), FormatResponses)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch for file_id block, got %d", len(patches))
	}
	img := PrepareImage(t.Context(), patches[0], nil, MaxDecodedBytes)
	if img.Err == nil {
		t.Fatal("file_id block should fail (no download capability)")
	}
	if enumFromErr(img.Err) != EnumUnsupportedFormat {
		t.Fatalf("file_id should map to unsupported_format, got %q", enumFromErr(img.Err))
	}
	enhanced, err := Apply([]byte(raw), []*PatchedImage{img}, map[string]string{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	out := string(enhanced)
	if strings.Contains(out, `"type":"input_image"`) {
		t.Error("input_image block remains")
	}
	if strings.Contains(out, "file-abc123") || strings.Contains(out, `"file_id"`) {
		t.Error("file_id leaked into replacement block (should be dropped)")
	}
	if !strings.Contains(out, "unavailable:") {
		t.Error("placeholder missing for file_id block")
	}
}
