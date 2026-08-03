package relayobserver

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
)

// This file implements the semantic evidence selector (P0-C, PR #17A): the
// pure budget decision that turns semantic units into the canonical stream a
// truncated capture will persist. It never writes storage, never touches
// configuration, and never modifies the normalizer.
//
// Selection policy (audited, frozen):
//
//  1. Exact measurement first: if the full canonical total fits the limit, the
//     whole stream is returned with zero truncation (Gap = nil).
//  2. Anchor budget is an upper bound, not a reservation (D4): the selector
//     first selects anchors within their capped share, then ALL remaining
//     budget goes to the tail. If no anchor fits, the tail gets the full
//     limit. ("最新证据优先": the tail always gets the majority share.)
//  3. Anchor selection uses a continuous-prefix strategy (D7): only
//     system/developer directives at the head of the stream are eligible;
//     compact summaries are eligible only at unit index 0; the latest user
//     instruction is NOT an anchor candidate (it is naturally retained by the
//     tail). This ensures exactly one gap interval.
//  4. The tail is selected from the newest unit backward as a contiguous
//     suffix of whole units.
//  5. The gap marker's real serialized size (via SelectionPolicy.MeasureGap)
//     is re-checked against the limit and the selection rolls back from the
//     tail's oldest end (then anchors in reverse priority) until the
//     assembled stream fits. The budget is never exceeded (D1).
//  6. The newest unit alone larger than the limit is never split and never
//     forces an empty selection: it is excluded, recorded in Oversized
//     (reason oversized_semantic_unit), and carried by the gap. An anchor
//     unit larger than the anchor budget is likewise omitted and recorded
//     (reason oversized_anchor).
//  7. Degenerate limit: when the limit cannot even hold the gap marker, the
//     marker is dropped and Gap is set with Reason=capture_limit_too_small
//     (D2). TotalBytes = 0. The integration layer's envelope reservation
//     (MinCaptureEnvelopeBytes) keeps production limits out of this regime.
//  8. Assembly uses global item indexes (D5), not unit flattening, so the
//     output Items are in strict protocol order.

// DefaultAnchorRatio is the fraction of the limit reserved for anchors.
// 0.25 = 1/4. This is a float for policy configurability; the selector uses
// integer math.
const DefaultAnchorRatio = 0.25

// DefaultAnchorCap is the hard byte cap of the anchor share (8 KiB).
const DefaultAnchorCap = 8 * 1024

// Gap positions.
const (
	GapPositionHead   = "head"
	GapPositionMiddle = "middle"
	GapPositionTail   = "tail"
)

// Gap reasons.
const (
	GapReasonBudget        = "budget_exceeded"
	GapReasonOversized     = "oversized_semantic_unit"
	GapReasonLimitTooSmall = "capture_limit_too_small"
)

// Oversized reasons.
const (
	OversizedReasonUnit   = "oversized_semantic_unit"
	OversizedReasonAnchor = "oversized_anchor"
)

// SelectionPolicy carries the tunable parameters and the gap measurement
// callback for one evidence selection. The selector is a pure function; the
// policy is the only source of variability.
type SelectionPolicy struct {
	// Limit is the byte budget for the assembled stream (items + gap marker).
	Limit int64
	// AnchorRatio is the fraction of Limit reserved for anchors (e.g. 0.25).
	AnchorRatio float64
	// AnchorCap is the hard byte cap of the anchor share.
	AnchorCap int64
	// MeasureGap serializes a gap marker and returns the item, its marshaled
	// size, and any error. The integration layer provides the real
	// implementation (withHmac); tests use a synthetic 64-byte placeholder.
	MeasureGap func(GapInfo) (CanonicalItem, int64, error)
}

// DefaultSelectionPolicy returns a SelectionPolicy with the standard defaults
// and a gap marker that returns an empty-HMAC gap (the HMAC placeholder is
// the caller's responsibility).
func DefaultSelectionPolicy(limit int64) SelectionPolicy {
	return SelectionPolicy{
		Limit:       limit,
		AnchorRatio: DefaultAnchorRatio,
		AnchorCap:   DefaultAnchorCap,
		MeasureGap:  defaultMeasureGap,
	}
}

func defaultMeasureGap(g GapInfo) (CanonicalItem, int64, error) {
	it := CanonicalItem{Kind: CanonicalKindGap, LogicalBytes: g.LogicalBytes, Truncated: true}
	payload, err := common.Marshal(it)
	if err != nil {
		return it, 0, err
	}
	return it, int64(len(payload)), nil
}

// GapInfo describes the single omitted interval of a truncated selection.
type GapInfo struct {
	Position        string `json:"position"` // head | middle | tail
	Reason          string `json:"reason"`
	OmittedItems    int    `json:"omitted_items"`
	LogicalBytes    int64  `json:"logical_bytes"`
	SourceTruncated bool   `json:"source_truncated,omitempty"`
}

// OversizedUnitInfo records a unit the selection had to exclude whole.
type OversizedUnitInfo struct {
	Kind            SemanticUnitKind `json:"kind"`
	CallIDs         []string         `json:"call_ids,omitempty"`
	LogicalBytes    int64            `json:"logical_bytes"`
	CanonicalBytes  int64            `json:"canonical_bytes"`
	SourceTruncated bool             `json:"source_truncated,omitempty"`
	Reason          string           `json:"reason"`
}

// SelectionResult is the outcome of one evidence selection.
// Items holds the retained canonical items in strict protocol order (by
// global item index, not unit flattening). When Gap is non-nil, the gap
// marker is already embedded in Items at the correct position. TotalBytes
// is the marshaled size of Items and never exceeds the policy limit.
// TotalBytes == 0 is valid only for a degenerate limit (Gap != nil,
// Reason=capture_limit_too_small) or for an empty input stream.
type SelectionResult struct {
	Items      []CanonicalItem
	Gap        *GapInfo
	Oversized  []OversizedUnitInfo
	TotalBytes int64
}

// SelectEvidence selects evidence from semantic units under a policy.
// Deterministic: identical units and policy always yield the same result.
func SelectEvidence(units []SemanticUnit, policy SelectionPolicy) (SelectionResult, error) {
	var res SelectionResult
	n := len(units)
	if n == 0 {
		return res, nil
	}
	limit := policy.Limit
	if limit < 0 {
		limit = 0
	}

	// Step 1: exact full measurement.
	var total int64
	for i := range units {
		total += units[i].CanonicalBytes
	}

	// Step 2: full fit — zero truncation.
	if total <= limit {
		items := make([]CanonicalItem, 0, countUnitItems(units))
		// Global index assembly (D5).
		for i := range units {
			items = append(items, units[i].Items...)
		}
		res.Items = items
		res.TotalBytes = total
		return res, nil
	}

	// Step 3a: anchor budget as an upper bound (D4). Compute anchor budget,
	// then the tail gets the rest.
	anchorBudget := int64(float64(limit) * policy.AnchorRatio)
	if anchorBudget > policy.AnchorCap {
		anchorBudget = policy.AnchorCap
	}
	if anchorBudget < 0 {
		anchorBudget = 0
	}

	// Step 3b: the newest unit alone over the limit is never split.
	excluded := make([]bool, n)
	if units[n-1].CanonicalBytes > limit {
		res.Oversized = append(res.Oversized, oversizedInfo(units[n-1], OversizedReasonUnit))
		excluded[n-1] = true
	}

	// Step 3c: continuous-prefix anchor selection (D7). Only system/developer
	// directives at the head of the stream (contiguous from index 0), and
	// compact summaries at index 0. No latest-user anchor — the tail naturally
	// retains it.
	selected := make([]bool, n)
	var anchorBytes int64
	for i := 0; i < n; i++ {
		if excluded[i] {
			continue
		}
		u := units[i]
		// Anchor eligible: system/developer type at head of stream, or
		// compact summary at index 0.
		eligible := false
		if u.Kind == SemanticUnitAnchor || isSystemDeveloperMessage(&u) {
			eligible = true
		}
		if u.Kind == SemanticUnitCompactSummary && i == 0 {
			eligible = true
		}
		if !eligible {
			break // stop at the first non-anchor — continuous prefix (D7)
		}
		if u.CanonicalBytes > anchorBudget {
			// An oversized anchor cannot fit the anchor share. Recording it
			// and continuing would create an anchor island inside the gap
			// (D7 forbids a retained anchor between two omitted ranges), so
			// the continuous prefix ends here.
			res.Oversized = append(res.Oversized, oversizedInfo(u, OversizedReasonAnchor))
			break
		}
		if anchorBytes+u.CanonicalBytes > anchorBudget {
			// The anchor share is exhausted: the prefix must stay continuous,
			// so no later unit can be an anchor either.
			break
		}
		selected[i] = true
		anchorBytes += u.CanonicalBytes
	}

	// Step 3d: tail selection from the newest unit backward. The full
	// remaining budget (limit - anchorBytes) is available to the tail (D4).
	tailBudget := limit - anchorBytes
	var tailBytes int64
	for i := n - 1; i >= 0; i-- {
		if excluded[i] || selected[i] {
			continue
		}
		u := units[i]
		if u.CanonicalBytes > tailBudget-tailBytes {
			break
		}
		selected[i] = true
		tailBytes += u.CanonicalBytes
	}

	// Step 3e: build the gap info over the omitted interval. Excluded
	// oversized units are also omitted evidence: they are recorded in
	// Oversized AND carried by the gap (their bytes and item count appear
	// here), so reconstruction sees the full extent of the loss.
	firstOmitted, lastOmitted := -1, -1
	gap := GapInfo{Reason: GapReasonBudget}
	if excluded[n-1] {
		gap.Reason = GapReasonOversized
	}
	for i := range units {
		if selected[i] {
			continue
		}
		if firstOmitted < 0 {
			firstOmitted = i
		}
		lastOmitted = i
		gap.OmittedItems += len(units[i].Items)
		gap.LogicalBytes += units[i].LogicalBytes
		if units[i].SourceTruncated {
			gap.SourceTruncated = true
		}
	}
	switch {
	case firstOmitted == 0:
		gap.Position = GapPositionHead
	case lastOmitted == n-1:
		gap.Position = GapPositionTail
	default:
		gap.Position = GapPositionMiddle
	}

	// Step 3f: measure the gap marker and re-validate (D1).
	gapMarker, markerBytes, err := policy.MeasureGap(gap)
	if err != nil {
		return SelectionResult{}, fmt.Errorf("measure gap: %w", err)
	}

	selectedBytes := anchorBytes + tailBytes
	for selectedBytes+markerBytes > limit {
		dropped := false
		// Drop the tail's oldest retained unit first.
		if lastOmitted+1 < n {
			for i := lastOmitted + 1; i < n; i++ {
				if selected[i] {
					selected[i] = false
					selectedBytes -= units[i].CanonicalBytes
					dropFromGap(&gap, &units[i], &firstOmitted, &lastOmitted, i)
					gapMarker, markerBytes, err = policy.MeasureGap(gap)
					if err != nil {
						return SelectionResult{}, fmt.Errorf("measure gap (rollback): %w", err)
					}
					dropped = true
					break
				}
			}
		}
		if !dropped {
			// Drop the last anchor (highest index).
			for i := n - 1; i >= 0; i-- {
				if selected[i] && (units[i].Kind == SemanticUnitAnchor || isSystemDeveloperMessage(&units[i]) || (units[i].Kind == SemanticUnitCompactSummary && i == 0)) {
					selected[i] = false
					selectedBytes -= units[i].CanonicalBytes
					dropFromGap(&gap, &units[i], &firstOmitted, &lastOmitted, i)
					gapMarker, markerBytes, err = policy.MeasureGap(gap)
					if err != nil {
						return SelectionResult{}, fmt.Errorf("measure gap (anchor rollback): %w", err)
					}
					dropped = true
					break
				}
			}
		}
		if !dropped {
			break
		}
	}

	// Step 3g: degenerate limit (D2).
	if selectedBytes+markerBytes > limit {
		res.Gap = &GapInfo{
			Position:     GapPositionHead,
			Reason:       GapReasonLimitTooSmall,
			OmittedItems: countUnitItems(units),
			LogicalBytes: totalLogicalBytes(units),
		}
		return res, nil
	}

	// Step 4: assemble in strict protocol order (D5). The gap marker goes
	// between the anchor part and the tail part: at the position of the
	// first omitted unit. Items of each selected unit are in original
	// protocol order (ItemIndexes are sorted), and units themselves are
	// sorted by their first item index, so flattening selected units in
	// unit order yields the global protocol order.
	out := make([]CanonicalItem, 0, countUnitItems(units)+1)
	for i := 0; i < n; i++ {
		if i == firstOmitted {
			out = append(out, gapMarker)
		}
		if selected[i] {
			out = append(out, units[i].Items...)
		}
	}
	res.Items = out
	g := gap
	res.Gap = &g
	res.TotalBytes = selectedBytes + markerBytes
	return res, nil
}

// GapMarker builds the canonical gap marker item from a GapInfo. The returned
// item has no digest (Hmac is empty): the integration layer attaches the
// keyed digest via its MeasureGap callback.
func GapMarker(g GapInfo) CanonicalItem {
	return CanonicalItem{Kind: CanonicalKindGap, LogicalBytes: g.LogicalBytes, Truncated: true}
}

// dropFromGap moves one retained unit into the omitted interval.
func dropFromGap(gap *GapInfo, u *SemanticUnit, first, last *int, idx int) {
	gap.OmittedItems += len(u.Items)
	gap.LogicalBytes += u.LogicalBytes
	if u.SourceTruncated {
		gap.SourceTruncated = true
	}
	if idx < *first || *first < 0 {
		*first = idx
	}
	if idx > *last {
		*last = idx
	}
}

func oversizedInfo(u SemanticUnit, reason string) OversizedUnitInfo {
	return OversizedUnitInfo{
		Kind:            u.Kind,
		CallIDs:         u.CallIDs,
		LogicalBytes:    u.LogicalBytes,
		CanonicalBytes:  u.CanonicalBytes,
		SourceTruncated: u.SourceTruncated,
		Reason:          reason,
	}
}

func countUnitItems(units []SemanticUnit) int {
	n := 0
	for i := range units {
		n += len(units[i].Items)
	}
	return n
}

func totalLogicalBytes(units []SemanticUnit) int64 {
	var t int64
	for i := range units {
		t += units[i].LogicalBytes
	}
	return t
}
