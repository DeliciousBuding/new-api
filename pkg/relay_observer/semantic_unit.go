package relayobserver

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/QuantumNous/new-api/common"
)

// This file implements the semantic unit layer (P0-C, PR #17A): the pure
// grouping of canonical items into atomic semantic units — a message, a system
// directive, or a tool call/result exchange — that the evidence selector
// (semantic_selector.go) retains or omits as wholes. The layer is deliberately
// free of storage, HTTP, and configuration concerns: it consumes canonical
// items and produces deterministic units.
//
// Atomicity contract: a canonical item is a digest-keyed storage object, so a
// unit never splits an item, and a tool call and its result(s) never enter
// different units. Pairing is by stable call ID (codex call_id / claude
// tool_use_id) using union-find connected components, so parallel calls,
// interleaved calls, and multi-result items referencing multiple call owners
// all group into the correct atomic unit.

// SemanticUnitKind is the frozen vocabulary of semantic unit kinds.
type SemanticUnitKind string

const (
	// SemanticUnitAnchor is a stable system/developer directive unit.
	SemanticUnitAnchor SemanticUnitKind = "anchor"
	// SemanticUnitMessage is a plain role-bearing message unit (no tool parts).
	SemanticUnitMessage SemanticUnitKind = "message"
	// SemanticUnitToolExchange is a tool call, a tool result, or a call with
	// its matched result(s) atomized into one unit.
	SemanticUnitToolExchange SemanticUnitKind = "tool_exchange"
	// SemanticUnitCompactSummary is a compaction summary unit (the summary
	// that follows a compact boundary).
	SemanticUnitCompactSummary SemanticUnitKind = "compact_summary"
	// SemanticUnitUnknown is a unit the builder cannot classify semantically
	// (unknown / gap canonical items).
	SemanticUnitUnknown SemanticUnitKind = "unknown"
)

// canonicalKindCompactSummary is the canonical item kind of a compaction
// summary. The current normalizer does not emit it (there is no compaction
// kind in the frozen vocabulary yet); the builder maps it so the selector is
// forward-compatible when a compacting normalizer lands.
const canonicalKindCompactSummary = "compact_summary"

// SemanticUnit is one atomic semantic unit of a canonical stream.
//
// ItemIndexes holds the original positions of the unit's items in the
// canonical stream, sorted. The selector uses ItemIndexes for global-index
// assembly (D5), so the unit's Items field is an aggregate convenience and
// the ItemIndexes field is the authoritative position record.
type SemanticUnit struct {
	Kind           SemanticUnitKind
	Items          []CanonicalItem
	ItemIndexes    []int // original positions in the input stream, sorted
	LogicalBytes   int64
	CanonicalBytes int64
	// CallIDs is the sorted, de-duplicated set of stable call IDs of the unit:
	// call-part IDs plus result-referenced IDs. For a paired unit this is
	// exactly the component's call IDs; for an orphan result it is the missing
	// call ID the result references.
	CallIDs []string
	// UnmatchedResultIDs records result references for which this component
	// contains no declaring call, including partially orphaned result items.
	UnmatchedResultIDs []string
	Anchor             bool
	SourceTruncated    bool
	Orphan             bool
}

// codexTruncationMarker is the prefix Codex prepends to tool output it
// truncated at the source. The canonical item keeps output text verbatim, so
// the marker survives into the part payload.
const codexTruncationMarker = "Warning: truncated output"

// BuildSemanticUnits groups canonical items into deterministic semantic units.
// Errors are marshal failures: every item must be serializable.
//
// Pairing uses union-find connected components (D6): each item is a node; two
// items sharing a call ID (one declaring it, another referencing it) are
// edges. Every connected component becomes one atomic unit. Unpaired results
// survive as their own one-item units marked Orphan.
func BuildSemanticUnits(items []CanonicalItem) ([]SemanticUnit, error) {
	n := len(items)
	if n == 0 {
		return nil, nil
	}

	// Classify each item.
	cls := make([]itemClass, n)
	for i := range items {
		cls[i] = classifyItem(items[i])
	}

	// Union-find: items sharing a call ID are connected.
	uf := newItemUF(n)
	callIDOwners := make(map[string]int)
	for i := range items {
		calls, results := toolIDsOfParts(items[i].Content)
		if items[i].ToolCallID != "" {
			results = append(results, items[i].ToolCallID)
		}
		allIDs := append(calls, results...)
		for _, id := range allIDs {
			if owner, ok := callIDOwners[id]; ok {
				uf.union(owner, i)
			} else {
				callIDOwners[id] = i
			}
		}
	}

	// Group by root, then order components by their earliest original item.
	// A union-find root is an implementation detail and is not guaranteed to be
	// the component's minimum index when rank-based union is used.
	rootToComp := make(map[int]*component, n)
	for i := range items {
		r := uf.find(i)
		comp, ok := rootToComp[r]
		if !ok {
			comp = &component{}
			rootToComp[r] = comp
		}
		comp.itemIndexes = append(comp.itemIndexes, i)
	}
	components := make([]*component, 0, len(rootToComp))
	for _, comp := range rootToComp {
		sort.Ints(comp.itemIndexes)
		components = append(components, comp)
	}
	sort.Slice(components, func(i, j int) bool {
		return components[i].itemIndexes[0] < components[j].itemIndexes[0]
	})

	units := make([]SemanticUnit, 0, len(components))
	for _, comp := range components {
		u, err := finalizeUnit(comp, items, cls)
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}
	flagAnchorCandidates(units)
	return units, nil
}

// itemClass holds the parsed classification of one canonical item.
type itemClass struct {
	kind      SemanticUnitKind
	callIDs   []string
	resultIDs []string
}

// classifyItem maps one canonical item to its unit kind and pairing IDs.
func classifyItem(it CanonicalItem) itemClass {
	c := itemClass{kind: SemanticUnitUnknown}
	switch it.Kind {
	case CanonicalKindSystem:
		c.kind = SemanticUnitAnchor
	case canonicalKindCompactSummary:
		c.kind = SemanticUnitCompactSummary
	case CanonicalKindToolCall:
		c.kind = SemanticUnitToolExchange
		c.callIDs, _ = toolIDsOfParts(it.Content)
	case CanonicalKindToolResult:
		c.kind = SemanticUnitToolExchange
		_, c.resultIDs = toolIDsOfParts(it.Content)
	default: // message, unknown, gap
		switch it.Kind {
		case CanonicalKindMessage:
			calls, results := toolIDsOfParts(it.Content)
			if it.ToolCallID != "" {
				results = append(results, it.ToolCallID)
			}
			if len(calls) > 0 || len(results) > 0 {
				c.kind = SemanticUnitToolExchange
				c.callIDs = calls
				c.resultIDs = results
			} else {
				c.kind = SemanticUnitMessage
			}
		case CanonicalKindUnknown, CanonicalKindGap:
			c.kind = SemanticUnitUnknown
		}
	}
	return c
}

// component is the union-find connected component of a unit.
type component struct {
	itemIndexes []int
}

// itemUF is a union-find over canonical item indexes.
type itemUF struct {
	parent []int
	rank   []int
}

func newItemUF(n int) *itemUF {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &itemUF{parent: p, rank: make([]int, n)}
}

func (uf *itemUF) find(x int) int {
	for uf.parent[x] != x {
		uf.parent[x] = uf.parent[uf.parent[x]]
		x = uf.parent[x]
	}
	return x
}

func (uf *itemUF) union(x, y int) {
	xr, yr := uf.find(x), uf.find(y)
	if xr == yr {
		return
	}
	if uf.rank[xr] < uf.rank[yr] {
		uf.parent[xr] = yr
	} else if uf.rank[xr] > uf.rank[yr] {
		uf.parent[yr] = xr
	} else {
		uf.parent[yr] = xr
		uf.rank[xr]++
	}
}

// toolIDsOfParts collects the call IDs and result-referenced IDs of an item's
// content parts. Empty IDs are ignored.
func toolIDsOfParts(parts []CanonicalPart) (calls, results []string) {
	for _, p := range parts {
		switch p.Type {
		case partTypeToolCall:
			if p.Call != nil && p.Call.ID != "" {
				calls = append(calls, p.Call.ID)
			}
		case partTypeToolResult:
			if p.Result != nil && p.Result.ToolCallID != "" {
				results = append(results, p.Result.ToolCallID)
			}
		}
	}
	return calls, results
}

// finalizeUnit derives a unit's aggregates from its component.
func finalizeUnit(comp *component, items []CanonicalItem, cls []itemClass) (SemanticUnit, error) {
	kind := cls[comp.itemIndexes[0]].kind
	u := SemanticUnit{
		Kind:        kind,
		Items:       make([]CanonicalItem, 0, len(comp.itemIndexes)),
		ItemIndexes: append([]int(nil), comp.itemIndexes...),
	}
	var declaredIDs, resultIDs []string
	for _, idx := range comp.itemIndexes {
		it := items[idx]
		u.Items = append(u.Items, it)
		u.LogicalBytes += it.LogicalBytes
		cb, err := canonicalBytesOf(it)
		if err != nil {
			return SemanticUnit{}, fmt.Errorf("item %d: %w", idx, err)
		}
		u.CanonicalBytes += cb
		if itemSourceTruncated(it) {
			u.SourceTruncated = true
		}
		if cls[idx].kind == SemanticUnitToolExchange {
			u.Kind = SemanticUnitToolExchange
		}
		declaredIDs = append(declaredIDs, cls[idx].callIDs...)
		resultIDs = append(resultIDs, cls[idx].resultIDs...)
	}
	declaredIDs = sortedUnique(declaredIDs)
	resultIDs = sortedUnique(resultIDs)
	u.CallIDs = sortedUnique(append(append([]string(nil), declaredIDs...), resultIDs...))
	for _, id := range resultIDs {
		if !containsSortedID(declaredIDs, id) {
			u.UnmatchedResultIDs = append(u.UnmatchedResultIDs, id)
		}
	}
	u.Orphan = len(u.UnmatchedResultIDs) > 0
	return u, nil
}

// flagAnchorCandidates marks the anchor candidates on the final units.
// D7: only system/developer directives and compact summaries (at the head of
// the stream) are anchor candidates. The latest user instruction is NOT one —
// it is naturally retained by the tail selector. The unit-level Anchor flag
// is a hint for the selector, which applies its own continuous-prefix +
// compact-summary-at-0 filter.
func flagAnchorCandidates(units []SemanticUnit) {
	for i := range units {
		u := &units[i]
		if u.Kind == SemanticUnitAnchor || u.Kind == SemanticUnitCompactSummary || isSystemDeveloperMessage(u) {
			u.Anchor = true
		}
	}
}

// isSystemDeveloperMessage reports whether a message unit is a system or
// developer directive (chat format keeps these as role-bearing messages).
func isSystemDeveloperMessage(u *SemanticUnit) bool {
	if u.Kind != SemanticUnitMessage {
		return false
	}
	for _, it := range u.Items {
		if it.Role == "system" || it.Role == "developer" {
			return true
		}
	}
	return false
}

// itemSourceTruncated reports whether a canonical item carries a source
// truncation signal.
func itemSourceTruncated(it CanonicalItem) bool {
	if it.Truncated || it.Kind == CanonicalKindGap {
		return true
	}
	for _, p := range it.Content {
		if p.Type == partTypeToolResult && p.Result != nil && bytes.Contains(p.Result.Output, []byte(codexTruncationMarker)) {
			return true
		}
	}
	return false
}

// canonicalBytesOf is the marshaled canonical size of an item.
func canonicalBytesOf(it CanonicalItem) (int64, error) {
	payload, err := common.Marshal(it)
	if err != nil {
		return 0, fmt.Errorf("marshal canonical item: %w", err)
	}
	return int64(len(payload)), nil
}

// sortedUnique returns the de-duplicated, lexicographically sorted copy of ids.
func sortedUnique(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	copy(out, ids)
	sort.Strings(out)
	uniq := out[:0]
	var prev string
	for i, id := range out {
		if i == 0 || id != prev {
			uniq = append(uniq, id)
		}
		prev = id
	}
	return uniq
}

func containsSortedID(ids []string, want string) bool {
	i := sort.SearchStrings(ids, want)
	return i < len(ids) && ids[i] == want
}
