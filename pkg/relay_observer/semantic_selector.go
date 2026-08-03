package relayobserver

import (
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"sort"
)

// This file implements the semantic evidence selector (P0-C, PR #17A).
// The selector stays inside the relay observer package and has no storage,
// HTTP, service, or database dependencies.
//
// A semantic unit may contain non-adjacent protocol items when a tool call and
// result are paired by ID. A single-gap output cannot safely cut through such
// a span. The selector therefore derives internal selection blocks by merging
// overlapping unit spans. Block boundaries are exactly the safe protocol cut
// points: selecting whole blocks preserves tool atomicity, original item order,
// and the v1 layout of a continuous prefix, one gap, and a continuous suffix.

const DefaultAnchorRatio = 0.25
const DefaultAnchorCap = 8 * 1024

const (
	GapPositionHead   = "head"
	GapPositionMiddle = "middle"
	GapPositionTail   = "tail"
)

const (
	GapReasonBudget        = "capture_budget"
	GapReasonOversized     = "oversized_semantic_unit"
	GapReasonLimitTooSmall = "capture_limit_too_small"
	GapReasonItemCount     = "item_count_limit"
)

const (
	OversizedReasonUnit   = "oversized_semantic_unit"
	OversizedReasonAnchor = "oversized_anchor"
)

// GapBuilder returns the final canonical gap item, including its keyed HMAC.
// SelectEvidence measures the returned item itself, so callers cannot provide a
// stale or hand-written byte estimate.
type GapBuilder func(GapInfo) (CanonicalItem, error)

type SelectionPolicy struct {
	Limit       int64
	AnchorRatio float64
	AnchorCap   int64
	BuildGap    GapBuilder
}

func DefaultSelectionPolicy(limit int64, buildGap GapBuilder) SelectionPolicy {
	return SelectionPolicy{
		Limit:       limit,
		AnchorRatio: DefaultAnchorRatio,
		AnchorCap:   DefaultAnchorCap,
		BuildGap:    buildGap,
	}
}

// GapInfo describes the single omitted protocol interval of a truncated
// selection. It is embedded in the canonical gap item and therefore survives
// persistence and reconstruction without a database schema change.
type GapInfo struct {
	Position        string              `json:"position"`
	Reason          string              `json:"reason"`
	OmittedItems    int                 `json:"omitted_items"`
	LogicalBytes    int64               `json:"logical_bytes"`
	SourceTruncated bool                `json:"source_truncated,omitempty"`
	Oversized       []OversizedUnitInfo `json:"oversized_units,omitempty"`
}

type OversizedUnitInfo struct {
	Kind            SemanticUnitKind `json:"kind"`
	CallIDs         []string         `json:"call_ids,omitempty"`
	LogicalBytes    int64            `json:"logical_bytes"`
	CanonicalBytes  int64            `json:"canonical_bytes"`
	SourceTruncated bool             `json:"source_truncated,omitempty"`
	Reason          string           `json:"reason"`
}

type SelectionResult struct {
	Items      []CanonicalItem
	Gap        *GapInfo
	Oversized  []OversizedUnitInfo
	TotalBytes int64
}

type indexedSemanticStream struct {
	items             []CanonicalItem
	itemToUnit        []int
	unitBytes         []int64
	unitStarts        []int
	unitEnds          []int
	totalBytes        int64
	totalLogicalBytes int64
	sourceTruncated   bool
}

type selectionBlock struct {
	Start           int
	End             int
	UnitIndexes     []int
	CanonicalBytes  int64
	LogicalBytes    int64
	SourceTruncated bool
	Anchor          bool
}

type unitSpan struct {
	Unit  int
	Start int
	End   int
}

// SelectEvidence selects a deterministic canonical subsequence under policy.
// Full-fit requests return the exact original item order. Truncated requests
// retain whole safe-cut blocks and add at most one structured capture gap.
// Existing source/item-count gap items are evidence in the input stream and
// may coexist with that capture gap when they describe a different omission.
func SelectEvidence(units []SemanticUnit, policy SelectionPolicy) (SelectionResult, error) {
	var res SelectionResult
	if len(units) == 0 {
		return res, nil
	}

	stream, err := indexSemanticStream(units)
	if err != nil {
		return SelectionResult{}, err
	}
	limit := policy.Limit
	if limit < 0 {
		limit = 0
	}
	if stream.totalBytes <= limit {
		res.Items = append([]CanonicalItem(nil), stream.items...)
		res.TotalBytes = stream.totalBytes
		return res, nil
	}
	if policy.BuildGap == nil {
		return SelectionResult{}, fmt.Errorf("semantic selector: gap builder is required for truncation")
	}

	blocks, unitToBlock := buildSelectionBlocks(units, stream)
	selected := make([]bool, len(blocks))
	excluded := make([]bool, len(blocks))
	recordedOversized := make(map[int]bool)

	ratio := policy.AnchorRatio
	if math.IsNaN(ratio) || ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	anchorCap := policy.AnchorCap
	if anchorCap < 0 {
		anchorCap = 0
	}
	anchorBudget := int64(float64(limit) * ratio)
	if anchorBudget > anchorCap {
		anchorBudget = anchorCap
	}

	latestUnit := stream.itemToUnit[len(stream.items)-1]
	latestBlock := unitToBlock[latestUnit]
	latestOversized := stream.unitBytes[latestUnit] > limit
	if latestOversized {
		excluded[latestBlock] = true
		appendOversized(&res, recordedOversized, latestUnit, units[latestUnit], stream.unitBytes[latestUnit], OversizedReasonUnit)
	}

	var selectedBytes int64
	for i := 0; i < len(blocks); i++ {
		block := blocks[i]
		if excluded[i] || !block.Anchor {
			break
		}
		if block.CanonicalBytes > anchorBudget-selectedBytes {
			for _, unitIndex := range block.UnitIndexes {
				if stream.unitBytes[unitIndex] > anchorBudget {
					appendOversized(&res, recordedOversized, unitIndex, units[unitIndex], stream.unitBytes[unitIndex], OversizedReasonAnchor)
				}
			}
			break
		}
		selected[i] = true
		selectedBytes += block.CanonicalBytes
	}

	if latestOversized {
		// With an omitted newest block, a one-gap layout can only retain a
		// continuous prefix. Extend the prefix as far as the content budget
		// allows; marker rollback below reserves the final exact gap size.
		for i := 0; i < latestBlock; i++ {
			if excluded[i] {
				break
			}
			if selected[i] {
				continue
			}
			if i > 0 && !selected[i-1] {
				break
			}
			if blocks[i].CanonicalBytes > limit-selectedBytes {
				break
			}
			selected[i] = true
			selectedBytes += blocks[i].CanonicalBytes
		}
	} else {
		// Anchors are optional. If they prevent the newest block from fitting,
		// release them from the end of the prefix before selecting the tail.
		newestBlock := len(blocks) - 1
		for blocks[newestBlock].CanonicalBytes > limit-selectedBytes {
			dropAnchor := -1
			for i := newestBlock - 1; i >= 0; i-- {
				if selected[i] {
					dropAnchor = i
					break
				}
			}
			if dropAnchor < 0 {
				break
			}
			selected[dropAnchor] = false
			selectedBytes -= blocks[dropAnchor].CanonicalBytes
		}

		// Retain a continuous suffix from the newest safe block.
		for i := newestBlock; i >= 0; i-- {
			if selected[i] {
				break
			}
			if excluded[i] || blocks[i].CanonicalBytes > limit-selectedBytes {
				break
			}
			selected[i] = true
			selectedBytes += blocks[i].CanonicalBytes
		}
	}

	gapReason := GapReasonBudget
	if latestOversized {
		gapReason = GapReasonOversized
	}
	gap, firstOmittedBlock, lastOmittedBlock, err := gapForSelection(blocks, selected, len(stream.items), gapReason, res.Oversized)
	if err != nil {
		return SelectionResult{}, err
	}
	gapMarker, markerBytes, err := buildAndMeasureGap(policy.BuildGap, gap)
	if err != nil {
		return SelectionResult{}, err
	}

	for selectedBytes+markerBytes > limit {
		dropBlock := -1
		suffixStart := lastOmittedBlock + 1
		prefixEnd := firstOmittedBlock - 1
		switch {
		case suffixStart < len(blocks) && suffixStart+1 < len(blocks):
			// More than one tail block remains: remove the oldest one while
			// preserving the newest evidence.
			dropBlock = suffixStart
		case prefixEnd >= 0:
			// Only the newest tail block remains (or there is no suffix).
			// Release the optional prefix before sacrificing that block.
			dropBlock = prefixEnd
		case suffixStart < len(blocks):
			// The marker and the sole newest block cannot coexist.
			dropBlock = suffixStart
		}
		if dropBlock < 0 || !selected[dropBlock] {
			break
		}
		selected[dropBlock] = false
		selectedBytes -= blocks[dropBlock].CanonicalBytes
		gap, firstOmittedBlock, lastOmittedBlock, err = gapForSelection(blocks, selected, len(stream.items), gapReason, res.Oversized)
		if err != nil {
			return SelectionResult{}, err
		}
		gapMarker, markerBytes, err = buildAndMeasureGap(policy.BuildGap, gap)
		if err != nil {
			return SelectionResult{}, err
		}
	}

	if selectedBytes+markerBytes > limit {
		res.Gap = &GapInfo{
			Position:        GapPositionHead,
			Reason:          GapReasonLimitTooSmall,
			OmittedItems:    len(stream.items),
			LogicalBytes:    stream.totalLogicalBytes,
			SourceTruncated: stream.sourceTruncated,
			Oversized:       cloneOversizedUnits(res.Oversized),
		}
		return res, nil
	}

	selectedItems := make([]bool, len(stream.items))
	for i, block := range blocks {
		if !selected[i] {
			continue
		}
		for itemIndex := block.Start; itemIndex <= block.End; itemIndex++ {
			selectedItems[itemIndex] = true
		}
	}
	firstOmittedItem := blocks[firstOmittedBlock].Start
	out := make([]CanonicalItem, 0, len(stream.items)-gap.OmittedItems+1)
	for itemIndex, item := range stream.items {
		if itemIndex == firstOmittedItem {
			out = append(out, gapMarker)
		}
		if selectedItems[itemIndex] {
			out = append(out, item)
		}
	}
	res.Items = out
	res.Gap = &gap
	res.TotalBytes = selectedBytes + markerBytes
	return res, nil
}

func indexSemanticStream(units []SemanticUnit) (indexedSemanticStream, error) {
	var stream indexedSemanticStream
	itemCount := countUnitItems(units)
	if itemCount == 0 {
		return stream, fmt.Errorf("semantic selector: units contain no items")
	}
	maxIndex := -1
	for unitIndex := range units {
		u := units[unitIndex]
		if len(u.Items) == 0 || len(u.Items) != len(u.ItemIndexes) {
			return stream, fmt.Errorf("semantic selector: unit %d has inconsistent items and indexes", unitIndex)
		}
		for _, itemIndex := range u.ItemIndexes {
			if itemIndex < 0 {
				return stream, fmt.Errorf("semantic selector: unit %d has negative item index %d", unitIndex, itemIndex)
			}
			if itemIndex > maxIndex {
				maxIndex = itemIndex
			}
		}
	}
	if maxIndex+1 != itemCount {
		return stream, fmt.Errorf("semantic selector: item indexes must form a dense zero-based stream")
	}

	stream.items = make([]CanonicalItem, itemCount)
	stream.itemToUnit = make([]int, itemCount)
	stream.unitBytes = make([]int64, len(units))
	stream.unitStarts = make([]int, len(units))
	stream.unitEnds = make([]int, len(units))
	seen := make([]bool, itemCount)
	for unitIndex := range units {
		u := units[unitIndex]
		start, end := itemCount, -1
		for localIndex, itemIndex := range u.ItemIndexes {
			if itemIndex >= itemCount || seen[itemIndex] {
				return stream, fmt.Errorf("semantic selector: duplicate or out-of-range item index %d", itemIndex)
			}
			seen[itemIndex] = true
			item := u.Items[localIndex]
			itemBytes, err := canonicalBytesOf(item)
			if err != nil {
				return stream, fmt.Errorf("semantic selector: item %d: %w", itemIndex, err)
			}
			stream.items[itemIndex] = item
			stream.itemToUnit[itemIndex] = unitIndex
			stream.unitBytes[unitIndex] += itemBytes
			stream.totalBytes += itemBytes
			stream.totalLogicalBytes += item.LogicalBytes
			stream.sourceTruncated = stream.sourceTruncated || itemSourceTruncated(item)
			if itemIndex < start {
				start = itemIndex
			}
			if itemIndex > end {
				end = itemIndex
			}
		}
		stream.unitStarts[unitIndex] = start
		stream.unitEnds[unitIndex] = end
	}
	for itemIndex, ok := range seen {
		if !ok {
			return stream, fmt.Errorf("semantic selector: missing item index %d", itemIndex)
		}
	}
	return stream, nil
}

func buildSelectionBlocks(units []SemanticUnit, stream indexedSemanticStream) ([]selectionBlock, []int) {
	spans := make([]unitSpan, len(units))
	for unitIndex := range units {
		spans[unitIndex] = unitSpan{Unit: unitIndex, Start: stream.unitStarts[unitIndex], End: stream.unitEnds[unitIndex]}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Start != spans[j].Start {
			return spans[i].Start < spans[j].Start
		}
		return spans[i].End < spans[j].End
	})

	blocks := make([]selectionBlock, 0, len(spans))
	unitToBlock := make([]int, len(units))
	for _, span := range spans {
		if len(blocks) == 0 || span.Start > blocks[len(blocks)-1].End {
			blocks = append(blocks, selectionBlock{Start: span.Start, End: span.End, Anchor: true})
		}
		blockIndex := len(blocks) - 1
		block := &blocks[blockIndex]
		if span.End > block.End {
			block.End = span.End
		}
		block.UnitIndexes = append(block.UnitIndexes, span.Unit)
		block.CanonicalBytes += stream.unitBytes[span.Unit]
		block.LogicalBytes += units[span.Unit].LogicalBytes
		block.SourceTruncated = block.SourceTruncated || units[span.Unit].SourceTruncated
		block.Anchor = block.Anchor && anchorEligible(units[span.Unit], span.Start)
		unitToBlock[span.Unit] = blockIndex
	}
	return blocks, unitToBlock
}

func anchorEligible(unit SemanticUnit, start int) bool {
	if unit.Kind == SemanticUnitCompactSummary {
		return start == 0
	}
	return unit.Kind == SemanticUnitAnchor || isSystemDeveloperMessage(&unit)
}

func gapForSelection(blocks []selectionBlock, selected []bool, itemCount int, reason string, oversized []OversizedUnitInfo) (GapInfo, int, int, error) {
	first, last := -1, -1
	runs := 0
	inRun := false
	gap := GapInfo{Reason: reason, Oversized: cloneOversizedUnits(oversized)}
	for blockIndex, block := range blocks {
		if selected[blockIndex] {
			inRun = false
			continue
		}
		if !inRun {
			runs++
			inRun = true
		}
		if first < 0 {
			first = blockIndex
		}
		last = blockIndex
		gap.OmittedItems += block.End - block.Start + 1
		gap.LogicalBytes += block.LogicalBytes
		gap.SourceTruncated = gap.SourceTruncated || block.SourceTruncated
	}
	if first < 0 {
		return GapInfo{}, -1, -1, fmt.Errorf("semantic selector: truncated selection has no omitted block")
	}
	if runs != 1 {
		return GapInfo{}, -1, -1, fmt.Errorf("semantic selector: selection produced %d omitted intervals", runs)
	}
	switch {
	case blocks[first].Start == 0:
		gap.Position = GapPositionHead
	case blocks[last].End == itemCount-1:
		gap.Position = GapPositionTail
	default:
		gap.Position = GapPositionMiddle
	}
	return gap, first, last, nil
}

func buildAndMeasureGap(build GapBuilder, gap GapInfo) (CanonicalItem, int64, error) {
	marker, err := build(gap)
	if err != nil {
		return CanonicalItem{}, 0, fmt.Errorf("semantic selector: build gap: %w", err)
	}
	if marker.Kind != CanonicalKindGap || marker.Gap == nil || !reflect.DeepEqual(*marker.Gap, gap) {
		return CanonicalItem{}, 0, fmt.Errorf("semantic selector: gap builder returned an invalid marker")
	}
	digest, decodeErr := hex.DecodeString(marker.Hmac)
	if decodeErr != nil || len(digest) != 32 {
		return CanonicalItem{}, 0, fmt.Errorf("semantic selector: gap builder must attach a final SHA-256 HMAC")
	}
	markerBytes, err := canonicalBytesOf(marker)
	if err != nil {
		return CanonicalItem{}, 0, fmt.Errorf("semantic selector: measure gap: %w", err)
	}
	return marker, markerBytes, nil
}

// GapMarker builds the structured canonical marker before the caller attaches
// the keyed HMAC. A copy of GapInfo is embedded so later caller mutation cannot
// change the selected result.
func GapMarker(gap GapInfo) CanonicalItem {
	info := gap
	info.Oversized = cloneOversizedUnits(gap.Oversized)
	return CanonicalItem{
		Kind:         CanonicalKindGap,
		LogicalBytes: gap.LogicalBytes,
		Truncated:    true,
		Gap:          &info,
	}
}

func appendOversized(res *SelectionResult, recorded map[int]bool, unitIndex int, unit SemanticUnit, canonicalBytes int64, reason string) {
	if recorded[unitIndex] {
		return
	}
	recorded[unitIndex] = true
	res.Oversized = append(res.Oversized, OversizedUnitInfo{
		Kind:            unit.Kind,
		CallIDs:         append([]string(nil), unit.CallIDs...),
		LogicalBytes:    unit.LogicalBytes,
		CanonicalBytes:  canonicalBytes,
		SourceTruncated: unit.SourceTruncated,
		Reason:          reason,
	})
}

func cloneOversizedUnits(units []OversizedUnitInfo) []OversizedUnitInfo {
	if len(units) == 0 {
		return nil
	}
	cloned := make([]OversizedUnitInfo, len(units))
	copy(cloned, units)
	for i := range cloned {
		cloned[i].CallIDs = append([]string(nil), cloned[i].CallIDs...)
	}
	return cloned
}

func countUnitItems(units []SemanticUnit) int {
	count := 0
	for i := range units {
		count += len(units[i].Items)
	}
	return count
}
