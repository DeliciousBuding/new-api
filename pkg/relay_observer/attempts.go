package relayobserver

import "net/http"

// This file implements the request-local bounded attempt collector. Downstream
// channel attempts are turn metadata (SSOT Event And Retry Semantics): one
// client request creates at most one turn, and the retry loop records a
// bounded attempt summary per attempt. The collector is a plain in-memory
// accumulator owned by the request path — the worker never touches it — and
// its snapshot is merged into the final turn event at settlement.

// Attempt bounds mirror the SSOT Runtime Limits row "attempts/turn": default
// 8, hard maximum 16. A sequence beyond the cap retains the first max-1
// entries plus the final attempt; every displaced entry is counted omitted.
const (
	// DefaultMaxAttempts is the per-turn attempt entry cap of the default
	// runtime configuration.
	DefaultMaxAttempts = 8
	// MaxMaxAttempts is the hard maximum any configured cap is clamped to.
	MaxMaxAttempts = 16
)

// AttemptAccumulator is the request-local, bounded attempt collector. It is
// not safe for concurrent use: the retry loop appends synchronously on the
// request path and the settlement hook reads one snapshot at the end.
type AttemptAccumulator struct {
	attempts []AttemptSummary
	omitted  int
	max      int
}

// NewAttemptAccumulator builds an accumulator with the given cap, clamped to
// [DefaultMaxAttempts, MaxMaxAttempts]: a non-positive request falls back to
// the default, an oversized request is capped at the hard maximum.
func NewAttemptAccumulator(max int) *AttemptAccumulator {
	if max <= 0 {
		max = DefaultMaxAttempts
	}
	if max > MaxMaxAttempts {
		max = MaxMaxAttempts
	}
	return &AttemptAccumulator{max: max}
}

// Add records one downstream attempt. At capacity the first max-1 entries are
// retained verbatim and the final slot rolls to the newest attempt (SSOT:
// "retain first 7 + final attempt"); the displaced entry is counted omitted.
func (a *AttemptAccumulator) Add(at AttemptSummary) {
	if len(a.attempts) < a.max {
		a.attempts = append(a.attempts, at)
		return
	}
	a.omitted++
	a.attempts[a.max-1] = at
}

// AddSuccessful records the turn's final successful downstream attempt at
// settlement time (SSOT: "retain first 7 + final attempt"). The settlement
// hook calls it once, after the last write to the parsed request and before
// the publish snapshot, so the successful attempt becomes the final retained
// entry of the published event. ObserveTurnAttemptEnd must not record the
// successful attempt again: on the success path the settlement publish runs
// before that hook, so only the settlement record can reach the event.
func (a *AttemptAccumulator) AddSuccessful(channelID int64, group string, elapsedMS int64) {
	a.Add(AttemptSummary{
		ChannelID:  channelID,
		Group:      group,
		StatusCode: http.StatusOK,
		ElapsedMS:  elapsedMS,
	})
}

// Len returns the number of retained attempt entries (excludes omitted ones).
func (a *AttemptAccumulator) Len() int { return len(a.attempts) }

// Snapshot returns an independent copy of the retained entries plus the count
// of omitted entries. The copy isolates the event from the accumulator, so
// the request path can keep appending without mutating a published event.
func (a *AttemptAccumulator) Snapshot() ([]AttemptSummary, int) {
	out := make([]AttemptSummary, len(a.attempts))
	copy(out, a.attempts)
	return out, a.omitted
}
