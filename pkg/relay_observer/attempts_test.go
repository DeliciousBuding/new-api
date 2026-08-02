package relayobserver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAttemptAccumulatorBoundedMerge is the retry-storm contract: a synthetic
// 100-attempt sequence merges into one accumulator with at most the default
// cap entries — first 7 attempts retained verbatim plus the final attempt —
// and every displaced entry is counted as omitted (SSOT Runtime Limits:
// attempts/turn 8, "retain first 7 + final attempt; count omitted").
func TestAttemptAccumulatorBoundedMerge(t *testing.T) {
	acc := NewAttemptAccumulator(DefaultMaxAttempts)
	for i := 1; i <= 100; i++ {
		acc.Add(AttemptSummary{
			ChannelID:  int64(i),
			Group:      "default",
			StatusCode: 429,
			ErrorCode:  fmt.Sprintf("attempt-%d", i),
			ElapsedMS:  int64(i),
		})
	}
	require.Equal(t, DefaultMaxAttempts, acc.Len())
	entries, omitted := acc.Snapshot()
	require.Len(t, entries, DefaultMaxAttempts)
	assert.Equal(t, 92, omitted)
	// The first 7 entries are retained verbatim.
	for i := 0; i < 7; i++ {
		assert.Equal(t, int64(i+1), entries[i].ChannelID)
		assert.Equal(t, fmt.Sprintf("attempt-%d", i+1), entries[i].ErrorCode)
	}
	// The final slot rolls to the newest attempt, not the 8th one.
	assert.Equal(t, int64(100), entries[7].ChannelID)
	assert.Equal(t, "attempt-100", entries[7].ErrorCode)
}

// TestAttemptAccumulatorExactlyAtCap keeps every entry when the sequence
// never exceeds the cap: no omission and the final attempt is the cap-th one.
func TestAttemptAccumulatorExactlyAtCap(t *testing.T) {
	acc := NewAttemptAccumulator(DefaultMaxAttempts)
	for i := 1; i <= DefaultMaxAttempts; i++ {
		acc.Add(AttemptSummary{ChannelID: int64(i), Group: "g"})
	}
	require.Equal(t, DefaultMaxAttempts, acc.Len())
	entries, omitted := acc.Snapshot()
	assert.Len(t, entries, DefaultMaxAttempts)
	assert.Equal(t, 0, omitted)
	assert.Equal(t, int64(DefaultMaxAttempts), entries[DefaultMaxAttempts-1].ChannelID)
}

// TestAttemptAccumulatorSingleAttempt covers the default RetryTimes=0 shape:
// one downstream attempt still produces one bounded entry.
func TestAttemptAccumulatorSingleAttempt(t *testing.T) {
	acc := NewAttemptAccumulator(DefaultMaxAttempts)
	acc.Add(AttemptSummary{ChannelID: 7, Group: "default", StatusCode: 200, ElapsedMS: 5})
	require.Equal(t, 1, acc.Len())
	entries, omitted := acc.Snapshot()
	assert.Len(t, entries, 1)
	assert.Equal(t, 0, omitted)
	assert.Equal(t, int64(7), entries[0].ChannelID)
}

// TestAttemptAccumulatorHardCap clamps a configured cap above the hard
// maximum: attempts never exceed MaxMaxAttempts (SSOT hard maximum 16).
func TestAttemptAccumulatorHardCap(t *testing.T) {
	acc := NewAttemptAccumulator(1000)
	for i := 1; i <= 100; i++ {
		acc.Add(AttemptSummary{ChannelID: int64(i), Group: "g"})
	}
	require.Equal(t, MaxMaxAttempts, acc.Len())
	entries, omitted := acc.Snapshot()
	assert.Len(t, entries, MaxMaxAttempts)
	assert.Equal(t, 100-MaxMaxAttempts, omitted)
}

// TestAttemptAccumulatorNonPositiveCapFallsBackToDefault locks the default
// for a zero or negative cap request.
func TestAttemptAccumulatorNonPositiveCapFallsBackToDefault(t *testing.T) {
	acc := NewAttemptAccumulator(0)
	for i := 1; i <= 30; i++ {
		acc.Add(AttemptSummary{ChannelID: int64(i), Group: "g"})
	}
	assert.Equal(t, DefaultMaxAttempts, acc.Len())
}

// TestAttemptAccumulatorAddSuccessfulFinalAttempt locks the settlement-time
// successful attempt contract (SSOT "retain first 7 + final attempt"): the
// final successful attempt is recorded once by the settlement hook via
// AddSuccessful and becomes the final retained entry, with a 200 status and
// no error code, while earlier failure attempts stay verbatim.
func TestAttemptAccumulatorAddSuccessfulFinalAttempt(t *testing.T) {
	acc := NewAttemptAccumulator(DefaultMaxAttempts)
	acc.Add(AttemptSummary{ChannelID: 1, Group: "g", StatusCode: 429, ErrorCode: "rate_limit"})
	acc.AddSuccessful(2, "g", 40)
	entries, omitted := acc.Snapshot()
	require.Len(t, entries, 2)
	assert.Equal(t, 0, omitted)
	assert.Equal(t, int64(1), entries[0].ChannelID)
	assert.Equal(t, 429, entries[0].StatusCode)
	assert.Equal(t, int64(2), entries[1].ChannelID)
	assert.Equal(t, 200, entries[1].StatusCode)
	assert.Empty(t, entries[1].ErrorCode)
	assert.Equal(t, int64(40), entries[1].ElapsedMS)
}

// TestAttemptAccumulatorAddSuccessfulInHundredSequence is the retry-storm
// shape with a successful final round: 99 failed attempts plus the settlement
// successful attempt merge into at most the default cap with the successful
// round as the final retained entry and 92 omitted.
func TestAttemptAccumulatorAddSuccessfulInHundredSequence(t *testing.T) {
	acc := NewAttemptAccumulator(DefaultMaxAttempts)
	for i := 1; i <= 99; i++ {
		acc.Add(AttemptSummary{ChannelID: int64(i), Group: "g", StatusCode: 429, ErrorCode: "rate_limit"})
	}
	acc.AddSuccessful(100, "g", 99)
	entries, omitted := acc.Snapshot()
	require.Len(t, entries, DefaultMaxAttempts)
	assert.Equal(t, 92, omitted)
	assert.Equal(t, int64(100), entries[7].ChannelID)
	assert.Equal(t, 200, entries[7].StatusCode)
	assert.Empty(t, entries[7].ErrorCode)
}

// TestAttemptAccumulatorSnapshotIsCopy proves Snapshot returns an independent
// copy: mutating it must not mutate the accumulator's internal state.
func TestAttemptAccumulatorSnapshotIsCopy(t *testing.T) {
	acc := NewAttemptAccumulator(DefaultMaxAttempts)
	acc.Add(AttemptSummary{ChannelID: 1, Group: "g", ErrorCode: "orig"})
	entries, _ := acc.Snapshot()
	entries[0].ErrorCode = "mutated"

	again, _ := acc.Snapshot()
	assert.Equal(t, "orig", again[0].ErrorCode)
	assert.Equal(t, 1, acc.Len())
}
