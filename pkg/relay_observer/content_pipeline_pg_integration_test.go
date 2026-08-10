//go:build relay_observer_pg_integration

package relayobserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file runs the T2.6 full-chain content capture against the real
// disposable PostgreSQL: a golden-corpus request (the same bytes a client
// would send) is parsed into its DTO, attached to an event exactly like the
// service hooks do, admitted to the dispatcher, and the worker's
// normalize → WriteBatch → AppendTurns pipeline lands in the observer tables;
// reconstruction then rebuilds the canonical content and must match the
// committed golden fixture byte for byte. The worker, the dispatcher, the
// store, and the schema are the real production paths — only the HTTP request
// handler layer is replaced by the request-path equivalence of the service
// hooks (relayobserver cannot import service, so the attach is reproduced
// here with the same fields and semantics). Compiled only under the
// relay_observer_pg_integration build tag.

// goldenResponsesInput is the request body of the responses_text golden
// fixture: instructions plus two messages, with user identifiers and request
// options that the whitelist must strip.
const goldenResponsesInput = `{
	"model": "gpt-5",
	"instructions": "Be brief.",
	"input": [
		{"role":"user","content":[{"type":"input_text","text":"hello world"}]},
		{"role":"assistant","content":[{"type":"output_text","text":"hi there"}]}
	],
	"metadata": {"user_id":"u-secret"},
	"user": "u-secret",
	"temperature": 0.7,
	"prompt_cache_key": "cache-9"
}`

// goldenFixturePath returns the responses_text golden fixture path.
func goldenFixturePath() string {
	return filepath.Join("testdata", "normalizer", "responses_text.jsonl")
}

// readGoldenLines loads the committed golden fixture, one canonical payload
// per line.
func readGoldenLines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(goldenFixturePath())
	require.NoError(t, err)
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// attachGoldenEvent builds the request-path equivalence of a settled turn:
// the golden corpus is parsed into its DTO over a real httptest request
// object, and the event carries the parsed reference plus the identity
// material exactly as service.buildTurnEvent and publishTurnEvent produce.
func attachGoldenEvent(t *testing.T, nodeScope, eventID string) Event {
	t.Helper()
	reqObj := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(goldenResponsesInput))
	reqObj.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"t26-thr-1","session_id":"t26-ses-1"}`)

	var req dto.Request = &dto.OpenAIResponsesRequest{}
	require.NoError(t, common.Unmarshal([]byte(goldenResponsesInput), req))

	ev := sampleEvent()
	ev.EventID = eventID
	ev.NodeScope = nodeScope
	ev.RelayFormat = string(types.RelayFormatOpenAIResponses)
	ev.Request = &req
	ev.Identity = IdentityInput{
		Headers: reqObj.Header,
		Body:    []byte(goldenResponsesInput),
	}
	return ev
}

// newPipelineDispatcher wires a real PG store into a started dispatcher with
// the observer HMAC key; the caller must Stop it.
func newPipelineDispatcher(t *testing.T, store Store, mutate func(*Config)) *Dispatcher {
	t.Helper()
	cfg := DefaultConfig()
	cfg.HMACKey = testHMACKey
	cfg.HMACKeyVersion = 1
	if mutate != nil {
		mutate(&cfg)
	}
	disp := NewDispatcher(cfg, store)
	disp.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		disp.Stop(ctx)
	})
	return disp
}

// waitContextRow polls until the turn's context row exists and returns its
// session id.
func waitContextRow(t *testing.T, db *sql.DB, turnID uuid.UUID) uuid.UUID {
	t.Helper()
	var sessionID string
	require.Eventually(t, func() bool {
		err := db.QueryRow(`SELECT session_id::text FROM observer_contexts WHERE turn_id = $1`, turnID.String()).Scan(&sessionID)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	sid, err := uuid.Parse(sessionID)
	require.NoError(t, err)
	return sid
}

// TestIntegrationContentPipelineCapturesAndReconstructs is the full-chain
// contract: a golden request flows through the worker pipeline into
// observer_turns / observer_content_objects / observer_contexts, and
// ReconstructTurn rebuilds canonical items that match the committed golden
// fixture byte for byte. Replaying the same event id is absorbed idempotently
// — no duplicate turn, context, or content rows.
func TestIntegrationContentPipelineCapturesAndReconstructs(t *testing.T) {
	dsn := integrationDSN(t)
	resetObserverSchema(t, dsn)
	store := openVerifyStore(t, dsn)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	disp := newPipelineDispatcher(t, store, func(c *Config) { c.BatchSize = 2 })
	ev := attachGoldenEvent(t, "t26-node", "t26-req-1")
	// The admission reservation bounds canonical bytes at min(reservation,
	// MaxRequestBytes); a full-capture fixture needs a reservation above the
	// normalized total, not the raw body size (the body size would truncate
	// the canonical tail to a gap marker, which is the expected budget
	// behavior, not this test's contract).
	require.True(t, disp.TryEnqueue(&ev, 1<<20))

	db := openFixturePool(t, dsn)
	turnID := turnRowID("t26-node", "t26-req-1")
	sid := waitContextRow(t, db, turnID)

	// The metadata row carries the true content_state and the deterministic
	// row id that the context row references.
	var state string
	require.NoError(t, db.QueryRow(`SELECT content_state FROM observer_turns WHERE node_scope = $1 AND event_id = $2`, "t26-node", "t26-req-1").Scan(&state))
	assert.Equal(t, ContentStateFull, state)

	// The turn row is bound to the resolved session: session-scoped queries
	// (sessions/:id/turns, the session EXISTS filters) join on turn.session_id,
	// so the content resolution must backfill it.
	var turnSession sql.NullString
	require.NoError(t, db.QueryRow(`SELECT session_id::text FROM observer_turns WHERE node_scope = $1 AND event_id = $2`, "t26-node", "t26-req-1").Scan(&turnSession))
	require.True(t, turnSession.Valid, "content resolution must backfill the turn's session_id")
	assert.Equal(t, sid.String(), turnSession.String)

	// Reconstruction matches the committed golden fixture byte for byte.
	cp, ok := store.(ContentPersistence)
	require.True(t, ok, "the PG store must implement the content persistence port")
	rec, err := cp.ReconstructTurn(context.Background(), sid, turnID, testHMACKey)
	require.NoError(t, err)
	golden := readGoldenLines(t)
	require.Len(t, rec.Items, len(golden))
	for i, want := range golden {
		got, err := common.Marshal(rec.Items[i])
		require.NoError(t, err)
		assert.Equal(t, want, string(got), "canonical item %d must match the golden fixture", i)
	}

	// The whitelist held: no raw user identifier survived in any item.
	for _, it := range rec.Items {
		assert.NotContains(t, fmt.Sprintf("%+v", it), "u-secret")
	}

	// Idempotent replay: the same event id writes nothing new.
	ev2 := ev
	require.True(t, disp.TryEnqueue(&ev2, int64(len(goldenResponsesInput))))
	require.Eventually(t, func() bool { return disp.Status().WrittenTotal >= 2 }, 10*time.Second, 10*time.Millisecond)
	var turns, contexts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_turns WHERE node_scope = $1`, "t26-node").Scan(&turns))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_contexts WHERE session_id = $1`, sid.String()).Scan(&contexts))
	assert.Equal(t, 1, turns, "replayed event must not duplicate the turn row")
	assert.Equal(t, 1, contexts, "replayed event must not duplicate the context row")
}

// TestIntegrationContentPipelineUnknownFormatLeavesNoContent is the
// fail-open contract of an unsupported relay format: the turn row lands with
// metadata_only and no content or context rows exist for it.
func TestIntegrationContentPipelineUnknownFormatLeavesNoContent(t *testing.T) {
	dsn := integrationDSN(t)
	resetObserverSchema(t, dsn)
	store := openVerifyStore(t, dsn)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	disp := newPipelineDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })
	ev := attachGoldenEvent(t, "t26-node", "t26-req-unknown")
	ev.RelayFormat = "gemini" // outside the normalizer whitelist
	require.True(t, disp.TryEnqueue(&ev, int64(len(goldenResponsesInput))))

	require.Eventually(t, func() bool { return disp.Status().WrittenTotal >= 1 }, 10*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return disp.Status().ContentGapsTotal >= 1 }, 10*time.Second, 10*time.Millisecond)

	db := openFixturePool(t, dsn)
	var state string
	require.NoError(t, db.QueryRow(`SELECT content_state FROM observer_turns WHERE node_scope = $1 AND event_id = $2`, "t26-node", "t26-req-unknown").Scan(&state))
	assert.Equal(t, ContentStateMetadataOnly, state)

	turnID := turnRowID("t26-node", "t26-req-unknown")
	var contextCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_contexts WHERE turn_id = $1`, turnID.String()).Scan(&contextCount))
	assert.Equal(t, 0, contextCount, "unknown format must produce no context row")
	var objectCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_content_objects WHERE session_id IN (SELECT session_id FROM observer_session_aliases WHERE node_scope = $1)`, "t26-node").Scan(&objectCount))
	assert.Equal(t, 0, objectCount)
}

// TestIntegrationContentPipelineTruncationWritesGapMarker is the gap
// contract end to end: the configured canonical cap retains the latest
// semantic block beside one structured gap, the turn is written as gap, the
// context row exists, and PostgreSQL reconstruction preserves both marker
// metadata and protocol order. Queue reservation is admission-only.
func TestIntegrationContentPipelineTruncationWritesGapMarker(t *testing.T) {
	dsn := integrationDSN(t)
	resetObserverSchema(t, dsn)
	store := openVerifyStore(t, dsn)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha"},{"role":"user","content":"bravo bravo bravo bravo bravo bravo bravo bravo bravo bravo"}]}`
	reqObj := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	reqObj.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"t26-thr-gap"}`)

	var req dto.Request = &dto.GeneralOpenAIRequest{}
	require.NoError(t, common.Unmarshal([]byte(body), req))
	full := NormalizeRequest(string(types.RelayFormatOpenAI), req, NormalizeOptions{
		CaptureLimit: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey,
	})
	require.Equal(t, ContentStateFull, full.ContentState)
	var total int64
	for _, it := range full.Items {
		p, err := common.Marshal(it)
		require.NoError(t, err)
		total += int64(len(p))
	}
	require.Len(t, full.Items, 2)
	gapInfo := GapInfo{
		Position:     GapPositionHead,
		Reason:       GapReasonBudget,
		OmittedItems: 1,
		LogicalBytes: full.Items[0].LogicalBytes,
	}
	marker := withHmac(GapMarker(gapInfo), NormalizeOptions{HMACKey: testHMACKey})
	markerPayload, err := common.Marshal(marker)
	require.NoError(t, err)
	latestPayload, err := common.Marshal(full.Items[1])
	require.NoError(t, err)
	captureLimit := int64(len(markerPayload) + len(latestPayload))
	require.Less(t, captureLimit, total)

	disp := newPipelineDispatcher(t, store, func(c *Config) {
		c.BatchSize = 1
		c.MaxCaptureBytesPerTurn = captureLimit
	})
	ev := sampleEvent()
	ev.EventID = "t26-req-gap"
	ev.NodeScope = "t26-node"
	ev.RelayFormat = string(types.RelayFormatOpenAI)
	ev.Request = &req
	ev.Identity = IdentityInput{Headers: reqObj.Header, Body: []byte(body)}
	require.True(t, disp.TryEnqueue(&ev, total-1))

	db := openFixturePool(t, dsn)
	turnID := turnRowID("t26-node", "t26-req-gap")
	sid := waitContextRow(t, db, turnID)

	var state string
	require.NoError(t, db.QueryRow(`SELECT content_state FROM observer_turns WHERE node_scope = $1 AND event_id = $2`, "t26-node", "t26-req-gap").Scan(&state))
	assert.Equal(t, ContentStateGap, state)

	// The gap marker is data: a content object row of kind gap exists.
	var gapCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_content_objects WHERE session_id = $1 AND kind = $2`, sid.String(), CanonicalKindGap).Scan(&gapCount))
	assert.Equal(t, 1, gapCount, "truncated capture must persist an explicit gap marker object")

	cp, ok := store.(ContentPersistence)
	require.True(t, ok, "the PG store must implement the content persistence port")
	rec, err := cp.ReconstructTurn(context.Background(), sid, turnID, testHMACKey)
	require.NoError(t, err)
	require.Len(t, rec.Items, 2)
	assert.Equal(t, marker, rec.Items[0])
	require.NotNil(t, rec.Items[0].Gap)
	assert.Equal(t, gapInfo, *rec.Items[0].Gap)
	assert.Equal(t, full.Items[1], rec.Items[1], "the latest message survives persistence and reconstruction")
}

// TestIntegrationSessionOnlyAppendTracksIdentityWithoutContent locks the T2
// decoupling contract: a turn with a resolvable session identity whose
// capture produced no items (budget truncation below the minimal envelope)
// still binds the session — the session row, the alias bindings, the turn's
// session_id backfill, last_seen, turn_count, and gap_count all advance —
// while no content objects, context rows, or head rows are created.
func TestIntegrationSessionOnlyAppendTracksIdentityWithoutContent(t *testing.T) {
	dsn := integrationDSN(t)
	resetObserverSchema(t, dsn)
	store := openVerifyStore(t, dsn)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	// A tiny reservation truncates normalization to zero items (the canonical
	// overhead of one message item exceeds the budget, and the gap marker
	// does not fit either), which used to short-circuit before identity
	// resolution and lose the session entirely.
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"tiny"}]}`
	reqObj := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	reqObj.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"t26-thr-only","session_id":"t26-ses-only"}`)

	var req dto.Request = &dto.GeneralOpenAIRequest{}
	require.NoError(t, common.Unmarshal([]byte(body), req))

	disp := newPipelineDispatcher(t, store, func(c *Config) {
		c.BatchSize = 1
		// A capture cap below the minimal envelope truncates to zero items
		// (the decoupled session-only path).
		c.MaxCaptureBytesPerTurn = 8
	})
	ev := sampleEvent()
	ev.EventID = "t26-req-only"
	ev.RelayFormat = string(types.RelayFormatOpenAI)
	ev.Request = &req
	ev.Identity = IdentityInput{Headers: reqObj.Header, Body: []byte(body)}
	require.True(t, disp.TryEnqueue(&ev, 16))

	db := openFixturePool(t, dsn)
	turnID := turnRowID("node-a", "t26-req-only")
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_turns WHERE id = $1 AND session_id IS NOT NULL`, turnID.String()).Scan(&n))
		return n == 1
	}, 5*time.Second, 10*time.Millisecond)

	// The session exists with one turn and one gap. The session id is a uuid
	// column; scan it as text.
	var sid string
	var turnCount, gapCount int64
	require.NoError(t, db.QueryRow(`SELECT id::text, turn_count, gap_count FROM observer_sessions WHERE turn_count = 1 AND gap_count = 1`).Scan(&sid, &turnCount, &gapCount))
	assert.Equal(t, int64(1), turnCount)
	assert.Equal(t, int64(1), gapCount, "the session-only gap state must advance gap_count")

	// The aliases are bound.
	var aliasCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_session_aliases WHERE session_id = $1`, sid).Scan(&aliasCount))
	assert.Equal(t, 2, aliasCount, "turn_thread and turn_session aliases must both bind")

	// No content, no context, no head rows for the session.
	var objects, contexts, heads int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_content_objects WHERE session_id = $1`, sid).Scan(&objects))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_contexts WHERE session_id = $1`, sid).Scan(&contexts))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_session_heads WHERE session_id = $1`, sid).Scan(&heads))
	assert.Zero(t, objects, "session-only append must not create content objects")
	assert.Zero(t, contexts, "session-only append must not create context rows")
	assert.Zero(t, heads, "session-only append must not create a session head")
}

// TestIntegrationSessionOnlyAppendMetadataOnly locks the decoupled contract
// for the metadata-only outcome: a requestless-but-identified event still
// binds the session and the turn, and creates no content rows.
func TestIntegrationSessionOnlyAppendMetadataOnly(t *testing.T) {
	dsn := integrationDSN(t)
	resetObserverSchema(t, dsn)
	store := openVerifyStore(t, dsn)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	disp := newPipelineDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })
	ev := sampleEvent()
	ev.EventID = "t26-req-meta"
	// No request: the metadata-only outcome. Identity material still present.
	ev.Identity = IdentityInput{
		Headers: http.Header{"X-Codex-Turn-Metadata": {`{"thread_id":"t26-thr-meta","session_id":"t26-ses-meta"}`}},
	}
	require.True(t, disp.TryEnqueue(&ev, 16))

	db := openFixturePool(t, dsn)
	turnID := turnRowID("node-a", "t26-req-meta")
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_turns WHERE id = $1 AND session_id IS NOT NULL`, turnID.String()).Scan(&n))
		return n == 1
	}, 5*time.Second, 10*time.Millisecond)

	var sid string
	var turnCount, gapCount int64
	require.NoError(t, db.QueryRow(`SELECT id::text, turn_count, gap_count FROM observer_sessions WHERE turn_count = 1`).Scan(&sid, &turnCount, &gapCount))
	assert.Equal(t, int64(1), turnCount)
	assert.Zero(t, gapCount, "a metadata-only outcome is not a capture gap")

	var objects, contexts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_content_objects WHERE session_id = $1`, sid).Scan(&objects))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_contexts WHERE session_id = $1`, sid).Scan(&contexts))
	assert.Zero(t, objects)
	assert.Zero(t, contexts)
}

// TestIntegrationCrossProfileEqualAliasStaysSeparate proves the v4 database
// identity contract on real PostgreSQL. Codex and Claude may expose the same
// raw session value, which intentionally produces the same HMAC digest, but
// provider remains part of the alias identity. Each profile must therefore
// keep one stable session across repeated turns, and the two sessions must
// never merge or force one profile into a fresh unbound session per turn.
func TestIntegrationCrossProfileEqualAliasStaysSeparate(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())
	cp, ok := store.(ContentPersistence)
	require.True(t, ok)
	db := openFixturePool(t, dsn)
	ctx := context.Background()

	scope := uniqueScope("t54-profile")
	const userID int64 = 41
	km := KeyMaterial{CurrentKey: testHMACKey, CurrentVersion: 1}
	codex, err := GenerateAlias("same-raw-session", SourceTurnThread, ScopeCodexCLI, km)
	require.NoError(t, err)
	claude, err := GenerateAlias("same-raw-session", SourceClaudeHeader, ScopeClaudeCLI, km)
	require.NoError(t, err)
	require.Equal(t, codex.Digest, claude.Digest, "fixture must exercise one digest across profiles")

	profiles := []struct {
		name  string
		alias Alias
	}{
		{name: "codex", alias: codex},
		{name: "claude", alias: claude},
	}
	resolved := map[string]string{}
	for round := 1; round <= 2; round++ {
		for _, profile := range profiles {
			eventID := fmt.Sprintf("%s-%d", profile.name, round)
			ev := sampleEvent()
			ev.NodeScope = scope
			ev.UserID = userID
			ev.EventID = eventID
			ev.OccurredAt = time.Now().UTC()
			require.NoError(t, store.WriteBatch(ctx, []Event{ev}))

			turnID := turnRowID(scope, eventID)
			require.NoError(t, cp.AppendTurns(ctx, []ContentInput{{
				NodeScope:    scope,
				UserID:       userID,
				Aliases:      []Alias{profile.alias},
				TurnID:       turnID,
				ContentState: ContentStateMetadataOnly,
			}}))

			var sessionID string
			require.NoError(t, db.QueryRow(`SELECT session_id::text FROM observer_turns WHERE id = $1`, turnID.String()).Scan(&sessionID))
			if round == 1 {
				resolved[profile.name] = sessionID
			} else {
				assert.Equal(t, resolved[profile.name], sessionID, "%s must preserve session continuity", profile.name)
			}
		}
	}

	require.NotEqual(t, resolved["codex"], resolved["claude"], "cross-profile aliases must never merge")
	var sessionCount, aliasCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_sessions WHERE node_scope = $1 AND user_id = $2`, scope, userID).Scan(&sessionCount))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_session_aliases WHERE node_scope = $1 AND user_id = $2`, scope, userID).Scan(&aliasCount))
	assert.Equal(t, 2, sessionCount)
	assert.Equal(t, 2, aliasCount)
	for _, sessionID := range resolved {
		turns, gaps := sessionCounts(t, db, sessionID)
		assert.Equal(t, int64(2), turns)
		assert.Zero(t, gaps)
	}
}

// --- PR #14: exactly-once claim for session-only appends ---

// insertAppendFixture seeds one session, its codex_cli alias binding (user id
// 1, key version 1 — the real identity-chain lookup parameters) and one
// unbound observer_turns row for turnID, returning the session id. The
// binding digest is digestA.
func insertAppendFixture(t *testing.T, db *sql.DB, scope string, lastSeen time.Time, turnID uuid.UUID) string {
	t.Helper()
	sid := insertSessionRow(t, db, scope, lastSeen)
	_, err := db.Exec(`INSERT INTO observer_session_aliases (node_scope, user_id, key_version, provider, source, alias_digest, session_id, first_seen, last_seen)
		VALUES ($1, 1, 1, 'codex_cli', 'header', $2, $3, now(), now())`, scope, digestA, sid)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO observer_turns (id, node_scope, event_id, occurred_at) VALUES ($1, $2, $3, now())`,
		turnID.String(), scope, "exactly-once")
	require.NoError(t, err)
	return sid
}

// exactlyOnceInput builds the ContentInput of the exactly-once suite: the
// codex_cli alias matching the fixture binding, and either the given items
// (content-bearing) or none (session-only).
func exactlyOnceInput(scope string, turnID uuid.UUID, state string, items []CanonicalItem) ContentInput {
	return ContentInput{
		NodeScope:    scope,
		UserID:       1,
		Aliases:      []Alias{{Version: 1, Digest: digestAHex, Scope: ScopeCodexCLI, Source: SourceTurnThread}},
		TurnID:       turnID,
		Items:        items,
		ContentState: state,
	}
}

// sessionCounts reads one session's (turn_count, gap_count).
func sessionCounts(t *testing.T, db *sql.DB, sid string) (int64, int64) {
	t.Helper()
	var turns, gaps int64
	require.NoError(t, db.QueryRow(`SELECT turn_count, gap_count FROM observer_sessions WHERE id = $1`, sid).Scan(&turns, &gaps))
	return turns, gaps
}

// TestIntegrationSessionOnlyAppendExactlyOnceGap is scenario 1: the same gap
// session-only turn appended twice sequentially — the claim makes the replay
// a no-op, so turn_count=1 and gap_count=1 (before the fix the second append
// bumped both again).
func TestIntegrationSessionOnlyAppendExactlyOnceGap(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())
	cp, ok := store.(ContentPersistence)
	require.True(t, ok)
	db := openFixturePool(t, dsn)

	scope := uniqueScope("t53-gap")
	turnID := uuid.New()
	sid := insertAppendFixture(t, db, scope, time.Now(), turnID)
	in := exactlyOnceInput(scope, turnID, ContentStateGap, nil)

	require.NoError(t, cp.AppendTurns(context.Background(), []ContentInput{in}))
	require.NoError(t, cp.AppendTurns(context.Background(), []ContentInput{in}))

	turns, gaps := sessionCounts(t, db, sid)
	assert.Equal(t, int64(1), turns, "the replayed gap turn must count exactly once")
	assert.Equal(t, int64(1), gaps)
	var bound bool
	require.NoError(t, db.QueryRow(`SELECT session_id = $1 FROM observer_turns WHERE id = $2`, sid, turnID.String()).Scan(&bound))
	assert.True(t, bound, "the turn must be bound to the session")
}

// TestIntegrationSessionOnlyAppendExactlyOnceMetadataOnly is scenario 2: the
// same metadata-only turn appended twice sequentially — one turn, no gaps.
func TestIntegrationSessionOnlyAppendExactlyOnceMetadataOnly(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())
	cp, ok := store.(ContentPersistence)
	require.True(t, ok)
	db := openFixturePool(t, dsn)

	scope := uniqueScope("t53-meta")
	turnID := uuid.New()
	sid := insertAppendFixture(t, db, scope, time.Now(), turnID)
	in := exactlyOnceInput(scope, turnID, ContentStateMetadataOnly, nil)

	require.NoError(t, cp.AppendTurns(context.Background(), []ContentInput{in}))
	require.NoError(t, cp.AppendTurns(context.Background(), []ContentInput{in}))

	turns, gaps := sessionCounts(t, db, sid)
	assert.Equal(t, int64(1), turns, "the replayed metadata-only turn must count exactly once")
	assert.Equal(t, int64(0), gaps, "a metadata-only outcome is not a capture gap")
}

// TestIntegrationSessionOnlyAppendExactlyOnceConcurrent is scenario 3: the
// same session-only turn appended concurrently from two connections. Both
// calls must succeed — the session row lock serializes them, the second
// claim sees 0 affected rows and no-ops — and the counters advance once.
func TestIntegrationSessionOnlyAppendExactlyOnceConcurrent(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())
	cp, ok := store.(ContentPersistence)
	require.True(t, ok)
	db := openFixturePool(t, dsn)

	scope := uniqueScope("t53-race")
	turnID := uuid.New()
	sid := insertAppendFixture(t, db, scope, time.Now(), turnID)
	in := exactlyOnceInput(scope, turnID, ContentStateGap, nil)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = cp.AppendTurns(context.Background(), []ContentInput{in})
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0], "both racing appends must succeed")
	require.NoError(t, errs[1], "both racing appends must succeed")

	turns, gaps := sessionCounts(t, db, sid)
	assert.Equal(t, int64(1), turns, "the racing appends must count exactly once")
	assert.Equal(t, int64(1), gaps)
	var bound bool
	require.NoError(t, db.QueryRow(`SELECT session_id = $1 FROM observer_turns WHERE id = $2`, sid, turnID.String()).Scan(&bound))
	assert.True(t, bound)
}

// TestIntegrationSessionOnlyAppendExactlyOnceRollbackRetry is scenario 4: the
// first append transaction fails after the claim (the test hook cancels the
// context inside the transaction, so the counter bump aborts) and rolls back
// — the claim binding disappears with it — and the retry claims fresh and
// counts exactly once.
func TestIntegrationSessionOnlyAppendExactlyOnceRollbackRetry(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())
	cp, ok := store.(ContentPersistence)
	require.True(t, ok)
	db := openFixturePool(t, dsn)

	scope := uniqueScope("t53-retry")
	turnID := uuid.New()
	sid := insertAppendFixture(t, db, scope, time.Now(), turnID)
	in := exactlyOnceInput(scope, turnID, ContentStateGap, nil)

	// The hook runs inside the append transaction after the claim while the
	// session row lock is held; canceling the append context fails the next
	// statement (the counter bump) and database/sql rolls the transaction
	// back, undoing the claim.
	ctx, cancel := context.WithCancel(context.Background())
	appendHook = func() { cancel() }
	t.Cleanup(func() { appendHook = nil })

	err := cp.AppendTurns(ctx, []ContentInput{in})
	require.Error(t, err, "the canceled context must abort the append")

	// The rolled-back transaction left nothing durable: the turn is still
	// unbound and the counters untouched.
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_turns WHERE id = $1 AND session_id IS NULL`, turnID.String()).Scan(&n))
		return n == 1
	}, 5*time.Second, 10*time.Millisecond)

	// The retry with a fresh context claims fresh and counts exactly once.
	require.NoError(t, cp.AppendTurns(context.Background(), []ContentInput{in}))
	turns, gaps := sessionCounts(t, db, sid)
	assert.Equal(t, int64(1), turns, "the retry must count exactly once")
	assert.Equal(t, int64(1), gaps)
}

// TestIntegrationSessionOnlyAppendExactlyOnceContentReplay is scenario 5: a
// content-bearing turn replayed lands exactly one context row, one object,
// and one counter increment — the claim gates the content path too.
func TestIntegrationSessionOnlyAppendExactlyOnceContentReplay(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())
	cp, ok := store.(ContentPersistence)
	require.True(t, ok)
	db := openFixturePool(t, dsn)

	scope := uniqueScope("t53-content")
	turnID := uuid.New()
	sid := insertAppendFixture(t, db, scope, time.Now(), turnID)
	in := exactlyOnceInput(scope, turnID, ContentStateFull, []CanonicalItem{contentItemWith(t, "exactly-once")})

	require.NoError(t, cp.AppendTurns(context.Background(), []ContentInput{in}))
	require.NoError(t, cp.AppendTurns(context.Background(), []ContentInput{in}))

	var contexts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_contexts WHERE turn_id = $1`, turnID.String()).Scan(&contexts))
	assert.Equal(t, 1, contexts, "the replay must not duplicate the context row")
	turns, gaps := sessionCounts(t, db, sid)
	assert.Equal(t, int64(1), turns, "the replay must count exactly once")
	assert.Equal(t, int64(0), gaps)
	rec, err := cp.ReconstructTurn(context.Background(), uuid.MustParse(sid), turnID, testHMACKey)
	require.NoError(t, err)
	require.Len(t, rec.Items, 1)
	assert.Equal(t, "exactly-once", rec.Items[0].Content[0].Text)
}

// --- PR #14: T3 end-to-end append vs retention concurrency ---

// TestIntegrationAppendVsRetentionEndToEndScenarioA is the true end-to-end
// scenario A of the T3 lock-order contract, calling the production methods on
// two connections: AppendTurns has acquired the session row lock (the
// appendHook pauses it after the claim), DeleteSessionRetention blocks
// behind it, and after the append commits (refreshing last_seen), the
// retention re-check sees the fresh row and no-ops — the session survives
// with exactly one turn.
func TestIntegrationAppendVsRetentionEndToEndScenarioA(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())
	cp, ok := store.(ContentPersistence)
	require.True(t, ok)
	rs, ok := store.(RetentionStore)
	require.True(t, ok)
	db := openFixturePool(t, dsn)

	scope := uniqueScope("t53-scenario-a")
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	turnID := uuid.New()
	sid := insertAppendFixture(t, db, scope, time.Now().Add(-31*24*time.Hour), turnID)
	in := exactlyOnceInput(scope, turnID, ContentStateGap, nil)

	// Hold the append inside its transaction, after the claim, while it owns
	// the session row lock.
	appendLocked := make(chan struct{})
	release := make(chan struct{})
	appendHook = func() {
		close(appendLocked)
		<-release
	}
	t.Cleanup(func() { appendHook = nil })

	appendDone := make(chan error, 1)
	go func() { appendDone <- cp.AppendTurns(context.Background(), []ContentInput{in}) }()
	<-appendLocked

	// The retention delete must block on the session row lock held by the
	// append.
	retDone := make(chan error, 1)
	go func() { retDone <- rs.DeleteSessionRetention(context.Background(), uuid.MustParse(sid), cutoff) }()
	select {
	case err := <-retDone:
		t.Fatalf("retention finished while the append held the session lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Release the append: it bumps last_seen to now and commits; the retention
	// delete unblocks, re-checks the cutoff, and no-ops.
	close(release)
	require.NoError(t, <-appendDone, "the append must complete after release")
	select {
	case err := <-retDone:
		require.NoError(t, err, "retention must complete without error after the append commits")
	case <-time.After(5 * time.Second):
		t.Fatal("retention stayed blocked after the append committed")
	}

	// The reactivated session survived retention with exactly one turn.
	turns, gaps := sessionCounts(t, db, sid)
	assert.Equal(t, int64(1), turns)
	assert.Equal(t, int64(1), gaps)
	var bound bool
	require.NoError(t, db.QueryRow(`SELECT session_id = $1 FROM observer_turns WHERE id = $2`, sid, turnID.String()).Scan(&bound))
	assert.True(t, bound)
}

// TestIntegrationAppendVsRetentionEndToEndScenarioB is the true end-to-end
// scenario B of the T3 lock-order contract: DeleteSessionRetention has
// acquired the session row lock (the retentionHook pauses it after the
// lock and the last_seen re-check), AppendTurns blocks on the alias lookup,
// and after the retention deletes the session and commits, the append
// unblocks, observes the session is gone, and creates a complete fresh
// session — claim, content context, and counters.
func TestIntegrationAppendVsRetentionEndToEndScenarioB(t *testing.T) {
	dsn := integrationDSN(t)
	store := resetObserverSchema(t, dsn)
	defer store.Close(context.Background())
	cp, ok := store.(ContentPersistence)
	require.True(t, ok)
	rs, ok := store.(RetentionStore)
	require.True(t, ok)
	db := openFixturePool(t, dsn)

	scope := uniqueScope("t53-scenario-b")
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	turnID := uuid.New()
	oldSid := insertAppendFixture(t, db, scope, time.Now().Add(-31*24*time.Hour), turnID)
	in := exactlyOnceInput(scope, turnID, ContentStateFull, []CanonicalItem{contentItemWith(t, "fresh-session")})

	// Hold the retention inside its transaction, after the session lock and
	// the last_seen re-check, before any delete.
	retentionLocked := make(chan struct{})
	release := make(chan struct{})
	retentionHook = func() {
		close(retentionLocked)
		<-release
	}
	t.Cleanup(func() { retentionHook = nil })

	retDone := make(chan error, 1)
	go func() { retDone <- rs.DeleteSessionRetention(context.Background(), uuid.MustParse(oldSid), cutoff) }()
	<-retentionLocked

	// The append must block on the alias lookup (FOR UPDATE OF s).
	appendDone := make(chan error, 1)
	go func() { appendDone <- cp.AppendTurns(context.Background(), []ContentInput{in}) }()
	select {
	case err := <-appendDone:
		t.Fatalf("append finished while retention held the session lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Release retention: it deletes the expired session and commits; the
	// append unblocks, sees the session is gone, and recreates it.
	close(release)
	require.NoError(t, <-retDone, "retention must complete after release")
	require.NoError(t, <-appendDone, "the append must complete once the session lock is free")

	// The old session is gone; the append created a fresh session and bound
	// the turn, the content, and the counters to it.
	var oldCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_sessions WHERE id = $1`, oldSid).Scan(&oldCount))
	assert.Zero(t, oldCount, "the expired session must be deleted")
	var newSid string
	require.NoError(t, db.QueryRow(`SELECT session_id::text FROM observer_turns WHERE id = $1`, turnID.String()).Scan(&newSid))
	assert.NotEqual(t, oldSid, newSid, "the append must create a fresh session, never resurrect the deleted one")
	turns, gaps := sessionCounts(t, db, newSid)
	assert.Equal(t, int64(1), turns, "the fresh session counts the turn exactly once")
	assert.Equal(t, int64(0), gaps)
	var contexts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM observer_contexts WHERE turn_id = $1`, turnID.String()).Scan(&contexts))
	assert.Equal(t, 1, contexts, "the fresh session carries the appended content")
}
