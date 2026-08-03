package relayobserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// This file hosts the T5.2 p99 delta harness: it measures the observer's
// incremental latency on the relay path by running the request-path observer
// mount segment — the event construction and the bounded publish — in both
// the disabled and the enabled state, against a fixed golden corpus and a
// fake upstream. Results are reported only, never judged against a hard
// threshold: the SSOT p99 target (<1 ms) is recorded as measured.
//
// The corpus is a real relay request from the relaykit golden matrix; the
// fake upstream is an httptest server answering an OpenAI chat completion
// (its latency is the same in both states, so the delta isolates the
// observer segment). The enabled state uses a real Dispatcher over a scripted
// store; the disabled state is the unwired runtime the hooks see while the
// observer is not configured.

// p99DeltaIterations is the harness sample count: enough for a stable p99
// percentile while staying far inside the fixed-corpus budget (no long
// runs, no sleeps in the measured segment).
const p99DeltaIterations = 200

// p99DeltaBatch is the operation count per sample. The local wall clock
// granularity is coarse on this Windows host (measured ~400 µs ticks, so a
// single observer publish — sub-microsecond — reads as zero), therefore each
// sample is the mean of one batch of consecutive operations sized so the
// batch wall time (~2-6 ms) dominates the clock granularity. The p99 then
// captures tail segments (GC, scheduling) that slow a whole batch, and the
// per-operation mean keeps microsecond resolution.
const p99DeltaBatch = 20000

// goldenRequestPath is the fixed corpus file: a real chat completion request
// from the relaykit conversion matrix.
const goldenRequestPath = "../../relaykit/relayconvert/testdata/golden/request/openai_to_openai_responses.golden.json"

// TestRelayObserverP99DeltaHarness measures and reports the observer p99
// delta on the relay path. See the file comment for the contract.
func TestRelayObserverP99DeltaHarness(t *testing.T) {
	corpus, err := os.ReadFile(goldenRequestPath)
	requireNoError(t, err, "golden corpus must be readable: %s", goldenRequestPath)
	if len(corpus) == 0 {
		t.Fatalf("golden corpus %s is empty", goldenRequestPath)
	}

	// Fake upstream: an httptest server answering a fixed chat completion.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"id":"bench","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	// Baseline: one fake-upstream round trip, same for both states.
	baseStart := time.Now()
	resp, err := http.Post(upstream.URL, "application/json", nil)
	requireNoError(t, err, "fake upstream must answer")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	upstreamBase := time.Since(baseStart)

	bs, err := common.CreateBodyStorage(corpus)
	requireNoError(t, err, "body storage over the corpus must construct")

	disabled := measureObserverSegment(t, false, corpus, bs)
	enabled := measureObserverSegment(t, true, corpus, bs)

	p50D, p99D := percentiles(disabled)
	p50E, p99E := percentiles(enabled)

	t.Logf("p99 delta harness (corpus=%d bytes, upstream round trip=%s, n=%d):",
		len(corpus), upstreamBase, p99DeltaIterations)
	t.Logf("  disabled: median=%s p99=%s", p50D, p99D)
	t.Logf("  enabled : median=%s p99=%s", p50E, p99E)
	t.Logf("  delta   : median=%s p99=%s (SSOT target <1ms: %s)",
		p50E-p50D, p99E-p99D, verdict(p99E-p99D))
}

// measureObserverSegment runs one state's observer mount segment
// p99DeltaIterations times and returns the per-iteration latencies.
func measureObserverSegment(t *testing.T, enabled bool, corpus []byte, bs common.BodyStorage) []time.Duration {
	t.Helper()
	var rt *Runtime
	var disp *Dispatcher
	if enabled {
		rt = NewRuntime()
		cfg := DefaultConfig()
		disp = NewDispatcher(cfg, &scriptedStore{})
		disp.Start()
		rt.mu.Lock()
		rt.state = stateEnabled
		rt.disp = disp
		rt.mu.Unlock()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			disp.Stop(ctx)
		})
	} else {
		rt = NewRuntime() // unwired: every hook and publish is a no-op
	}

	samples := make([]time.Duration, 0, p99DeltaIterations)
	for i := 0; i < p99DeltaIterations; i++ {
		start := time.Now()
		for j := 0; j < p99DeltaBatch; j++ {
			if enabled {
				// The relay-path observer segment: event construction (the
				// settlement hook's build step) plus the bounded publish with
				// the body-size reservation.
				ev := harnessEvent(corpus)
				rt.TryPublishTurn(ev, bs.Size())
			} else {
				// Disabled: the hook nil-check and the unwired publish.
				rt.TryPublishTurn(Event{}, 0)
			}
		}
		samples = append(samples, time.Since(start)/p99DeltaBatch)
	}
	return samples
}

// harnessEvent mirrors the field set the settlement hook constructs for one
// turn; only the byte cost matters for the harness, never the values.
func harnessEvent(corpus []byte) Event {
	return Event{
		EventID:          "bench-req",
		NodeScope:        "bench-node",
		OccurredAt:       time.Now(),
		UserID:           1,
		TokenID:          1,
		Model:            "gpt-5",
		UpstreamModel:    "gpt-5",
		RelayFormat:      "openai",
		Success:          true,
		StatusCode:       200,
		LatencyMS:        12,
		FirstResponseMS:  9,
		PromptTokens:     40,
		CompletionTokens: 3,
		Quota:            4000,
		IPTrust:          IPTrustNone,
		ContentState:     ContentStateMetadataOnly,
	}
}

// percentiles returns the median and the p99 of a sorted sample set.
func percentiles(samples []time.Duration) (p50, p99 time.Duration) {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	p50 = sorted[n/2]
	idx99 := (n * 99) / 100
	if idx99 >= n {
		idx99 = n - 1
	}
	p99 = sorted[idx99]
	return p50, p99
}

func verdict(delta time.Duration) string {
	if delta < time.Millisecond {
		return "within target"
	}
	return "above target (reported, not judged)"
}

func requireNoError(t *testing.T, err error, msg string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf(msg+": %v", append(args, err)...)
	}
}
