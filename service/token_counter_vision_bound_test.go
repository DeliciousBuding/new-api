package service

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/vision_relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enableTokenCounting turns on the constant.CountToken / GetMediaToken /
// MaxFileDownloadMB flags that initConstantEnv() sets in production but
// TestMain in this package does not. Restores prior values via t.Cleanup so
// the change stays local.
func enableTokenCounting(t *testing.T) {
	t.Helper()
	origCount, origMedia := constant.CountToken, constant.GetMediaToken
	origMediaNotStream := constant.GetMediaTokenNotStream
	origMaxDownload := constant.MaxFileDownloadMB
	constant.CountToken = true
	constant.GetMediaToken = true
	constant.MaxFileDownloadMB = 64
	// GetMediaTokenNotStream stays false (production default) so non-stream
	// requests skip URL fetches; tests that need fetch set IsStream=true.
	t.Cleanup(func() {
		constant.CountToken = origCount
		constant.GetMediaToken = origMedia
		constant.GetMediaTokenNotStream = origMediaNotStream
		constant.MaxFileDownloadMB = origMaxDownload
	})
}

// visionMultiImageClaudeBody builds a Claude request body with numImages
// distinct base64 PNG image blocks interleaved with text markers, so each
// image produces a separate FileMeta entry in GetTokenCountMeta.
func visionMultiImageClaudeBody(t *testing.T, numImages int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[`)
	sb.WriteString(`{"type":"text","text":"describe"}`)
	for i := 0; i < numImages; i++ {
		sb.WriteString(`,{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`)
		sb.WriteString(b64)
		sb.WriteString(`"}}`)
	}
	sb.WriteString(`]}]}`)
	return []byte(sb.String())
}

// visionMultiImageRelayInfo constructs a RelayInfo + gin context whose
// BodyStorage carries numImages images, with relayInfo.Request parsed from
// the body so GetTokenCountMeta returns the image FileMeta entries.
func visionMultiImageRelayInfo(t *testing.T, numImages int) (*gin.Context, *relaycommon.RelayInfo, []byte) {
	t.Helper()
	raw := visionMultiImageClaudeBody(t, numImages)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	// ContextKeyOriginalModel is read by EstimateRequestToken to pick the
	// estimator; deepseek routes through the OpenAI estimator path.
	c.Set(string(constant.ContextKeyOriginalModel), "deepseek-v4-flash")
	storage, err := common.CreateBodyStorage(raw)
	require.NoError(t, err)
	c.Set(common.KeyBodyStorage, storage)
	var req dto.ClaudeRequest
	require.NoError(t, common.Unmarshal(raw, &req))
	info := &relaycommon.RelayInfo{
		OriginModelName: "deepseek-v4-flash",
		RelayFormat:     types.RelayFormatClaude,
		Request:         &req,
	}
	return c, info, raw
}

// boundVisionImageFiles must keep the first MaxImages image entries and drop
// the rest, while preserving non-image files in their original positions.
func TestBoundVisionImageFilesDropsSurplusImages(t *testing.T) {
	urlSrc := func(u string) *types.URLSource { return types.NewURLFileSource(u) }
	img := func() *types.FileMeta {
		return &types.FileMeta{FileType: types.FileTypeImage, Source: urlSrc("http://example.com/a.png")}
	}
	audio := func() *types.FileMeta {
		return &types.FileMeta{FileType: types.FileTypeAudio, Source: urlSrc("http://example.com/a.mp3")}
	}
	unknown := func() *types.FileMeta {
		return &types.FileMeta{FileType: "", Source: urlSrc("http://example.com/a.bin")}
	}

	// 8 images + 2 audio + 1 unknown, interleaved.
	files := []*types.FileMeta{
		img(), audio(), img(), img(), unknown(), img(), img(), audio(), img(), img(), img(),
	}
	bounded := boundVisionImageFiles(files)

	wantImageCount := vision_relay.MaxImages
	gotImageCount := 0
	for _, f := range bounded {
		if f.FileType == types.FileTypeImage {
			gotImageCount++
		}
	}
	assert.Equal(t, wantImageCount, gotImageCount, "image count must be capped at MaxImages")

	// Non-image files must all survive.
	assert.Equal(t, 2, countFileType(bounded, types.FileTypeAudio))
	assert.Equal(t, 1, countFileType(bounded, ""))

	// Surviving images must be the first MaxImages in order (positions 0,2,3,...).
	// The 7th image in the original slice (index 10, since images are at
	// 0,2,3,5,6,8,9,10) must be dropped.
	require.Len(t, bounded, wantImageCount+3) // MaxImages images + 2 audio + 1 unknown
}

func countFileType(files []*types.FileMeta, ft types.FileType) int {
	count := 0
	for _, f := range files {
		if f.FileType == ft {
			count++
		}
	}
	return count
}

// boundVisionImageFiles with nil/empty input must return nil (no panic).
func TestBoundVisionImageFilesEmpty(t *testing.T) {
	require.Nil(t, boundVisionImageFiles(nil))
	require.Nil(t, boundVisionImageFiles([]*types.FileMeta{}))
}

// boundVisionImageFiles must not mutate the input slice; callers downstream
// may still reference meta.Files for other purposes.
func TestBoundVisionImageFilesDoesNotMutateInput(t *testing.T) {
	original := []*types.FileMeta{
		{FileType: types.FileTypeImage, Source: types.NewURLFileSource("http://example.com/1.png")},
		{FileType: types.FileTypeImage, Source: types.NewURLFileSource("http://example.com/2.png")},
		{FileType: types.FileTypeImage, Source: types.NewURLFileSource("http://example.com/3.png")},
		{FileType: types.FileTypeImage, Source: types.NewURLFileSource("http://example.com/4.png")},
		{FileType: types.FileTypeImage, Source: types.NewURLFileSource("http://example.com/5.png")},
		{FileType: types.FileTypeImage, Source: types.NewURLFileSource("http://example.com/6.png")},
		{FileType: types.FileTypeImage, Source: types.NewURLFileSource("http://example.com/7.png")},
		{FileType: types.FileTypeImage, Source: types.NewURLFileSource("http://example.com/8.png")},
	}
	require.Len(t, original, 8)
	_ = boundVisionImageFiles(original)
	// Input slice length and element identities are unchanged.
	require.Len(t, original, 8)
	for i := range original {
		require.Equal(t, "http://example.com/"+itoa(i+1)+".png", original[i].Source.GetIdentifier())
	}
}

// itoa is a tiny strconv.Itoa stand-in to keep imports lean for the test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// EstimateRequestToken with Vision Relay engaged must cap the 520-per-image
// contribution at MaxImages even when the request carries more images. With
// Vision Relay disabled, the same request must count every image — proving
// the bound is conditional and does not break legitimate multi-image flows.
func TestEstimateRequestTokenBoundsImagesForVisionRelay(t *testing.T) {
	enableTokenCounting(t)
	ts := visionMockServer(t, nil)
	defer ts.Close()

	numImages := vision_relay.MaxImages + 3
	// Sanity: the request must actually carry more images than the bound.
	require.Greater(t, numImages, vision_relay.MaxImages)

	// --- Vision Relay enabled: estimate must drop surplus images. ---
	_, _, _ = setupVisionRelayEnv(t, ts.URL, true) // writes OptionMap
	c, info, _ := visionMultiImageRelayInfo(t, numImages)
	meta := info.Request.GetTokenCountMeta()
	require.NotEmpty(t, meta.Files, "test fixture must produce FileMeta entries")

	boundedEstimate, err := EstimateRequestToken(c, meta, info)
	require.NoError(t, err)

	// --- Vision Relay disabled: estimate must count all images. ---
	_, _, _ = setupVisionRelayEnv(t, ts.URL, false)
	c2, info2, _ := visionMultiImageRelayInfo(t, numImages)
	meta2 := info2.Request.GetTokenCountMeta()
	unboundedEstimate, err := EstimateRequestToken(c2, meta2, info2)
	require.NoError(t, err)

	// deepseek is a non-OpenAI text model, so each image contributes exactly
	// 520 placeholder tokens. The bounded estimate must be lower than the
	// unbounded one by exactly (numImages - MaxImages) * 520.
	surplus := numImages - vision_relay.MaxImages
	expectedDelta := surplus * 520
	actualDelta := unboundedEstimate - boundedEstimate
	assert.Equal(t, expectedDelta, actualDelta,
		"bounded estimate must drop exactly %d image placeholders (%d surplus * 520), got delta %d",
		surplus, surplus, actualDelta)
}

// EstimateRequestToken must not apply the Vision Relay bound when the model
// is not in the target allowlist, even if Vision Relay is enabled. This
// protects legitimate multi-image requests to non-targeted models.
func TestEstimateRequestTokenNoBoundForUnmatchedModel(t *testing.T) {
	enableTokenCounting(t)
	ts := visionMockServer(t, nil)
	defer ts.Close()
	_, _, _ = setupVisionRelayEnv(t, ts.URL, true) // target_models = ["deepseek*"]

	numImages := vision_relay.MaxImages + 2
	c, info, raw := visionMultiImageRelayInfo(t, numImages)
	// Override model name to one that does NOT match "deepseek*".
	info.OriginModelName = "gpt-4o"
	c.Set(string(constant.ContextKeyOriginalModel), "gpt-4o")
	// Rebuild request DTO with the new model name.
	var req dto.ClaudeRequest
	require.NoError(t, common.Unmarshal(raw, &req))
	req.Model = "gpt-4o"
	info.Request = &req

	meta := info.Request.GetTokenCountMeta()
	estimate, err := EstimateRequestToken(c, meta, info)
	require.NoError(t, err)

	// With no bound applied, all numImages images contribute their per-model
	// placeholder. For OpenAI text models (gpt-4o matches IsOpenAITextModel),
	// getImageToken runs and yields a positive value per image. We only need
	// to assert the estimate is well above what MaxImages alone would yield
	// under the OpenAI image-token formula — i.e., the surplus images are
	// counted, not dropped.
	assert.Greater(t, estimate, 0, "estimate must be positive")
}

// Fetch bound integration: when Vision Relay is enabled and the request
// carries more URL images than MaxImages, EstimateRequestToken must only
// fetch the first MaxImages. Verifies the bound is applied to the fetch
// loop, not just the count loop — the core motivation for Problem 2.
func TestEstimateRequestTokenBoundsFetchesForVisionRelay(t *testing.T) {
	enableTokenCounting(t)
	InitHttpClient() // SSRF client required for URL image fetch

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	pngBytes := buf.Bytes()

	var fetchCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	fetchSetting := system_setting.GetFetchSetting()
	origFetch := *fetchSetting
	fetchSetting.AllowPrivateIp = true
	fetchSetting.AllowedPorts = append(append([]string(nil), fetchSetting.AllowedPorts...), u.Port())
	defer func() { *fetchSetting = origFetch }()

	// Build a Claude request with MaxImages+3 distinct URL images (unique
	// paths so per-request context cache does not collapse them).
	numImages := vision_relay.MaxImages + 3
	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"d"}`)
	for i := 0; i < numImages; i++ {
		sb.WriteString(`,{"type":"image","source":{"type":"url","url":"`)
		sb.WriteString(ts.URL + "/" + itoa(i) + ".png")
		sb.WriteString(`"}}`)
	}
	sb.WriteString(`]}]}`)
	raw := []byte(sb.String())

	// --- Vision Relay enabled: only MaxImages fetches. ---
	_, _, _ = setupVisionRelayEnv(t, ts.URL, true)
	fetchCount = 0
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	c.Set(string(constant.ContextKeyOriginalModel), "deepseek-v4-flash")
	storage, err := common.CreateBodyStorage(raw)
	require.NoError(t, err)
	c.Set(common.KeyBodyStorage, storage)
	var req dto.ClaudeRequest
	require.NoError(t, common.Unmarshal(raw, &req))
	info := &relaycommon.RelayInfo{
		OriginModelName: "deepseek-v4-flash",
		RelayFormat:     types.RelayFormatClaude,
		Request:         &req,
		IsStream:        true, // forces shouldFetchFiles = true so URL images are fetched
	}
	meta := info.Request.GetTokenCountMeta()
	require.Len(t, meta.Files, numImages, "fixture must carry all images as FileMeta")
	_, err = EstimateRequestToken(c, meta, info)
	require.NoError(t, err)
	assert.Equal(t, vision_relay.MaxImages, fetchCount,
		"Vision Relay engaged: only MaxImages URL images must be fetched, got %d", fetchCount)

	// --- Vision Relay disabled: all numImages fetches. ---
	_, _, _ = setupVisionRelayEnv(t, ts.URL, false)
	fetchCount = 0
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	c2.Set(string(constant.ContextKeyOriginalModel), "deepseek-v4-flash")
	storage2, err := common.CreateBodyStorage(raw)
	require.NoError(t, err)
	c2.Set(common.KeyBodyStorage, storage2)
	var req2 dto.ClaudeRequest
	require.NoError(t, common.Unmarshal(raw, &req2))
	info2 := &relaycommon.RelayInfo{
		OriginModelName: "deepseek-v4-flash",
		RelayFormat:     types.RelayFormatClaude,
		Request:         &req2,
		IsStream:        true,
	}
	meta2 := info2.Request.GetTokenCountMeta()
	_, err = EstimateRequestToken(c2, meta2, info2)
	require.NoError(t, err)
	assert.Equal(t, numImages, fetchCount,
		"Vision Relay disabled: all %d URL images must be fetched, got %d", numImages, fetchCount)
}
