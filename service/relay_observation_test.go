package service

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/pkg/relay_observer"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
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
// the full hook flow on a valid context runs without panicking, records the
// failed attempt in the request-local accumulator, and publishes nothing (the
// disabled runtime drops the turn). This is the "wiring does not regress the
// disabled path" check.
func TestObserveTurnFlowWhileWiredDisabled(t *testing.T) {
	SetRelayObserverRuntime(relayobserver.NewRuntime())
	t.Cleanup(func() { SetRelayObserverRuntime(nil) })
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	// ChannelMeta must be initialized: ChannelId is a promoted field of the
	// embedded ChannelMeta pointer, so a nil embed dereferences inside the
	// settlement hook.
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
	// The failed attempt and the settlement-time successful attempt are both
	// retained in the request-local accumulator; only the publish is dropped.
	st := turnObserverStateFrom(ctx)
	require.NotNil(t, st)
	assert.Equal(t, 2, st.acc.Len())
}
