package relayobserver

import (
	"github.com/google/uuid"
)

// This file implements the incremental group logic (T2.3): the deterministic
// group planner behind the fixed one-full/eight-delta context scheme, the
// common-prefix delta computation, and the one-hop reconstruction assembly.
// These are pure functions — the PostgreSQL adapter feeds them row data and
// persists their output, so the group rules are testable without a database.
//
// Group rules per the architecture SSOT (Incremental Conversation Model):
// the first context of a group is a full ordered digest list (ordinal 0);
// the next eight contexts reference that full checkpoint and store
// common_prefix_count plus the current suffix digests; the tenth context
// starts a new full checkpoint. Every delta reconstructs from one full
// checkpoint plus one suffix row — there are no delta chains.

// Group bounds. A storage group is exactly one full checkpoint (ordinal 0)
// plus at most maxDeltaPerGroup deltas (ordinals 1..8); the ninth delta
// rotates to a new full checkpoint.
const (
	// groupFullOrdinal is the ordinal of a full checkpoint row.
	groupFullOrdinal = 0
	// maxDeltaPerGroup is the per-group delta cap: 1 full + 8 deltas.
	maxDeltaPerGroup = 8
)

// sessionHead is the in-memory view of one observer_session_heads row.
type sessionHead struct {
	checkpointID int64
	ordinal      int
}

// groupPlan is the shape of the next context write: either a new full
// checkpoint (rotate, ordinal 0) or a delta referencing the current group's
// full checkpoint with a common prefix and a suffix digest list.
type groupPlan struct {
	// ordinal is the planned group_ordinal (0 for a full, 1..8 for a delta).
	ordinal int
	// checkpointID is the full row the delta references; 0 for a full.
	checkpointID int64
	// prefixCount is the delta's common_prefix_count against the full list.
	prefixCount int
	// suffix is the delta's suffix digest list; nil for a full.
	suffix []string
	// fullDigests is the turn's complete digest list (the full list, or the
	// list the delta extends).
	fullDigests []string
	// rotate marks a new full checkpoint.
	rotate bool
}

// planGroup decides the next context shape. A session without a head starts
// a full; a head on the ninth delta (ordinal 8) rotates to a new full; any
// other head yields a delta with the common prefix against the current
// group's full digest list and the new suffix. A divergence compacts the
// prefix to zero. The planned ordinal always stays inside 0..8.
func planGroup(head *sessionHead, headFullDigests, digests []string) groupPlan {
	if head == nil || head.ordinal >= maxDeltaPerGroup {
		return groupPlan{ordinal: groupFullOrdinal, fullDigests: digests, rotate: true}
	}
	prefix := commonPrefix(headFullDigests, digests)
	return groupPlan{
		ordinal:      head.ordinal + 1,
		checkpointID: head.checkpointID,
		prefixCount:  prefix,
		suffix:       digests[prefix:],
		fullDigests:  digests,
	}
}

// commonPrefix returns the length of the longest shared prefix of a and b.
func commonPrefix(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// assembleDigests reconstructs one turn's complete digest list from a full
// checkpoint list and a delta's prefix count plus suffix. The prefix must fit
// inside the base and the counts must line up with the delta's declared item
// count; anything else is a classified corrupt delta — a delta is never
// assembled silently into wrong content.
func assembleDigests(full []string, prefixCount int, suffix []string, itemCount int) ([]string, error) {
	if prefixCount < 0 || prefixCount > len(full) {
		return nil, classifiedError(ContentErrCorruptDelta, "prefix count %d outside base list of %d", prefixCount, len(full))
	}
	if prefixCount+len(suffix) != itemCount {
		return nil, classifiedError(ContentErrCorruptDelta, "prefix %d + suffix %d != declared item count %d", prefixCount, len(suffix), itemCount)
	}
	out := make([]string, 0, itemCount)
	out = append(out, full[:prefixCount]...)
	out = append(out, suffix...)
	return out, nil
}

// ContentInput is the T2.3 write input of one turn's content: the session
// aliases resolved by T2.1 (primary first) and the canonical items produced
// by T2.2. It is the consumer seam the persistence adapter persists.
type ContentInput struct {
	// NodeScope is the event's node scope; UserID the event's user id. They
	// scope the alias lookup exactly like the identity contract, so equal
	// alias values across profiles never collide.
	NodeScope string
	UserID    int64
	// ClientProfile is the fine-grained client profile for display only (the
	// service-layer DetectClientProfile result). It never participates in
	// alias resolution: session grouping stays on SessionScope, so the same
	// Claude Code session opened from VS Code and CLI still resolves to one
	// observer session even though their display profile differs.
	ClientProfile string
	// Aliases are the resolved session aliases, primary first. A turn with no
	// aliases has no session and produces no content or context rows.
	Aliases []Alias
	// PreviousAliases are the same raw values re-keyed under the previous
	// generation (parallel to Aliases, primary first). A rotation window uses
	// them to adopt a session bound under the old key instead of orphaning it
	//. Empty when no previous key is configured.
	PreviousAliases []Alias
	// TurnID is the owning turn row; the idempotency key of a context.
	TurnID uuid.UUID
	// ContentState is the turn's normalized content state (full / gap /
	// metadata-only). It drives the session counters of the decoupled
	// session-only path: a turn with aliases but no items still binds the
	// session and advances last_seen / turn_count, and a gap state advances
	// gap_count even when no content was persisted.
	ContentState string
	// Transient marks a per-turn synthetic session created for a request
	// without a resolvable session identity. The observer_session row is
	// flagged is_transient so the session list can exclude it by default;
	// retention cleans it up like any other session.
	Transient bool
	// Items are the ordered canonical items of the turn (T2.2 output). Empty
	// with a non-empty Aliases is the session-only append: identity and
	// counters are persisted, content is not.
	Items []CanonicalItem
}

// ReconstructedTurn is the deterministic reconstruction output of one turn:
// its group ordinal and the full ordered canonical items.
type ReconstructedTurn struct {
	TurnID  uuid.UUID
	Ordinal int
	Items   []CanonicalItem
}

// contentTurnMeta is the per-turn accounting derived from the canonical
// items: the ordered digest list (the reconstruction key — every digest
// resolves to one stored content object), the canonical JSON byte total, and
// whether a gap marker is present.
type contentTurnMeta struct {
	digests      []string
	logicalBytes int64
	hasGap       bool
}

// metaOfItems derives the digest list and gap presence of a turn's items.
func metaOfItems(items []CanonicalItem) contentTurnMeta {
	meta := contentTurnMeta{digests: make([]string, 0, len(items))}
	for _, it := range items {
		meta.digests = append(meta.digests, it.Hmac)
		if it.Kind == CanonicalKindGap {
			meta.hasGap = true
		}
	}
	return meta
}
