package vision_relay

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 解压炸弹：小文件、超大像素在 DecodeConfig 阶段拒绝，不触发完整 Decode（验收 11）
func TestCompressDecodeConfigRejectsHugePixels(t *testing.T) {
	// 4000×4000 PNG 体积小，但像素 16M > MaxPixels（12M）
	data := makePNG(t, 4000, 4000)
	_, _, err := CompressForVision(data, "image/png")
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected size limit rejection, got %v", err)
	}
}

// 压缩：大图降采样转 JPEG；小 PNG 无损保留；边长超限拒绝
func TestCompressDownsampleAndKeepSmall(t *testing.T) {
	// 大图：2800×2800（7.84M 像素 < 12M，但 > 2000px → 降采样转 JPEG）
	big := makePNG(t, 2800, 2800)
	out, mt, err := CompressForVision(big, "image/png")
	if err != nil {
		t.Fatalf("compress big: %v", err)
	}
	if mt != "image/jpeg" {
		t.Fatalf("big png should be converted to jpeg, got %s", mt)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "jpeg" {
		t.Fatalf("output should be valid jpeg: %v %s", err, format)
	}
	if cfg.Width > 2000 || cfg.Height > 2000 {
		t.Fatalf("downsample target 2000px, got %dx%d", cfg.Width, cfg.Height)
	}
	// 小 PNG（<300KB 且 ≤2000px）原样保留
	small := makePNG(t, 100, 100)
	out2, mt2, err := CompressForVision(small, "image/png")
	if err != nil {
		t.Fatalf("compress small: %v", err)
	}
	if mt2 != "image/png" || !bytes.Equal(out2, small) {
		t.Fatal("small png should pass through unchanged")
	}
	// 边长超 MaxDimension(4096) → 拒绝
	huge := makePNG(t, 5000, 100)
	_, _, err = CompressForVision(huge, "image/png")
	if err == nil {
		t.Fatal("dimension over limit should be rejected")
	}
}

// 有限流下载（门槛三/验收 22）：服务端持续输出大响应，在专用上限处中断
func TestLimitedFetchWithLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		chunk := bytes.Repeat([]byte{0x89}, 1024*1024)
		for i := 0; i < 8; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer ts.Close()
	_, _, err := LimitedFetch(context.Background(), ts.Client(), ts.URL, 1024*1024) // 1MB 上限
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected size limit, got %v", err)
	}
	// 小响应正常
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("small"))
	}))
	defer ts2.Close()
	data, mt, err := LimitedFetch(context.Background(), ts2.Client(), ts2.URL, 1024*1024)
	if err != nil {
		t.Fatalf("download small: %v", err)
	}
	if string(data) != "small" || mt != "image/png" {
		t.Fatalf("unexpected download result: %q %q", data, mt)
	}
}

// PrepareImage：base64 预检查（超限不解码直接拒绝）+ 正常解码 + digest
func TestPrepareImageBase64(t *testing.T) {
	p := Patch{Path: "messages.0.content.0", Index: 1, Source: ImageSource{Data: "QUJD", MediaType: "image/png"}}
	img := PrepareImage(context.Background(), p, nil, MaxDecodedBytes)
	if img.Err != nil || img.Digest == "" || img.Data == nil {
		t.Fatalf("prepare small: %+v", img)
	}
	if string(img.Data) != "ABC" {
		t.Fatalf("unexpected decoded data %q", img.Data)
	}
	// 超限：30MB 编码
	big := Patch{Path: "messages.0.content.0", Index: 1,
		Source: ImageSource{Data: strings.Repeat("A", 30*1024*1024), MediaType: "image/png"}}
	bigImg := PrepareImage(context.Background(), big, nil, MaxDecodedBytes)
	if bigImg.Err == nil || !strings.Contains(bigImg.Err.Error(), "size") {
		t.Fatalf("oversized base64 should be rejected in precheck, got %v", bigImg.Err)
	}
}
