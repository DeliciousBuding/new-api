package relayobserver

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the P0-C semantic selector (PR #17A): unit building,
// evidence selection, and the frozen invariants — tool pair atomicity, one
// gap, deterministic output, and a byte budget that is never exceeded.
//
// The unit tests are synthetic (inline canonical items, no fixture
// dependency); the two fixture tests at the bottom load the read-only
// agent-corpus samples and assert invariants only, never exact contents.

// ---------------------------------------------------------------------------
// test helpers

const testHMACPlaceholder = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" // 64 hex chars

// testPolicy returns a SelectionPolicy with a MeasureGap that includes a
// 64-byte HMAC placeholder (D1), so gap measurement is realistic.
func testPolicy(limit int64) SelectionPolicy {
	return SelectionPolicy{
		Limit:       limit,
		AnchorRatio: DefaultAnchorRatio,
		AnchorCap:   DefaultAnchorCap,
		MeasureGap:  testMeasureGap,
	}
}

func testMeasureGap(g GapInfo) (CanonicalItem, int64, error) {
	it := CanonicalItem{
		Kind:         CanonicalKindGap,
		LogicalBytes: g.LogicalBytes,
		Truncated:    true,
		Hmac:         testHMACPlaceholder,
	}
	payload, err := json.Marshal(it)
	if err != nil {
		return it, 0, err
	}
	return it, int64(len(payload)), nil
}

func sp(text string) CanonicalPart {
	return CanonicalPart{Type: partTypeText, Text: text}
}

func callp(id string) CanonicalPart {
	return CanonicalPart{Type: partTypeToolCall, Call: &ToolCallRef{ID: id, Name: "fx_tool", Arguments: json.RawMessage(`{}`)}}
}

func resp(id string) CanonicalPart {
	return CanonicalPart{Type: partTypeToolResult, Result: &ToolResultRef{ToolCallID: id, Output: json.RawMessage(`{}`)}}
}

func mkItem(kind, role, toolCallID string, logical int64, parts ...CanonicalPart) CanonicalItem {
	return CanonicalItem{Kind: kind, Role: role, ToolCallID: toolCallID, Content: parts, LogicalBytes: logical, Hmac: "h-" + kind + "-" + role}
}

func mkMsg(role, text string, logical int64) CanonicalItem {
	return mkItem(CanonicalKindMessage, role, "", logical, sp(text))
}

func mkCallItem(id string, logical int64) CanonicalItem {
	return mkItem(CanonicalKindToolCall, "", "", logical, callp(id))
}

func mkResultItem(id string, logical int64) CanonicalItem {
	return mkItem(CanonicalKindToolResult, "", "", logical, resp(id))
}

// unitTotal sums the canonical bytes of units.
func unitTotal(units []SemanticUnit) int64 {
	var t int64
	for i := range units {
		t += units[i].CanonicalBytes
	}
	return t
}

// gapKindCount counts the gap-marker items of a selection.
func gapKindCount(items []CanonicalItem) int {
	n := 0
	for _, it := range items {
		if it.Kind == CanonicalKindGap {
			n++
		}
	}
	return n
}

// assertSelectionInvariants locks the frozen selection rules for any input.
func assertSelectionInvariants(t *testing.T, units []SemanticUnit, limit int64, res SelectionResult, err error) {
	t.Helper()
	require.NoError(t, err)
	assert.LessOrEqual(t, res.TotalBytes, limit, "byte budget must never be exceeded")
	selected := 0
	for _, it := range res.Items {
		if it.Kind == CanonicalKindGap {
			continue
		}
		selected++
	}
	if res.Gap == nil {
		if res.TotalBytes == 0 && unitTotal(units) > limit {
			// Degenerate limit: gap marker cannot fit, but with D2 the Gap
			// is present with Reason=capture_limit_too_small.
			require.NotNil(t, res.Gap)
			assert.Equal(t, GapReasonLimitTooSmall, res.Gap.Reason)
			return
		}
		assert.Equal(t, unitTotal(units), res.TotalBytes, "full fit reports the exact full total")
		assert.Equal(t, 0, gapKindCount(res.Items), "no gap marker without truncation")
		assert.Equal(t, selected, countUnitItems(units), "full fit returns every item")
	} else {
		if res.Gap.Reason == GapReasonLimitTooSmall {
			assert.Empty(t, res.Items)
			assert.Equal(t, int64(0), res.TotalBytes)
			assert.Equal(t, countUnitItems(units), res.Gap.OmittedItems)
			return
		}
		assert.Equal(t, 1, gapKindCount(res.Items), "truncation has exactly one gap marker")
		assert.Equal(t, selected, countUnitItems(units)-res.Gap.OmittedItems, "gap omitted_items must match the selection")
	}
	// Verify protocol order (D5).
	var input []CanonicalItem
	for i := range units {
		input = append(input, units[i].Items...)
	}
	assertSubsequence(t, input, res.Items)
}

// assertSubsequence verifies that got (minus gap markers) appears in want in order.
func assertSubsequence(t *testing.T, want, got []CanonicalItem) {
	t.Helper()
	j := 0
	for _, g := range got {
		if g.Kind == CanonicalKindGap {
			continue
		}
		for j < len(want) {
			if assert.ObjectsAreEqual(want[j], g) {
				j++
				break
			}
			j++
		}
		if j > len(want) {
			t.Fatalf("item not found in input order: %+v", g)
		}
	}
}

func containsItem(items []CanonicalItem, want CanonicalItem) bool {
	for _, it := range items {
		if assert.ObjectsAreEqual(it, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// unit building

func TestBuildUnitsClassifiesPlainStream(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 10, sp("sys")),
		mkMsg("user", "question", 20),
		mkMsg("assistant", "answer", 30),
		mkItem(CanonicalKindUnknown, "", "", 40),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 4)
	assert.Equal(t, SemanticUnitAnchor, units[0].Kind)
	assert.Equal(t, SemanticUnitMessage, units[1].Kind)
	assert.Equal(t, SemanticUnitMessage, units[2].Kind)
	assert.Equal(t, SemanticUnitUnknown, units[3].Kind)
	assert.True(t, units[0].Anchor, "system directive is an anchor candidate")
	// D7: latest user is no longer an anchor candidate — it's retained by the tail.
	assert.False(t, units[1].Anchor, "D7: user message is not an anchor candidate")
	assert.False(t, units[2].Anchor)
	assert.False(t, units[3].Anchor)
	for i := range units {
		assert.Empty(t, units[i].CallIDs)
		assert.False(t, units[i].Orphan)
		assert.Equal(t, items[i].LogicalBytes, units[i].LogicalBytes)
		cb, err := canonicalBytesOf(items[i])
		require.NoError(t, err)
		assert.Equal(t, cb, units[i].CanonicalBytes)
	}
}

func TestBuildUnitsMergesClaudePairByID(t *testing.T) {
	call := mkMsg("assistant", "calling", 10)
	call.Content = append(call.Content, callp("call_A"))
	result := mkMsg("user", "", 20)
	result.Content = append(result.Content, resp("call_A"))
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "go", 7),
		call,
		result,
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 3, "call and result merge into one unit")
	ex := units[2]
	assert.Equal(t, SemanticUnitToolExchange, ex.Kind)
	assert.Len(t, ex.Items, 2)
	assert.Equal(t, call, ex.Items[0])
	assert.Equal(t, result, ex.Items[1])
	assert.Equal(t, []string{"call_A"}, ex.CallIDs)
	assert.False(t, ex.Orphan)
}

func TestBuildUnitsMergesResponsesPairNonAdjacent(t *testing.T) {
	items := []CanonicalItem{
		mkCallItem("call_A", 10),
		mkMsg("user", "middle", 20),
		mkResultItem("call_A", 30),
		mkMsg("user", "last", 40),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 3)
	ex := units[0]
	assert.Equal(t, SemanticUnitToolExchange, ex.Kind)
	assert.Len(t, ex.Items, 2, "pair matched by ID, not adjacency")
	assert.Equal(t, items[0], ex.Items[0], "call keeps its protocol position")
	assert.Equal(t, items[2], ex.Items[1], "result follows the call")
	assert.Equal(t, []string{"call_A"}, ex.CallIDs)
	// D7: latest user is no longer an anchor candidate.
	assert.False(t, units[2].Anchor, "D7: latest user is not an anchor candidate")
	assert.False(t, units[1].Anchor)
}

func TestBuildUnitsParallelCallsInOneItem(t *testing.T) {
	asst := mkMsg("assistant", "parallel", 10)
	asst.Content = append(asst.Content, callp("call_A"), callp("call_B"))
	items := []CanonicalItem{
		asst,
		mkItem(CanonicalKindMessage, "tool", "call_A", 20),
		mkItem(CanonicalKindMessage, "tool", "call_B", 30),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 1, "all parallel calls merge into one exchange unit via union-find")
	ex := units[0]
	assert.Len(t, ex.Items, 3)
	assert.Equal(t, []string{"call_A", "call_B"}, ex.CallIDs, "parallel call IDs all recorded, sorted")
	assert.False(t, ex.Orphan)
}

func TestBuildUnitsParallelCallsResultsOutOfOrder(t *testing.T) {
	items := []CanonicalItem{
		mkCallItem("call_A", 10),
		mkCallItem("call_B", 10),
		mkResultItem("call_B", 20),
		mkResultItem("call_A", 20),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 2)
	assert.Equal(t, []string{"call_A"}, units[0].CallIDs)
	assert.Equal(t, []string{"call_B"}, units[1].CallIDs)
	assert.Len(t, units[0].Items, 2)
	assert.Equal(t, items[0], units[0].Items[0])
	assert.Equal(t, items[3], units[0].Items[1])
	assert.Equal(t, items[2], units[1].Items[1])
	assert.False(t, units[0].Orphan)
	assert.False(t, units[1].Orphan)
}

// TestBuildUnitsUnionFindMultiResult tests D6: a result item referencing
// multiple call IDs forms a single connected component via union-find.
func TestBuildUnitsUnionFindMultiResult(t *testing.T) {
	items := []CanonicalItem{
		mkCallItem("call_A", 10),
		mkCallItem("call_B", 10),
		mkMsg("assistant", "both", 10),
	}
	// The third item has two result parts referencing call_A and call_B.
	items[2].Content = append(items[2].Content, resp("call_A"), resp("call_B"))
	items[2].Kind = CanonicalKindMessage
	items[2].ToolCallID = "call_A" // also contributes to the component

	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	// All three items share a call ID transitive closure -> one component.
	require.Len(t, units, 1, "union-find merges cross-referencing items into one component")
	assert.Len(t, units[0].Items, 3)
	assert.Equal(t, []string{"call_A", "call_B"}, units[0].CallIDs)
}

func TestBuildUnitsOrphanResultKept(t *testing.T) {
	items := []CanonicalItem{
		mkResultItem("call_X", 50),
		mkMsg("user", "hello", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 2)
	orphan := units[0]
	assert.Equal(t, SemanticUnitToolExchange, orphan.Kind)
	assert.True(t, orphan.Orphan, "unpaired result survives and is marked orphan")
	assert.Equal(t, []string{"call_X"}, orphan.CallIDs)
	assert.Len(t, orphan.Items, 1)
	assert.False(t, units[1].Orphan)
}

func TestBuildUnitsCallWithoutResultIsNotOrphan(t *testing.T) {
	items := []CanonicalItem{
		mkCallItem("call_A", 10),
		mkMsg("user", "next", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 2)
	assert.False(t, units[0].Orphan, "a call awaiting its result is not an orphan")
	assert.Equal(t, []string{"call_A"}, units[0].CallIDs)
}

func TestBuildUnitsSourceTruncated(t *testing.T) {
	truncated := mkMsg("user", "cut", 10)
	truncated.Truncated = true
	gapItem := mkItem(CanonicalKindGap, "", "", 99, sp(""))
	codexOut := mkResultItem("call_A", 10)
	codexOut.Content[0].Result.Output = json.RawMessage(`"Warning: truncated output: 30k chars dropped"`)

	units, err := BuildSemanticUnits([]CanonicalItem{truncated, gapItem, mkCallItem("call_A", 10), codexOut})
	require.NoError(t, err)
	require.Len(t, units, 3)
	assert.True(t, units[0].SourceTruncated, "item Truncated flag propagates")
	assert.True(t, units[1].SourceTruncated, "explicit gap item is source-truncated")
	assert.True(t, units[2].SourceTruncated, "codex truncated-output marker propagates through the merge")
}

func TestBuildUnitsCompactSummaryAnchor(t *testing.T) {
	items := []CanonicalItem{
		mkItem(canonicalKindCompactSummary, "", "", 30),
		mkMsg("user", "latest", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 2)
	assert.Equal(t, SemanticUnitCompactSummary, units[0].Kind)
	assert.True(t, units[0].Anchor, "compact summary is an anchor candidate")
	// D7: latest user is not an anchor candidate.
	assert.False(t, units[1].Anchor, "D7: latest user is not an anchor candidate")
}

func TestBuildUnitsDeveloperDirectiveAnchor(t *testing.T) {
	items := []CanonicalItem{
		mkMsg("developer", "be careful", 30),
		mkMsg("user", "hi", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 2)
	assert.True(t, units[0].Anchor, "chat developer directive is an anchor candidate")
	assert.Equal(t, SemanticUnitMessage, units[0].Kind)
}

func TestBuildUnitsDeterministic(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkCallItem("call_A", 10),
		mkMsg("user", "mid", 10),
		mkResultItem("call_A", 20),
		mkResultItem("call_orphan", 20),
		mkMsg("user", "last", 10),
	}
	first, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	second, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	assert.Equal(t, first, second, "same input must produce identical units")
}

func TestBuildUnitsItemIndexes(t *testing.T) {
	items := []CanonicalItem{
		mkCallItem("call_A", 10),
		mkMsg("user", "middle", 20),
		mkResultItem("call_A", 30),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 2)
	// Unit 0: items[0] and items[2] merged.
	assert.Equal(t, []int{0, 2}, units[0].ItemIndexes)
	// Unit 1: items[1] alone.
	assert.Equal(t, []int{1}, units[1].ItemIndexes)
}

// ---------------------------------------------------------------------------
// selection

func TestSelectFullFit(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkCallItem("call_A", 10),
		mkResultItem("call_A", 20),
		mkMsg("user", "last", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	total := unitTotal(units)
	for _, limit := range []int64{total, total + 500} {
		res, err := SelectEvidence(units, testPolicy(limit))
		assertSelectionInvariants(t, units, limit, res, err)
		assert.Nil(t, res.Gap, "zero truncation keeps Gap nil")
		assert.Empty(t, res.Oversized)
		assert.Equal(t, total, res.TotalBytes)
	}
}

func TestSelectTruncatesMiddle(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "old", 100),
		mkCallItem("call_A", 100),
		mkResultItem("call_A", 100),
		mkMsg("user", "newest", 100),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	total := unitTotal(units)
	// The limit is one byte short of the full total: only the old middle user
	// message is omitted — system anchor, tool chain, and newest user message
	// are retained.
	limit := total - 1
	res, err := SelectEvidence(units, testPolicy(limit))
	require.NoError(t, err)

	require.NotNil(t, res.Gap)
	assert.Equal(t, 1, gapKindCount(res.Items), "exactly one gap marker")
	assert.True(t, containsItem(res.Items, items[4]), "latest user message retained")
	assert.True(t, containsItem(res.Items, items[2]), "tool call retained")
	assert.True(t, containsItem(res.Items, items[3]), "tool result retained")
	assert.True(t, containsItem(res.Items, items[0]), "system anchor retained")
	assert.False(t, containsItem(res.Items, items[1]), "old user message omitted")
	assert.Equal(t, GapPositionMiddle, res.Gap.Position)
	assert.Equal(t, 1, res.Gap.OmittedItems)
	assert.Equal(t, items[1].LogicalBytes, res.Gap.LogicalBytes)
	assert.Equal(t, GapReasonBudget, res.Gap.Reason)
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res, err)
}

func TestSelectToolPairAtomicity(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "old", 10),
		mkCallItem("call_A", 100),
		mkResultItem("call_A", 100),
		mkMsg("user", "mid", 10),
		mkCallItem("call_B", 100),
		mkResultItem("call_B", 100),
		mkMsg("user", "newest", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	total := unitTotal(units)
	for _, limit := range []int64{1, 60, 120, 200, 260, 300, 340, 380, 420, total - 1, total} {
		res, err := SelectEvidence(units, testPolicy(limit))
		assertSelectionInvariants(t, units, limit, res, err)
		for _, u := range units {
			if u.Kind != SemanticUnitToolExchange || u.Orphan {
				continue
			}
			kept := 0
			for _, it := range u.Items {
				if containsItem(res.Items, it) {
					kept++
				}
			}
			assert.Equalf(t, kept == 0 || kept == len(u.Items), true, "limit %d: pair %v must be all-in or all-out (kept %d/%d)", limit, u.CallIDs, kept, len(u.Items))
		}
	}
}

func TestSelectParallelPairAtomic(t *testing.T) {
	asst := mkMsg("assistant", "parallel", 10)
	asst.Content = append(asst.Content, callp("call_A"), callp("call_B"))
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		asst,
		mkItem(CanonicalKindMessage, "tool", "call_A", 100),
		mkItem(CanonicalKindMessage, "tool", "call_B", 100),
		mkMsg("user", "newest", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	require.Len(t, units, 3)
	// A limit that cannot hold the whole parallel exchange must omit it whole.
	res, err := SelectEvidence(units, testPolicy(unitTotal(units)-1))
	require.NoError(t, err)
	assert.False(t, containsItem(res.Items, items[1]), "parallel exchange all-out")
	assert.False(t, containsItem(res.Items, items[2]))
	assert.False(t, containsItem(res.Items, items[3]))
	assert.True(t, containsItem(res.Items, items[4]), "newest user retained")
	assertSelectionInvariants(t, units, unitTotal(units)-1, res, err)
}

func TestSelectOrphanResultNearTail(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "old", 10),
		mkResultItem("call_orphan", 50),
		mkMsg("user", "newest", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	total := unitTotal(units)
	res, err := SelectEvidence(units, testPolicy(total-1))
	require.NoError(t, err)
	assert.True(t, containsItem(res.Items, items[2]), "orphan result survives selection near the tail")
	assert.True(t, containsItem(res.Items, items[3]))
	assertSelectionInvariants(t, units, total-1, res, err)
}

func TestSelectDeterministic(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkCallItem("call_A", 100),
		mkResultItem("call_A", 100),
		mkResultItem("call_orphan", 50),
		mkMsg("user", "newest", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	for _, limit := range []int64{1, 100, 200, 300, unitTotal(units)} {
		first, err := SelectEvidence(units, testPolicy(limit))
		require.NoError(t, err)
		second, err := SelectEvidence(units, testPolicy(limit))
		require.NoError(t, err)
		assert.Equal(t, first, second, "identical input+policy must produce the identical selection")
	}
}

func TestSelectBudgetFuzz(t *testing.T) {
	asst := mkMsg("assistant", "parallel", 10)
	asst.Content = append(asst.Content, callp("call_A"), callp("call_B"))
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "old", 10),
		asst,
		mkItem(CanonicalKindMessage, "tool", "call_A", 100),
		mkItem(CanonicalKindMessage, "tool", "call_B", 100),
		mkResultItem("call_orphan", 50),
		mkMsg("user", "newest", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	total := unitTotal(units)
	for _, limit := range []int64{0, 1, 10, 57, 58, 59, 60, 100, 300, 500, total - 1, total, total + 1, 1 << 20} {
		res, err := SelectEvidence(units, testPolicy(limit))
		assertSelectionInvariants(t, units, limit, res, err)
		assert.LessOrEqual(t, res.TotalBytes, limit, "limit %d", limit)
		if res.Gap != nil && res.Gap.Reason == GapReasonLimitTooSmall {
			assert.Empty(t, res.Items)
			assert.Equal(t, int64(0), res.TotalBytes)
		} else if res.Gap == nil {
			assert.Equal(t, items, res.Items, "limit %d: full fit", limit)
		} else {
			assert.Equal(t, 1, gapKindCount(res.Items), "limit %d", limit)
		}
	}

	// Randomized streams.
	rng := rand.New(rand.NewSource(17))
	for round := 0; round < 100; round++ {
		var randItems []CanonicalItem
		for k := 0; k < 1+rng.Intn(10); k++ {
			switch rng.Intn(5) {
			case 0:
				randItems = append(randItems, mkItem(CanonicalKindSystem, "", "", int64(rng.Intn(200)), sp("s")))
			case 1:
				randItems = append(randItems, mkMsg("user", "u", int64(rng.Intn(200))))
			case 2:
				id := fmt.Sprintf("call_%d", rng.Intn(4))
				randItems = append(randItems, mkCallItem(id, int64(rng.Intn(200))))
			case 3:
				id := fmt.Sprintf("call_%d", rng.Intn(4))
				randItems = append(randItems, mkResultItem(id, int64(rng.Intn(200))))
			default:
				randItems = append(randItems, mkResultItem(fmt.Sprintf("orphan_%d", rng.Intn(4)), int64(rng.Intn(200))))
			}
		}
		units, err := BuildSemanticUnits(randItems)
		require.NoError(t, err)
		limit := int64(rng.Intn(int(unitTotal(units)) + 2))
		res, err := SelectEvidence(units, testPolicy(limit))
		require.NoError(t, err)
		assert.LessOrEqual(t, res.TotalBytes, limit, "round %d limit %d", round, limit)
		assertSelectionInvariants(t, units, limit, res, err)
	}
}

func TestSelectOversizedLatestUnit(t *testing.T) {
	bigResult := mkResultItem("call_A", 12000)
	bigResult.Content[0].Result.Output = json.RawMessage(`"` + strings.Repeat("x", 12000) + `"`)
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "before", 10),
		mkCallItem("call_A", 200),
		bigResult,
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	limit := int64(10000)
	require.Len(t, units, 3)
	require.Greater(t, units[2].CanonicalBytes, limit, "precondition: newest unit alone over the limit")
	res, err := SelectEvidence(units, testPolicy(limit))
	require.NoError(t, err)

	require.Len(t, res.Oversized, 1)
	ov := res.Oversized[0]
	assert.Equal(t, SemanticUnitToolExchange, ov.Kind)
	assert.Equal(t, []string{"call_A"}, ov.CallIDs)
	assert.Equal(t, int64(12200), ov.LogicalBytes)
	assert.Equal(t, OversizedReasonUnit, ov.Reason)
	assert.False(t, ov.SourceTruncated)
	assert.False(t, containsItem(res.Items, items[2]), "oversized pair not split: call omitted")
	assert.False(t, containsItem(res.Items, items[3]), "oversized pair not split: result omitted")
	assert.True(t, containsItem(res.Items, items[0]), "anchor retained")
	assert.True(t, containsItem(res.Items, items[1]), "rest selected normally")
	require.NotNil(t, res.Gap)
	assert.Equal(t, GapPositionTail, res.Gap.Position)
	assert.Equal(t, GapReasonOversized, res.Gap.Reason)
	assert.Equal(t, 2, res.Gap.OmittedItems)
	assert.Equal(t, int64(12200), res.Gap.LogicalBytes)
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res, err)
}

func TestSelectOversizedLatestOnlyUnit(t *testing.T) {
	bigResult := mkResultItem("call_A", 12000)
	bigResult.Content[0].Result.Output = json.RawMessage(`"` + strings.Repeat("x", 12000) + `"`)
	items := []CanonicalItem{
		mkCallItem("call_A", 200),
		bigResult,
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	limit := int64(10000)
	require.Greater(t, units[0].CanonicalBytes, limit, "precondition: the only unit over the limit")
	res, err := SelectEvidence(units, testPolicy(limit))
	require.NoError(t, err)
	require.Len(t, res.Oversized, 1)
	assert.Equal(t, OversizedReasonUnit, res.Oversized[0].Reason)
	require.NotNil(t, res.Gap)
	assert.Equal(t, GapPositionHead, res.Gap.Position, "a gap covering the whole stream is head")
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res, err)
}

func TestSelectAnchorBudgetCap(t *testing.T) {
	bigSystem := mkItem(CanonicalKindSystem, "", "", 23000, sp(strings.Repeat("s", 23000)))
	user := mkMsg("user", "latest", 100)
	items := []CanonicalItem{bigSystem, user}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	limit := int64(20000)
	require.Greater(t, unitTotal(units), limit)
	res, err := SelectEvidence(units, testPolicy(limit))
	require.NoError(t, err)

	require.NotNil(t, res.Gap)
	assert.False(t, containsItem(res.Items, bigSystem), "giant system must not crowd out the tail")
	assert.True(t, containsItem(res.Items, user), "tail survives a giant anchor")
	require.Len(t, res.Oversized, 1)
	assert.Equal(t, OversizedReasonAnchor, res.Oversized[0].Reason)
	assert.Equal(t, SemanticUnitAnchor, res.Oversized[0].Kind)
	assert.Equal(t, int64(23000), res.Oversized[0].LogicalBytes)
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res, err)

	// A system within the anchor budget is retained.
	smallSystem := mkItem(CanonicalKindSystem, "", "", 2000, sp(strings.Repeat("s", 2000)))
	middle := mkMsg("user", strings.Repeat("u", 9000), 9000)
	units2, err := BuildSemanticUnits([]CanonicalItem{smallSystem, middle, user})
	require.NoError(t, err)
	res2, err := SelectEvidence(units2, testPolicy(10000))
	require.NoError(t, err)
	require.NotNil(t, res2.Gap)
	assert.True(t, containsItem(res2.Items, smallSystem), "anchor within budget retained")
	assert.True(t, containsItem(res2.Items, user), "newest user retained")
	assert.False(t, containsItem(res2.Items, middle), "middle omitted")
	assert.Empty(t, res2.Oversized)
	assert.Equal(t, GapPositionMiddle, res2.Gap.Position)
	assertSelectionInvariants(t, units2, 10000, res2, err)
}

// TestSelectAnchorBudgetUnusedReturned tests D4: unused anchor budget flows
// back to the tail.
func TestSelectAnchorBudgetUnusedReturned(t *testing.T) {
	// No anchors at all: tail should be able to use the full limit.
	items := []CanonicalItem{
		mkMsg("user", "old", 6000),
		mkMsg("user", "newest", 6000),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	// Total ~12KB, limit=10KB, anchor budget ~2.5KB but unused,
	// so tail should be able to retain the newest user message.
	limit := int64(10000)
	res, err := SelectEvidence(units, testPolicy(limit))
	require.NoError(t, err)
	require.NotNil(t, res.Gap)
	assert.True(t, containsItem(res.Items, items[1]), "D4: newest user retained with full budget")
	// The old user message is omitted.
	assert.False(t, containsItem(res.Items, items[0]))
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res, err)
}

func TestSelectBoundaryTotalEqualsLimit(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "latest", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	res, err := SelectEvidence(units, testPolicy(unitTotal(units)))
	require.NoError(t, err)
	assert.Nil(t, res.Gap, "total == limit is a full fit, zero truncation")
	assert.Equal(t, unitTotal(units), res.TotalBytes)
}

func TestSelectDegenerateLimit(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "latest", 10),
	}
	units, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	for _, limit := range []int64{0, 1, 5, 10} {
		res, err := SelectEvidence(units, testPolicy(limit))
		require.NoError(t, err)
		require.NotNil(t, res.Gap, "limit %d: degenerate limit has a gap with reason", limit)
		assert.Equal(t, GapReasonLimitTooSmall, res.Gap.Reason, "limit %d", limit)
		assert.Empty(t, res.Items, "limit %d", limit)
		assert.Equal(t, int64(0), res.TotalBytes, "limit %d", limit)
	}
}

func TestGapMarkerShape(t *testing.T) {
	g := GapMarker(GapInfo{LogicalBytes: 12345})
	assert.Equal(t, CanonicalKindGap, g.Kind)
	assert.Equal(t, int64(12345), g.LogicalBytes)
	assert.True(t, g.Truncated)
}

func TestSelectErrorPropagation(t *testing.T) {
	// A policy with a MeasureGap that always fails.
	badPolicy := SelectionPolicy{
		Limit:       100,
		AnchorRatio: DefaultAnchorRatio,
		AnchorCap:   DefaultAnchorCap,
		MeasureGap: func(g GapInfo) (CanonicalItem, int64, error) {
			return CanonicalItem{}, 0, fmt.Errorf("test error")
		},
	}
	units, err := BuildSemanticUnits([]CanonicalItem{mkMsg("user", "hello", 10)})
	require.NoError(t, err)
	_, err = SelectEvidence(units, badPolicy)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// end-to-end shape tests over the read-only agent corpus (invariants only)

func TestSelectE2EFixtureCodexMultiturn(t *testing.T) {
	units, items := fixtureUnits(t, "codex-multiturn.json", string(types.RelayFormatOpenAIResponses), &dto.OpenAIResponsesRequest{})
	assert.NotEmpty(t, units)
	assertPairCompleteness(t, items, units)
	assertDeterministicUnits(t, items)
	total := unitTotal(units)
	for _, limit := range []int64{1, 60, 100, total - 1, total, total + 1} {
		res, err := SelectEvidence(units, testPolicy(limit))
		require.NoError(t, err)
		assert.LessOrEqual(t, res.TotalBytes, limit, "limit %d", limit)
		if res.Gap != nil && res.Gap.Reason == GapReasonLimitTooSmall {
			assert.Empty(t, res.Items)
		} else if res.Gap == nil {
			assert.Equal(t, items, res.Items, "limit %d: full fit", limit)
		} else {
			assert.Equal(t, 1, gapKindCount(res.Items), "limit %d", limit)
		}
		assertSelectionInvariants(t, units, limit, res, err)
		again, err := SelectEvidence(units, testPolicy(limit))
		require.NoError(t, err)
		assert.Equal(t, res, again, "limit %d: deterministic", limit)
	}
}

func TestSelectE2EFixtureClaudeToolChain(t *testing.T) {
	units, items := fixtureUnits(t, "claude-tool-chain.json", string(types.RelayFormatClaude), &dto.ClaudeRequest{})
	assert.NotEmpty(t, units)
	assertPairCompleteness(t, items, units)
	assertDeterministicUnits(t, items)
	total := unitTotal(units)
	for _, limit := range []int64{1, 60, 100, total - 1, total, total + 1} {
		res, err := SelectEvidence(units, testPolicy(limit))
		require.NoError(t, err)
		assert.LessOrEqual(t, res.TotalBytes, limit, "limit %d", limit)
		if res.Gap != nil && res.Gap.Reason == GapReasonLimitTooSmall {
			assert.Empty(t, res.Items)
		} else if res.Gap == nil {
			assert.Equal(t, items, res.Items, "limit %d: full fit", limit)
		} else {
			assert.Equal(t, 1, gapKindCount(res.Items), "limit %d", limit)
		}
		assertSelectionInvariants(t, units, limit, res, err)
		again, err := SelectEvidence(units, testPolicy(limit))
		require.NoError(t, err)
		assert.Equal(t, res, again, "limit %d: deterministic", limit)
	}
}

// fixtureUnits normalizes one agent-corpus fixture body into canonical items.
func fixtureUnits(t *testing.T, name, format string, req dto.Request) ([]SemanticUnit, []CanonicalItem) {
	t.Helper()
	rawBody, err := os.ReadFile("testdata/agent-corpus/" + name)
	require.NoError(t, err)
	var wrapper struct {
		Body json.RawMessage `json:"body"`
	}
	require.NoError(t, json.Unmarshal(rawBody, &wrapper))
	require.NotEmpty(t, wrapper.Body)
	require.NoError(t, json.Unmarshal(wrapper.Body, req))
	res := NormalizeRequest(format, req, NormalizeOptions{
		CaptureLimit: 1 << 30, MaxRequestBytes: 1 << 30, MinCaptureEnvelopeBytes: 0, HMACKey: testHMACKey,
	})
	require.NotEqual(t, ContentStateMetadataOnly, res.ContentState, "fixture must normalize")
	require.NotEmpty(t, res.Items)
	units, err := BuildSemanticUnits(res.Items)
	require.NoError(t, err)
	require.NotEmpty(t, units)
	return units, res.Items
}

func assertPairCompleteness(t *testing.T, items []CanonicalItem, units []SemanticUnit) {
	t.Helper()
	unitByID := map[string]*SemanticUnit{}
	for i := range units {
		for _, id := range units[i].CallIDs {
			unitByID[id] = &units[i]
		}
	}
	for _, it := range items {
		calls, results := toolIDsOfParts(it.Content)
		for _, id := range calls {
			u, ok := unitByID[id]
			require.Truef(t, ok, "call %s must belong to a unit", id)
			assert.NotEqual(t, 0, len(u.CallIDs), "unit must keep its call IDs")
		}
		for _, id := range results {
			if u, ok := unitByID[id]; ok {
				assert.Equal(t, SemanticUnitToolExchange, u.Kind, "paired unit kind")
			}
			assert.True(t, containsIDInUnits(units, id), "result %s must survive in a unit", id)
		}
	}
}

func containsIDInUnits(units []SemanticUnit, id string) bool {
	for i := range units {
		for _, u := range units[i].CallIDs {
			if u == id {
				return true
			}
		}
	}
	return false
}

func assertDeterministicUnits(t *testing.T, items []CanonicalItem) {
	t.Helper()
	first, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	second, err := BuildSemanticUnits(items)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}