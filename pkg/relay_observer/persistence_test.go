package relayobserver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the T2.3 append orchestration and reconstruction decisions
// without a database. The append flow runs against fakeContentTx, an
// in-memory stand-in for the contentTx seam, mirroring how the dispatcher is
// tested against the Store port: the orchestration decisions (session
// binding, idempotency, dedup, rotation, conflicts) are exercised here, and
// the SQL itself is exercised by the relay_observer_pg_integration suite.

// fakeResult is the RowsAffected carrier of the fake data layer.
type fakeResult struct{ n int64 }

func (fakeResult) LastInsertId() (int64, error)   { return 0, fmt.Errorf("fake: no last insert id") }
func (r fakeResult) RowsAffected() (int64, error) { return r.n, nil }

// fakeRow is one query result row of the fake data layer.
type fakeRow struct {
	values []any
	err    error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if err := scanInto(d, r.values[i]); err != nil {
			return fmt.Errorf("fake scan col %d: %w", i, err)
		}
	}
	return nil
}

// scanInto assigns one fake value into a scan destination.
func scanInto(dest any, val any) error {
	switch d := dest.(type) {
	case *sql.NullInt64:
		switch v := val.(type) {
		case sql.NullInt64:
			*d = v
		case int64:
			d.Int64, d.Valid = v, true
		}
	case *string:
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("fake scan: want string, got %T", val)
		}
		*d = s
	case *int64:
		v, ok := val.(int64)
		if !ok {
			return fmt.Errorf("fake scan: want int64, got %T", val)
		}
		*d = v
	case *int:
		v, ok := val.(int)
		if !ok {
			return fmt.Errorf("fake scan: want int, got %T", val)
		}
		*d = v
	case *[]byte:
		v, ok := val.([]byte)
		if !ok {
			return fmt.Errorf("fake scan: want []byte, got %T", val)
		}
		*d = v
	default:
		return fmt.Errorf("fake scan: unsupported destination %T", dest)
	}
	return nil
}

// fakeHead is the in-memory session head row.
type fakeHead struct {
	contextID  sql.NullInt64
	checkpoint sql.NullInt64
	ordinal    sql.NullInt64
}

// fakeContext is the in-memory context row.
type fakeContext struct {
	id           int64
	turnID       string
	sessionID    string
	checkpointID int64
	ordinal      int
	prefix       int
	itemCount    int
	digests      []string
	logicalBytes int64
}

// fakeRows is the in-memory multi-row result of the fake data layer.
type fakeRows struct {
	rows []*fakeRow
	idx  int
}

func (r *fakeRows) Next() bool {
	r.idx++
	return r.idx <= len(r.rows)
}

func (r *fakeRows) Scan(dest ...any) error {
	return r.rows[r.idx-1].Scan(dest...)
}

func (r *fakeRows) Err() error { return nil }

// fakeContentTx implements contentTx and contentQuerier over in-memory maps.
// It routes by the operation the SQL expresses, not by parsing SQL.
type fakeContentTx struct {
	aliases     map[string]string // "node|user|ver|digesthex|scope" -> session id
	sessions    map[string]bool
	heads       map[string]*fakeHead
	contexts    map[string]*fakeContext // by turn id
	objects     map[string]bool         // "session|digesthex"
	objectsData map[string]contentObjectRow
	counts      map[string][2]int64 // session -> {turns, gaps}
	nextID      int64
}

func newFakeContentTx() *fakeContentTx {
	return &fakeContentTx{
		aliases:     map[string]string{},
		sessions:    map[string]bool{},
		heads:       map[string]*fakeHead{},
		contexts:    map[string]*fakeContext{},
		objects:     map[string]bool{},
		objectsData: map[string]contentObjectRow{},
		counts:      map[string][2]int64{},
	}
}

// aliasKey mirrors the v1 UNIQUE key plus the scope-provider dimension.
func aliasKey(node string, user int64, ver int, digest []byte, scope string) string {
	return fmt.Sprintf("%s|%d|%d|%s|%s", node, user, ver, hex.EncodeToString(digest), scope)
}

// toInt64 normalizes the int/int64 mix the production call sites pass for
// checkpoint columns.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	default:
		panic(fmt.Sprintf("fake: checkpoint value is %T", v))
	}
}

func (f *fakeContentTx) QueryRow(ctx context.Context, query string, args ...any) rowScanner {
	switch {
	case strings.Contains(query, "turn_id") && !strings.Contains(query, "INSERT"):
		// Two shapes: the idempotency probe (SELECT id ... turn_id = $1) and
		// the full row load (SELECT ... session_id = $1 AND turn_id = $2).
		// INSERT ... RETURNING id also carries turn_id but is handled below.
		turn := args[len(args)-1].(string)
		c, ok := f.contexts[turn]
		if !ok {
			return &fakeRow{err: sql.ErrNoRows}
		}
		if len(args) == 1 {
			return &fakeRow{values: []any{c.id}}
		}
		raw, err := common.Marshal(c.digests)
		if err != nil {
			return &fakeRow{err: err}
		}
		return &fakeRow{values: []any{c.id, c.checkpointID, c.ordinal, c.prefix, c.itemCount, raw, c.logicalBytes}}
	case strings.Contains(query, "FROM observer_session_aliases"):
		key := aliasKey(args[0].(string), args[1].(int64), args[2].(int), args[3].([]byte), args[4].(string))
		sid, ok := f.aliases[key]
		if !ok {
			return &fakeRow{err: sql.ErrNoRows}
		}
		return &fakeRow{values: []any{sid}}
	case strings.Contains(query, "FROM observer_session_heads"):
		sid := args[0].(string)
		h, ok := f.heads[sid]
		if !ok {
			return &fakeRow{err: sql.ErrNoRows}
		}
		return &fakeRow{values: []any{h.contextID, h.checkpoint, h.ordinal}}
	case strings.Contains(query, "FROM observer_contexts WHERE id"):
		sid := args[1].(string)
		id := args[0].(int64)
		for _, c := range f.contexts {
			if c.id == id && c.sessionID == sid {
				raw, err := common.Marshal(c.digests)
				if err != nil {
					return &fakeRow{err: err}
				}
				if strings.Contains(query, "SELECT item_digests") {
					return &fakeRow{values: []any{raw}}
				}
				return &fakeRow{values: []any{c.id, c.checkpointID, c.ordinal, c.prefix, c.itemCount, raw, c.logicalBytes}}
			}
		}
		return &fakeRow{err: sql.ErrNoRows}
	case strings.Contains(query, "RETURNING id"):
		// INSERT ... RETURNING id runs through QueryRow: insert the row and
		// hand back its id.
		c := &fakeContext{
			turnID:       args[1].(string),
			sessionID:    args[0].(string),
			checkpointID: toInt64(args[2]),
			ordinal:      args[3].(int),
			prefix:       args[4].(int),
			itemCount:    args[5].(int),
			logicalBytes: args[7].(int64),
		}
		if err := common.Unmarshal([]byte(args[6].(string)), &c.digests); err != nil {
			return &fakeRow{err: fmt.Errorf("fake: bad digests: %w", err)}
		}
		f.nextID++
		c.id = f.nextID
		f.contexts[args[1].(string)] = c
		return &fakeRow{values: []any{c.id}}
	}
	return &fakeRow{err: fmt.Errorf("fake: unhandled query %q", query)}
}

func (f *fakeContentTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	switch {
	case strings.Contains(query, "ON CONFLICT (id) DO UPDATE"):
		// The session counter upsert: INSERT ... ON CONFLICT (id) DO UPDATE.
		// It must match before the plain session-insert branch below.
		sid := args[0].(string)
		c := f.counts[sid]
		c[0]++                       // turns
		c[1] += int64(args[4].(int)) // gaps
		f.counts[sid] = c
		f.sessions[sid] = true
		return fakeResult{n: 1}, nil
	case strings.Contains(query, "INSERT INTO observer_sessions"):
		f.sessions[args[0].(string)] = true
		return fakeResult{n: 1}, nil
	case strings.Contains(query, "INSERT INTO observer_session_aliases"):
		key := aliasKey(args[0].(string), args[1].(int64), args[2].(int), args[5].([]byte), args[3].(string))
		if _, ok := f.aliases[key]; ok {
			return fakeResult{n: 0}, nil
		}
		f.aliases[key] = args[6].(string)
		return fakeResult{n: 1}, nil
	case strings.Contains(query, "INSERT INTO observer_session_heads"):
		sid := args[0].(string)
		if _, ok := f.heads[sid]; ok {
			return fakeResult{n: 0}, nil
		}
		f.heads[sid] = &fakeHead{}
		return fakeResult{n: 1}, nil
	case strings.Contains(query, "INSERT INTO observer_content_objects"):
		key := args[0].(string) + "|" + hex.EncodeToString(args[1].([]byte))
		if f.objects[key] {
			return fakeResult{n: 0}, nil
		}
		f.objects[key] = true
		f.objectsData[key] = contentObjectRow{payload: args[5].([]byte), logicalBytes: args[6].(int64)}
		return fakeResult{n: 1}, nil
	case strings.Contains(query, "UPDATE observer_contexts SET checkpoint_id"):
		id := args[0].(int64)
		for _, c := range f.contexts {
			if c.id == id {
				c.checkpointID = id
			}
		}
		return fakeResult{n: 1}, nil
	case strings.Contains(query, "DELETE FROM observer_contexts"):
		sid := args[0].(string)
		cp := args[1].(int64)
		for turn, c := range f.contexts {
			if c.sessionID == sid && c.checkpointID == cp {
				delete(f.contexts, turn)
			}
		}
		return fakeResult{n: 1}, nil
	case strings.Contains(query, "SET context_id = NULL"):
		sid := args[0].(string)
		cp := args[1].(int64)
		if h, ok := f.heads[sid]; ok && h.checkpoint.Int64 == cp {
			h.contextID = sql.NullInt64{}
			h.checkpoint = sql.NullInt64{}
			h.ordinal = sql.NullInt64{}
		}
		return fakeResult{n: 1}, nil
	case strings.Contains(query, "UPDATE observer_session_heads"):
		sid := args[3].(string)
		h := f.heads[sid]
		h.contextID = sql.NullInt64{Int64: args[0].(int64), Valid: true}
		h.checkpoint = sql.NullInt64{Int64: args[1].(int64), Valid: true}
		h.ordinal = sql.NullInt64{Int64: int64(args[2].(int)), Valid: true}
		return fakeResult{n: 1}, nil
	}
	return nil, fmt.Errorf("fake: unhandled exec %q", query)
}

// Query implements the multi-row read surface of contentQuerier.
func (f *fakeContentTx) Query(ctx context.Context, query string, args ...any) (rowIter, error) {
	switch {
	case strings.Contains(query, "FROM observer_content_objects"):
		sid := args[0].(string)
		want := map[string]bool{}
		for _, d := range args[1].([][]byte) {
			want[hex.EncodeToString(d)] = true
		}
		var rows []*fakeRow
		for key, row := range f.objectsData {
			if !strings.HasPrefix(key, sid+"|") {
				continue
			}
			d := strings.TrimPrefix(key, sid+"|")
			if !want[d] {
				continue
			}
			raw, err := hex.DecodeString(d)
			if err != nil {
				return nil, err
			}
			rows = append(rows, &fakeRow{values: []any{raw, row.payload, row.logicalBytes}})
		}
		return &fakeRows{rows: rows}, nil
	case strings.Contains(query, "ORDER BY group_ordinal"):
		sid := args[0].(string)
		cp := args[1].(int64)
		var list []*fakeContext
		for _, c := range f.contexts {
			if c.sessionID == sid && c.checkpointID == cp {
				list = append(list, c)
			}
		}
		for i := 1; i < len(list); i++ {
			for j := i; j > 0 && list[j-1].ordinal > list[j].ordinal; j-- {
				list[j-1], list[j] = list[j], list[j-1]
			}
		}
		var rows []*fakeRow
		for _, c := range list {
			raw, err := common.Marshal(c.digests)
			if err != nil {
				return nil, err
			}
			rows = append(rows, &fakeRow{values: []any{c.id, c.turnID, c.checkpointID, c.ordinal, c.prefix, c.itemCount, raw, c.logicalBytes}})
		}
		return &fakeRows{rows: rows}, nil
	}
	return nil, fmt.Errorf("fake: unhandled query %q", query)
}

// fixture state for the orchestration tests.
type fakeFixture struct {
	t     *testing.T
	tx    *fakeContentTx
	key   KeyMaterial
	node  string
	user  int64
	alias Alias
}

func newFakeFixture(t *testing.T, value string) *fakeFixture {
	t.Helper()
	km := KeyMaterial{CurrentKey: testHMACKey, CurrentVersion: 1}
	alias, err := GenerateAlias(value, SourceTurnThread, ScopeCodexCLI, km)
	require.NoError(t, err)
	return &fakeFixture{t: t, tx: newFakeContentTx(), key: km, node: "n", user: 7, alias: alias}
}

func (f *fakeFixture) turn(items ...string) ContentInput {
	its := make([]CanonicalItem, 0, len(items))
	for _, text := range items {
		it := CanonicalItem{Kind: CanonicalKindMessage, Role: "user", Content: []CanonicalPart{{Type: partTypeText, Text: text}}}
		it.Hmac = digestOfItem(f.t, it)
		its = append(its, it)
	}
	return ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias}, TurnID: uuid.New(), Items: its}
}

// sessionID returns the bound session id of the fixture alias.
func (f *fakeFixture) sessionID() uuid.UUID {
	t := f.t
	raw, err := itemDigestBytes(f.alias.Digest)
	require.NoError(t, err)
	key := aliasKey(f.node, f.user, f.alias.Version, raw, string(f.alias.Scope))
	sid, ok := f.tx.aliases[key]
	require.True(t, ok, "alias must be bound")
	id, err := uuid.Parse(sid)
	require.NoError(t, err)
	return id
}

// TestAppendTurnFirstIsFullAndBinds locks the first-write orchestration: a
// fresh session is created, the primary alias is bound, the context is a
// full, the head points at it, and the session counters advance.
func TestAppendTurnFirstIsFullAndBinds(t *testing.T) {
	f := newFakeFixture(t, "first-session")
	turn := f.turn("a", "b")
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &turn))

	sid := f.sessionID()
	assert.True(t, f.tx.sessions[sid.String()], "session row must exist")
	c := f.tx.contexts[turn.TurnID.String()]
	require.NotNil(t, c, "context row must exist")
	assert.Equal(t, 0, c.ordinal, "first context is a full")
	assert.Equal(t, c.id, c.checkpointID, "full self-references its checkpoint")
	assert.Equal(t, 2, c.itemCount)

	h := f.tx.heads[sid.String()]
	require.True(t, h.ordinal.Valid)
	assert.Equal(t, int64(0), h.ordinal.Int64)
	assert.Equal(t, int64(c.id), h.contextID.Int64)

	counts := f.tx.counts[sid.String()]
	assert.Equal(t, [2]int64{1, 0}, counts, "one turn, no gaps")
}

// TestAppendTurnIdempotentReplay locks the replay orchestration: appending
// the same turn twice creates one context, does not duplicate objects, and
// does not advance the head.
func TestAppendTurnIdempotentReplay(t *testing.T) {
	f := newFakeFixture(t, "replay-session")
	turn := f.turn("x")
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &turn))
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &turn))

	sid := f.sessionID()
	assert.Len(t, f.tx.contexts, 1, "replay must not duplicate contexts")
	assert.Len(t, f.tx.objects, 1, "replay must not duplicate objects")
	h := f.tx.heads[sid.String()]
	assert.Equal(t, int64(0), h.ordinal.Int64, "head stays on the first write")

	next := f.turn("y")
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &next))
	assert.Equal(t, int64(1), f.tx.heads[sid.String()].ordinal.Int64, "the next fresh turn gets ordinal 1, not 2")
}

// TestAppendTurnDedupContentObjects locks the payload-stored-once rule at the
// orchestration level: equal items across turns insert one object.
func TestAppendTurnDedupContentObjects(t *testing.T) {
	f := newFakeFixture(t, "dedup-session")
	t1, t2 := f.turn("same"), f.turn("same")
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &t1))
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &t2))
	assert.Len(t, f.tx.objects, 1, "one digest, one object")
}

// TestAppendTurnGroupRotation locks the rotation orchestration: nine turns
// fill one group (full + 8 deltas), the tenth rotates to a new full, and
// every delta stores the common prefix plus the new suffix against its full.
func TestAppendTurnGroupRotation(t *testing.T) {
	f := newFakeFixture(t, "rotate-session")
	var all []string
	var inputs []ContentInput
	for i := 0; i < 10; i++ {
		all = append(all, fmt.Sprintf("item-%d", i))
		inputs = append(inputs, f.turn(all...))
	}
	for i := range inputs {
		require.NoError(t, appendTurnTx(context.Background(), f.tx, &inputs[i]))
	}
	sid := f.sessionID()

	// Group 1's full is the smallest self-referenced full (insertion order).
	var firstGroup int64
	for _, c := range f.tx.contexts {
		if c.ordinal == 0 && (firstGroup == 0 || c.checkpointID < firstGroup) {
			firstGroup = c.checkpointID
		}
	}
	fullRows, deltaRows, rotatedItems := 0, 0, 0
	for _, c := range f.tx.contexts {
		if c.ordinal == 0 {
			fullRows++
			assert.Equal(t, c.id, c.checkpointID, "full self-references")
			if c.checkpointID == firstGroup {
				rotatedItems = c.itemCount // group 1's full is turn 0, one item
			}
		} else {
			deltaRows++
			assert.Equal(t, firstGroup, c.checkpointID, "first-group delta points at its full")
			// The full holds item-0 only, so every first-group delta shares
			// exactly that one-item prefix and stores only the new tail —
			// nothing from the full is copied into the delta.
			assert.Equal(t, 1, c.prefix, "delta prefix against the one-item full")
			assert.Equal(t, c.ordinal+1, c.itemCount, "delta item count")
			assert.Len(t, c.digests, c.ordinal, "delta stores only the suffix after the prefix")
		}
	}
	assert.Equal(t, 2, fullRows, "the tenth turn rotates to a new full")
	assert.Equal(t, 8, deltaRows)
	assert.Equal(t, 1, rotatedItems, "group 1's full holds only its first item")
	require.True(t, f.tx.heads[sid.String()].ordinal.Valid)
	assert.Equal(t, int64(0), f.tx.heads[sid.String()].ordinal.Int64, "head points at the new full")
}

// TestAppendTurnNoSessionOrItemsIsNoop locks the fail-open seam: turns
// without aliases or items produce no rows.
func TestAppendTurnNoSessionOrItemsIsNoop(t *testing.T) {
	f := newFakeFixture(t, "noop-session")

	noAlias := ContentInput{NodeScope: f.node, UserID: f.user, TurnID: uuid.New(), Items: []CanonicalItem{{Kind: CanonicalKindMessage}}}
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &noAlias))
	assert.Empty(t, f.tx.contexts)

	noItems := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias}, TurnID: uuid.New()}
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &noItems))
	assert.Empty(t, f.tx.contexts)
	assert.Empty(t, f.tx.sessions)
}

// TestAppendTurnAuxiliaryConflictStaysSeparate locks the v1 conflict rule: an
// auxiliary alias already bound to another session is left untouched.
func TestAppendTurnAuxiliaryConflictStaysSeparate(t *testing.T) {
	f := newFakeFixture(t, "primary-session")
	km := f.key
	otherPrimary, err := GenerateAlias("other-primary", SourceTurnThread, ScopeCodexCLI, km)
	require.NoError(t, err)
	sharedAux, err := GenerateAlias("shared-aux", SourceClaudeHeader, ScopeClaudeCLI, km)
	require.NoError(t, err)

	t1 := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{otherPrimary, sharedAux}, TurnID: uuid.New(), Items: []CanonicalItem{contentItemWith(t, "a")}}
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &t1))
	firstSid := f.sessionIDOf(otherPrimary)

	// Second session's turn tries to bind the same auxiliary alias.
	t2 := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias, sharedAux}, TurnID: uuid.New(), Items: []CanonicalItem{contentItemWith(t, "b")}}
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &t2))
	secondSid := f.sessionIDOf(f.alias)
	assert.NotEqual(t, firstSid, secondSid)

	raw, err := itemDigestBytes(sharedAux.Digest)
	require.NoError(t, err)
	key := aliasKey(f.node, f.user, sharedAux.Version, raw, string(sharedAux.Scope))
	assert.Equal(t, firstSid.String(), f.tx.aliases[key], "the auxiliary alias keeps its first binding")
}

func (f *fakeFixture) sessionIDOf(a Alias) uuid.UUID {
	raw, err := itemDigestBytes(a.Digest)
	require.NoError(f.t, err)
	key := aliasKey(f.node, f.user, a.Version, raw, string(a.Scope))
	sid, ok := f.tx.aliases[key]
	require.True(f.t, ok, "alias must be bound")
	id, err := uuid.Parse(sid)
	require.NoError(f.t, err)
	return id
}

// TestAppendTurnCrossProfileEqualValueStaysSeparate locks the identity
// review rule: equal raw values across profiles resolve to two sessions and
// never collide on the alias UNIQUE key.
func TestAppendTurnCrossProfileEqualValueStaysSeparate(t *testing.T) {
	km := KeyMaterial{CurrentKey: testHMACKey, CurrentVersion: 1}
	codex, err := GenerateAlias("same-value", SourceTurnThread, ScopeCodexCLI, km)
	require.NoError(t, err)
	claude, err := GenerateAlias("same-value", SourceClaudeHeader, ScopeClaudeCLI, km)
	require.NoError(t, err)
	require.Equal(t, codex.Digest, claude.Digest, "fixture: equal values hash to one digest")

	tx := newFakeContentTx()
	t1 := ContentInput{NodeScope: "n", UserID: 7, Aliases: []Alias{codex}, TurnID: uuid.New(), Items: []CanonicalItem{contentItemWith(t, "codex")}}
	require.NoError(t, appendTurnTx(context.Background(), tx, &t1))
	t2 := ContentInput{NodeScope: "n", UserID: 7, Aliases: []Alias{claude}, TurnID: uuid.New(), Items: []CanonicalItem{contentItemWith(t, "claude")}}
	require.NoError(t, appendTurnTx(context.Background(), tx, &t2))

	raw, err := itemDigestBytes(codex.Digest)
	require.NoError(t, err)
	codexKey := aliasKey("n", 7, codex.Version, raw, string(codex.Scope))
	claudeKey := aliasKey("n", 7, claude.Version, raw, string(claude.Scope))
	assert.NotEqual(t, tx.aliases[codexKey], tx.aliases[claudeKey], "two sessions, never a UNIQUE collision")
	assert.NotEmpty(t, tx.aliases[codexKey])
	assert.NotEmpty(t, tx.aliases[claudeKey])
	assert.Len(t, tx.sessions, 2)
}

// TestAppendTurnGapMarkerCountsGap locks the session gap accounting: a turn
// carrying a gap marker increments the session's gap_count. The gap marker's
// digest is a real 64-hex value (in production it is the digest of the
// dropped tail start, never an arbitrary string).
func TestAppendTurnGapMarkerCountsGap(t *testing.T) {
	f := newFakeFixture(t, "gap-session")
	turn := f.turn("ok")
	gap := CanonicalItem{Kind: CanonicalKindGap, LogicalBytes: 5, Hmac: strings.Repeat("a", 64), Truncated: true}
	turn.Items = append(turn.Items, gap)
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &turn))
	sid := f.sessionID()
	assert.Equal(t, [2]int64{1, 1}, f.tx.counts[sid.String()], "one turn with one gap")
}

// TestAppendTurnsStopsOnError locks the batch seam: an error from any turn
// aborts the remaining appends (the transaction is rolled back by the real
// adapter).
func TestAppendTurnsStopsOnError(t *testing.T) {
	f := newFakeFixture(t, "batch-session")
	bad := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias}, TurnID: uuid.New(), Items: []CanonicalItem{{Kind: CanonicalKindMessage, Hmac: "zz-not-hex"}}}
	err := appendTurnsTx(context.Background(), f.tx, []ContentInput{f.turn("a"), bad, f.turn("b")})
	require.Error(t, err, "an invalid digest aborts the batch")
	assert.Len(t, f.tx.contexts, 1, "the append after the failure never runs")
}

// TestReconstructDigestsFull locks the full-row decision: the row's own list
// is validated against its declared count.
func TestReconstructDigestsFull(t *testing.T) {
	raw, err := common.Marshal([]string{"d1", "d2"})
	require.NoError(t, err)
	row := contextRow{id: 1, checkpointID: 1, groupOrdinal: 0, itemCount: 2, itemDigests: raw}
	got, err := reconstructDigests(row, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"d1", "d2"}, got)

	row.itemCount = 3
	_, err = reconstructDigests(row, nil)
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrCorrupt, code)
}

// TestReconstructDigestsDelta locks the one-hop decision: a delta loads its
// full through the seam and assembles prefix plus suffix.
func TestReconstructDigestsDelta(t *testing.T) {
	rawFull, err := common.Marshal([]string{"a", "b", "c", "d"})
	require.NoError(t, err)
	rawSuffix, err := common.Marshal([]string{"e"})
	require.NoError(t, err)
	load := func(id int64) (contextRow, error) {
		assert.Equal(t, int64(9), id)
		return contextRow{id: 9, checkpointID: 9, groupOrdinal: 0, itemCount: 4, itemDigests: rawFull}, nil
	}
	row := contextRow{id: 10, checkpointID: 9, groupOrdinal: 2, commonPrefix: 3, itemCount: 4, itemDigests: rawSuffix}
	got, err := reconstructDigests(row, load)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c", "e"}, got)
}

// TestReconstructDigestsRejectsBadBase locks the fail-closed base checks:
// a missing base, a chained base, a prefix overflow, and a count mismatch
// are all classified errors.
func TestReconstructDigestsRejectsBadBase(t *testing.T) {
	rawFull, err := common.Marshal([]string{"a", "b"})
	require.NoError(t, err)
	rawSuffix, err := common.Marshal([]string{"x"})
	require.NoError(t, err)

	t.Run("missing base", func(t *testing.T) {
		row := contextRow{id: 2, checkpointID: 99, groupOrdinal: 1, commonPrefix: 1, itemCount: 2, itemDigests: rawSuffix}
		_, err := reconstructDigests(row, func(int64) (contextRow, error) {
			return contextRow{}, classifiedError(ContentErrMissingBase, "base gone")
		})
		require.Error(t, err)
		code, ok := ContentErrorOf(err)
		require.True(t, ok)
		assert.Equal(t, ContentErrMissingBase, code)
	})

	t.Run("chained base", func(t *testing.T) {
		row := contextRow{id: 2, checkpointID: 3, groupOrdinal: 1, commonPrefix: 1, itemCount: 2, itemDigests: rawSuffix}
		_, err := reconstructDigests(row, func(id int64) (contextRow, error) {
			return contextRow{id: 3, checkpointID: 1, groupOrdinal: 2, itemDigests: rawFull}, nil
		})
		require.Error(t, err)
		code, ok := ContentErrorOf(err)
		require.True(t, ok)
		assert.Equal(t, ContentErrChainBase, code, "a base that is itself a delta is a chain")
	})

	t.Run("prefix overflow", func(t *testing.T) {
		row := contextRow{id: 2, checkpointID: 3, groupOrdinal: 1, commonPrefix: 5, itemCount: 6, itemDigests: rawSuffix}
		_, err := reconstructDigests(row, func(id int64) (contextRow, error) {
			return contextRow{id: 3, checkpointID: 3, groupOrdinal: 0, itemDigests: rawFull}, nil
		})
		require.Error(t, err)
		code, ok := ContentErrorOf(err)
		require.True(t, ok)
		assert.Equal(t, ContentErrCorruptDelta, code)
	})

	t.Run("count mismatch", func(t *testing.T) {
		row := contextRow{id: 2, checkpointID: 3, groupOrdinal: 1, commonPrefix: 1, itemCount: 9, itemDigests: rawSuffix}
		_, err := reconstructDigests(row, func(id int64) (contextRow, error) {
			return contextRow{id: 3, checkpointID: 3, groupOrdinal: 0, itemDigests: rawFull}, nil
		})
		require.Error(t, err)
		code, ok := ContentErrorOf(err)
		require.True(t, ok)
		assert.Equal(t, ContentErrCorruptDelta, code)
	})
}

// addContext seeds one context row directly into the fake data layer.
func (f *fakeContentTx) addContext(turnID, sessionID string, ordinal int, checkpointID int64, digests []string) *fakeContext {
	f.nextID++
	c := &fakeContext{
		id: f.nextID, turnID: turnID, sessionID: sessionID,
		checkpointID: checkpointID, ordinal: ordinal,
		itemCount: len(digests), digests: digests,
	}
	f.contexts[turnID] = c
	return c
}

// addObject seeds one stored content object into the fake data layer.
func (f *fakeContentTx) addObject(sessionID string, item CanonicalItem) {
	payload, logical, err := encodeItem(item)
	if err != nil {
		panic(err)
	}
	key := sessionID + "|" + item.Hmac
	f.objects[key] = true
	f.objectsData[key] = contentObjectRow{payload: payload, logicalBytes: logical}
}

// TestReconstructTurnThroughSeam locks the full reconstruction chain on the
// fake data layer: a full turn and a delta turn rebuild their exact items,
// and a turn with no context row fails with the missing-context
// classification.
func TestReconstructTurnThroughSeam(t *testing.T) {
	fx := newFakeContentTx()
	sid := uuid.New().String()
	t1, t2 := uuid.NewString(), uuid.NewString()
	fullDigests := []string{contentItemWith(t, "one").Hmac, contentItemWith(t, "two").Hmac}
	deltaDigests := []string{contentItemWith(t, "three").Hmac}
	fx.addContext(t1, sid, 0, 0, fullDigests)
	c := fx.addContext(t2, sid, 1, 0, deltaDigests)
	c.prefix = 2
	c.itemCount = 3    // prefix 2 + suffix 1
	c.checkpointID = 1 // full row id
	fx.addObject(sid, contentItemWith(t, "one"))
	fx.addObject(sid, contentItemWith(t, "two"))
	fx.addObject(sid, contentItemWith(t, "three"))

	// Self-reference the full row (SSOT: checkpoint_id = id).
	full := fx.contexts[t1]
	full.checkpointID = full.id

	ctx := context.Background()
	got, err := reconstructTurnQ(ctx, fx, uuid.MustParse(sid), uuid.MustParse(t1), testHMACKey)
	require.NoError(t, err)
	require.Len(t, got.Items, 2)
	assert.Equal(t, "one", got.Items[0].Content[0].Text)
	assert.Equal(t, "two", got.Items[1].Content[0].Text)

	got, err = reconstructTurnQ(ctx, fx, uuid.MustParse(sid), uuid.MustParse(t2), testHMACKey)
	require.NoError(t, err)
	require.Len(t, got.Items, 3)
	assert.Equal(t, "three", got.Items[2].Content[0].Text)

	_, err = reconstructTurnQ(ctx, fx, uuid.MustParse(sid), uuid.New(), testHMACKey)
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrMissingContext, code)
}

// TestReconstructMissingObjectAndCorruptObject locks the content-object
// decode chain on the fake data layer: a referenced digest with no stored
// object is missing_content, and a corrupt stored payload is rejected with a
// classified codec error.
func TestReconstructMissingObjectAndCorruptObject(t *testing.T) {
	t.Run("missing object", func(t *testing.T) {
		fx := newFakeContentTx()
		sid := uuid.New().String()
		turn := uuid.NewString()
		item := contentItemWith(t, "ghost")
		fx.addContext(turn, sid, 0, 0, []string{item.Hmac})
		_, err := reconstructTurnQ(context.Background(), fx, uuid.MustParse(sid), uuid.MustParse(turn), testHMACKey)
		require.Error(t, err)
		code, ok := ContentErrorOf(err)
		require.True(t, ok)
		assert.Equal(t, ContentErrMissingContent, code)
	})
	t.Run("corrupt object", func(t *testing.T) {
		fx := newFakeContentTx()
		sid := uuid.New().String()
		turn := uuid.NewString()
		item := contentItemWith(t, "fragile")
		fx.addContext(turn, sid, 0, 0, []string{item.Hmac})
		fx.addObject(sid, item)
		key := sid + "|" + item.Hmac
		row := fx.objectsData[key]
		row.payload = row.payload[:len(row.payload)-3] // truncate the frame
		fx.objectsData[key] = row
		_, err := reconstructTurnQ(context.Background(), fx, uuid.MustParse(sid), uuid.MustParse(turn), testHMACKey)
		require.Error(t, err)
		_, ok := ContentErrorOf(err)
		require.True(t, ok, "a corrupt stored object classifies")
	})
}

// TestReconstructGroupOrdinalOrder locks the group reconstruction order on
// the fake data layer: contexts come back full first, then deltas in ordinal
// order, each with its full items.
func TestReconstructGroupOrdinalOrder(t *testing.T) {
	fx := newFakeContentTx()
	sid := uuid.New().String()
	fullDigests := []string{contentItemWith(t, "g0").Hmac, contentItemWith(t, "g1").Hmac}
	deltaDigests := []string{contentItemWith(t, "g2").Hmac}
	fx.addContext(uuid.NewString(), sid, 0, 0, fullDigests)
	d := fx.addContext(uuid.NewString(), sid, 1, 0, deltaDigests)
	d.prefix = 2
	d.itemCount = 3 // prefix 2 + suffix 1
	fx.addObject(sid, contentItemWith(t, "g0"))
	fx.addObject(sid, contentItemWith(t, "g1"))
	fx.addObject(sid, contentItemWith(t, "g2"))
	var full *fakeContext
	for _, c := range fx.contexts {
		if c.ordinal == 0 {
			full = c
			full.checkpointID = full.id
		}
	}
	require.NotNil(t, full)
	d.checkpointID = full.id

	group, err := reconstructGroupQ(context.Background(), fx, uuid.MustParse(sid), full.id, testHMACKey)
	require.NoError(t, err)
	require.Len(t, group, 2)
	assert.Equal(t, 0, group[0].Ordinal)
	assert.Equal(t, 1, group[1].Ordinal)
	assert.Len(t, group[0].Items, 2)
	assert.Len(t, group[1].Items, 3)
}

// TestContentErrorFormatting locks the classified error rendering: every
// combination of code, message, and cause produces a stable, code-leading
// error string.
func TestContentErrorFormatting(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "code only", err: &ContentError{Code: ContentErrChainBase}, want: "relayobserver: content chain_base"},
		{name: "code and message", err: &ContentError{Code: ContentErrCorrupt, Msg: "broken"}, want: "relayobserver: content corrupt: broken"},
		{name: "code and cause", err: &ContentError{Code: ContentErrCodec, Err: assert.AnError}, want: "relayobserver: content codec_error: " + assert.AnError.Error()},
		{name: "all three", err: &ContentError{Code: ContentErrMissingBase, Msg: "gone", Err: assert.AnError}, want: "relayobserver: content missing_base: gone: " + assert.AnError.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.Error())
			assert.Contains(t, fmt.Sprintf("%+v", tt.err), tt.want)
		})
	}
}

// TestMetaOfItems locks the digest-list and gap accounting of a turn's items.
func TestMetaOfItems(t *testing.T) {
	gap := CanonicalItem{Kind: CanonicalKindGap, Hmac: strings.Repeat("b", 64)}
	meta := metaOfItems([]CanonicalItem{contentItemWith(t, "x"), gap})
	assert.Equal(t, []string{contentItemWith(t, "x").Hmac, gap.Hmac}, meta.digests)
	assert.True(t, meta.hasGap)

	meta = metaOfItems(nil)
	assert.Empty(t, meta.digests)
	assert.False(t, meta.hasGap)
}

// TestAppendTurnAuxiliaryIdempotent locks the auxiliary re-bind path: an
// auxiliary alias already bound to the same session is left untouched on
// later appends (no duplicate binding, no error).
func TestAppendTurnAuxiliaryIdempotent(t *testing.T) {
	f := newFakeFixture(t, "aux-primary")
	aux, err := GenerateAlias("aux-value", SourceTurnSession, ScopeCodexCLI, f.key)
	require.NoError(t, err)

	t1 := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias, aux}, TurnID: uuid.New(), Items: []CanonicalItem{contentItemWith(t, "a")}}
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &t1))
	t2 := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias, aux}, TurnID: uuid.New(), Items: []CanonicalItem{contentItemWith(t, "b")}}
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &t2))

	raw, err := itemDigestBytes(aux.Digest)
	require.NoError(t, err)
	key := aliasKey(f.node, f.user, aux.Version, raw, string(aux.Scope))
	sid := f.sessionID()
	assert.Equal(t, sid.String(), f.tx.aliases[key], "the auxiliary alias keeps one binding")
	assert.Equal(t, 1, f.countAliasBindings(aux))
}

// countAliasBindings counts alias rows sharing the auxiliary digest.
func (f *fakeFixture) countAliasBindings(a Alias) int {
	n := 0
	raw, err := itemDigestBytes(a.Digest)
	if err != nil {
		panic(err)
	}
	prefix := fmt.Sprintf("%s|%d|%d|%s|", f.node, f.user, a.Version, hex.EncodeToString(raw))
	for k := range f.tx.aliases {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// TestAppendTurnMissingBaseClassifies locks the missing-base path of the
// head read: a head pointing at a context row that no longer exists fails
// with the missing_base classification instead of planning a bogus delta.
func TestAppendTurnMissingBaseClassifies(t *testing.T) {
	f := newFakeFixture(t, "missing-base")
	turn := f.turn("a", "b")
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &turn))
	// Simulate external deletion of the full checkpoint behind the head.
	f.tx.contexts = map[string]*fakeContext{}
	fresh := f.turn("x")
	err := appendTurnTx(context.Background(), f.tx, &fresh)
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrMissingBase, code)
}

// TestDeleteGroupThroughSeam locks the group deletion orchestration: the
// context rows of the group disappear and a pointing head is cleared.
func TestDeleteGroupThroughSeam(t *testing.T) {
	fx := newFakeContentTx()
	sid := uuid.New().String()
	fx.addContext(uuid.NewString(), sid, 0, 1, []string{"d1"})
	fx.heads[sid] = &fakeHead{
		contextID:  sql.NullInt64{Int64: 1, Valid: true},
		checkpoint: sql.NullInt64{Int64: 1, Valid: true},
		ordinal:    sql.NullInt64{Int64: 0, Valid: true},
	}
	require.NoError(t, deleteGroupTx(context.Background(), fx, uuid.MustParse(sid), 1))
	assert.Empty(t, fx.contexts, "group deletion removes its context rows")
	h := fx.heads[sid]
	require.False(t, h.contextID.Valid, "head context is cleared")
	require.False(t, h.ordinal.Valid, "head ordinal is cleared")

	// A head pointing at a different (undeleted) group survives deletion.
	other := uuid.NewString()
	fx.addContext(uuid.NewString(), other, 0, 7, []string{"d2"})
	fx.heads[other] = &fakeHead{
		contextID:  sql.NullInt64{Int64: 5, Valid: true},
		checkpoint: sql.NullInt64{Int64: 9, Valid: true}, // another group, not 7
		ordinal:    sql.NullInt64{Int64: 1, Valid: true},
	}
	// Deleting a group that is not the head's group leaves both the head and
	// the other group's rows intact.
	require.NoError(t, deleteGroupTx(context.Background(), fx, uuid.MustParse(other), 99))
	require.True(t, fx.heads[other].ordinal.Valid, "a head outside the deleted group survives")
	assert.Len(t, fx.contexts, 1, "the other group's context row survives")
}

// TestDecodeItemRejectsInvalidJSON locks the JSON decode step: a payload
// that decompresses but is not a canonical item JSON is rejected as corrupt.
func TestDecodeItemRejectsInvalidJSON(t *testing.T) {
	raw, err := zstdCompress([]byte("this is not json"))
	require.NoError(t, err)
	_, err = decodeItem(raw, strings.Repeat("c", 64), int64(len("this is not json")), testHMACKey)
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrCorrupt, code)
}

// TestReconstructEmptyDigestList locks the empty turn: a context row with no
// digests reconstructs to no items.
func TestReconstructEmptyDigestList(t *testing.T) {
	fx := newFakeContentTx()
	sid := uuid.New().String()
	turn := uuid.NewString()
	fx.addContext(turn, sid, 0, 0, nil)
	full := fx.contexts[turn]
	full.checkpointID = full.id

	got, err := reconstructTurnQ(context.Background(), fx, uuid.MustParse(sid), uuid.MustParse(turn), testHMACKey)
	require.NoError(t, err)
	assert.Empty(t, got.Items)
}

// TestAppendTurnsEmptyIsNoop locks the empty-batch seam.
func TestAppendTurnsEmptyIsNoop(t *testing.T) {
	fx := newFakeContentTx()
	require.NoError(t, appendTurnsTx(context.Background(), fx, nil))
}

// TestGapMarkerDigestNeverCollidesWithRealItem locks the dedup invariant
// under gap markers: a gap marker's digest must be its own content digest,
// never the digest of a real item. Content objects are keyed by
// (session_id, item_digest) and inserted with ON CONFLICT DO NOTHING, so a
// marker keyed by the first dropped item's digest silently collides with that
// item's stored object: the marker (data, not silent loss) disappears from
// reconstruction, or a real item is replaced by a marker, with no error
// either way. Both directions drive the real normalizer, whose truncation
// cap produces the marker.
func TestGapMarkerDigestNeverCollidesWithRealItem(t *testing.T) {
	// buildChat builds an OpenAI Chat request of msgCount messages whose
	// canonical payloads comfortably exceed the marker payload size.
	buildChat := func(msgCount int) *dto.GeneralOpenAIRequest {
		req := &dto.GeneralOpenAIRequest{Model: "gpt-5"}
		for i := 0; i < msgCount; i++ {
			req.Messages = append(req.Messages, dto.Message{Role: "user", Content: fmt.Sprintf("message-%d-%s", i, strings.Repeat("x", 200))})
		}
		return req
	}
	// payloadOf returns the marshaled canonical payload length of one item.
	payloadOf := func(t *testing.T, it CanonicalItem) int64 {
		p, err := common.Marshal(it)
		require.NoError(t, err)
		return int64(len(p))
	}

	t.Run("real item stored first, marker lost", func(t *testing.T) {
		f := newFakeFixture(t, "gap-collision-forward")
		opts := NormalizeOptions{Reservation: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey}

		// Turn 1 fits the cap: every item is stored as real content.
		full := NormalizeRequest(string(types.RelayFormatOpenAI), buildChat(7), opts)
		require.Equal(t, ContentStateFull, full.ContentState)
		t1 := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias}, TurnID: uuid.New(), Items: full.Items}
		require.NoError(t, appendTurnTx(context.Background(), f.tx, &t1))

		// Turn 2 exceeds the cap with one more item: the normalizer drops the
		// tail and appends a gap marker. Its digest must not collide with any
		// stored real item. The limit keeps exactly six items plus the marker
		// (a ~170-byte payload), so items 6 and 7 of the eight are dropped.
		var sum6 int64
		for _, it := range full.Items[:6] {
			sum6 += payloadOf(t, it)
		}
		limit := sum6 + 250
		truncated := NormalizeRequest(string(types.RelayFormatOpenAI), buildChat(8), NormalizeOptions{Reservation: limit, MaxRequestBytes: limit, HMACKey: testHMACKey})
		require.Equal(t, ContentStateGap, truncated.ContentState)
		require.Len(t, truncated.Items, 7, "six kept items plus one marker")
		marker := truncated.Items[len(truncated.Items)-1]
		require.Equal(t, CanonicalKindGap, marker.Kind)
		require.True(t, marker.Truncated)
		require.NotEqual(t, full.Items[6].Hmac, marker.Hmac, "the marker's digest is its own content digest")

		t2 := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias}, TurnID: uuid.New(), Items: truncated.Items}
		require.NoError(t, appendTurnTx(context.Background(), f.tx, &t2))

		got, err := reconstructTurnQ(context.Background(), f.tx, f.sessionID(), t2.TurnID, testHMACKey)
		require.NoError(t, err)
		require.NotEmpty(t, got.Items)
		last := got.Items[len(got.Items)-1]
		assert.Equal(t, CanonicalKindGap, last.Kind, "the truncation marker is data, not silent loss")
		assert.True(t, last.Truncated)
	})

	t.Run("marker stored first, real item replaced", func(t *testing.T) {
		f := newFakeFixture(t, "gap-collision-reverse")
		// Turn 1 truncates early: the dropped tail's first item is "c", which
		// is stored as a real item only in the later divergent turn 2. The
		// limit keeps a, b and the marker (~170 bytes) and drops c.
		c := buildChat(3)
		first := NormalizeRequest(string(types.RelayFormatOpenAI), c, NormalizeOptions{Reservation: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey})
		limit := payloadOf(t, first.Items[0]) + payloadOf(t, first.Items[1]) + 250
		truncated := NormalizeRequest(string(types.RelayFormatOpenAI), c, NormalizeOptions{Reservation: limit, MaxRequestBytes: limit, HMACKey: testHMACKey})
		require.Equal(t, ContentStateGap, truncated.ContentState)
		require.Len(t, truncated.Items, 3, "two kept items plus one marker")
		marker := truncated.Items[len(truncated.Items)-1]
		require.Equal(t, CanonicalKindGap, marker.Kind)
		require.NotEqual(t, first.Items[2].Hmac, marker.Hmac, "the marker's digest is its own content digest")

		t1 := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias}, TurnID: uuid.New(), Items: truncated.Items}
		require.NoError(t, appendTurnTx(context.Background(), f.tx, &t1))

		// Turn 2 diverges (prefix compaction is valid): the request fits and
		// "c" is a real item again.
		full := NormalizeRequest(string(types.RelayFormatOpenAI), c, NormalizeOptions{Reservation: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey})
		require.Equal(t, ContentStateFull, full.ContentState)
		t2 := ContentInput{NodeScope: f.node, UserID: f.user, Aliases: []Alias{f.alias}, TurnID: uuid.New(), Items: full.Items}
		require.NoError(t, appendTurnTx(context.Background(), f.tx, &t2))

		got, err := reconstructTurnQ(context.Background(), f.tx, f.sessionID(), t2.TurnID, testHMACKey)
		require.NoError(t, err)
		require.Len(t, got.Items, 3)
		last := got.Items[len(got.Items)-1]
		require.Equal(t, CanonicalKindMessage, last.Kind, "a real item is never replaced by a gap marker")
		require.NotEmpty(t, last.Content)
		assert.Equal(t, "message-2-"+strings.Repeat("x", 200), last.Content[0].Text)
	})
}

// TestLookupAliasRejectsCorruptBinding locks the binding sanity check: an
// alias row whose session id is not a UUID is a corrupt binding, never a
// silently adopted session.
func TestLookupAliasRejectsCorruptBinding(t *testing.T) {
	fx := newFakeContentTx()
	raw, err := itemDigestBytes(strings.Repeat("e", 64))
	require.NoError(t, err)
	fx.aliases[aliasKey("n", 7, 1, raw, "codex_cli")] = "not-a-uuid"
	alias := Alias{Version: 1, Digest: strings.Repeat("e", 64), Scope: ScopeCodexCLI}
	_, err = lookupAliasSessionTx(context.Background(), fx, "n", 7, alias)
	require.Error(t, err)
	_, ok := ContentErrorOf(err)
	assert.False(t, ok, "a corrupt binding is a data error, not a content classification")
}
