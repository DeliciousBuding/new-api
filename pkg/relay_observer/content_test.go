package relayobserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the incremental group logic (T2.3): the deterministic
// group planner (1 full + at most 8 deltas, rotation on the ninth delta), the
// common-prefix delta computation, and the one-hop reconstruction assembly.
// These are pure functions: the PostgreSQL adapter feeds them row data and
// persists their output. Every reject path returns a classified ContentError;
// reconstruction never silently produces wrong content.

// sessionDigests builds a digest list helper for group-planning tests.
func digestList(n int, prefix string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix
	}
	return out
}

// TestCommonPrefix locks the prefix computation: empty lists, a full match,
// a partial match, and a full divergence (compaction to zero).
func TestCommonPrefix(t *testing.T) {
	a := []string{"1", "2", "3"}
	tests := []struct {
		name string
		a, b []string
		want int
	}{
		{name: "empty both", a: nil, b: nil, want: 0},
		{name: "empty base", a: nil, b: []string{"1"}, want: 0},
		{name: "full match", a: a, b: []string{"1", "2", "3", "4"}, want: 3},
		{name: "partial match", a: a, b: []string{"1", "2", "9"}, want: 2},
		{name: "divergence at start", a: a, b: []string{"9"}, want: 0},
		{name: "shorter b inside", a: a, b: []string{"1", "2"}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, commonPrefix(tt.a, tt.b))
		})
	}
}

// TestPlanGroupFirstIsFull locks the group rule: a session without a head
// starts a full checkpoint with ordinal 0, and the full digest list is the
// whole turn.
func TestPlanGroupFirstIsFull(t *testing.T) {
	digests := []string{"a", "b", "c"}
	plan := planGroup(nil, nil, digests)
	assert.True(t, plan.rotate, "a new group always starts a full checkpoint")
	assert.Equal(t, groupFullOrdinal, plan.ordinal)
	assert.Equal(t, digests, plan.fullDigests)
	assert.Nil(t, plan.suffix, "a full carries no suffix")
}

// TestPlanGroupDeltaAfterFull locks the delta rule: a head on a full with
// room left yields ordinal head+1 with the common prefix and the new suffix.
func TestPlanGroupDeltaAfterFull(t *testing.T) {
	head := &sessionHead{checkpointID: 7, ordinal: 0}
	fullDigests := []string{"a", "b", "c"}
	newDigests := []string{"a", "b", "c", "d"}

	plan := planGroup(head, fullDigests, newDigests)
	assert.False(t, plan.rotate)
	assert.Equal(t, 1, plan.ordinal)
	assert.Equal(t, int64(7), plan.checkpointID, "a delta points at the full checkpoint")
	assert.Equal(t, 3, plan.prefixCount)
	assert.Equal(t, []string{"d"}, plan.suffix)
	assert.Equal(t, newDigests, plan.fullDigests)
}

// TestPlanGroupDeltaDivergence locks the compaction rule: the prefix may
// shrink to zero, the suffix then covers the whole new list.
func TestPlanGroupDeltaDivergence(t *testing.T) {
	head := &sessionHead{checkpointID: 7, ordinal: 2}
	fullDigests := []string{"a", "b", "c"}
	newDigests := []string{"x", "y"}

	plan := planGroup(head, fullDigests, newDigests)
	assert.False(t, plan.rotate)
	assert.Equal(t, 3, plan.ordinal)
	assert.Equal(t, 0, plan.prefixCount, "a diverged prefix compacts to zero")
	assert.Equal(t, newDigests, plan.suffix)
}

// TestPlanGroupRotatesOnNinthDelta locks the rotation rule: the ninth delta
// (head ordinal 8) starts a new full checkpoint instead of a ninth delta, so
// a group never holds more than one full plus eight deltas.
func TestPlanGroupRotatesOnNinthDelta(t *testing.T) {
	head := &sessionHead{checkpointID: 7, ordinal: 8}
	digests := []string{"a", "b", "c"}

	plan := planGroup(head, digestList(3, "old"), digests)
	assert.True(t, plan.rotate, "the ninth delta must rotate to a new full")
	assert.Equal(t, groupFullOrdinal, plan.ordinal)
	assert.Equal(t, digests, plan.fullDigests)
}

// TestPlanGroupOrdinalRange locks the invariant: every planned ordinal stays
// inside 0..8 regardless of head state.
func TestPlanGroupOrdinalRange(t *testing.T) {
	for ordinal := 0; ordinal <= 8; ordinal++ {
		head := &sessionHead{checkpointID: 7, ordinal: ordinal}
		plan := planGroup(head, digestList(2, "base"), digestList(2, "new"))
		assert.LessOrEqual(t, plan.ordinal, 8)
		assert.GreaterOrEqual(t, plan.ordinal, 0)
	}
}

// TestAssembleDigests locks the one-hop reconstruction: the full digest list
// plus a delta's prefix count and suffix reproduces the turn's complete list.
func TestAssembleDigests(t *testing.T) {
	full := []string{"a", "b", "c", "d"}
	got, err := assembleDigests(full, 3, []string{"e", "f"}, 5)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c", "e", "f"}, got)
}

// TestAssembleDigestsRejectsPrefixOverflow proves a delta whose prefix count
// exceeds the base full list is rejected as corrupt: the delta cannot be
// derived from this base and must never assemble silently.
func TestAssembleDigestsRejectsPrefixOverflow(t *testing.T) {
	full := []string{"a", "b"}
	_, err := assembleDigests(full, 3, []string{"c"}, 3)
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrCorruptDelta, code)
}

// TestAssembleDigestsRejectsCountMismatch proves a delta whose item count
// does not equal prefix plus suffix is rejected as corrupt.
func TestAssembleDigestsRejectsCountMismatch(t *testing.T) {
	full := []string{"a", "b", "c"}
	_, err := assembleDigests(full, 2, []string{"x"}, 2) // 2+1 != 2
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrCorruptDelta, code)
}

// TestAssembleDigestsZeroSuffix locks the degenerate delta: a zero prefix and
// an empty suffix still reconstruct when the counts line up.
func TestAssembleDigestsZeroSuffix(t *testing.T) {
	got, err := assembleDigests(nil, 0, nil, 0)
	require.NoError(t, err)
	assert.Empty(t, got)
}
