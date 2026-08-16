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

func TestCompressMismatchedPNGAndJPEGDeclarationsReencodeToJPEG(t *testing.T) {
	sourceImage := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for rowIndex := 0; rowIndex < sourceImage.Bounds().Dy(); rowIndex++ {
		for columnIndex := 0; columnIndex < sourceImage.Bounds().Dx(); columnIndex++ {
			sourceImage.SetRGBA(columnIndex, rowIndex, color.RGBA{
				R: uint8((columnIndex*5 + rowIndex*3) % 256),
				G: uint8((columnIndex*2 + rowIndex*7) % 256),
				B: uint8((columnIndex*11 + rowIndex) % 256),
				A: 255,
			})
		}
	}

	var pngBuffer bytes.Buffer
	require.NoError(t, png.Encode(&pngBuffer, sourceImage))
	var jpegBuffer bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegBuffer, sourceImage, &jpeg.Options{Quality: 90}))

	testCases := []struct {
		name              string
		sourceBytes       []byte
		actualFormat      string
		declaredMediaType string
	}{
		{
			name:              "PNG bytes declared as JPEG",
			sourceBytes:       append([]byte(nil), pngBuffer.Bytes()...),
			actualFormat:      "png",
			declaredMediaType: "image/jpeg",
		},
		{
			name:              "JPEG bytes declared as PNG",
			sourceBytes:       append([]byte(nil), jpegBuffer.Bytes()...),
			actualFormat:      "jpeg",
			declaredMediaType: "image/png",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, sourceFormat, err := image.DecodeConfig(bytes.NewReader(testCase.sourceBytes))
			require.NoError(t, err)
			assert.Equal(t, testCase.actualFormat, sourceFormat)

			outputBytes, outputMediaType, err := CompressForVision(testCase.sourceBytes, testCase.declaredMediaType)
			require.NoError(t, err)
			assert.Equal(t, "image/jpeg", outputMediaType)
			assert.False(t, bytes.Equal(testCase.sourceBytes, outputBytes), "mismatched declarations must trigger JPEG re-encoding")

			_, outputFormat, err := image.DecodeConfig(bytes.NewReader(outputBytes))
			require.NoError(t, err)
			assert.Equal(t, "jpeg", outputFormat)
		})
	}
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
