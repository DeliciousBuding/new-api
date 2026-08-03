package relayobserver

import (
	"github.com/QuantumNous/new-api/common"
)

// This file implements the semantic evidence selector (P0-C, PR #17A): the
// pure budget decision that turns semantic units into the canonical stream a
// truncated capture will persist — anchors (stable prefix), one explicit gap
// marker (data, not silent loss), and the newest tail evidence. It never
// writes storage, never touches configuration, and never modifies the
// normalizer; the integration layer assembles the final canonical stream from
// SelectionResult.Items and GapMarker(Gap).
//
// Selection policy (audited and frozen):
//
//  1. Exact measurement first: if the full canonical total fits the limit, the
//     whole stream is returned with zero truncation (Gap = nil).
//  2. When truncated, anchors are selected within a capped anchor budget —
//     one quarter of the limit, hard-capped at 8 KiB — so a giant system
//     directive can never crowd out the tail ("最新证据优先": the newest
//     evidence always keeps the larger share; anchors keep a guaranteed but
//     bounded share).
//  3. The tail is selected from the newest unit backward as a contiguous
//     suffix of whole units, so the newest user instruction and tool chains
//     are retained at 100% whenever they fit.
//  4. The gap marker's real serialized size is re-checked against the limit
//     and the selection rolls back from the tail's oldest end (then anchors in
//     reverse priority) until the assembled stream fits. The budget is never
//     exceeded.
//  5. The newest unit alone larger than the limit is never split and never
//     forces an empty selection: it is excluded, recorded in Oversized
//     (reason oversized_semantic_unit), and carried by the gap; the rest is
//     selected normally. An anchor unit larger than the anchor budget is
//     likewise omitted and recorded (reason oversized_anchor) — trimming an
//     anchor's head is deferred to v2 because splitting a canonical item would
//     invalidate its digest.
//
// Degenerate limit: when the limit cannot even hold the gap marker alone, the
// marker is dropped and Gap = nil — the byte budget is absolute. The
// integration layer's envelope reservation (MinCaptureEnvelopeBytes, mirroring
// finishNormalize) keeps production limits out of this regime.

const (
	// anchorBudgetFraction is the share of the limit reserved for anchor
	// candidates. limit/4 is exact integer math, no floating point.
	anchorBudgetFraction = 4
	// anchorBudgetCap is the hard byte cap of the anchor share (8 KiB): a
	// 21 KiB system directive must not consume a multi-megabyte budget.
	anchorBudgetCap = 8 * 1024
	// gap positions.
	gapPositionHead   = "head"
	gapPositionMiddle = "middle"
	gapPositionTail   = "tail"
	// gap reasons.
	gapReasonBudget    = "budget_exceeded"
	gapReasonOversized = "oversized_semantic_unit"
	// oversized unit reasons.
	oversizedReasonUnit   = "oversized_semantic_unit"
	oversizedReasonAnchor = "oversized_anchor"
)

// GapInfo describes the single omitted interval of a truncated selection.
// Position is where the omitted interval sat in the original stream: "head"
// (nothing retained before it), "middle" (anchors before, tail after), or
// "tail" (the omitted interval reaches the original end, e.g. an oversized
// newest unit). SourceTruncated reports that the omitted interval contains
// content that was already truncated at the source.
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
	Reason          string           `json:"reason"` // oversized_semantic_unit | oversized_anchor
}

// SelectionResult is the outcome of one evidence selection. Items holds the
// retained canonical items in protocol order; when Gap is non-nil the items
// are the anchor part followed by the tail part, and the gap marker
// (GapMarker(Gap)) is inserted by the caller between the two parts — the
// marker is also the shape the caller persists as the truncation record.
// TotalBytes is the marshaled size of the assembled stream (Items plus the
// gap marker when truncated) and never exceeds the limit.
type SelectionResult struct {
	Items      []CanonicalItem
	Gap        *GapInfo
	Oversized  []OversizedUnitInfo
	TotalBytes int64
}

// SelectEvidence selects evidence from semantic units under a byte limit.
// Deterministic: identical units and limit always yield the identical result.
func SelectEvidence(units []SemanticUnit, limit int64) SelectionResult {
	if limit < 0 {
		limit = 0
	}
	var res SelectionResult
	n := len(units)
	if n == 0 {
		return res
	}

	// Step 1: exact full measurement.
	var total int64
	for i := range units {
		total += units[i].CanonicalBytes
	}
	res.TotalBytes = total

	// Step 2: full fit — zero truncation.
	if total <= limit {
		for i := range units {
			res.Items = append(res.Items, units[i].Items...)
		}
		return res
	}

	// Step 3a: the newest unit alone over the limit is never split and never
	// cleared: exclude it, record it, and let the gap carry it.
	excluded := make([]bool, n)
	if units[n-1].CanonicalBytes > limit {
		res.Oversized = append(res.Oversized, oversizedInfo(units[n-1], oversizedReasonUnit))
		excluded[n-1] = true
	}

	// Step 3b: tail first (newest evidence), then anchors within their capped
	// budget. The tail keeps the larger share; the anchor share is bounded so
	// a giant system directive cannot consume the budget.
	anchorBudget := limit / anchorBudgetFraction
	if anchorBudget > anchorBudgetCap {
		anchorBudget = anchorBudgetCap
	}
	tailBudget := limit - anchorBudget

	selected := make([]bool, n)
	var tailBytes int64
	for i := n - 1; i >= 0; i-- {
		if excluded[i] {
			continue
		}
		u := units[i]
		if u.CanonicalBytes > tailBudget-tailBytes {
			break
		}
		selected[i] = true
		tailBytes += u.CanonicalBytes
	}

	// Anchor candidates in priority order: system/developer directives in
	// stream order, compact summaries in stream order, then the latest user
	// instruction. A candidate alone over the anchor budget is omitted and
	// recorded (v1 degradation: marking, not head-trimming — splitting a
	// canonical item would break its digest); a candidate that merely no
	// longer fits beside higher-priority anchors is silently left in the gap.
	cands := make([]int, 0, n)
	for i := range units {
		if !units[i].Anchor || selected[i] || excluded[i] {
			continue
		}
		if units[i].Kind == SemanticUnitAnchor || isSystemDeveloperMessage(&units[i]) {
			cands = append(cands, i)
		}
	}
	for i := range units {
		if !units[i].Anchor || selected[i] || excluded[i] {
			continue
		}
		if units[i].Kind == SemanticUnitCompactSummary {
			cands = append(cands, i)
		}
	}
	for i := n - 1; i >= 0; i-- {
		if !units[i].Anchor || selected[i] || excluded[i] {
			continue
		}
		if isUserMessage(&units[i]) {
			cands = append(cands, i)
			break
		}
	}
	var anchorBytes int64
	for _, i := range cands {
		u := units[i]
		if u.CanonicalBytes > anchorBudget {
			res.Oversized = append(res.Oversized, oversizedInfo(u, oversizedReasonAnchor))
			continue
		}
		if anchorBytes+u.CanonicalBytes > anchorBudget {
			continue
		}
		selected[i] = true
		anchorBytes += u.CanonicalBytes
	}

	// Step 3c: the omitted interval, gap info, and the marker.
	firstOmitted, lastOmitted := -1, -1
	gap := GapInfo{Reason: gapReasonBudget}
	if excluded[n-1] {
		gap.Reason = gapReasonOversized
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
		gap.Position = gapPositionHead
	case lastOmitted == n-1:
		gap.Position = gapPositionTail
	default:
		gap.Position = gapPositionMiddle
	}
	markerBytes := int64(len(mustMarshalItem(GapMarker(gap))))

	// Step 3d: re-validate with the real marker size; roll back the tail's
	// oldest unit (then anchors in reverse priority) until the assembled
	// stream fits. Dropping the tail's oldest end keeps the newest evidence.
	selectedBytes := tailBytes + anchorBytes
	for selectedBytes+markerBytes > limit {
		dropped := false
		if lastOmitted+1 < n {
			for i := lastOmitted + 1; i < n; i++ {
				if selected[i] {
					selected[i] = false
					selectedBytes -= units[i].CanonicalBytes
					dropFromGap(&gap, &units[i], &firstOmitted, &lastOmitted, i)
					markerBytes = int64(len(mustMarshalItem(GapMarker(gap))))
					dropped = true
					break
				}
			}
		}
		if !dropped {
			for k := len(cands) - 1; k >= 0; k-- {
				i := cands[k]
				if selected[i] {
					selected[i] = false
					selectedBytes -= units[i].CanonicalBytes
					dropFromGap(&gap, &units[i], &firstOmitted, &lastOmitted, i)
					markerBytes = int64(len(mustMarshalItem(GapMarker(gap))))
					dropped = true
					break
				}
			}
		}
		if !dropped {
			break
		}
	}

	// Degenerate limit: the marker alone does not fit — drop it and report no
	// gap; the budget is absolute. The integration layer's envelope
	// reservation keeps production limits out of this regime.
	if selectedBytes+markerBytes > limit {
		return SelectionResult{TotalBytes: 0}
	}

	// Assemble in protocol order: anchor part, gap marker, tail part. With an
	// empty anchor part the marker leads the stream (position head); with an
	// empty tail part it closes the stream (position tail).
	out := make([]CanonicalItem, 0, len(units)*2)
	for i := 0; i < n; i++ {
		if i == firstOmitted {
			out = append(out, GapMarker(gap))
		}
		if selected[i] {
			out = append(out, units[i].Items...)
		}
	}
	res.Items = out
	g := gap
	res.Gap = &g
	res.TotalBytes = selectedBytes + markerBytes
	return res
}

// GapMarker builds the canonical gap marker item of a gap: the marker is data,
// not silent loss — it carries the dropped logical bytes. The returned item
// has no digest (Hmac is empty): the keyed digest is the integration layer's
// concern (it owns the observer key); a caller that attaches the 64-hex
// digest must account for the delta against its envelope, exactly like the
// normalizer's MinCaptureEnvelopeBytes reservation.
func GapMarker(g GapInfo) CanonicalItem {
	return CanonicalItem{
		Kind:         CanonicalKindGap,
		LogicalBytes: g.LogicalBytes,
		Truncated:    true,
	}
}

// dropFromGap moves one retained unit into the omitted interval and keeps the
// interval bounds contiguous.
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

func mustMarshalItem(it CanonicalItem) []byte {
	payload, err := common.Marshal(it)
	if err != nil {
		return nil
	}
	return payload
}
