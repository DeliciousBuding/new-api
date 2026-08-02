//go:build relay_observer_pg_integration

package relayobserver

import (
	"context"
	"database/sql"
	"fmt"
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
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	disp := newPipelineDispatcher(t, store, func(c *Config) { c.BatchSize = 2 })
	ev := attachGoldenEvent(t, "t26-node", "t26-req-1")
	require.True(t, disp.TryEnqueue(&ev, int64(len(goldenResponsesInput))))

	db := openFixturePool(t, dsn)
	turnID := turnRowID("t26-node", "t26-req-1")
	sid := waitContextRow(t, db, turnID)

	// The metadata row carries the true content_state and the deterministic
	// row id that the context row references.
	var state string
	require.NoError(t, db.QueryRow(`SELECT content_state FROM observer_turns WHERE node_scope = $1 AND event_id = $2`, "t26-node", "t26-req-1").Scan(&state))
	assert.Equal(t, ContentStateFull, state)

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
	ensureV1(t, dsn)
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
	ensureV1(t, dsn)
	store := openVerifyStore(t, dsn)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"alpha alpha alpha"},{"role":"user","content":"bravo bravo bravo"}]}`
	reqObj := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	reqObj.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"t26-thr-gap"}`)

	var req dto.Request = &dto.GeneralOpenAIRequest{}
	require.NoError(t, common.Unmarshal([]byte(body), req))
	full := NormalizeRequest(string(types.RelayFormatOpenAI), req, NormalizeOptions{
		Reservation: 1 << 20, MaxRequestBytes: 1 << 20, HMACKey: testHMACKey,
	})
	require.Equal(t, ContentStateFull, full.ContentState)
	var total int64
	for _, it := range full.Items {
		p, err := common.Marshal(it)
		require.NoError(t, err)
		total += int64(len(p))
	}

	disp := newPipelineDispatcher(t, store, func(c *Config) { c.BatchSize = 1 })
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
