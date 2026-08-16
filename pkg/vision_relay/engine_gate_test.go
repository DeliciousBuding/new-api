package vision_relay

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var decodeGateReservationMutex sync.Mutex

type imageFetchObservation struct {
	rawURL      string
	maxBytes    int64
	deadline    time.Time
	hasDeadline bool
}

type blockingImageFetcher struct {
	imageData    []byte
	mediaType    string
	fetchStarted chan<- imageFetchObservation
	fetchRelease <-chan struct{}
}

func (fetcher *blockingImageFetcher) Fetch(
	requestContext context.Context,
	rawURL string,
	maxBytes int64,
) ([]byte, string, error) {
	if int64(len(fetcher.imageData)) > maxBytes {
		return nil, "", ErrSizeLimit
	}

	deadline, hasDeadline := requestContext.Deadline()
	select {
	case fetcher.fetchStarted <- imageFetchObservation{
		rawURL:      rawURL,
		maxBytes:    maxBytes,
		deadline:    deadline,
		hasDeadline: hasDeadline,
	}:
	case <-requestContext.Done():
		return nil, "", requestContext.Err()
	}

	select {
	case <-fetcher.fetchRelease:
		return append([]byte(nil), fetcher.imageData...), fetcher.mediaType, nil
	case <-requestContext.Done():
		return nil, "", requestContext.Err()
	}
}

type engineEnhanceOutcome struct {
	enhancedBody []byte
	statistics   *Stats
	enhanceError error
}

func startClaudeEnhancement(
	requestContext context.Context,
	engine *Engine,
	requestBody []byte,
	config Config,
) <-chan engineEnhanceOutcome {
	outcomeChannel := make(chan engineEnhanceOutcome, 1)
	go func() {
		statistics := &Stats{}
		enhancedBody, enhanceError := engine.Enhance(
			requestContext,
			requestBody,
			FormatClaude,
			config,
			statistics,
		)
		outcomeChannel <- engineEnhanceOutcome{
			enhancedBody: enhancedBody,
			statistics:   statistics,
			enhanceError: enhanceError,
		}
	}()
	return outcomeChannel
}

func reserveGlobalDecodeSlots(t *testing.T, slotCount int) {
	t.Helper()
	require.GreaterOrEqual(t, slotCount, 0)
	require.LessOrEqual(t, slotCount, GlobalDecodeSlots)

	decodeGateReservationMutex.Lock()
	reservedDecodeSlots := make([]chan struct{}, 0, slotCount)
	t.Cleanup(func() {
		for _, reservedDecodeSlot := range reservedDecodeSlots {
			globalDecodeGate.release(reservedDecodeSlot)
		}
		decodeGateReservationMutex.Unlock()
	})

	reservationContext, cancelReservation := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelReservation()
	for slotIndex := 0; slotIndex < slotCount; slotIndex++ {
		reservedDecodeSlot, acquireError := globalDecodeGate.acquire(reservationContext)
		require.NoError(t, acquireError, "reserve global decode slot %d", slotIndex+1)
		reservedDecodeSlots = append(reservedDecodeSlots, reservedDecodeSlot)
	}
}

func TestEngineBlockedRemoteFetchDoesNotConsumeDecodeGate(t *testing.T) {
	reserveGlobalDecodeSlots(t, GlobalDecodeSlots-1)

	imageData := makePNG(t, 20, 20)
	fetchStarted := make(chan imageFetchObservation)
	fetchRelease := make(chan struct{})
	imageFetcher := &blockingImageFetcher{
		imageData:    imageData,
		mediaType:    "image/png",
		fetchStarted: fetchStarted,
		fetchRelease: fetchRelease,
	}

	var fetchReleaseOnce sync.Once
	releaseBlockedFetch := func() {
		fetchReleaseOnce.Do(func() {
			close(fetchRelease)
		})
	}
	t.Cleanup(releaseBlockedFetch)

	visionRequestStarted := make(chan struct{}, 2)
	visionServer := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		visionRequestStarted <- struct{}{}
		_, _ = responseWriter.Write([]byte(`{"choices":[{"message":{"content":"vision description"}}]}`))
	}))
	t.Cleanup(visionServer.Close)

	engine := &Engine{
		Client:  &VisionClient{HTTPClient: visionServer.Client()},
		Fetcher: imageFetcher,
	}
	config := testConfig(visionServer.URL)
	remoteRequestBody := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/blocked.png"}}]}]}`)
	base64RequestBody := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		base64.StdEncoding.EncodeToString(imageData) + `"}}]}]}`)

	requestContext, cancelRequest := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancelRequest)
	requestDeadline, hasRequestDeadline := requestContext.Deadline()
	require.True(t, hasRequestDeadline)
	remoteOutcomeChannel := startClaudeEnhancement(requestContext, engine, remoteRequestBody, config)

	var fetchObservation imageFetchObservation
	select {
	case fetchObservation = <-fetchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("remote fetch did not reach the blocking fetcher")
	}
	assert.Equal(t, "https://example.test/blocked.png", fetchObservation.rawURL)
	assert.Equal(t, int64(MaxDecodedBytes), fetchObservation.maxBytes)
	assert.True(t, fetchObservation.hasDeadline)
	assert.True(t, fetchObservation.deadline.Equal(requestDeadline), "fetch must inherit the request deadline")

	base64OutcomeChannel := startClaudeEnhancement(requestContext, engine, base64RequestBody, config)
	select {
	case <-visionRequestStarted:
		// The base64 request reached the vision endpoint while the remote fetch remained blocked.
	case <-time.After(5 * time.Second):
		t.Fatal("blocked remote fetch consumed the only decode slot")
	}

	select {
	case remoteOutcome := <-remoteOutcomeChannel:
		t.Fatalf("remote request completed before its fetch was released: %v", remoteOutcome.enhanceError)
	default:
	}

	releaseBlockedFetch()

	var base64Outcome engineEnhanceOutcome
	select {
	case base64Outcome = <-base64OutcomeChannel:
	case <-time.After(5 * time.Second):
		t.Fatal("base64 request did not finish after acquiring the decode slot")
	}
	var remoteOutcome engineEnhanceOutcome
	select {
	case remoteOutcome = <-remoteOutcomeChannel:
	case <-time.After(5 * time.Second):
		t.Fatal("remote request did not finish after its fetch was released")
	}

	require.NoError(t, base64Outcome.enhanceError)
	require.NoError(t, remoteOutcome.enhanceError)
	assert.Equal(t, 1, base64Outcome.statistics.Success)
	assert.Zero(t, base64Outcome.statistics.Failed)
	assert.Equal(t, 1, remoteOutcome.statistics.Success)
	assert.Zero(t, remoteOutcome.statistics.Failed)
	assert.NotContains(t, string(base64Outcome.enhancedBody), `"type":"image"`)
	assert.NotContains(t, string(remoteOutcome.enhancedBody), `"type":"image"`)
}

func TestEngineBase64PreparationAndDigestDoNotRequireDecodeGate(t *testing.T) {
	reserveGlobalDecodeSlots(t, GlobalDecodeSlots)

	imageData := makePNG(t, 20, 20)
	config := testConfig("http://vision.invalid")
	cachedDescription := "cached vision description"
	cache := &fakeDescriptionCache{hits: map[string]string{
		descriptionCacheKey(DigestBytes(imageData), BuildInstruction(config)): cachedDescription,
	}}
	engine := &Engine{Cache: cache}
	requestBody := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		base64.StdEncoding.EncodeToString(imageData) + `"}}]}]}`)

	requestContext, cancelRequest := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancelRequest)
	outcomeChannel := startClaudeEnhancement(requestContext, engine, requestBody, config)

	var outcome engineEnhanceOutcome
	select {
	case outcome = <-outcomeChannel:
		// A digest-dependent cache hit completes without waiting for an image decode slot.
	case <-time.After(5 * time.Second):
		t.Fatal("base64 acquisition or digest preparation waited for the decode gate")
	}

	require.NoError(t, outcome.enhanceError)
	assert.Equal(t, 1, outcome.statistics.Success)
	assert.Zero(t, outcome.statistics.Failed)
	assert.Equal(t, 1, outcome.statistics.UniqueImages)
	assert.Equal(t, 1, outcome.statistics.CacheServed)
	assert.Zero(t, outcome.statistics.VisionCalls)
	assert.Contains(t, string(outcome.enhancedBody), cachedDescription)
	assert.NotContains(t, string(outcome.enhancedBody), `"type":"image"`)
}
