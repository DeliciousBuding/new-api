package relayobserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the T3.1 bounded Root query port without a database. The
// list and overview orchestration runs against fakeQueryDB, an in-memory
// stand-in for the contentQuerier seam that simulates the PostgreSQL keyset
// semantics (strict tuple ordering, LIMIT) and records every SQL statement it
// sees, exactly like the dispatcher and the T2.3 append are tested against
// their ports. The SQL itself is exercised by the
// relay_observer_pg_integration suite.

// ---------------------------------------------------------------------------
// fake data layer

type fakeSessionRow struct {
	id           string
	nodeScope    string
	userID       int64
	clientFamily string
	firstSeen    time.Time
	lastSeen     time.Time
	turnCount    int64
	gapCount     int64
}

type fakeTurnRow struct {
	id               string
	nodeScope        string
	eventID          string
	sessionID        *string
	occurredAt       time.Time
	userID           int64
	tokenID          int64
	clientProfile    string
	model            string
	upstreamModel    string
	relayFormat      string
	success          bool
	statusCode       int
	errorType        string
	errorCode        string
	latencyMS        int64
	firstResponseMS  int64
	stream           bool
	promptTokens     int64
	completionTokens int64
	cachedTokens     int64
	quota            int64
	attempts         []byte
	attemptsOmitted  int
	contentState     string
}

type fakeContextRow struct {
	id           int64
	sessionID    string
	turnID       string
	checkpointID int64
	ordinal      int
	prefix       int
	itemCount    int
	digests      []string
	logicalBytes int64
}

// fakeQueryDB implements contentQuerier over in-memory rows. It routes by the
// table the SQL reads, simulates the keyset tuple ordering and LIMIT, and
// records every SQL statement for shape assertions.
type fakeQueryDB struct {
	mu       sync.Mutex
	sessions []fakeSessionRow // ordered last_seen DESC, id DESC
	turns    []fakeTurnRow    // ordered occurred_at DESC, id DESC
	contexts []fakeContextRow
	objects  map[string]contentObjectRow // digest hex -> stored object
	sqls     []string
	err      error // forced failure for every query
	block    chan struct{}
	scanErr  error // forced failure when scanning any result row
	rowsErr  error // forced failure returned by rows.Err()
}

func newFakeQueryDB() *fakeQueryDB {
	return &fakeQueryDB{objects: map[string]contentObjectRow{}}
}

func (f *fakeQueryDB) QueryRow(ctx context.Context, query string, args ...any) rowScanner {
	f.mu.Lock()
	f.sqls = append(f.sqls, query)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-ctx.Done():
			return &fakeQueryRow{err: ctx.Err()}
		case <-f.block:
		}
	}
	if f.err != nil {
		return &fakeQueryRow{err: f.err}
	}
	switch {
	case strings.Contains(query, "FROM observer_sessions") && strings.Contains(query, "count(*)"):
		return &fakeQueryRow{values: []any{int64(len(f.sessions))}}
	case strings.Contains(query, "FROM observer_turns") && strings.Contains(query, "content_state"):
		var n int64
		for _, tr := range f.turns {
			if tr.contentState == "gap" || tr.contentState == "metadata_only" {
				n++
			}
		}
		return &fakeQueryRow{values: []any{n}}
	case strings.Contains(query, "FROM observer_turns") && strings.Contains(query, "count(*)"):
		return &fakeQueryRow{values: []any{int64(len(f.turns))}}
	case strings.Contains(query, "FROM observer_contexts WHERE session_id"):
		sid := args[0].(string)
		turn := args[1].(string)
		for _, c := range f.contexts {
			if c.sessionID == sid && c.turnID == turn {
				raw, err := common.Marshal(c.digests)
				if err != nil {
					return &fakeQueryRow{err: err}
				}
				return &fakeQueryRow{values: []any{c.id, c.checkpointID, c.ordinal, c.prefix, c.itemCount, raw, c.logicalBytes}}
			}
		}
		return &fakeQueryRow{err: sql.ErrNoRows}
	}
	return &fakeQueryRow{err: fmt.Errorf("fake: unhandled query row %q", query)}
}

func (f *fakeQueryDB) Query(ctx context.Context, query string, args ...any) (rowIter, error) {
	f.mu.Lock()
	f.sqls = append(f.sqls, query)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.block:
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case strings.Contains(query, "FROM observer_sessions"):
		rows := f.sessionRows(args)
		return &fakeQueryRows{rows: rows, scanErr: f.scanErr, rowsErr: f.rowsErr}, nil
	case strings.Contains(query, "extract(epoch"):
		rows := f.windowRows(args)
		return &fakeQueryRows{rows: rows, scanErr: f.scanErr, rowsErr: f.rowsErr}, nil
	case strings.Contains(query, "FROM observer_turns"):
		rows := f.turnRows(args)
		return &fakeQueryRows{rows: rows, scanErr: f.scanErr, rowsErr: f.rowsErr}, nil
	case strings.Contains(query, "FROM observer_content_objects"):
		digests := args[1].([][]byte)
		var rows []*fakeQueryRow
		for _, d := range digests {
			obj, ok := f.objects[hex.EncodeToString(d)]
			if ok {
				rows = append(rows, &fakeQueryRow{values: []any{d, obj.payload, obj.logicalBytes}})
			}
		}
		return &fakeQueryRows{rows: rows, scanErr: f.scanErr, rowsErr: f.rowsErr}, nil
	}
	return nil, fmt.Errorf("fake: unhandled query %q", query)
}

func (f *fakeQueryDB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return nil, fmt.Errorf("fake: query seam never executes writes: %q", query)
}

// keysetArgs decodes the cursor tuple and limit the seam passes. A cursor is
// always the trailing (time.Time, string, int) tuple; filters never end in a
// string immediately after a time value, so the type pattern is unambiguous.
func keysetArgs(args []any) (time.Time, string, int) {
	if len(args) >= 3 {
		if _, ok := args[len(args)-3].(time.Time); ok {
			if _, ok := args[len(args)-2].(string); ok {
				return args[len(args)-3].(time.Time), args[len(args)-2].(string), args[len(args)-1].(int)
			}
		}
	}
	return time.Time{}, "", args[len(args)-1].(int)
}

// rowsAfterCursor returns the rows strictly after (at, id) in an already
// DESC-ordered slice, bounded by limit. A zero at means the first page.
func rowsAfterCursor[T any](rows []T, at time.Time, id string, limit int, cmp func(T) (time.Time, string)) []T {
	idx := 0
	if !at.IsZero() {
		idx = len(rows)
		for i, r := range rows {
			t, rID := cmp(r)
			if t.Before(at) || (t.Equal(at) && rID < id) {
				idx = i
				break
			}
		}
	}
	end := idx + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[idx:end]
}

func (f *fakeQueryDB) sessionRows(args []any) []*fakeQueryRow {
	at, id, limit := keysetArgs(args)
	rows := rowsAfterCursor(f.sessions, at, id, limit, func(r fakeSessionRow) (time.Time, string) {
		return r.lastSeen, r.id
	})
	out := make([]*fakeQueryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, &fakeQueryRow{values: []any{
			r.id, r.nodeScope, r.userID, r.clientFamily,
			r.firstSeen, r.lastSeen, r.turnCount, r.gapCount,
		}})
	}
	return out
}

func (f *fakeQueryDB) turnRows(args []any) []*fakeQueryRow {
	at, id, limit := keysetArgs(args)
	rows := rowsAfterCursor(f.turns, at, id, limit, func(r fakeTurnRow) (time.Time, string) {
		return r.occurredAt, r.id
	})
	out := make([]*fakeQueryRow, 0, len(rows))
	for _, r := range rows {
		var sid any
		if r.sessionID != nil {
			sid = *r.sessionID
		} else {
			sid = sql.NullString{}
		}
		out = append(out, &fakeQueryRow{values: []any{
			r.id, r.nodeScope, r.eventID, sid, r.occurredAt,
			r.userID, r.tokenID, r.clientProfile, r.model, r.upstreamModel, r.relayFormat,
			r.success, r.statusCode, r.errorType, r.errorCode,
			r.latencyMS, r.firstResponseMS, r.stream,
			r.promptTokens, r.completionTokens, r.cachedTokens, r.quota,
			r.attempts, r.attemptsOmitted, r.contentState,
		}})
	}
	return out
}

func (f *fakeQueryDB) windowRows(args []any) []*fakeQueryRow {
	winSec := args[0].(float64)
	start := args[1].(time.Time)
	bucket := func(t time.Time) int64 {
		return int64(math.Floor(float64(t.Unix()) / winSec))
	}
	counts := map[int64][2]int64{}
	for _, tr := range f.turns {
		if tr.occurredAt.Before(start) {
			continue
		}
		c := counts[bucket(tr.occurredAt)]
		c[0]++
		if tr.success {
			c[1]++
		}
		counts[bucket(tr.occurredAt)] = c
	}
	buckets := make([]int64, 0, len(counts))
	for b := range counts {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
	out := make([]*fakeQueryRow, 0, len(buckets))
	for _, b := range buckets {
		c := counts[b]
		out = append(out, &fakeQueryRow{values: []any{b, c[0], c[1]}})
	}
	return out
}

// fakeQueryRow is the single-row result of the fake data layer; fakeQueryRows
// is the multi-row result.
type fakeQueryRow struct {
	values []any
	err    error
}

func (r *fakeQueryRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if err := queryScanInto(d, r.values[i]); err != nil {
			return fmt.Errorf("fake query scan col %d: %w", i, err)
		}
	}
	return nil
}

type fakeQueryRows struct {
	rows    []*fakeQueryRow
	idx     int
	scanErr error
	rowsErr error
}

func (r *fakeQueryRows) Next() bool {
	r.idx++
	return r.idx <= len(r.rows)
}

func (r *fakeQueryRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return r.rows[r.idx-1].Scan(dest...)
}

func (r *fakeQueryRows) Err() error { return r.rowsErr }

// Close satisfies the optional closeRows seam exactly like *sql.Rows.
func (r *fakeQueryRows) Close() error { return nil }

func queryScanInto(dest any, val any) error {
	switch d := dest.(type) {
	case *time.Time:
		v, ok := val.(time.Time)
		if !ok {
			return fmt.Errorf("fake query scan: want time.Time, got %T", val)
		}
		*d = v
	case *sql.NullString:
		switch v := val.(type) {
		case sql.NullString:
			*d = v
		case string:
			d.String, d.Valid = v, true
		default:
			return fmt.Errorf("fake query scan: want string, got %T", val)
		}
	case *sql.NullInt64:
		switch v := val.(type) {
		case sql.NullInt64:
			*d = v
		case int64:
			d.Int64, d.Valid = v, true
		case int:
			d.Int64, d.Valid = int64(v), true
		default:
			return fmt.Errorf("fake query scan: want int64, got %T", val)
		}
	case *bool:
		v, ok := val.(bool)
		if !ok {
			return fmt.Errorf("fake query scan: want bool, got %T", val)
		}
		*d = v
	case *int:
		v, ok := val.(int)
		if !ok {
			return fmt.Errorf("fake query scan: want int, got %T", val)
		}
		*d = v
	case *int64:
		v, ok := val.(int64)
		if !ok {
			return fmt.Errorf("fake query scan: want int64, got %T", val)
		}
		*d = v
	case *string:
		v, ok := val.(string)
		if !ok {
			return fmt.Errorf("fake query scan: want string, got %T", val)
		}
		*d = v
	case *[]byte:
		v, ok := val.([]byte)
		if !ok {
			return fmt.Errorf("fake query scan: want []byte, got %T", val)
		}
		*d = v
	default:
		return fmt.Errorf("fake query scan: unsupported destination %T", dest)
	}
	return nil
}

// ---------------------------------------------------------------------------
// fixture builders

// sessionAt builds the i-th session row; larger i is later, so a DESC-ordered
// slice is rows[0] = newest. The uuid text form keeps numeric order.
func sessionAt(i int) fakeSessionRow {
	return fakeSessionRow{
		id:           fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1),
		nodeScope:    "node-a",
		userID:       7,
		clientFamily: "codex",
		firstSeen:    epoch.Add(time.Duration(i) * time.Minute),
		lastSeen:     epoch.Add(time.Duration(i) * time.Minute),
		turnCount:    int64(i + 1),
		gapCount:     int64(i % 3),
	}
}

func turnAt(i int) fakeTurnRow {
	occ := epoch.Add(time.Duration(i) * time.Second)
	return fakeTurnRow{
		id:               fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1),
		nodeScope:        "node-a",
		eventID:          fmt.Sprintf("req-%d", i+1),
		occurredAt:       occ,
		userID:           7,
		tokenID:          42,
		clientProfile:    "codex",
		model:            "gpt-5",
		upstreamModel:    "gpt-5",
		relayFormat:      "responses",
		success:          i%2 == 0,
		statusCode:       200,
		latencyMS:        int64(100 + i),
		firstResponseMS:  30,
		stream:           true,
		promptTokens:     10,
		completionTokens: 20,
		cachedTokens:     5,
		quota:            3000,
		attempts:         []byte(`[{"ChannelID":1,"Group":"default","StatusCode":200,"ErrorCode":"","ElapsedMS":5}]`),
		attemptsOmitted:  0,
		contentState:     ContentStateFull,
	}
}

func fillSessions(f *fakeQueryDB, n int) {
	for i := n - 1; i >= 0; i-- {
		f.sessions = append(f.sessions, sessionAt(i))
	}
}

func fillTurns(f *fakeQueryDB, n int) {
	for i := n - 1; i >= 0; i-- {
		f.turns = append(f.turns, turnAt(i))
	}
}

// queryErrKind extracts the QueryError classification of err, failing the
// test when err is not a QueryError.
func queryErrKind(t *testing.T, err error) QueryErrorKind {
	t.Helper()
	var qe *QueryError
	require.ErrorAs(t, err, &qe)
	return qe.Kind
}

// ---------------------------------------------------------------------------
// cursor encode/decode

func TestQueryCursorRoundTrip(t *testing.T) {
	at := epoch.Add(123*time.Second + 456*time.Nanosecond)
	id := uuid.MustParse("00000000-0000-0000-0000-000000000abc")
	c, err := encodeKeysetCursor(at, id)
	require.NoError(t, err)
	gotAt, gotID, err := decodeKeysetCursor(c)
	require.NoError(t, err)
	assert.Equal(t, at, gotAt, "cursor time must round-trip at nanosecond precision")
	assert.Equal(t, id, gotID)

	// Determinism: the same keyset encodes to the exact same cursor.
	again, err := encodeKeysetCursor(at, id)
	require.NoError(t, err)
	assert.Equal(t, c, again, "cursor encoding must be deterministic")
}

func TestQueryCursorMalformed(t *testing.T) {
	// base64 payloads that decode but carry no valid keyset.
	badJSON, err := common.Marshal(map[string]any{"nope": 1})
	require.NoError(t, err)
	missingID, err := common.Marshal(map[string]any{"at": 123})
	require.NoError(t, err)
	badAtType, err := common.Marshal(map[string]any{"at": "soon", "id": "00000000-0000-0000-0000-000000000abc"})
	require.NoError(t, err)
	badID, err := common.Marshal(map[string]any{"at": 123, "id": "not-a-uuid"})
	require.NoError(t, err)

	// base64url of some raw bytes that is not JSON at all.
	notJSON := base64RawEncode([]byte{0xde, 0xad})

	cases := []string{
		"%%%not-base64%%%",
		"e30", // base64url of "{}": decodes but has no id
		string(badJSON),
		string(missingID),
		string(badAtType),
		string(badID),
		notJSON,
	}
	for _, c := range cases {
		_, _, err := decodeKeysetCursor(c)
		require.Error(t, err, "cursor %q must be rejected", c)
		assert.Equal(t, QueryErrMalformedCursor, queryErrKind(t, err), "cursor %q", c)
	}
}

func base64RawEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// page size clamp

func TestQueryClampPageSize(t *testing.T) {
	assert.Equal(t, DefaultPageSize, clampPageSize(0))
	assert.Equal(t, DefaultPageSize, clampPageSize(-5))
	assert.Equal(t, 1, clampPageSize(1))
	assert.Equal(t, 50, clampPageSize(50))
	assert.Equal(t, MaxPageSize, clampPageSize(100))
	assert.Equal(t, MaxPageSize, clampPageSize(1000))
	assert.Equal(t, MaxPageSize, clampPageSize(1<<30))
}

// ---------------------------------------------------------------------------
// sessions list

func TestListSessionsEmpty(t *testing.T) {
	f := newFakeQueryDB()
	page, err := listSessionsQ(context.Background(), f, SessionQuery{})
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	assert.False(t, page.Meta.HasMore)
	assert.Empty(t, page.Meta.NextCursor)
}

func TestListSessionsSinglePage(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 5)
	page, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 5)
	assert.False(t, page.Meta.HasMore)
	assert.Empty(t, page.Meta.NextCursor)
	assert.Equal(t, "00000000-0000-0000-0000-000000000005", page.Items[0].SessionID.String(), "newest first")
	assert.Equal(t, int64(5), page.Items[0].TurnCount)
}

func TestListSessionsMultiPage(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 250)

	page, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 100})
	require.NoError(t, err)
	require.Len(t, page.Items, 100)
	assert.True(t, page.Meta.HasMore)
	require.NotEmpty(t, page.Meta.NextCursor)
	lastOfPage1 := page.Items[99].SessionID

	page2, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 100, Cursor: page.Meta.NextCursor})
	require.NoError(t, err)
	require.Len(t, page2.Items, 100)
	assert.True(t, page2.Meta.HasMore)
	require.NotEmpty(t, page2.Meta.NextCursor)
	assert.NotEqual(t, lastOfPage1, page2.Items[0].SessionID, "pages must not overlap")

	page3, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 100, Cursor: page2.Meta.NextCursor})
	require.NoError(t, err)
	require.Len(t, page3.Items, 50)
	assert.False(t, page3.Meta.HasMore)
	assert.Empty(t, page3.Meta.NextCursor, "final page has no next cursor")

	// Every session appears exactly once across the three pages.
	seen := map[string]bool{}
	for _, p := range []SessionPage{page, page2, page3} {
		for _, it := range p.Items {
			assert.False(t, seen[it.SessionID.String()], "duplicate session %s across pages", it.SessionID)
			seen[it.SessionID.String()] = true
		}
	}
	assert.Len(t, seen, 250)
}

func TestListSessionsCursorStable(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 250)
	page1, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 100})
	require.NoError(t, err)
	page2, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 100, Cursor: page1.Meta.NextCursor})
	require.NoError(t, err)

	again, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 100, Cursor: page1.Meta.NextCursor})
	require.NoError(t, err)
	require.Len(t, again.Items, len(page2.Items))
	for i := range page2.Items {
		assert.Equal(t, page2.Items[i], again.Items[i], "the same cursor must deterministically return the same page")
	}
	assert.Equal(t, page2.Meta, again.Meta)
}

func TestListSessionsDefaultPageSize(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 120)
	page, err := listSessionsQ(context.Background(), f, SessionQuery{})
	require.NoError(t, err)
	require.Len(t, page.Items, DefaultPageSize)
	assert.True(t, page.Meta.HasMore)
}

func TestListSessionsPageClamped(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 250)
	page, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 1000})
	require.NoError(t, err)
	require.Len(t, page.Items, MaxPageSize, "page sizes above the hard cap must be clamped")
	assert.True(t, page.Meta.HasMore)
}

// ---------------------------------------------------------------------------
// turns list

func TestListTurnsEmpty(t *testing.T) {
	f := newFakeQueryDB()
	page, err := listTurnsQ(context.Background(), f, TurnQuery{})
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	assert.False(t, page.Meta.HasMore)
	assert.Empty(t, page.Meta.NextCursor)
}

func TestListTurnsSinglePage(t *testing.T) {
	f := newFakeQueryDB()
	fillTurns(f, 5)
	page, err := listTurnsQ(context.Background(), f, TurnQuery{PageSize: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 5)
	assert.False(t, page.Meta.HasMore)
	assert.Empty(t, page.Meta.NextCursor)
	first := page.Items[0]
	assert.Equal(t, "req-5", first.EventID, "newest turn first")
	assert.Equal(t, "gpt-5", first.Model)
	assert.True(t, first.Success)
	assert.Equal(t, int64(3000), first.Quota)
	assert.Equal(t, ContentStateFull, first.ContentState)
	require.Len(t, first.Attempts, 1)
	assert.Equal(t, int64(1), first.Attempts[0].ChannelID)
}

func TestListTurnsMultiPage(t *testing.T) {
	f := newFakeQueryDB()
	fillTurns(f, 205)

	page, err := listTurnsQ(context.Background(), f, TurnQuery{PageSize: 100})
	require.NoError(t, err)
	require.Len(t, page.Items, 100)
	assert.True(t, page.Meta.HasMore)

	page2, err := listTurnsQ(context.Background(), f, TurnQuery{PageSize: 100, Cursor: page.Meta.NextCursor})
	require.NoError(t, err)
	require.Len(t, page2.Items, 100)
	assert.True(t, page2.Meta.HasMore)

	page3, err := listTurnsQ(context.Background(), f, TurnQuery{PageSize: 100, Cursor: page2.Meta.NextCursor})
	require.NoError(t, err)
	require.Len(t, page3.Items, 5)
	assert.False(t, page3.Meta.HasMore)
	assert.Empty(t, page3.Meta.NextCursor)

	seen := map[string]bool{}
	for _, p := range []TurnPage{page, page2, page3} {
		for _, it := range p.Items {
			assert.False(t, seen[it.TurnID.String()], "duplicate turn %s across pages", it.TurnID)
			seen[it.TurnID.String()] = true
		}
	}
	assert.Len(t, seen, 205)
}

func TestListTurnsCursorStable(t *testing.T) {
	f := newFakeQueryDB()
	fillTurns(f, 205)
	page1, err := listTurnsQ(context.Background(), f, TurnQuery{PageSize: 100})
	require.NoError(t, err)
	page2, err := listTurnsQ(context.Background(), f, TurnQuery{PageSize: 100, Cursor: page1.Meta.NextCursor})
	require.NoError(t, err)

	again, err := listTurnsQ(context.Background(), f, TurnQuery{PageSize: 100, Cursor: page1.Meta.NextCursor})
	require.NoError(t, err)
	require.Len(t, again.Items, len(page2.Items))
	for i := range page2.Items {
		assert.Equal(t, page2.Items[i], again.Items[i])
	}
}

func TestListTurnsPageClamped(t *testing.T) {
	f := newFakeQueryDB()
	fillTurns(f, 250)
	page, err := listTurnsQ(context.Background(), f, TurnQuery{PageSize: 1 << 20})
	require.NoError(t, err)
	require.Len(t, page.Items, MaxPageSize)
	assert.True(t, page.Meta.HasMore)
}

// ---------------------------------------------------------------------------
// overview

func TestOverviewEmpty(t *testing.T) {
	f := newFakeQueryDB()
	out, err := overviewQ(context.Background(), f, OverviewQuery{})
	require.NoError(t, err)
	assert.Empty(t, out.Windows)
	assert.Zero(t, out.SessionCount)
	assert.Zero(t, out.TurnCount)
	assert.Zero(t, out.GapCount)
}

func TestOverviewWindows(t *testing.T) {
	f := newFakeQueryDB()
	// Turns spread over the last 110 seconds; a 3600s window bucket cannot
	// straddle a boundary, so the aggregate is deterministic.
	base := time.Now().Add(-110 * time.Second)
	for i := 0; i < 12; i++ {
		tr := turnAt(i)
		tr.occurredAt = base.Add(time.Duration(i) * 10 * time.Second)
		f.turns = append(f.turns, tr)
	}
	out, err := overviewQ(context.Background(), f, OverviewQuery{WindowSeconds: 3600, Windows: 12})
	require.NoError(t, err)
	assert.Equal(t, 3600, out.WindowSeconds)
	require.NotEmpty(t, out.Windows, "the window aggregate must see the fixture turns")
	var turns, success int64
	for _, w := range out.Windows {
		assert.Zero(t, w.Start.Unix()%3600, "window starts must align to the window span")
		turns += w.Turns
		success += w.Success
	}
	assert.Equal(t, int64(12), turns, "every fixture turn lands in exactly one window")
	assert.Equal(t, int64(6), success, "even turns succeed in the fixture")
}

func TestOverviewCounts(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 3)
	fillTurns(f, 10)
	f.turns[0].contentState = ContentStateGap
	f.turns[1].contentState = ContentStateMetadataOnly
	out, err := overviewQ(context.Background(), f, OverviewQuery{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), out.SessionCount)
	assert.Equal(t, int64(10), out.TurnCount)
	assert.Equal(t, int64(2), out.GapCount, "gap and metadata_only both count as gaps")
}

// ---------------------------------------------------------------------------
// lists never read content

func TestListQueriesNeverReadPayload(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 3)
	fillTurns(f, 5)

	_, err := listSessionsQ(context.Background(), f, SessionQuery{PageSize: 2})
	require.NoError(t, err)
	_, err = listTurnsQ(context.Background(), f, TurnQuery{PageSize: 2})
	require.NoError(t, err)
	_, err = overviewQ(context.Background(), f, OverviewQuery{})
	require.NoError(t, err)

	f.mu.Lock()
	sqls := append([]string(nil), f.sqls...)
	f.mu.Unlock()
	require.NotEmpty(t, sqls, "the seam must have issued SQL")
	for _, q := range sqls {
		assert.NotContains(t, q, "observer_content_objects", "list queries must never touch the content table: %s", q)
		assert.NotContains(t, q, "payload", "list queries must never select payload: %s", q)
	}
}

// ---------------------------------------------------------------------------
// turn context (bounded content read)

// contextItem is the fixture carrier of one stored content object.
func contextItem(t *testing.T, f *fakeQueryDB, sessionID, turnID string, i int) string {
	t.Helper()
	it := CanonicalItem{Kind: "text", Role: "user", LogicalBytes: 4, Hmac: fmt.Sprintf("%064x", i+1)}
	payload, logical, err := encodeItem(it)
	require.NoError(t, err)
	f.objects[it.Hmac] = contentObjectRow{payload: payload, logicalBytes: logical}
	f.contexts = append(f.contexts, fakeContextRow{
		id:           int64(i + 1),
		sessionID:    sessionID,
		turnID:       turnID,
		checkpointID: int64(i + 1),
		ordinal:      0,
		itemCount:    i + 1,
		digests:      nil, // patched by the caller
		logicalBytes: logical,
	})
	return it.Hmac
}

func TestTurnContextBounded(t *testing.T) {
	f := newFakeQueryDB()
	sid := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	turn := uuid.MustParse("00000000-0000-0000-0000-0000000000bb")
	hmacs := []string{
		contextItem(t, f, sid.String(), turn.String(), 0),
		contextItem(t, f, sid.String(), turn.String(), 1),
	}
	f.contexts[0].digests = hmacs
	f.contexts[0].itemCount = len(hmacs)

	out, err := turnContextQ(context.Background(), f, ContextQuery{SessionID: sid, TurnID: turn})
	require.NoError(t, err)
	assert.Equal(t, turn, out.TurnID)
	assert.Equal(t, 0, out.Ordinal)
	require.Len(t, out.Items, 2)
	assert.Equal(t, hmacs[1], out.Items[1].Hmac)

	f.mu.Lock()
	defer f.mu.Unlock()
	var contentReads int
	for _, q := range f.sqls {
		if strings.Contains(q, "FROM observer_content_objects") {
			contentReads++
		}
	}
	assert.Equal(t, 1, contentReads, "a turn context must read content objects exactly once")
}

func TestTurnContextMissingContext(t *testing.T) {
	f := newFakeQueryDB()
	sid := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	turn := uuid.MustParse("00000000-0000-0000-0000-0000000000bb")
	_, err := turnContextQ(context.Background(), f, ContextQuery{SessionID: sid, TurnID: turn})
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok, "a missing context must surface the classified content error")
	assert.Equal(t, ContentErrMissingContext, code)
}

// ---------------------------------------------------------------------------
// timeout and degraded paths

func TestQueryExpiredContext(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := listSessionsQ(ctx, f, SessionQuery{PageSize: 2})
	require.Error(t, err)
	assert.Equal(t, QueryErrTimeout, queryErrKind(t, err), "a canceled context must classify as timeout")
}

func TestQueryDeadlineContext(t *testing.T) {
	f := newFakeQueryDB()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := listTurnsQ(ctx, f, TurnQuery{PageSize: 2})
	require.Error(t, err)
	assert.Equal(t, QueryErrTimeout, queryErrKind(t, err))
}

func TestQueryStoreFailurePropagates(t *testing.T) {
	f := newFakeQueryDB()
	f.err = fmt.Errorf("relayobserver: simulate store failure")
	_, err := listSessionsQ(context.Background(), f, SessionQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulate store failure", "store errors must surface to the caller unchanged")
}

// ---------------------------------------------------------------------------
// query semaphore

func TestAcquireQuerySlot(t *testing.T) {
	sem := make(chan struct{}, querySlotCapacity)

	require.NoError(t, acquireQuerySlot(context.Background(), sem))
	require.Len(t, sem, 1, "first query holds the only slot")

	// The second query waits on the slot; with a short deadline it must fail
	// with the timeout classification instead of queueing unbounded.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := acquireQuerySlot(ctx, sem)
	require.Error(t, err)
	assert.Equal(t, QueryErrTimeout, queryErrKind(t, err))

	releaseQuerySlot(sem)
	require.NoError(t, acquireQuerySlot(context.Background(), sem), "the slot is available again after release")
	releaseQuerySlot(sem)
}

func TestQueryStoreWithSlot(t *testing.T) {
	store := &pgQueryStore{store: &pgStore{}, sem: make(chan struct{}, querySlotCapacity)}

	ran := false
	err := store.withSlot(context.Background(), func() error {
		ran = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, ran, "an idle slot must admit the query")
	require.Empty(t, store.sem, "the slot must be released after the query")

	// Hold the slot, then the next query times out rather than queueing.
	require.NoError(t, acquireQuerySlot(context.Background(), store.sem))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = store.withSlot(ctx, func() error {
		t.Fatal("the gated query must not run while the slot is held")
		return nil
	})
	require.Error(t, err)
	assert.Equal(t, QueryErrTimeout, queryErrKind(t, err))
}

func TestNewQueryStoreRejectsNonPGStore(t *testing.T) {
	_, err := NewQueryStore(&fakeStore{})
	require.Error(t, err, "the query port needs the PostgreSQL adapter's dedicated pool")
}

// ---------------------------------------------------------------------------
// error branches of the row and scan paths

func TestQueryErrorUnwrapChain(t *testing.T) {
	cause := context.Canceled
	err := classifiedQueryError(QueryErrTimeout, "query slot busy", cause)
	require.ErrorIs(t, err, cause, "QueryError must unwrap its cause")
	assert.Contains(t, err.Error(), "query timeout")
	assert.Contains(t, err.Error(), "query slot busy")
	// The message must carry the classification, never raw SQL or secrets.
	assert.NotContains(t, err.Error(), "SELECT")
}

func TestNewQueryStoreAcceptsPGAdapter(t *testing.T) {
	qs, err := NewQueryStore(&pgStore{})
	require.NoError(t, err)
	_, ok := qs.(*pgQueryStore)
	assert.True(t, ok, "the PostgreSQL adapter must wrap into the gated query store")
}

func TestListSessionsScanError(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 3)
	f.scanErr = fmt.Errorf("relayobserver: simulate scan failure")
	_, err := listSessionsQ(context.Background(), f, SessionQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scan")
}

func TestListSessionsRowsError(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 3)
	f.rowsErr = fmt.Errorf("relayobserver: simulate row iteration failure")
	_, err := listSessionsQ(context.Background(), f, SessionQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row")
}

func TestListSessionsInvalidRowID(t *testing.T) {
	f := newFakeQueryDB()
	row := sessionAt(0)
	row.id = "not-a-uuid"
	f.sessions = []fakeSessionRow{row}
	_, err := listSessionsQ(context.Background(), f, SessionQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid session id")
}

func TestListTurnsScanError(t *testing.T) {
	f := newFakeQueryDB()
	fillTurns(f, 3)
	f.scanErr = fmt.Errorf("relayobserver: simulate turn scan failure")
	_, err := listTurnsQ(context.Background(), f, TurnQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scan")
}

func TestListTurnsRowsError(t *testing.T) {
	f := newFakeQueryDB()
	fillTurns(f, 3)
	f.rowsErr = fmt.Errorf("relayobserver: simulate turn iteration failure")
	_, err := listTurnsQ(context.Background(), f, TurnQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query turns")
}

func TestListTurnsInvalidRowIDs(t *testing.T) {
	f := newFakeQueryDB()
	bad := turnAt(0)
	bad.id = "not-a-uuid"
	f.turns = []fakeTurnRow{bad}
	_, err := listTurnsQ(context.Background(), f, TurnQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid turn id")

	f = newFakeQueryDB()
	badSid := "not-a-uuid"
	row := turnAt(0)
	row.sessionID = &badSid
	f.turns = []fakeTurnRow{row}
	_, err = listTurnsQ(context.Background(), f, TurnQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid session id")
}

func TestListTurnsBadAttemptsJSON(t *testing.T) {
	f := newFakeQueryDB()
	row := turnAt(0)
	row.attempts = []byte("not-json")
	f.turns = []fakeTurnRow{row}
	_, err := listTurnsQ(context.Background(), f, TurnQuery{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode attempts")
}

func TestOverviewScanError(t *testing.T) {
	f := newFakeQueryDB()
	base := time.Now().Add(-60 * time.Second)
	tr := turnAt(0)
	tr.occurredAt = base
	f.turns = []fakeTurnRow{tr}
	f.scanErr = fmt.Errorf("relayobserver: simulate window scan failure")
	_, err := overviewQ(context.Background(), f, OverviewQuery{WindowSeconds: 3600, Windows: 12})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scan")
}

// ---------------------------------------------------------------------------
// bounded filters reach the SQL

func TestListSessionsFilterArgs(t *testing.T) {
	f := newFakeQueryDB()
	fillSessions(f, 5)
	from := epoch.Add(time.Minute)
	to := epoch.Add(3 * time.Minute)
	_, err := listSessionsQ(context.Background(), f, SessionQuery{
		NodeScope: "node-b",
		UserID:    9,
		From:      from,
		To:        to,
		PageSize:  2,
	})
	require.NoError(t, err)
	// The session list has exactly four filter conditions; the fifth argument
	// is the LIMIT backstop.
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.sqls, 1)
	sqlText := f.sqls[0]
	assert.Contains(t, sqlText, "node_scope = $1")
	assert.Contains(t, sqlText, "user_id = $2")
	assert.Contains(t, sqlText, "last_seen >= $3")
	assert.Contains(t, sqlText, "last_seen < $4")
	assert.Contains(t, sqlText, "LIMIT $5")
}

func TestListTurnsFilterArgs(t *testing.T) {
	f := newFakeQueryDB()
	fillTurns(f, 5)
	sid := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	_, err := listTurnsQ(context.Background(), f, TurnQuery{SessionID: &sid, UserID: 9, PageSize: 2})
	require.NoError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.sqls, 1)
	sqlText := f.sqls[0]
	assert.Contains(t, sqlText, "session_id = $1")
	assert.Contains(t, sqlText, "user_id = $2")
	assert.Contains(t, sqlText, "LIMIT $3")
}
