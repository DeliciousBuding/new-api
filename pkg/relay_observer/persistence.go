package relayobserver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
)

// This file is the T2.3 content persistence port and its PostgreSQL
// implementation. The port is an additive extension of the Store surface
// (types.go: "Query and retention surfaces are additive extensions of this
// port added by their owning phases"); the concrete adapter extends the
// existing *pgStore so content persistence shares the single dedicated
// observer pool, its 2/1/60s tuning, and the v1 schema with the turn
// writer. Core logic (group planning, reconstruction, classification) lives
// in content.go and codec.go as pure functions; the append orchestration in
// this file is tested against a row-access seam (contentTx) with a fake,
// exactly like the dispatcher is tested against the Store port — the SQL
// itself is exercised by the relay_observer_pg_integration suite.
//
// All writes for one append run inside one transaction. The only row locked
// is the session head row (FOR UPDATE): concurrent appends to the same
// session serialize on it, so group ordinals stay unique without a
// table-wide lock (SSOT: "All writes for a batch use one transaction and
// acquire no table-wide lock").

// contentCodecZstd is the codec name stored on every content object row.
const contentCodecZstd = "zstd"

// ContentPersistence is the T2.3 content port: incremental content writes,
// deterministic one-hop reconstruction, and group-level atomic deletion.
// It is the consumer seam of the T2.1 aliases and T2.2 canonical items; the
// Phase 3 query API and the T5.1 retention pass consume it.
type ContentPersistence interface {
	// AppendTurns persists the content of one or more turns in one idempotent
	// transaction: content objects are deduplicated per session digest,
	// contexts follow the one-full/eight-delta group scheme, and a turn whose
	// context already exists is a no-op. Duplicate appends never create
	// duplicate objects or groups. A turn without aliases or without items is
	// a no-op (no session, or metadata-only content). The context carries the
	// write timeout and must be respected.
	AppendTurns(ctx context.Context, turns []ContentInput) error
	// ReconstructTurn deterministically rebuilds one turn's full ordered
	// canonical items from its context row: a full reads one row and its
	// objects; a delta reads exactly one full checkpoint plus one suffix row
	// (decode depth 1). Missing, chained, corrupt, truncated, or
	// digest-mismatched bases, deltas, and objects are rejected with a
	// classified ContentError. hmacKey re-verifies every item's content-layer
	// digest; an empty key skips the digest step but keeps the structural
	// checks.
	ReconstructTurn(ctx context.Context, sessionID, turnID uuid.UUID, hmacKey string) (ReconstructedTurn, error)
	// ReconstructGroup deterministically rebuilds every context of one group
	// in ordinal order (full first, then deltas 1..8). Each delta still reads
	// at most one full checkpoint plus one suffix row.
	ReconstructGroup(ctx context.Context, sessionID uuid.UUID, checkpointID int64, hmacKey string) ([]ReconstructedTurn, error)
	// DeleteGroup atomically deletes one group's context rows and clears the
	// session head when it points into that group, all in one transaction.
	// After commit no row references the deleted group. Content objects are
	// not deleted here: the design has no refcounts (SSOT) and orphan-safe
	// content cleanup is the retention pass's (T5.1) responsibility, so
	// deletion never breaks a reference to still-retained content.
	DeleteGroup(ctx context.Context, sessionID uuid.UUID, checkpointID int64) error
}

var _ ContentPersistence = (*pgStore)(nil)

// rowScanner is the minimal scan surface the append orchestration needs;
// *sql.Row satisfies it.
type rowScanner interface {
	Scan(dest ...any) error
}

// rowIter is the minimal multi-row surface reconstruction needs; *sql.Rows
// satisfies it.
type rowIter interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// contentTx is the transaction surface the append orchestration is written
// against. *sql.Tx satisfies it through sqlTxAdapter, so the orchestration
// is testable with a fake data layer exactly like the dispatcher is tested
// against the Store port.
type contentTx interface {
	QueryRow(ctx context.Context, query string, args ...any) rowScanner
	Exec(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// contentQuerier is the full read-write surface reconstruction and the
// append orchestration need. *sql.DB satisfies it through sqlDBAdapter, so
// the reconstruction decisions are testable with the same fake data layer.
type contentQuerier interface {
	contentTx
	Query(ctx context.Context, query string, args ...any) (rowIter, error)
}

// sqlTxAdapter adapts *sql.Tx to the contentTx seam.
type sqlTxAdapter struct{ tx *sql.Tx }

func (a sqlTxAdapter) QueryRow(ctx context.Context, query string, args ...any) rowScanner {
	return a.tx.QueryRowContext(ctx, query, args...)
}

func (a sqlTxAdapter) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return a.tx.ExecContext(ctx, query, args...)
}

// sqlDBAdapter adapts *sql.DB to the contentQuerier seam.
type sqlDBAdapter struct{ db *sql.DB }

func (a sqlDBAdapter) QueryRow(ctx context.Context, query string, args ...any) rowScanner {
	return a.db.QueryRowContext(ctx, query, args...)
}

func (a sqlDBAdapter) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return a.db.ExecContext(ctx, query, args...)
}

func (a sqlDBAdapter) Query(ctx context.Context, query string, args ...any) (rowIter, error) {
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// contextRow is the in-memory view of one observer_contexts row.
type contextRow struct {
	id           int64
	checkpointID int64
	groupOrdinal int
	commonPrefix int
	itemCount    int
	itemDigests  []byte
	logicalBytes int64
}

// contentObjectRow is the in-memory view of one observer_content_objects row
// during reconstruction.
type contentObjectRow struct {
	payload      []byte
	logicalBytes int64
}

// AppendTurns implements ContentPersistence. See the interface comment for
// the full contract.
func (s *pgStore) AppendTurns(ctx context.Context, turns []ContentInput) error {
	if len(turns) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("relayobserver: append content: begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit
	if err := appendTurnsTx(ctx, sqlTxAdapter{tx: tx}, turns); err != nil {
		return err
	}
	return tx.Commit()
}

// appendTurnsTx persists every turn inside one transaction.
func appendTurnsTx(ctx context.Context, tx contentTx, turns []ContentInput) error {
	for i := range turns {
		if err := appendTurnTx(ctx, tx, &turns[i]); err != nil {
			return err
		}
	}
	return nil
}

// appendTurnTx persists one turn inside the caller's transaction: session
// resolution, idempotency check, content dedup, head serialization, context
// insert, head advance, and session counters.
func appendTurnTx(ctx context.Context, tx contentTx, in *ContentInput) error {
	if len(in.Aliases) == 0 || len(in.Items) == 0 {
		// No session alias, or metadata-only content: nothing to persist.
		// The turn row itself is written by the turn writer (WriteBatch).
		return nil
	}
	sessionID, err := resolveSessionTx(ctx, tx, in)
	if err != nil {
		return err
	}
	// Idempotency: a turn whose context row already exists is a no-op. The
	// content objects and the head were already written by the first append.
	var existingID int64
	err = tx.QueryRow(ctx, `SELECT id FROM observer_contexts WHERE turn_id = $1`, in.TurnID.String()).Scan(&existingID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("relayobserver: append content: check turn context: %w", err)
	}

	// Content object dedup: one row per (session, digest); repeated events
	// reference the same object, never a copy.
	meta, err := insertContentObjectsTx(ctx, tx, sessionID, in.Items)
	if err != nil {
		return err
	}

	// Serialize on the session head row, then plan the next context.
	head, headFull, err := lockHeadTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	plan := planGroup(head, headFull, meta.digests)

	contextID, err := insertContextTx(ctx, tx, sessionID, in.TurnID, plan, meta)
	if err != nil {
		return err
	}
	if err := updateHeadTx(ctx, tx, sessionID, contextID, plan); err != nil {
		return err
	}
	return bumpSessionTx(ctx, tx, in, sessionID, meta.hasGap)
}

// resolveSessionTx resolves the primary alias to a session id, creating the
// session and its first alias binding on first sight, then binds the
// auxiliary aliases when they are free.
//
// Lookup is scoped by the alias's profile (the provider column): an alias is
// an identity of all four fields, so equal raw values across profiles stay
// separate sessions even though the v1 UNIQUE key
// (node_scope, user_id, key_version, alias_digest) cannot hold both
// bindings. When a primary binding collides with a binding of another
// profile, the new session stays unbound and the conflicting alias keeps its
// existing binding (SSOT: conflicting aliases remain separate in v1; the
// worker never merges). An auxiliary alias already bound to a different
// session is likewise left untouched.
func resolveSessionTx(ctx context.Context, tx contentTx, in *ContentInput) (uuid.UUID, error) {
	primary := in.Aliases[0]
	sid, err := lookupAliasSessionTx(ctx, tx, in.NodeScope, in.UserID, primary)
	if err == nil {
		return sid, bindAuxiliaryTx(ctx, tx, in, sid)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, err
	}

	// First sight of this primary alias: create the session and bind it.
	sid = uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO observer_sessions (id, node_scope, user_id, client_family, first_seen, last_seen, turn_count, gap_count) VALUES ($1, $2, $3, $4, now(), now(), 0, 0) ON CONFLICT (id) DO NOTHING`,
		sid.String(), in.NodeScope, in.UserID, string(primary.Scope)); err != nil {
		return uuid.Nil, fmt.Errorf("relayobserver: append content: insert session: %w", err)
	}
	bound, err := bindAliasRowTx(ctx, tx, in.NodeScope, in.UserID, primary, sid)
	if err != nil {
		return uuid.Nil, err
	}
	if !bound {
		// The binding collided: either a concurrent append bound the same
		// (scope, digest) first, or another profile bound the same digest
		// (cross-profile equal value). Re-look-up by scope: a same-scope
		// race adopts the winning session; a cross-profile conflict leaves
		// the new session unbound (remain separate in v1).
		existing, err := lookupAliasSessionTx(ctx, tx, in.NodeScope, in.UserID, primary)
		if err == nil {
			sid = existing
		} else if !errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, err
		}
	}
	return sid, bindAuxiliaryTx(ctx, tx, in, sid)
}

// bindAuxiliaryTx binds every auxiliary alias to sid when it is free. An
// alias already bound to the same session is idempotent; an alias bound to a
// different session is a v1 conflict and is left separate.
func bindAuxiliaryTx(ctx context.Context, tx contentTx, in *ContentInput, sid uuid.UUID) error {
	for _, a := range in.Aliases[1:] {
		bound, err := lookupAliasSessionTx(ctx, tx, in.NodeScope, in.UserID, a)
		if err == nil {
			if bound == sid {
				continue // already bound to this session: idempotent
			}
			continue // bound to another session: v1 conflict, remain separate
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ok, err := bindAliasRowTx(ctx, tx, in.NodeScope, in.UserID, a, sid)
		if err != nil {
			return err
		}
		if !ok {
			// Lost a race with another session's binding: re-check, then
			// keep the existing binding.
			bound, err := lookupAliasSessionTx(ctx, tx, in.NodeScope, in.UserID, a)
			if err != nil {
				return fmt.Errorf("relayobserver: append content: auxiliary alias raced without a binding: %w", err)
			}
			if bound != sid {
				continue // v1 conflict: remain separate
			}
		}
	}
	return nil
}

// lookupAliasSessionTx returns the session bound to an alias, scoped by the
// alias's profile (provider column): an alias identity is
// (node_scope, user_id, key_version, digest, scope), so a value colliding
// across profiles never resolves to the other profile's session. It returns
// sql.ErrNoRows when none is bound.
func lookupAliasSessionTx(ctx context.Context, tx contentTx, nodeScope string, userID int64, a Alias) (uuid.UUID, error) {
	raw, err := itemDigestBytes(a.Digest)
	if err != nil {
		return uuid.Nil, fmt.Errorf("relayobserver: append content: invalid alias digest: %w", err)
	}
	var sid string
	err = tx.QueryRow(ctx, `SELECT session_id::text FROM observer_session_aliases WHERE node_scope = $1 AND user_id = $2 AND key_version = $3 AND alias_digest = $4 AND provider = $5`,
		nodeScope, userID, a.Version, raw, string(a.Scope)).Scan(&sid)
	if err != nil {
		return uuid.Nil, err
	}
	uid, err := uuid.Parse(sid)
	if err != nil {
		return uuid.Nil, fmt.Errorf("relayobserver: append content: corrupt alias binding session %q: %w", sid, err)
	}
	return uid, nil
}

// bindAliasRowTx inserts one alias binding with ON CONFLICT DO NOTHING. It
// reports whether this call created the binding.
func bindAliasRowTx(ctx context.Context, tx contentTx, nodeScope string, userID int64, a Alias, sid uuid.UUID) (bool, error) {
	raw, err := itemDigestBytes(a.Digest)
	if err != nil {
		return false, fmt.Errorf("relayobserver: append content: invalid alias digest: %w", err)
	}
	res, err := tx.Exec(ctx, `INSERT INTO observer_session_aliases (node_scope, user_id, key_version, provider, source, alias_digest, session_id, first_seen, last_seen) VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now()) ON CONFLICT (node_scope, user_id, key_version, alias_digest) DO NOTHING`,
		nodeScope, userID, a.Version, string(a.Scope), string(a.Source), raw, sid.String())
	if err != nil {
		return false, fmt.Errorf("relayobserver: append content: insert alias binding: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("relayobserver: append content: alias rows affected: %w", err)
	}
	return n > 0, nil
}

// insertContentObjectsTx inserts one content object per item with
// ON CONFLICT (session_id, item_digest) DO NOTHING: the same canonical
// content is stored once per session and later events reference it. It
// returns the turn's digest list, gap presence, and canonical byte total.
func insertContentObjectsTx(ctx context.Context, tx contentTx, sessionID uuid.UUID, items []CanonicalItem) (contentTurnMeta, error) {
	meta := contentTurnMeta{digests: make([]string, 0, len(items))}
	for _, it := range items {
		meta.digests = append(meta.digests, it.Hmac)
		if it.Kind == CanonicalKindGap {
			meta.hasGap = true
		}
		raw, err := itemDigestBytes(it.Hmac)
		if err != nil {
			return meta, fmt.Errorf("relayobserver: append content: invalid item digest: %w", err)
		}
		payload, logical, err := encodeItem(it)
		if err != nil {
			return meta, fmt.Errorf("relayobserver: append content: encode item: %w", err)
		}
		meta.logicalBytes += logical
		if _, err := tx.Exec(ctx, `INSERT INTO observer_content_objects (session_id, item_digest, kind, role, codec, payload, logical_bytes, stored_bytes, truncated) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (session_id, item_digest) DO NOTHING`,
			sessionID.String(), raw, it.Kind, it.Role, contentCodecZstd, payload, logical, len(payload), it.Truncated); err != nil {
			return meta, fmt.Errorf("relayobserver: append content: insert content object: %w", err)
		}
	}
	return meta, nil
}

// lockHeadTx ensures the session head row exists and locks it, returning the
// head (nil when empty) and the current group's full digest list when the
// head has room for a delta. Two concurrent appends to the same session
// serialize on this row, so ordinals stay unique.
func lockHeadTx(ctx context.Context, tx contentTx, sessionID uuid.UUID) (*sessionHead, []string, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO observer_session_heads (session_id) VALUES ($1) ON CONFLICT (session_id) DO NOTHING`, sessionID.String()); err != nil {
		return nil, nil, fmt.Errorf("relayobserver: append content: ensure head row: %w", err)
	}
	var contextID, checkpointID sql.NullInt64
	var ordinal sql.NullInt64
	if err := tx.QueryRow(ctx, `SELECT context_id, checkpoint_id, group_ordinal FROM observer_session_heads WHERE session_id = $1 FOR UPDATE`, sessionID.String()).Scan(&contextID, &checkpointID, &ordinal); err != nil {
		return nil, nil, fmt.Errorf("relayobserver: append content: lock head row: %w", err)
	}
	if !contextID.Valid || !ordinal.Valid {
		return nil, nil, nil // no head yet: start a full
	}
	head := &sessionHead{checkpointID: checkpointID.Int64, ordinal: int(ordinal.Int64)}
	if head.ordinal >= maxDeltaPerGroup {
		return head, nil, nil // rotate: no base needed
	}
	full, err := readContextDigestsTx(ctx, tx, sessionID, head.checkpointID)
	if err != nil {
		return nil, nil, err
	}
	return head, full, nil
}

// readContextDigestsTx reads one context row's digest list, classified as a
// missing base when the row is gone.
func readContextDigestsTx(ctx context.Context, tx contentTx, sessionID uuid.UUID, contextID int64) ([]string, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT item_digests FROM observer_contexts WHERE id = $1 AND session_id = $2`, contextID, sessionID.String()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, classifiedError(ContentErrMissingBase, "full checkpoint %d missing for session %s", contextID, sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("relayobserver: append content: read base digests: %w", err)
	}
	var digests []string
	if err := common.Unmarshal(raw, &digests); err != nil {
		return nil, classifiedErrorWrap(ContentErrCorrupt, "decode full checkpoint digests", err)
	}
	return digests, nil
}

// insertContextTx writes the planned context row and returns its id. A full
// row is written with checkpoint_id 0 and then self-referenced (SSOT:
// group_ordinal=0 is full and checkpoint_id=id); a delta references the
// group's full row and stores only the suffix digests.
func insertContextTx(ctx context.Context, tx contentTx, sessionID uuid.UUID, turnID uuid.UUID, plan groupPlan, meta contentTurnMeta) (int64, error) {
	var digestsJSON []byte
	var checkpointID any
	var prefixCount int
	var err error
	if plan.rotate {
		checkpointID = 0 // placeholder, self-referenced below
		digestsJSON, err = common.Marshal(meta.digests)
	} else {
		checkpointID = plan.checkpointID
		prefixCount = plan.prefixCount
		digestsJSON, err = common.Marshal(plan.suffix)
	}
	if err != nil {
		return 0, fmt.Errorf("relayobserver: append content: marshal digests: %w", err)
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO observer_contexts (session_id, turn_id, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		sessionID.String(), turnID.String(), checkpointID, plan.ordinal, prefixCount, len(meta.digests), string(digestsJSON), meta.logicalBytes).Scan(&id); err != nil {
		return 0, fmt.Errorf("relayobserver: append content: insert context: %w", err)
	}
	if plan.rotate {
		if _, err := tx.Exec(ctx, `UPDATE observer_contexts SET checkpoint_id = $1 WHERE id = $1`, id); err != nil {
			return 0, fmt.Errorf("relayobserver: append content: self-reference full context: %w", err)
		}
	}
	return id, nil
}

// updateHeadTx advances the session head to the just-written context.
func updateHeadTx(ctx context.Context, tx contentTx, sessionID uuid.UUID, contextID int64, plan groupPlan) error {
	checkpoint := plan.checkpointID
	if plan.rotate {
		checkpoint = contextID
	}
	if _, err := tx.Exec(ctx, `UPDATE observer_session_heads SET context_id = $1, checkpoint_id = $2, group_ordinal = $3 WHERE session_id = $4`,
		contextID, checkpoint, plan.ordinal, sessionID.String()); err != nil {
		return fmt.Errorf("relayobserver: append content: update head: %w", err)
	}
	return nil
}

// bumpSessionTx maintains the session recency counters: last_seen advances
// on every content write, turn_count on every context, gap_count on every
// gap-marked turn.
func bumpSessionTx(ctx context.Context, tx contentTx, in *ContentInput, sessionID uuid.UUID, hasGap bool) error {
	gapInc := 0
	if hasGap {
		gapInc = 1
	}
	if _, err := tx.Exec(ctx, `INSERT INTO observer_sessions (id, node_scope, user_id, client_family, first_seen, last_seen, turn_count, gap_count) VALUES ($1, $2, $3, $4, now(), now(), 1, $5) ON CONFLICT (id) DO UPDATE SET last_seen = now(), turn_count = observer_sessions.turn_count + 1, gap_count = observer_sessions.gap_count + excluded.gap_count`,
		sessionID.String(), in.NodeScope, in.UserID, string(in.Aliases[0].Scope), gapInc); err != nil {
		return fmt.Errorf("relayobserver: append content: update session counters: %w", err)
	}
	return nil
}

// ReconstructTurn implements ContentPersistence. See the interface comment
// for the full contract.
func (s *pgStore) ReconstructTurn(ctx context.Context, sessionID, turnID uuid.UUID, hmacKey string) (ReconstructedTurn, error) {
	return reconstructTurnQ(ctx, sqlDBAdapter{db: s.db}, sessionID, turnID, hmacKey)
}

// reconstructTurnQ rebuilds one turn through the query seam.
func reconstructTurnQ(ctx context.Context, q contentQuerier, sessionID, turnID uuid.UUID, hmacKey string) (ReconstructedTurn, error) {
	row, err := loadContextRowQ(ctx, q, sessionID, turnID)
	if err != nil {
		return ReconstructedTurn{}, err
	}
	items, err := reconstructItemsQ(ctx, q, sessionID, row, hmacKey)
	if err != nil {
		return ReconstructedTurn{}, err
	}
	return ReconstructedTurn{TurnID: turnID, Ordinal: row.groupOrdinal, Items: items}, nil
}

// loadContextRowQ loads one turn's context row, classified as missing when
// the turn has no context.
func loadContextRowQ(ctx context.Context, q contentQuerier, sessionID uuid.UUID, turnID uuid.UUID) (contextRow, error) {
	var row contextRow
	err := q.QueryRow(ctx, `SELECT id, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes FROM observer_contexts WHERE session_id = $1 AND turn_id = $2`,
		sessionID.String(), turnID.String()).Scan(&row.id, &row.checkpointID, &row.groupOrdinal, &row.commonPrefix, &row.itemCount, &row.itemDigests, &row.logicalBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return contextRow{}, classifiedError(ContentErrMissingContext, "turn %s has no context row in session %s", turnID, sessionID)
	}
	if err != nil {
		return contextRow{}, fmt.Errorf("relayobserver: reconstruct: read context row: %w", err)
	}
	return row, nil
}

// reconstructItemsQ rebuilds one turn's full canonical items with decode
// depth at most one: a full reads its own digest list; a delta reads exactly
// one full checkpoint (checked to be a full, never a chained delta) plus its
// suffix row.
func reconstructItemsQ(ctx context.Context, q contentQuerier, sessionID uuid.UUID, row contextRow, hmacKey string) ([]CanonicalItem, error) {
	digests, err := reconstructDigests(row, func(id int64) (contextRow, error) {
		fullRow, err := loadContextRowByIDQ(ctx, q, sessionID, id)
		if errors.Is(err, sql.ErrNoRows) {
			return contextRow{}, classifiedError(ContentErrMissingBase, "delta %d references missing full checkpoint %d", row.id, id)
		}
		return fullRow, err
	})
	if err != nil {
		return nil, err
	}
	return loadContentItemsQ(ctx, q, sessionID, digests, hmacKey)
}

// reconstructDigests decides the digest list of one context row with decode
// depth at most one. A full row validates its own list; a delta loads its
// full checkpoint through loadFull, rejects a chained base, and assembles
// the one-hop result. Every failure is classified.
func reconstructDigests(row contextRow, loadFull func(id int64) (contextRow, error)) ([]string, error) {
	if row.groupOrdinal == groupFullOrdinal {
		var digests []string
		if err := common.Unmarshal(row.itemDigests, &digests); err != nil {
			return nil, classifiedErrorWrap(ContentErrCorrupt, "decode full checkpoint digests", err)
		}
		if len(digests) != row.itemCount {
			return nil, classifiedError(ContentErrCorrupt, "full checkpoint declares %d digests, row says %d", len(digests), row.itemCount)
		}
		return digests, nil
	}
	fullRow, err := loadFull(row.checkpointID)
	if err != nil {
		return nil, err
	}
	if fullRow.groupOrdinal != groupFullOrdinal {
		return nil, classifiedError(ContentErrChainBase, "delta %d references checkpoint %d which is itself a delta", row.id, row.checkpointID)
	}
	var full []string
	if err := common.Unmarshal(fullRow.itemDigests, &full); err != nil {
		return nil, classifiedErrorWrap(ContentErrCorrupt, "decode full checkpoint digests", err)
	}
	var suffix []string
	if err := common.Unmarshal(row.itemDigests, &suffix); err != nil {
		return nil, classifiedErrorWrap(ContentErrCorrupt, "decode delta suffix digests", err)
	}
	return assembleDigests(full, row.commonPrefix, suffix, row.itemCount)
}

// loadContextRowByIDQ loads a context row by primary key plus session; it
// returns sql.ErrNoRows when absent.
func loadContextRowByIDQ(ctx context.Context, q contentQuerier, sessionID uuid.UUID, id int64) (contextRow, error) {
	var row contextRow
	err := q.QueryRow(ctx, `SELECT id, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes FROM observer_contexts WHERE id = $1 AND session_id = $2`,
		id, sessionID.String()).Scan(&row.id, &row.checkpointID, &row.groupOrdinal, &row.commonPrefix, &row.itemCount, &row.itemDigests, &row.logicalBytes)
	if err != nil {
		return contextRow{}, err
	}
	return row, nil
}

// loadContentItemsQ loads the stored objects for the digests in order and
// decodes each with the fail-closed validation of codec.go. A digest with no
// stored object is a classified missing-content error; a corrupt, truncated,
// or digest-mismatched object is rejected with its classification.
func loadContentItemsQ(ctx context.Context, q contentQuerier, sessionID uuid.UUID, digests []string, hmacKey string) ([]CanonicalItem, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	raw := make([][]byte, 0, len(digests))
	for _, d := range digests {
		b, err := itemDigestBytes(d)
		if err != nil {
			return nil, classifiedErrorWrap(ContentErrCorrupt, "invalid digest in context row", err)
		}
		raw = append(raw, b)
	}
	rows, err := q.Query(ctx, `SELECT item_digest, payload, logical_bytes FROM observer_content_objects WHERE session_id = $1 AND item_digest = ANY($2)`, sessionID.String(), raw)
	if err != nil {
		return nil, fmt.Errorf("relayobserver: reconstruct: read content objects: %w", err)
	}
	defer func() {
		if closer, ok := rows.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()
	found := make(map[string]contentObjectRow, len(digests))
	for rows.Next() {
		var d []byte
		var row contentObjectRow
		if err := rows.Scan(&d, &row.payload, &row.logicalBytes); err != nil {
			return nil, fmt.Errorf("relayobserver: reconstruct: scan content object: %w", err)
		}
		found[hex.EncodeToString(d)] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relayobserver: reconstruct: read content objects: %w", err)
	}
	items := make([]CanonicalItem, 0, len(digests))
	for _, d := range digests {
		row, ok := found[d]
		if !ok {
			return nil, classifiedError(ContentErrMissingContent, "digest %s has no content object in session %s", d, sessionID)
		}
		item, err := decodeItem(row.payload, d, row.logicalBytes, hmacKey)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// ReconstructGroup implements ContentPersistence: every context of one group
// in ordinal order, each with the same one-hop decode depth.
func (s *pgStore) ReconstructGroup(ctx context.Context, sessionID uuid.UUID, checkpointID int64, hmacKey string) ([]ReconstructedTurn, error) {
	return reconstructGroupQ(ctx, sqlDBAdapter{db: s.db}, sessionID, checkpointID, hmacKey)
}

// reconstructGroupQ rebuilds every context of one group through the query
// seam, in ordinal order.
func reconstructGroupQ(ctx context.Context, q contentQuerier, sessionID uuid.UUID, checkpointID int64, hmacKey string) ([]ReconstructedTurn, error) {
	rows, err := q.Query(ctx, `SELECT id, turn_id::text, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes FROM observer_contexts WHERE session_id = $1 AND checkpoint_id = $2 ORDER BY group_ordinal`,
		sessionID.String(), checkpointID)
	if err != nil {
		return nil, fmt.Errorf("relayobserver: reconstruct: read group rows: %w", err)
	}
	defer func() {
		if closer, ok := rows.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()
	var out []ReconstructedTurn
	for rows.Next() {
		var row contextRow
		var turnText string
		if err := rows.Scan(&row.id, &turnText, &row.checkpointID, &row.groupOrdinal, &row.commonPrefix, &row.itemCount, &row.itemDigests, &row.logicalBytes); err != nil {
			return nil, fmt.Errorf("relayobserver: reconstruct: scan group row: %w", err)
		}
		turnID, err := uuid.Parse(turnText)
		if err != nil {
			return nil, classifiedErrorWrap(ContentErrCorrupt, "invalid turn id in context row", err)
		}
		items, err := reconstructItemsQ(ctx, q, sessionID, row, hmacKey)
		if err != nil {
			return nil, err
		}
		out = append(out, ReconstructedTurn{TurnID: turnID, Ordinal: row.groupOrdinal, Items: items})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relayobserver: reconstruct: read group rows: %w", err)
	}
	return out, nil
}

// DeleteGroup implements ContentPersistence. See the interface comment for
// the full contract and the content-object boundary.
func (s *pgStore) DeleteGroup(ctx context.Context, sessionID uuid.UUID, checkpointID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("relayobserver: delete group: begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit
	if err := deleteGroupTx(ctx, sqlTxAdapter{tx: tx}, sessionID, checkpointID); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteGroupTx deletes one group's context rows and clears a pointing head
// inside the caller's transaction. After it returns, no row references the
// deleted group.
func deleteGroupTx(ctx context.Context, tx contentTx, sessionID uuid.UUID, checkpointID int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM observer_contexts WHERE session_id = $1 AND checkpoint_id = $2`, sessionID.String(), checkpointID); err != nil {
		return fmt.Errorf("relayobserver: delete group: delete context rows: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE observer_session_heads SET context_id = NULL, checkpoint_id = NULL, group_ordinal = NULL WHERE session_id = $1 AND checkpoint_id = $2`, sessionID.String(), checkpointID); err != nil {
		return fmt.Errorf("relayobserver: delete group: clear head: %w", err)
	}
	return nil
}
