package relayobserver

import (
	"bytes"
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
// tool_use_id), never by adjacency, so parallel or interleaved calls still
// pair correctly.

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
// Orphan is a v1 extension of the audited interface (the brief leaves the
// orphan marking mechanism to the implementer): it is true for a tool-exchange
// unit whose result references a call ID that no unit in the stream declares —
// the result is still retained as its own unit, never dropped.
type SemanticUnit struct {
	Kind           SemanticUnitKind
	Items          []CanonicalItem
	LogicalBytes   int64
	CanonicalBytes int64
	// CallIDs is the sorted, de-duplicated set of stable call IDs of the unit:
	// call-part IDs plus result-referenced IDs. For a paired unit this is
	// exactly the pair's call IDs; for an orphan result it is the missing
	// call ID the result references.
	CallIDs []string
	// Anchor marks an anchor candidate: a system/developer directive, a
	// compact summary, or the latest user instruction.
	Anchor          bool
	SourceTruncated bool
	Orphan          bool
}

// codexTruncationMarker is the prefix Codex prepends to tool output it
// truncated at the source. The canonical item keeps output text verbatim, so
// the marker survives into the part payload; it is one of the two source-
// truncation signals the unit builder recognizes (the other is the canonical
// item's own Truncated flag).
const codexTruncationMarker = "Warning: truncated output"

// BuildSemanticUnits groups canonical items into deterministic semantic units.
//
//  1. Each item is classified by its canonical kind and parts (system,
//     message, tool call, tool result, unknown/gap).
//  2. A tool result unit whose referenced call ID is declared by another unit
//     merges into that call's unit (stable-ID pairing, adjacency-free);
//     parallel calls in one item keep all their IDs.
//  3. Unpaired results survive as their own units marked Orphan.
//  4. Anchor candidates are flagged: system/developer directives (in stream
//     order), compact summaries (in stream order), and the latest user
//     instruction.
//
// Output is deterministic: identical input yields identical units.
func BuildSemanticUnits(items []CanonicalItem) []SemanticUnit {
	n := len(items)
	if n == 0 {
		return nil
	}
	b := make([]unitBuilder, n)
	for i := range items {
		b[i] = classifyItem(items[i])
		b[i].itemIndexes = []int{i}
	}

	// Stable-ID pairing: map each declared call ID to its first declaring
	// unit; a result-only unit merges into the first unit declaring any of its
	// referenced IDs. Call units are never consumed, so no merge chains form.
	callOwner := make(map[string]int, n)
	for i := range b {
		for _, id := range b[i].callIDs {
			if _, ok := callOwner[id]; !ok {
				callOwner[id] = i
			}
		}
	}
	mergedInto := make([]int, n)
	for i := range mergedInto {
		mergedInto[i] = -1
	}
	for i := range b {
		if len(b[i].callIDs) > 0 || len(b[i].resultIDs) == 0 {
			continue
		}
		for _, rid := range b[i].resultIDs {
			if owner, ok := callOwner[rid]; ok && owner != i {
				mergedInto[i] = owner
				break
			}
		}
	}

	// Final units: surviving builders (nothing merged into them), items in
	// protocol order, aggregates derived from the items.
	survivors := make([]*unitBuilder, 0, n)
	finalIdx := make(map[int]int, n)
	for i := range b {
		if mergedInto[i] == -1 {
			finalIdx[i] = len(survivors)
			survivors = append(survivors, &b[i])
		}
	}
	for i := range b {
		if mergedInto[i] != -1 {
			owner := &b[mergedInto[i]]
			owner.itemIndexes = append(owner.itemIndexes, b[i].itemIndexes...)
		}
	}

	units := make([]SemanticUnit, 0, len(survivors))
	for _, sb := range survivors {
		sort.Ints(sb.itemIndexes)
		units = append(units, finalizeUnit(sb, items))
	}
	flagAnchorCandidates(units)
	return units
}

// unitBuilder is the per-item construction state; SemanticUnit fields are
// finalized from the item set so merges always stay consistent.
type unitBuilder struct {
	kind        SemanticUnitKind
	callIDs     []string
	resultIDs   []string
	itemIndexes []int
}

// classifyItem maps one canonical item to its unit kind and pairing IDs.
func classifyItem(it CanonicalItem) unitBuilder {
	b := unitBuilder{kind: SemanticUnitUnknown}
	switch it.Kind {
	case CanonicalKindSystem:
		b.kind = SemanticUnitAnchor
	case canonicalKindCompactSummary:
		b.kind = SemanticUnitCompactSummary
	case CanonicalKindToolCall:
		b.kind = SemanticUnitToolExchange
		b.callIDs, _ = toolIDsOfParts(it.Content)
	case CanonicalKindToolResult:
		b.kind = SemanticUnitToolExchange
		_, b.resultIDs = toolIDsOfParts(it.Content)
	default: // message, unknown, gap
		switch it.Kind {
		case CanonicalKindMessage:
			calls, results := toolIDsOfParts(it.Content)
			if it.ToolCallID != "" {
				results = append(results, it.ToolCallID)
			}
			if len(calls) > 0 || len(results) > 0 {
				// Tool-bearing message: an assistant tool_use turn, a Claude
				// tool_result turn, or a chat role=tool result message. The
				// whole item (text and tool parts) is one exchange unit.
				b.kind = SemanticUnitToolExchange
				b.callIDs = calls
				b.resultIDs = results
			} else {
				b.kind = SemanticUnitMessage
			}
		case CanonicalKindUnknown, CanonicalKindGap:
			b.kind = SemanticUnitUnknown
		}
	}
	return b
}

// toolIDsOfParts collects the call IDs and result-referenced IDs of an item's
// content parts. Empty IDs are ignored (nothing to pair against).
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

// finalizeUnit derives a unit's aggregates from its item set, in protocol
// order, so merged units never drift from their items.
func finalizeUnit(b *unitBuilder, items []CanonicalItem) SemanticUnit {
	u := SemanticUnit{Kind: b.kind, Items: make([]CanonicalItem, 0, len(b.itemIndexes))}
	var ids []string
	for _, idx := range b.itemIndexes {
		it := items[idx]
		u.Items = append(u.Items, it)
		u.LogicalBytes += it.LogicalBytes
		u.CanonicalBytes += canonicalBytesOf(it)
		if itemSourceTruncated(it) {
			u.SourceTruncated = true
		}
		calls, results := toolIDsOfParts(it.Content)
		if it.ToolCallID != "" {
			results = append(results, it.ToolCallID)
		}
		ids = append(ids, calls...)
		ids = append(ids, results...)
	}
	u.CallIDs = sortedUnique(ids)
	// A surviving unit that carries results but never declared a call is an
	// orphan result: its referenced call ID is absent from the whole stream.
	if len(b.callIDs) == 0 && len(b.resultIDs) > 0 {
		u.Orphan = true
	}
	return u
}

// flagAnchorCandidates marks the anchor candidates on the final units:
// system/developer directives (stream order), compact summaries (stream
// order), and the latest user instruction.
func flagAnchorCandidates(units []SemanticUnit) {
	for i := range units {
		u := &units[i]
		if u.Kind == SemanticUnitAnchor || u.Kind == SemanticUnitCompactSummary || isSystemDeveloperMessage(u) {
			u.Anchor = true
		}
	}
	latestUser := -1
	for i := range units {
		if units[i].Kind == SemanticUnitMessage && isUserMessage(&units[i]) {
			latestUser = i
		}
	}
	if latestUser >= 0 {
		units[latestUser].Anchor = true
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

// isUserMessage reports whether a message unit is a user instruction (role
// user; tool-result carrier messages are tool exchanges, never instructions).
func isUserMessage(u *SemanticUnit) bool {
	for _, it := range u.Items {
		if it.Role == "user" {
			return true
		}
	}
	return false
}

// itemSourceTruncated reports whether a canonical item carries a source
// truncation signal: the item's own Truncated flag, an explicit gap item, or a
// tool result output carrying the Codex truncated-output marker.
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

// canonicalBytesOf is the marshaled canonical size of an item, using the same
// serializer the pipeline uses for byte accounting.
func canonicalBytesOf(it CanonicalItem) int64 {
	payload, err := common.Marshal(it)
	if err != nil {
		return 0
	}
	return int64(len(payload))
}

// sortedUnique returns the de-duplicated, lexicographically sorted copy of
// ids; the sort keeps pairing output deterministic across input layouts.
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
