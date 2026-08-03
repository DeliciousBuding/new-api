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
// synthetic item helpers

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

// unitTotal sums the canonical bytes of units — the exact measurement the
// selector starts from.
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
func assertSelectionInvariants(t *testing.T, units []SemanticUnit, limit int64, res SelectionResult) {
	t.Helper()
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
			// Degenerate limit: the gap marker itself cannot fit, so the
			// documented strategy returns an empty selection with the budget
			// intact (the integration layer's envelope reservation keeps
			// production out of this regime).
			assert.Empty(t, res.Items, "degenerate limit yields no items")
			return
		}
		assert.Equal(t, unitTotal(units), res.TotalBytes, "full fit reports the exact full total")
		assert.Equal(t, 0, gapKindCount(res.Items), "no gap marker without truncation")
		assert.Equal(t, selected, countItems(units), "full fit returns every item")
	} else {
		assert.Equal(t, 1, gapKindCount(res.Items), "truncation has exactly one gap marker")
		assert.Equal(t, selected, countItems(units)-res.Gap.OmittedItems, "gap omitted_items must match the selection")
	}
	// The selection (minus the marker) is a subsequence of the input in
	// protocol order.
	var input []CanonicalItem
	for i := range units {
		input = append(input, units[i].Items...)
	}
	assertSubsequence(t, input, res.Items)
}

func countItems(units []SemanticUnit) int {
	n := 0
	for i := range units {
		n += len(units[i].Items)
	}
	return n
}

// assertSubsequence verifies that got (minus gap markers) appears in want in
// order, item by item (reflect equality).
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

// ---------------------------------------------------------------------------
// unit building

func TestBuildUnitsClassifiesPlainStream(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 10, sp("sys")),
		mkMsg("user", "question", 20),
		mkMsg("assistant", "answer", 30),
		mkItem(CanonicalKindUnknown, "", "", 40),
	}
	units := BuildSemanticUnits(items)
	require.Len(t, units, 4)
	assert.Equal(t, SemanticUnitAnchor, units[0].Kind)
	assert.Equal(t, SemanticUnitMessage, units[1].Kind)
	assert.Equal(t, SemanticUnitMessage, units[2].Kind)
	assert.Equal(t, SemanticUnitUnknown, units[3].Kind)
	assert.True(t, units[0].Anchor, "system directive is an anchor candidate")
	assert.True(t, units[1].Anchor, "latest user instruction is an anchor candidate")
	assert.False(t, units[2].Anchor)
	assert.False(t, units[3].Anchor)
	for i := range units {
		assert.Empty(t, units[i].CallIDs)
		assert.False(t, units[i].Orphan)
		assert.Equal(t, items[i].LogicalBytes, units[i].LogicalBytes)
		assert.Equal(t, canonicalBytesOf(items[i]), units[i].CanonicalBytes)
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
	units := BuildSemanticUnits(items)
	require.Len(t, units, 3, "call and result merge into one unit")
	ex := units[2]
	assert.Equal(t, SemanticUnitToolExchange, ex.Kind)
	assert.Len(t, ex.Items, 2)
	assert.Equal(t, call, ex.Items[0])
	assert.Equal(t, result, ex.Items[1])
	assert.Equal(t, []string{"call_A"}, ex.CallIDs)
	assert.False(t, ex.Orphan)
	assert.Equal(t, items[0].LogicalBytes+items[1].LogicalBytes, units[0].LogicalBytes+units[1].LogicalBytes)
}

func TestBuildUnitsMergesResponsesPairNonAdjacent(t *testing.T) {
	items := []CanonicalItem{
		mkCallItem("call_A", 10),
		mkMsg("user", "middle", 20),
		mkResultItem("call_A", 30),
		mkMsg("user", "last", 40),
	}
	units := BuildSemanticUnits(items)
	require.Len(t, units, 3)
	ex := units[0]
	assert.Equal(t, SemanticUnitToolExchange, ex.Kind)
	assert.Len(t, ex.Items, 2, "pair matched by ID, not adjacency")
	assert.Equal(t, items[0], ex.Items[0], "call keeps its protocol position")
	assert.Equal(t, items[2], ex.Items[1], "result follows the call in protocol order")
	assert.Equal(t, []string{"call_A"}, ex.CallIDs)
	assert.True(t, units[2].Anchor, "latest user message is the anchor candidate")
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
	units := BuildSemanticUnits(items)
	require.Len(t, units, 1, "all parallel calls merge into one exchange unit")
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
	units := BuildSemanticUnits(items)
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

func TestBuildUnitsOrphanResultKept(t *testing.T) {
	items := []CanonicalItem{
		mkResultItem("call_X", 50),
		mkMsg("user", "hello", 10),
	}
	units := BuildSemanticUnits(items)
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
	units := BuildSemanticUnits(items)
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

	units := BuildSemanticUnits([]CanonicalItem{truncated, gapItem, mkCallItem("call_A", 10), codexOut})
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
	units := BuildSemanticUnits(items)
	require.Len(t, units, 2)
	assert.Equal(t, SemanticUnitCompactSummary, units[0].Kind)
	assert.True(t, units[0].Anchor, "compact summary is an anchor candidate")
	assert.True(t, units[1].Anchor, "latest user instruction is an anchor candidate")
}

func TestBuildUnitsDeveloperDirectiveAnchor(t *testing.T) {
	items := []CanonicalItem{
		mkMsg("developer", "be careful", 30),
		mkMsg("user", "hi", 10),
	}
	units := BuildSemanticUnits(items)
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
	first := BuildSemanticUnits(items)
	second := BuildSemanticUnits(items)
	assert.Equal(t, first, second, "same input must produce identical units")
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
	units := BuildSemanticUnits(items)
	total := unitTotal(units)
	for _, limit := range []int64{total, total + 500} {
		res := SelectEvidence(units, limit)
		assert.Nil(t, res.Gap, "zero truncation keeps Gap nil")
		assert.Empty(t, res.Oversized)
		assert.Equal(t, items, res.Items, "full fit returns the input verbatim")
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
	units := BuildSemanticUnits(items)
	total := unitTotal(units)
	// The limit is one byte short of the full total: only the old middle user
	// message is omitted — system anchor, tool chain, and newest user message
	// are retained in full.
	limit := total - 1
	res := SelectEvidence(units, limit)

	require.NotNil(t, res.Gap)
	assert.Equal(t, 1, gapKindCount(res.Items), "exactly one gap marker")
	// Tail: newest user message and the tool chain are retained 100%.
	assert.True(t, containsItem(res.Items, items[4]), "latest user message retained")
	assert.True(t, containsItem(res.Items, items[2]), "tool call retained")
	assert.True(t, containsItem(res.Items, items[3]), "tool result retained")
	assert.True(t, containsItem(res.Items, items[0]), "system anchor retained")
	assert.False(t, containsItem(res.Items, items[1]), "old user message omitted")
	assert.Equal(t, gapPositionMiddle, res.Gap.Position)
	assert.Equal(t, 1, res.Gap.OmittedItems)
	assert.Equal(t, items[1].LogicalBytes, res.Gap.LogicalBytes)
	assert.Equal(t, gapReasonBudget, res.Gap.Reason)
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res)
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
	units := BuildSemanticUnits(items)
	total := unitTotal(units)
	// Scan limits across the whole range: no limit may ever split a pair.
	for _, limit := range []int64{1, 60, 120, 200, 260, 300, 340, 380, 420, total - 1, total} {
		res := SelectEvidence(units, limit)
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
		assertSelectionInvariants(t, units, limit, res)
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
	units := BuildSemanticUnits(items)
	require.Len(t, units, 3)
	total := unitTotal(units)
	// A limit that cannot hold the whole parallel exchange must omit it whole.
	res := SelectEvidence(units, total-1)
	assert.False(t, containsItem(res.Items, items[1]), "parallel exchange all-out")
	assert.False(t, containsItem(res.Items, items[2]))
	assert.False(t, containsItem(res.Items, items[3]))
	assert.True(t, containsItem(res.Items, items[4]), "newest user retained")
	assertSelectionInvariants(t, units, total-1, res)
}

func TestSelectOrphanResultNearTail(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "old", 10),
		mkResultItem("call_orphan", 50),
		mkMsg("user", "newest", 10),
	}
	units := BuildSemanticUnits(items)
	total := unitTotal(units)
	res := SelectEvidence(units, total-1)
	assert.True(t, containsItem(res.Items, items[2]), "orphan result survives selection near the tail")
	assert.True(t, containsItem(res.Items, items[3]))
	assertSelectionInvariants(t, units, total-1, res)
}

func TestSelectDeterministic(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkCallItem("call_A", 100),
		mkResultItem("call_A", 100),
		mkResultItem("call_orphan", 50),
		mkMsg("user", "newest", 10),
	}
	units := BuildSemanticUnits(items)
	for _, limit := range []int64{1, 100, 200, 300, unitTotal(units)} {
		first := SelectEvidence(units, limit)
		second := SelectEvidence(units, limit)
		assert.Equal(t, first, second, "identical input+limit must produce the identical selection")
	}
}

// TestSelectBudgetFuzz sweeps fixed and random limits over synthetic streams
// and asserts the frozen invariants: budget never exceeded, at most one gap
// marker, protocol order preserved.
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
	units := BuildSemanticUnits(items)
	total := unitTotal(units)
	for _, limit := range []int64{0, 1, 10, 57, 58, 59, 60, 100, 300, 500, total - 1, total, total + 1, 1 << 20} {
		res := SelectEvidence(units, limit)
		assert.LessOrEqual(t, res.TotalBytes, limit, "limit %d", limit)
		if res.Gap == nil {
			if res.TotalBytes == 0 {
				assert.Empty(t, res.Items, "limit %d: degenerate limit", limit)
			} else {
				assert.Equal(t, items, res.Items, "limit %d: full fit", limit)
			}
		} else {
			assert.Equal(t, 1, gapKindCount(res.Items), "limit %d", limit)
		}
		assertSelectionInvariants(t, units, limit, res)
	}

	// Randomized streams: seeded for reproducibility.
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
		units := BuildSemanticUnits(randItems)
		limit := int64(rng.Intn(int(unitTotal(units)) + 2))
		res := SelectEvidence(units, limit)
		assert.LessOrEqual(t, res.TotalBytes, limit, "round %d limit %d", round, limit)
		if res.Gap == nil {
			if res.TotalBytes == 0 {
				assert.Empty(t, res.Items, "round %d limit %d: degenerate limit", round, limit)
			} else {
				assert.Equal(t, randItems, res.Items, "round %d limit %d: full fit", round, limit)
			}
		} else {
			assert.Equal(t, 1, gapKindCount(res.Items), "round %d limit %d", round, limit)
		}
		assertSelectionInvariants(t, units, limit, res)
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
	units := BuildSemanticUnits(items)
	limit := int64(10000)
	require.Len(t, units, 3)
	require.Greater(t, units[2].CanonicalBytes, limit, "precondition: newest unit alone over the limit")
	res := SelectEvidence(units, limit)

	require.Len(t, res.Oversized, 1)
	ov := res.Oversized[0]
	assert.Equal(t, SemanticUnitToolExchange, ov.Kind)
	assert.Equal(t, []string{"call_A"}, ov.CallIDs)
	assert.Equal(t, int64(12200), ov.LogicalBytes)
	assert.Equal(t, oversizedReasonUnit, ov.Reason)
	assert.False(t, ov.SourceTruncated)
	assert.False(t, containsItem(res.Items, items[2]), "oversized pair not split: call omitted")
	assert.False(t, containsItem(res.Items, items[3]), "oversized pair not split: result omitted")
	assert.True(t, containsItem(res.Items, items[0]), "anchor retained")
	assert.True(t, containsItem(res.Items, items[1]), "rest selected normally")
	require.NotNil(t, res.Gap)
	assert.Equal(t, gapPositionTail, res.Gap.Position)
	assert.Equal(t, gapReasonOversized, res.Gap.Reason)
	assert.Equal(t, 2, res.Gap.OmittedItems)
	assert.Equal(t, int64(12200), res.Gap.LogicalBytes)
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res)
}

func TestSelectOversizedLatestOnlyUnit(t *testing.T) {
	bigResult := mkResultItem("call_A", 12000)
	bigResult.Content[0].Result.Output = json.RawMessage(`"` + strings.Repeat("x", 12000) + `"`)
	items := []CanonicalItem{
		mkCallItem("call_A", 200),
		bigResult,
	}
	units := BuildSemanticUnits(items)
	limit := int64(10000)
	require.Greater(t, units[0].CanonicalBytes, limit, "precondition: the only unit over the limit")
	res := SelectEvidence(units, limit)
	require.Len(t, res.Oversized, 1)
	assert.Equal(t, oversizedReasonUnit, res.Oversized[0].Reason)
	require.NotNil(t, res.Gap)
	assert.Equal(t, gapPositionHead, res.Gap.Position, "a gap covering the whole stream is head")
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res)
}

func TestSelectAnchorBudgetCap(t *testing.T) {
	bigSystem := mkItem(CanonicalKindSystem, "", "", 23000, sp(strings.Repeat("s", 23000)))
	user := mkMsg("user", "latest", 100)
	items := []CanonicalItem{bigSystem, user}
	units := BuildSemanticUnits(items)
	limit := int64(20000) // total (~23.1KB) > limit, and the system alone > tail budget
	require.Greater(t, unitTotal(units), limit)
	res := SelectEvidence(units, limit)

	require.NotNil(t, res.Gap)
	assert.False(t, containsItem(res.Items, bigSystem), "giant system must not crowd out the tail")
	assert.True(t, containsItem(res.Items, user), "tail survives a giant anchor")
	require.Len(t, res.Oversized, 1)
	assert.Equal(t, oversizedReasonAnchor, res.Oversized[0].Reason)
	assert.Equal(t, SemanticUnitAnchor, res.Oversized[0].Kind)
	assert.Equal(t, int64(23000), res.Oversized[0].LogicalBytes)
	assert.LessOrEqual(t, res.TotalBytes, limit)
	assertSelectionInvariants(t, units, limit, res)

	// A system within the anchor budget is retained alongside the tail while
	// the middle is omitted.
	smallSystem := mkItem(CanonicalKindSystem, "", "", 2000, sp(strings.Repeat("s", 2000)))
	middle := mkMsg("user", strings.Repeat("u", 9000), 9000)
	units2 := BuildSemanticUnits([]CanonicalItem{smallSystem, middle, user})
	res2 := SelectEvidence(units2, 10000)
	require.NotNil(t, res2.Gap)
	assert.True(t, containsItem(res2.Items, smallSystem), "anchor within budget retained")
	assert.True(t, containsItem(res2.Items, user), "newest user retained")
	assert.False(t, containsItem(res2.Items, middle), "middle omitted")
	assert.Empty(t, res2.Oversized)
	assert.Equal(t, gapPositionMiddle, res2.Gap.Position)
	assertSelectionInvariants(t, units2, 10000, res2)
}

func TestSelectBoundaryTotalEqualsLimit(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "latest", 10),
	}
	units := BuildSemanticUnits(items)
	res := SelectEvidence(units, unitTotal(units))
	assert.Nil(t, res.Gap, "total == limit is a full fit, zero truncation")
	assert.Equal(t, items, res.Items)
	assert.Equal(t, unitTotal(units), res.TotalBytes)
}

func TestSelectDegenerateLimit(t *testing.T) {
	items := []CanonicalItem{
		mkItem(CanonicalKindSystem, "", "", 5, sp("sys")),
		mkMsg("user", "latest", 10),
	}
	units := BuildSemanticUnits(items)
	// The minimal gap marker alone is larger than these limits: the byte
	// budget is absolute, so the marker is dropped and Gap stays nil.
	for _, limit := range []int64{0, 1, 5, 10} {
		res := SelectEvidence(units, limit)
		assert.Empty(t, res.Items, "limit %d: degenerate limit yields no items", limit)
		assert.Nil(t, res.Gap, "limit %d: marker does not fit, no gap", limit)
		assert.Equal(t, int64(0), res.TotalBytes, "limit %d", limit)
	}
}

func TestGapMarkerShape(t *testing.T) {
	m := GapMarker(GapInfo{LogicalBytes: 12345})
	assert.Equal(t, CanonicalKindGap, m.Kind)
	assert.Equal(t, int64(12345), m.LogicalBytes)
	assert.True(t, m.Truncated)
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
// end-to-end shape tests over the read-only agent corpus (invariants only)

func TestSelectE2EFixtureCodexMultiturn(t *testing.T) {
	units, items := fixtureUnits(t, "codex-multiturn.json", string(types.RelayFormatOpenAIResponses), &dto.OpenAIResponsesRequest{})
	assert.NotEmpty(t, units)
	assertPairCompleteness(t, items, units)
	assertDeterministicUnits(t, items)
	total := unitTotal(units)
	for _, limit := range []int64{1, 60, 100, total - 1, total, total + 1} {
		res := SelectEvidence(units, limit)
		assert.LessOrEqual(t, res.TotalBytes, limit, "limit %d", limit)
		if res.Gap == nil {
			if res.TotalBytes == 0 {
				assert.Empty(t, res.Items, "limit %d: degenerate limit", limit)
			} else {
				assert.Equal(t, items, res.Items, "limit %d", limit)
			}
		} else {
			assert.Equal(t, 1, gapKindCount(res.Items), "limit %d", limit)
		}
		assertSelectionInvariants(t, units, limit, res)
		again := SelectEvidence(units, limit)
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
		res := SelectEvidence(units, limit)
		assert.LessOrEqual(t, res.TotalBytes, limit, "limit %d", limit)
		if res.Gap == nil {
			if res.TotalBytes == 0 {
				assert.Empty(t, res.Items, "limit %d: degenerate limit", limit)
			} else {
				assert.Equal(t, items, res.Items, "limit %d", limit)
			}
		} else {
			assert.Equal(t, 1, gapKindCount(res.Items), "limit %d", limit)
		}
		assertSelectionInvariants(t, units, limit, res)
		again := SelectEvidence(units, limit)
		assert.Equal(t, res, again, "limit %d: deterministic", limit)
	}
}

// fixtureUnits normalizes one agent-corpus fixture body into canonical items
// and builds the semantic units.
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
	units := BuildSemanticUnits(res.Items)
	require.NotEmpty(t, units)
	return units, res.Items
}

// assertPairCompleteness locks the pair invariant on real normalization
// output: every call ID declared by a tool call item has exactly one exchange
// unit holding the call and its result together.
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
			// Unpaired results survive as orphans — they must still be in some unit.
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
	first := BuildSemanticUnits(items)
	second := BuildSemanticUnits(items)
	assert.Equal(t, first, second)
}
