package vision_relay

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gif 等非 png/jpeg 小图不再透传，统一转 JPEG——避免上游视觉端点报
// unsupported image format（实测 Cerebras 渠道不支持 image/webp）。
func TestCompressNonPassthroughFormatConvertsToJPEG(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	img := image.NewPaletted(image.Rect(0, 0, 100, 100), palette)
	var buf bytes.Buffer
	require.NoError(t, gif.Encode(&buf, img, nil))

	out, mime, err := CompressForVision(buf.Bytes(), "image/gif")
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", mime, "gif 小图应转 JPEG 而非透传")

	_, format, err := image.DecodeConfig(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format, "输出应为合法 jpeg")
}

// png/jpeg 小图仍透传（回归：透传白名单只放行这两种格式）
func TestCompressPNGAndJPEGStillPassthrough(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))

	var pngBuf bytes.Buffer
	require.NoError(t, png.Encode(&pngBuf, img))
	pngOut, pngMime, err := CompressForVision(pngBuf.Bytes(), "image/png")
	require.NoError(t, err)
	assert.Equal(t, "image/png", pngMime)
	assert.True(t, bytes.Equal(pngOut, pngBuf.Bytes()), "小 png 应原样透传")

	var jpegBuf bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: 90}))
	jpegOut, jpegMime, err := CompressForVision(jpegBuf.Bytes(), "image/jpeg")
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", jpegMime)
	assert.True(t, bytes.Equal(jpegOut, jpegBuf.Bytes()), "小 jpeg 应原样透传")
}

// mediaType 带参数（如 Content-Type 的 "image/webp; charset=..."）也要能正确
// 归一化后判定：非 png/jpeg 仍转 JPEG。
func TestCompressMimeWithParametersConverts(t *testing.T) {
	palette := color.Palette{color.Black}
	img := image.NewPaletted(image.Rect(0, 0, 50, 50), palette)
	var buf bytes.Buffer
	require.NoError(t, gif.Encode(&buf, img, nil))

	_, mime, err := CompressForVision(buf.Bytes(), "image/gif; charset=binary")
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", mime, "带参数的 gif mime 也应转 JPEG")
}
