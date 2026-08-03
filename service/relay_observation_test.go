package service

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/relay_observer"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the service wiring face (SetRelayObserverRuntime /
// relayObserverSnapshot) and the request-path observer hooks that
// controller/relay.go and the settlement paths call. The hooks must be
// strictly fail-open: unwired they are no-ops, and a panic inside any entry
// point is recovered so it never reaches the request path (SSOT: all observer
// entry points recover panics).

// TestSetRelayObserverRuntimeWiresAndRestores locks the service wiring face:
// the setter installs the exact runtime instance the snapshot getter returns,
// and nil restores the unwired process state. main calls the setter once next
// to controller.SetRelayObserverRuntime.
func TestSetRelayObserverRuntimeWiresAndRestores(t *testing.T) {
	require.Nil(t, relayObserverSnapshot())
	rt := relayobserver.NewRuntime()
	SetRelayObserverRuntime(rt)
	require.Same(t, rt, relayObserverSnapshot())
	SetRelayObserverRuntime(nil)
	require.Nil(t, relayObserverSnapshot())
}

// TestObserveHooksNoopWhileUnwired is the unwired-process contract: with no
// runtime installed every hook returns before touching request state, and no
// turn state is created on the context.
func TestObserveHooksNoopWhileUnwired(t *testing.T) {
	require.Nil(t, relayObserverSnapshot())
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.NotPanics(t, func() {
		ObserveTurnAttemptBegin(ctx)
		ObserveTurnAttemptEnd(ctx, nil, 1, nil)
		ObserveTurnSettlement(ctx, nil, TurnUsage{})
		ObserveTurnFailure(ctx, nil)
	})
	require.Nil(t, turnObserverStateFrom(ctx))
}

// TestObserveHookEntryPointsRecoverPanics is the entry-point recover contract
// (SSOT: all observer entry points recover panics): a nil context dereferences
// inside each hook, and the recover must absorb the panic so it never
// propagates to the request path (the controller/relay.go call sites). The
// settlement hook gets a second injection: a RelayInfo without ChannelMeta
// panics on the promoted ChannelId field, which is the realistic edge case the
// recover must also cover.
func TestObserveHookEntryPointsRecoverPanics(t *testing.T) {
	SetRelayObserverRuntime(relayobserver.NewRuntime())
	t.Cleanup(func() { SetRelayObserverRuntime(nil) })
	require.NotPanics(t, func() { ObserveTurnAttemptBegin(nil) })
	require.NotPanics(t, func() { ObserveTurnAttemptEnd(nil, nil, 1, nil) })
	require.NotPanics(t, func() { ObserveTurnSettlement(nil, nil, TurnUsage{}) })
	require.NotPanics(t, func() { ObserveTurnFailure(nil, nil) })
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{RequestId: "turn-no-meta"}
	require.NotPanics(t, func() { ObserveTurnSettlement(ctx, info, TurnUsage{}) })
	require.NotPanics(t, func() { ObserveTurnFailure(ctx, info) })
}

// TestObserveTurnFlowWhileWiredDisabled is the wired-but-disabled contract:
// the full hook flow on a valid context runs without panicking and allocates
// nothing — the disabled runtime short-circuits before any request-local
// state, accumulator, event construction, or body read is touched (the
// lock-free CanPublish fast path). This is the "wiring does not regress the
// disabled path" check: zero state, zero publish.
func TestObserveTurnFlowWhileWiredDisabled(t *testing.T) {
	SetRelayObserverRuntime(relayobserver.NewRuntime())
	t.Cleanup(func() { SetRelayObserverRuntime(nil) })
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		RequestId:   "turn-wired-disabled",
		UsingGroup:  "default",
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7},
	}
	require.NotPanics(t, func() {
		ObserveTurnAttemptBegin(ctx)
		ObserveTurnAttemptEnd(ctx, info, 7, types.NewError(errors.New("upstream rate limited"), types.ErrorCodePreConsumeTokenQuotaFailed))
		ObserveTurnSettlement(ctx, info, TurnUsage{PromptTokens: 10, CompletionTokens: 5})
	})
	// The disabled path leaves no request-local state behind: no accumulator
	// was created, nothing was recorded, nothing was published.
	st := turnObserverStateFrom(ctx)
	assert.Nil(t, st, "disabled path must not create request-local observer state")
}

// TestBuildTurnEventAttachesRequestAndIdentity is the T2.6 attach contract:
// buildTurnEvent rides the already parsed request DTO along as a zero-copy
// reference and snapshots the session-identity material (header map and body
// bytes) before the publish send, so the worker can normalize content and
// resolve aliases without the request path doing any marshaling or copying.
// A nil request keeps the metadata-only default.
func TestBuildTurnEventAttachesRequestAndIdentity(t *testing.T) {
	SetRelayObserverRuntime(relayobserver.NewRuntime())
	t.Cleanup(func() { SetRelayObserverRuntime(nil) })

	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { storage.Close() })

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set(common.KeyBodyStorage, storage)
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"thr-9"}`)

	req := dto.Request(&dto.GeneralOpenAIRequest{
		Model:    "gpt-5",
		Messages: []dto.Message{{Role: "user", Content: "hello"}},
	})
	info := &relaycommon.RelayInfo{
		RequestId:   "turn-attach",
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7},
		Request:     req,
	}

	ev := buildTurnEvent(ctx, info, TurnUsage{}, true)
	require.NotNil(t, ev.Request)
	assert.Same(t, req, *ev.Request) // the exact parsed reference, zero-copy
	// Identity carries the raw header map (worker-side alias resolution reads
	// it) and the body bytes snapshot.
	assert.Equal(t, `{"thread_id":"thr-9"}`, ev.Identity.Headers.Get("X-Codex-Turn-Metadata"))
	assert.Equal(t, body, ev.Identity.Body)
	assert.Equal(t, relayobserver.ContentStateMetadataOnly, ev.ContentState)

	// A requestless turn keeps the nil reference: the worker degrades it to
	// metadata-only instead of dereferencing.
	info.Request = nil
	ev = buildTurnEvent(ctx, info, TurnUsage{}, false)
	assert.Nil(t, ev.Request)
	assert.Equal(t, relayobserver.ContentStateMetadataOnly, ev.ContentState)
}

// TestBuildTurnEventKeepsOriginalCaptureFormatAcrossConversion locks the
// request/format pairing used by worker normalization. The persisted turn
// keeps the final upstream format, while the zero-copy original Claude DTO is
// normalized with its original client format.
func TestBuildTurnEventKeepsOriginalCaptureFormatAcrossConversion(t *testing.T) {
	SetRelayObserverRuntime(relayobserver.NewRuntime())
	t.Cleanup(func() { SetRelayObserverRuntime(nil) })

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	req := dto.Request(&dto.ClaudeRequest{
		Model:    "claude-test",
		Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
	})
	info := &relaycommon.RelayInfo{
		RequestId:              "turn-claude-converted",
		RelayFormat:            types.RelayFormatClaude,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI},
		ChannelMeta:            &relaycommon.ChannelMeta{ChannelId: 7},
		Request:                req,
	}

	ev := buildTurnEvent(ctx, info, TurnUsage{}, true)
	assert.Equal(t, string(types.RelayFormatOpenAI), ev.RelayFormat)
	assert.Equal(t, string(types.RelayFormatClaude), ev.CaptureRelayFormat)
	require.NotNil(t, ev.Request)
	assert.Same(t, req, *ev.Request)
}
