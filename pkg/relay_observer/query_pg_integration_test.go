//go:build relay_observer_pg_integration

package relayobserver

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file exercises the T3.1 bounded Root query port against the real
// disposable PostgreSQL (the same TEST_RELAY_OBSERVER_POSTGRES_DSN guard and
// schema helpers as store_pg_integration_test.go). The fake-data-layer tests
// prove the orchestration decisions; this suite proves the SQL itself:
// keyset tuple ordering, LIMIT backstop, cursor stability, overview
// aggregation, and the bounded content read.

// insertQuerySession writes one observer_sessions row directly.
func insertQuerySession(t *testing.T, db *sql.DB, id uuid.UUID, scope string, lastSeen time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO observer_sessions (id, node_scope, user_id, client_family, first_seen, last_seen, turn_count, gap_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id.String(), scope, 7, "codex", lastSeen.Add(-time.Hour), lastSeen, 3, 1)
	require.NoError(t, err)
}

// queryTurnEvent builds the i-th turn event of a fixture with a distinct
// occurrence time and event id.
func queryTurnEvent(i int, scope string) Event {
	ev := sampleEvent()
	ev.NodeScope = scope
	ev.EventID = fmt.Sprintf("qreq-%d", i+1)
	ev.OccurredAt = epoch.Add(time.Duration(i) * time.Millisecond)
	return ev
}

// resetObserverSchema drops the observer tables and bootstraps the empty v1
// schema so a test sees exactly the rows it writes.
func resetObserverSchema(t *testing.T, dsn string) Store {
	t.Helper()
	db := openFixturePool(t, dsn)
	cleanupObserverSchema(t, db)
	store, err := OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err)
	return store
}

// TestIntegrationQueryKeysetSessions covers the session list on the real
// database: newest-first ordering, keyset cursor advance, and cursor
// stability (the same cursor deterministically returns the same page).
func TestIntegrationQueryKeysetSessions(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	db := openFixturePool(t, dsn)
	scope := "t13-query-sess"
	ids := []uuid.UUID{
		uuid.MustParse("10000000-0000-0000-0000-000000000001"),
		uuid.MustParse("10000000-0000-0000-0000-000000000002"),
		uuid.MustParse("10000000-0000-0000-0000-000000000003"),
	}
	for i, id := range ids {
		insertQuerySession(t, db, id, scope, epoch.Add(time.Duration(i)*time.Minute))
	}

	qs, err := NewQueryStore(store)
	require.NoError(t, err)

	page, err := qs.ListSessions(context.Background(), SessionQuery{PageSize: 2})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	assert.Equal(t, ids[2], page.Items[0].SessionID, "newest session first")
	assert.Equal(t, ids[1], page.Items[1].SessionID)
	assert.True(t, page.Meta.HasMore)
	require.NotEmpty(t, page.Meta.NextCursor)

	page2, err := qs.ListSessions(context.Background(), SessionQuery{PageSize: 2, Cursor: page.Meta.NextCursor})
	require.NoError(t, err)
	require.Len(t, page2.Items, 1)
	assert.Equal(t, ids[0], page2.Items[0].SessionID)
	assert.False(t, page2.Meta.HasMore)
	assert.Empty(t, page2.Meta.NextCursor)

	// The same cursor deterministically returns the same page.
	again, err := qs.ListSessions(context.Background(), SessionQuery{PageSize: 2, Cursor: page.Meta.NextCursor})
	require.NoError(t, err)
	require.Len(t, again.Items, 1)
	for i := range again.Items {
		assert.Equal(t, page2.Items[i], again.Items[i], "the same cursor must deterministically return the same page")
	}
}

// TestIntegrationQueryKeysetTurns covers the turn list on the real database:
// 205 turns page as 100/100/5 with no overlap and no loss, and the final page
// carries no next cursor.
func TestIntegrationQueryKeysetTurns(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	scope := "t13-query-turns"
	events := make([]Event, 205)
	for i := range events {
		events[i] = queryTurnEvent(i, scope)
	}
	require.NoError(t, store.WriteBatch(context.Background(), events))

	qs, err := NewQueryStore(store)
	require.NoError(t, err)

	seen := map[string]bool{}
	cursor := ""
	total := 0
	pages := 0
	for {
		page, err := qs.ListTurns(context.Background(), TurnQuery{PageSize: 100, Cursor: cursor})
		require.NoError(t, err)
		pages++
		for _, it := range page.Items {
			assert.False(t, seen[it.TurnID.String()], "duplicate turn across pages")
			seen[it.TurnID.String()] = true
			total++
		}
		if !page.Meta.HasMore {
			assert.Empty(t, page.Meta.NextCursor, "final page has no next cursor")
			break
		}
		require.NotEmpty(t, page.Meta.NextCursor)
		cursor = page.Meta.NextCursor
	}
	assert.Equal(t, 3, pages, "205 turns at 100 per page must take three pages")
	assert.Equal(t, 205, total, "every turn appears exactly once across the pages")

	db := openFixturePool(t, dsn)
	var inDB int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM observer_turns WHERE node_scope = $1", scope).Scan(&inDB))
	assert.Equal(t, 205, inDB)
}

// TestIntegrationQueryPageSizeClamp covers the hard page cap on the real
// database: a page size above 100 must never return more than 100 rows.
func TestIntegrationQueryPageSizeClamp(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	scope := "t13-query-clamp"
	events := make([]Event, 150)
	for i := range events {
		events[i] = queryTurnEvent(i, scope)
	}
	require.NoError(t, store.WriteBatch(context.Background(), events))

	qs, err := NewQueryStore(store)
	require.NoError(t, err)

	page, err := qs.ListTurns(context.Background(), TurnQuery{PageSize: 1000})
	require.NoError(t, err)
	require.Len(t, page.Items, MaxPageSize, "a page size above the hard cap must be clamped to 100")
	assert.True(t, page.Meta.HasMore)
}

// TestIntegrationQueryMalformedCursor covers the malformed-cursor
// classification on the real database: the store must reject it before any
// SQL runs.
func TestIntegrationQueryMalformedCursor(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	qs, err := NewQueryStore(store)
	require.NoError(t, err)

	for _, c := range []string{"%%%not-base64%%%", "e30", "not-a-keyset"} {
		_, err := qs.ListSessions(context.Background(), SessionQuery{Cursor: c})
		require.Error(t, err, "cursor %q must be rejected", c)
		var qe *QueryError
		require.ErrorAs(t, err, &qe, "cursor %q", c)
		assert.Equal(t, QueryErrMalformedCursor, qe.Kind, "cursor %q", c)
	}
}

// TestIntegrationQueryTimeout covers the timeout classification on the real
// database: an expired context fails before touching the pool and classifies
// as timeout.
func TestIntegrationQueryTimeout(t *testing.T) {
	dsn := integrationDSN(t)
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	defer store.Close(context.Background())

	qs, err := NewQueryStore(store)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = qs.ListTurns(ctx, TurnQuery{PageSize: 10})
	require.Error(t, err)
	var qe *QueryError
	require.ErrorAs(t, err, &qe)
	assert.Equal(t, QueryErrTimeout, qe.Kind)
}

// TestIntegrationQueryOverview covers the bounded aggregate windows on the
// real database: window alignment, per-window turn/success counts, and the
// session/turn/gap totals. It runs on a freshly bootstrapped schema so the
// totals are exact.
func TestIntegrationQueryOverview(t *testing.T) {
	dsn := integrationDSN(t)
	db := openFixturePool(t, dsn)
	cleanupObserverSchema(t, db)
	store, err := OpenPGStore(context.Background(), dsn, SchemaModeBootstrap)
	require.NoError(t, err)
	defer store.Close(context.Background())

	scope := "t13-query-overview"
	now := time.Now()
	events := make([]Event, 6)
	for i := range events {
		events[i] = sampleEvent()
		events[i].NodeScope = scope
		events[i].EventID = fmt.Sprintf("ov-%d", i+1)
		events[i].OccurredAt = now.Add(-time.Duration(6-i) * 10 * time.Second)
	}
	events[0].ContentState = ContentStateGap
	events[1].ContentState = ContentStateMetadataOnly
	require.NoError(t, store.WriteBatch(context.Background(), events))
	insertQuerySession(t, db, uuid.MustParse("20000000-0000-0000-0000-000000000001"), scope, now)
	insertQuerySession(t, db, uuid.MustParse("20000000-0000-0000-0000-000000000002"), scope, now)

	qs, err := NewQueryStore(store)
	require.NoError(t, err)
	out, err := qs.Overview(context.Background(), OverviewQuery{WindowSeconds: 3600, Windows: 12})
	require.NoError(t, err)
	assert.Equal(t, 3600, out.WindowSeconds)
	assert.Equal(t, int64(2), out.SessionCount)
	assert.Equal(t, int64(6), out.TurnCount)
	assert.Equal(t, int64(2), out.GapCount, "gap and metadata_only both count as gaps")

	var turns, success int64
	require.NotEmpty(t, out.Windows)
	for _, w := range out.Windows {
		assert.Zero(t, w.Start.Unix()%3600, "window starts must align to the window span")
		turns += w.Turns
		success += w.Success
	}
	assert.Equal(t, int64(6), turns, "every fixture turn lands in exactly one window")
	assert.Equal(t, int64(6), success, "all fixture events succeed in the sample fixture")
}

// TestIntegrationQueryTurnContext covers the bounded content read on the real
// database: an appended turn reconstructs through the query port with its
// full items, and a missing context surfaces the classified content error.
func TestIntegrationQueryTurnContext(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	scope := "t13-query-ctx"
	turnID := uuid.New()
	hmac := strings.Repeat("cd", 32)
	in := ContentInput{
		NodeScope: scope,
		UserID:    7,
		Aliases:   []Alias{{Version: 1, Digest: strings.Repeat("ab", 32), Scope: SessionScope("codex"), Source: SourceHeaderSession}},
		TurnID:    turnID,
		Items: []CanonicalItem{
			{Kind: "text", Role: "user", Content: []CanonicalPart{{Type: "text", Text: "hello query"}}, LogicalBytes: 11, Hmac: hmac},
		},
	}
	// The append claim gates on the metadata turn row: the production flush
	// always writes it before the content append, so the fixture seeds it the
	// same way (the claim binds the pre-seeded row, never a phantom).
	db := openFixturePool(t, dsn)
	_, err := db.Exec(`INSERT INTO observer_turns (id, node_scope, event_id, occurred_at) VALUES ($1, $2, $3, now())`,
		turnID.String(), scope, "query-ctx")
	require.NoError(t, err)
	require.NoError(t, store.(ContentPersistence).AppendTurns(context.Background(), []ContentInput{in}))

	var sidText string
	require.NoError(t, db.QueryRow(`SELECT session_id::text FROM observer_contexts WHERE turn_id = $1`, turnID.String()).Scan(&sidText))
	sid := uuid.MustParse(sidText)

	qs, err := NewQueryStore(store)
	require.NoError(t, err)
	out, err := qs.TurnContext(context.Background(), ContextQuery{SessionID: sid, TurnID: turnID})
	require.NoError(t, err)
	assert.Equal(t, turnID, out.TurnID)
	assert.Equal(t, 0, out.Ordinal)
	require.Len(t, out.Items, 1)
	assert.Equal(t, hmac, out.Items[0].Hmac)
	require.Len(t, out.Items[0].Content, 1)
	assert.Equal(t, "hello query", out.Items[0].Content[0].Text)

	// A turn with no context row fails closed with the classified error.
	_, err = qs.TurnContext(context.Background(), ContextQuery{SessionID: uuid.New(), TurnID: uuid.New()})
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrMissingContext, code)
}

// TestIntegrationQueryTranscriptNewestBoundedWindow covers the PostgreSQL
// query shape used for long transcripts: the bounded subquery keeps the
// newest contexts, restores chronological order, and can reconstruct a
// boundary predecessor whose full checkpoint sits just outside the window.
func TestIntegrationQueryTranscriptNewestBoundedWindow(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	db := openFixturePool(t, dsn)
	sessionID := uuid.MustParse("40000000-0000-0000-0000-000000000001")
	insertQuerySession(t, db, sessionID, "t-query-transcript-window", epoch.Add(time.Hour))
	totalContexts := maxTranscriptContextRows + 2
	_, err := db.Exec(`INSERT INTO observer_contexts
		(session_id, turn_id, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes)
		SELECT $1,
			('00000000-0000-0000-0000-' || lpad(context_number::text, 12, '0'))::uuid,
			0, 0, 0, 1,
			to_jsonb(ARRAY[lpad(to_hex(context_number), 64, '0')]),
			0
		FROM generate_series(1, $2) AS context_number`, sessionID.String(), totalContexts)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE observer_contexts SET checkpoint_id = id WHERE session_id = $1`, sessionID.String())
	require.NoError(t, err)

	var firstContextID, secondContextID int64
	rows, err := db.Query(`SELECT id FROM observer_contexts WHERE session_id = $1 ORDER BY id LIMIT 2`, sessionID.String())
	require.NoError(t, err)
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&firstContextID))
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&secondContextID))
	require.NoError(t, rows.Close())
	_, err = db.Exec(`UPDATE observer_contexts
		SET checkpoint_id = $1, group_ordinal = 1, common_prefix_count = 1, item_count = 2
		WHERE id = $2`, firstContextID, secondContextID)
	require.NoError(t, err)

	for contextNumber := totalContexts - 1; contextNumber <= totalContexts; contextNumber++ {
		digest := fmt.Sprintf("%064x", contextNumber)
		item := CanonicalItem{Kind: CanonicalKindMessage, Role: "user", LogicalBytes: 4, Hmac: digest}
		payload, logicalBytes, encodeErr := encodeItem(item)
		require.NoError(t, encodeErr)
		digestBytes, digestErr := itemDigestBytes(digest)
		require.NoError(t, digestErr)
		_, insertErr := db.Exec(`INSERT INTO observer_content_objects
			(session_id, item_digest, kind, role, codec, payload, logical_bytes, stored_bytes, truncated)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, false)`,
			sessionID.String(), digestBytes, item.Kind, item.Role, contentCodecZstd, payload, logicalBytes, len(payload))
		require.NoError(t, insertErr)
	}

	queryStore, err := NewQueryStore(store)
	require.NoError(t, err)
	page, err := queryStore.Transcript(context.Background(), TranscriptQuery{
		SessionID: sessionID,
		Direction: TranscriptDirLatest,
		PageSize:  2,
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	assert.Equal(t, fmt.Sprintf("00000000-0000-0000-0000-%012d", totalContexts-1), page.Items[0].TurnID.String())
	assert.Equal(t, fmt.Sprintf("00000000-0000-0000-0000-%012d", totalContexts), page.Items[1].TurnID.String())
	assert.Equal(t, int64(maxTranscriptContextRows-2), page.PrevCursor)
	assert.True(t, page.HasOlder)
}

// TestIntegrationQuerySemaphore covers the single-query semaphore on the real
// database: a held slot makes the next query wait and then fail with the
// timeout classification instead of queueing.
func TestIntegrationQuerySemaphore(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	qs, err := NewQueryStore(store)
	require.NoError(t, err)
	gated := qs.(*pgQueryStore)
	require.Len(t, gated.sem, 0, "the query port starts with a free slot")

	require.NoError(t, acquireQuerySlot(context.Background(), gated.sem))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = gated.ListTurns(ctx, TurnQuery{PageSize: 5})
	require.Error(t, err)
	var qe *QueryError
	require.ErrorAs(t, err, &qe)
	assert.Equal(t, QueryErrTimeout, qe.Kind, "a query behind a held slot must time out, never queue")
	releaseQuerySlot(gated.sem)

	// The slot is free again: the same query succeeds on the real database
	// and stays bounded by the page cap.
	page, err := qs.ListTurns(context.Background(), TurnQuery{PageSize: 5})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(page.Items), 5)
}

// TestIntegrationQueryJSONBContract covers the frozen payload mapping on the
// real database: attempts JSONB round-trips through the turn list, and the
// typed IPTrust columns stay out of the query surface until the Root
// controller chooses to expose them.
func TestIntegrationQueryJSONBContract(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	scope := "t13-query-jsonb"
	ev := queryTurnEvent(0, scope)
	ev.Attempts = []AttemptSummary{{ChannelID: 7, Group: "default", StatusCode: 429, ErrorCode: "rate_limit", ElapsedMS: 5}}
	ev.AttemptsOmitted = 2
	require.NoError(t, store.WriteBatch(context.Background(), []Event{ev}))

	qs, err := NewQueryStore(store)
	require.NoError(t, err)
	page, err := qs.ListTurns(context.Background(), TurnQuery{PageSize: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	it := page.Items[0]
	require.Len(t, it.Attempts, 1)
	assert.Equal(t, int64(7), it.Attempts[0].ChannelID)
	assert.Equal(t, "rate_limit", it.Attempts[0].ErrorCode)
	assert.Equal(t, 2, it.AttemptsOmitted)
	assert.Equal(t, ContentStateFull, it.ContentState)
	// The metadata-only read must not have touched content objects.
	var contentRows int
	require.NoError(t, openFixturePool(t, dsn).QueryRow(`SELECT count(*) FROM observer_content_objects`).Scan(&contentRows))
	assert.Zero(t, contentRows)
}

// TestIntegrationQueryGetSession covers GetSession on the real database: an
// existing session returns its metadata row and an unknown id classifies
// not_found (T3.2 contract extension).
func TestIntegrationQueryGetSession(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	db := openFixturePool(t, dsn)
	scope := "t32-query-getsession"
	id := uuid.MustParse("30000000-0000-0000-0000-000000000001")
	insertQuerySession(t, db, id, scope, epoch.Add(time.Hour))

	qs, err := NewQueryStore(store)
	require.NoError(t, err)

	out, err := qs.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, out.SessionID)
	assert.Equal(t, scope, out.NodeScope)
	assert.Equal(t, int64(7), out.UserID)
	assert.Equal(t, "codex", out.ClientFamily)
	assert.Equal(t, int64(3), out.TurnCount)
	assert.Equal(t, int64(1), out.GapCount)

	_, err = qs.GetSession(context.Background(), uuid.MustParse("30000000-0000-0000-0000-0000000000ff"))
	require.Error(t, err)
	var qe *QueryError
	require.ErrorAs(t, err, &qe)
	assert.Equal(t, QueryErrNotFound, qe.Kind)
}

// TestIntegrationQueryFilterDimensions covers the T3.2 filter dimensions on
// the real database: the session list's turn-derived EXISTS filters
// (model/success/ip) and the turn list's own filters
// (model/success/error_type) return exactly the matching rows.
func TestIntegrationQueryFilterDimensions(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())

	db := openFixturePool(t, dsn)
	scope := "t32-query-filters"
	sidA := uuid.MustParse("30000000-0000-0000-0000-00000000000a")
	sidB := uuid.MustParse("30000000-0000-0000-0000-00000000000b")
	insertQuerySession(t, db, sidA, scope, epoch.Add(2*time.Hour))
	insertQuerySession(t, db, sidB, scope, epoch.Add(time.Hour))

	// Session A: one successful gpt-5 turn from 198.51.100.7.
	// Session B: one failed claude-5 turn from 198.51.100.8.
	turnA := sampleEvent()
	turnA.NodeScope = scope
	turnA.EventID = "flt-a-1"
	turnA.SessionID = &sidA
	turnA.OccurredAt = epoch.Add(30 * time.Minute)
	turnA.Model = "gpt-5"
	turnA.Success = true
	turnA.ErrorType = ""
	turnA.ClientIP = net.ParseIP("198.51.100.7")
	turnA.IPTrust = IPTrustDirect

	turnB := sampleEvent()
	turnB.NodeScope = scope
	turnB.EventID = "flt-b-1"
	turnB.SessionID = &sidB
	turnB.OccurredAt = epoch.Add(20 * time.Minute)
	turnB.Model = "claude-5"
	turnB.Success = false
	turnB.ErrorType = "upstream_error"
	turnB.ClientIP = net.ParseIP("198.51.100.8")
	turnB.IPTrust = IPTrustDirect
	require.NoError(t, store.WriteBatch(context.Background(), []Event{turnA, turnB}))

	qs, err := NewQueryStore(store)
	require.NoError(t, err)

	sessionsWith := func(q SessionQuery) []uuid.UUID {
		t.Helper()
		page, err := qs.ListSessions(context.Background(), q)
		require.NoError(t, err)
		ids := make([]uuid.UUID, 0, len(page.Items))
		for _, it := range page.Items {
			ids = append(ids, it.SessionID)
		}
		return ids
	}
	assert.ElementsMatch(t, []uuid.UUID{sidA}, sessionsWith(SessionQuery{Model: "gpt-5"}))
	assert.ElementsMatch(t, []uuid.UUID{sidB}, sessionsWith(SessionQuery{Model: "claude-5"}))
	success := true
	assert.ElementsMatch(t, []uuid.UUID{sidA}, sessionsWith(SessionQuery{Success: &success}))
	success = false
	assert.ElementsMatch(t, []uuid.UUID{sidB}, sessionsWith(SessionQuery{Success: &success}))
	assert.ElementsMatch(t, []uuid.UUID{sidA}, sessionsWith(SessionQuery{IP: net.ParseIP("198.51.100.7")}))
	assert.ElementsMatch(t, []uuid.UUID{sidB}, sessionsWith(SessionQuery{IP: net.ParseIP("198.51.100.8")}))
	// No filter returns both sessions.
	assert.ElementsMatch(t, []uuid.UUID{sidA, sidB}, sessionsWith(SessionQuery{}))

	turnsWith := func(q TurnQuery) []uuid.UUID {
		t.Helper()
		page, err := qs.ListTurns(context.Background(), q)
		require.NoError(t, err)
		ids := make([]uuid.UUID, 0, len(page.Items))
		for _, it := range page.Items {
			ids = append(ids, *it.SessionID)
		}
		return ids
	}
	assert.ElementsMatch(t, []uuid.UUID{sidA}, turnsWith(TurnQuery{Model: "gpt-5"}))
	assert.ElementsMatch(t, []uuid.UUID{sidB}, turnsWith(TurnQuery{Model: "claude-5"}))
	success = true
	assert.ElementsMatch(t, []uuid.UUID{sidA}, turnsWith(TurnQuery{Success: &success}))
	success = false
	assert.ElementsMatch(t, []uuid.UUID{sidB}, turnsWith(TurnQuery{Success: &success}))
	assert.ElementsMatch(t, []uuid.UUID{sidB}, turnsWith(TurnQuery{ErrorType: "upstream_error"}))
	// An empty error type means "no filter", not "no error".
	assert.ElementsMatch(t, []uuid.UUID{sidA, sidB}, turnsWith(TurnQuery{ErrorType: ""}))
	assert.Empty(t, turnsWith(TurnQuery{ErrorType: "no_such_error"}))
}
