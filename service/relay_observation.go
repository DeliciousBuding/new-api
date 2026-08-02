package service

import (
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	relayobserver "github.com/QuantumNous/new-api/pkg/relay_observer"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

// This file wires the request-path observer hooks into the relay settlement
// flow. It mirrors the controller's wiring face (controller/relay_observer.go):
// main injects the process-level observer runtime once after Init, and every
// hook is a no-op with zero allocation while the observer is unwired or
// disabled. The hooks are passive instrumentation: they never change a retry,
// quota operation, channel decision, or log write, and they only enqueue a
// turn event after the last write to the parsed request on that path.

var (
	relayObserverMu sync.RWMutex
	relayObserverRT *relayobserver.Runtime
)

// SetRelayObserverRuntime wires the process-level observer runtime into the
// request-path hooks. main calls it once after observerRuntime.Init(), next
// to controller.SetRelayObserverRuntime; tests inject controlled runtimes
// (and nil to restore the unwired state). Concurrency-safe.
func SetRelayObserverRuntime(rt *relayobserver.Runtime) {
	relayObserverMu.Lock()
	defer relayObserverMu.Unlock()
	relayObserverRT = rt
}

// relayObserverSnapshot returns the wired runtime, or nil when the observer
// is not wired. Every hook starts with this check — the visible zero-cost
// branch of the disabled path — before touching any request state.
func relayObserverSnapshot() *relayobserver.Runtime {
	relayObserverMu.RLock()
	rt := relayObserverRT
	relayObserverMu.RUnlock()
	return rt
}

// turnObserverKey is the gin-context key of the request-local turn state
// (attempt accumulator plus the current attempt's timing window). The state
// lives on the request path only; the worker never sees it.
const turnObserverKey = "relay_observer_turn"

type turnObserverState struct {
	acc   *relayobserver.AttemptAccumulator
	begin time.Time
}

// turnObserverStateFrom returns the request-local turn state, or nil when the
// current turn never started one (observer unwired, or no attempt observed).
func turnObserverStateFrom(c *gin.Context) *turnObserverState {
	st, ok := c.Get(turnObserverKey)
	if !ok {
		return nil
	}
	s, _ := st.(*turnObserverState)
	return s
}

// TurnUsage is the settlement-time usage snapshot handed to the observer
// hook. It is a plain value copy: the hook reads it once and keeps no
// pointer into billing state.
type TurnUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Quota            int64
}

// ObserveTurnAttemptBegin opens the timing window of one downstream attempt
// and lazily creates the request-local attempt accumulator on the first
// attempt of the turn. No-op while the observer is unwired.
func ObserveTurnAttemptBegin(c *gin.Context) {
	defer func() {
		if recover() != nil {
			// Defensive (SSOT: all observer entry points recover panics): a
			// panic in the hook must never propagate to the request path. The
			// turn state is request-local, so losing it fails this turn's
			// observation only.
			common.SysError("relay observer: ObserveTurnAttemptBegin recovered from panic")
		}
	}()
	if relayObserverSnapshot() == nil {
		return
	}
	now := time.Now()
	if st := turnObserverStateFrom(c); st != nil {
		st.begin = now
		return
	}
	c.Set(turnObserverKey, &turnObserverState{
		acc:   relayobserver.NewAttemptAccumulator(relayobserver.DefaultMaxAttempts),
		begin: now,
	})
}

// ObserveTurnAttemptEnd records one bounded downstream attempt summary:
// channel id, group, status/error code (stable classification only — never
// the raw error text), and the attempt's elapsed milliseconds. The successful
// attempt is skipped here: on the success path the settlement publish
// (ObserveTurnSettlement) runs before this hook, so it could never reach the
// published event — the settlement hook owns and records the successful
// attempt once, before its own snapshot. No-op while the observer is unwired
// or when no turn state exists.
func ObserveTurnAttemptEnd(c *gin.Context, info *relaycommon.RelayInfo, channelID int, attemptErr *types.NewAPIError) {
	defer func() {
		if recover() != nil {
			// Defensive (SSOT: all observer entry points recover panics): a
			// panic in the hook must never propagate to the request path. The
			// missed attempt fails this turn's observation only.
			common.SysError("relay observer: ObserveTurnAttemptEnd recovered from panic")
		}
	}()
	st := turnObserverStateFrom(c)
	if st == nil {
		return
	}
	if attemptErr == nil {
		return
	}
	elapsedMS := time.Since(st.begin).Milliseconds()
	if elapsedMS < 0 {
		elapsedMS = 0
	}
	st.acc.Add(relayobserver.AttemptSummary{
		ChannelID:  int64(channelID),
		ElapsedMS:  elapsedMS,
		Group:      info.UsingGroup,
		StatusCode: attemptErr.StatusCode,
		ErrorCode:  string(attemptErr.GetErrorCode()),
	})
}

// ObserveTurnSettlement publishes the final turn event of the success path.
// It must be called after settlement and the consume log write — the last
// write to the parsed request — and publishes exactly once per client
// request. The turn's final successful attempt is recorded here, before the
// publish snapshot, so the event carries it as the final retained entry
// (SSOT: "retain first 7 + final attempt"); AttemptEnd skips the successful
// round. No-op while the observer is unwired.
func ObserveTurnSettlement(c *gin.Context, info *relaycommon.RelayInfo, usage TurnUsage) {
	defer func() {
		if recover() != nil {
			// Defensive (SSOT: all observer entry points recover panics): a
			// panic in the hook must never propagate to the request path or
			// change the settlement outcome. The turn event is dropped.
			common.SysError("relay observer: ObserveTurnSettlement recovered from panic")
		}
	}()
	if relayObserverSnapshot() == nil {
		return
	}
	if st := turnObserverStateFrom(c); st != nil {
		elapsedMS := time.Since(st.begin).Milliseconds()
		if elapsedMS < 0 {
			elapsedMS = 0
		}
		st.acc.AddSuccessful(int64(info.ChannelId), info.UsingGroup, elapsedMS)
	}
	ev := buildTurnEvent(c, info, usage, true)
	publishTurnEvent(c, relayObserverSnapshot(), ev)
}

// ObserveTurnFailure publishes the final turn event of the failure path: the
// retry loop exhausted, LastError set. It is mutually exclusive with
// ObserveTurnSettlement, so a turn is published exactly once. No-op while
// the observer is unwired.
func ObserveTurnFailure(c *gin.Context, info *relaycommon.RelayInfo) {
	defer func() {
		if recover() != nil {
			// Defensive (SSOT: all observer entry points recover panics): a
			// panic in the hook must never propagate to the request path or
			// change the failure outcome. The turn event is dropped.
			common.SysError("relay observer: ObserveTurnFailure recovered from panic")
		}
	}()
	if relayObserverSnapshot() == nil {
		return
	}
	ev := buildTurnEvent(c, info, TurnUsage{}, false)
	publishTurnEvent(c, relayObserverSnapshot(), ev)
}

// buildTurnEvent assembles the frozen turn event from request-path state. It
// is a synchronous pure-memory construction: no database, no blocking, no
// request copies. The event carries no endpoint, URL, credential, or raw
// error text — only stable classifications.
func buildTurnEvent(c *gin.Context, info *relaycommon.RelayInfo, usage TurnUsage, success bool) relayobserver.Event {
	ev := relayobserver.Event{
		EventID:          info.RequestId,
		NodeScope:        common.NodeName,
		OccurredAt:       time.Now(),
		UserID:           int64(info.UserId),
		TokenID:          int64(info.TokenId),
		Model:            info.OriginModelName,
		UpstreamModel:    info.UpstreamModelName,
		RelayFormat:      string(info.GetFinalRequestRelayFormat()),
		Success:          success,
		StatusCode:       200,
		LatencyMS:        time.Since(info.StartTime).Milliseconds(),
		Stream:           info.IsStream,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CachedTokens:     usage.CachedTokens,
		Quota:            usage.Quota,
		IPTrust:          relayobserver.IPTrustNone,
		// No content capture exists in this phase, so every turn is
		// metadata-only; the content capture phases fill "full" instead.
		ContentState: relayobserver.ContentStateMetadataOnly,
	}
	if info.HasSendResponse() {
		ev.FirstResponseMS = info.FirstResponseTime.Sub(info.StartTime).Milliseconds()
	}
	if !success && info.LastError != nil {
		ev.StatusCode = info.LastError.StatusCode
		ev.ErrorType = string(info.LastError.GetErrorType())
		ev.ErrorCode = string(info.LastError.GetErrorCode())
	}
	// Dual-opt-in IP capture (SSOT IP And GeoIP): the effective trust tier of
	// the running configuration comes from the runtime status, which is
	// "none" unless both opt-ins hold. Capture is in-memory only — the
	// peer string is parsed into the event; persistence lands with T2.3, and
	// no GeoIP lookup happens here.
	status := relayObserverSnapshot().Status()
	if status.Enabled && status.IPTrust != relayobserver.IPTrustNone {
		ev.ClientIP, ev.IPTrust = relayobserver.CaptureClientIP(status.IPTrust, c.ClientIP())
	}
	if st := turnObserverStateFrom(c); st != nil {
		ev.Attempts, ev.AttemptsOmitted = st.acc.Snapshot()
	}
	return ev
}

// publishTurnEvent hands the final event to the bounded dispatcher with the
// body size as the admission reservation (SSOT: the admission reservation
// uses the existing BodyStorage.Size() value). A missing or failed body
// storage falls back to a zero reservation; the dispatcher clamps oversized
// reservations at the per-request cap.
func publishTurnEvent(c *gin.Context, rt *relayobserver.Runtime, ev relayobserver.Event) {
	reservation := int64(0)
	if bs, err := common.GetBodyStorage(c); err == nil && bs != nil {
		reservation = bs.Size()
	}
	rt.TryPublishTurn(ev, reservation)
}
