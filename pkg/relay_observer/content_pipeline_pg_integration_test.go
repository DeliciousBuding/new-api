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
// contract end to end: an admission reservation below the canonical byte
// total truncates the tail, the turn is written as gap, the context row
// exists, the content objects carry an explicit gap marker row, and
// reconstruction closes with the marker (data, not silent loss).
func TestIntegrationContentPipelineTruncationWritesGapMarker(t *testing.T) {
	dsn := integrationDSN(t)
	resetObserverSchema(t, dsn)
	store := openVerifyStore(t, dsn)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"alpha alpha alpha"},{"role":"user","content":"bravo bravo bravo"}]}`
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

	disp := newPipelineDispatcher(t, store, func(c *Config) {
		c.BatchSize = 1
		// The capture cap (P0-B) drives the truncation, no longer the
		// admission reservation. 200 bytes fits the gap marker but not the
		// first message item, so the capture truncates with an explicit
		// marker.
		c.MaxCaptureBytesPerTurn = 200
	})
	ev := sampleEvent()
	ev.EventID = "t26-req-gap"
	ev.NodeScope = "t26-node"
	ev.RelayFormat = string(types.RelayFormatOpenAI)
	ev.Request = &req
	ev.Identity = IdentityInput{Headers: reqObj.Header, Body: []byte(body)}
	require.True(t, disp.TryEnqueue(&ev, total-1)) // one canonical byte short

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
	require.NotEmpty(t, rec.Items)
	last := rec.Items[len(rec.Items)-1]
	assert.Equal(t, CanonicalKindGap, last.Kind)
	assert.True(t, last.Truncated)
	assert.Greater(t, last.LogicalBytes, int64(0))
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
