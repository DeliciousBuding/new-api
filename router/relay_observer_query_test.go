package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	relayobserver "github.com/QuantumNous/new-api/pkg/relay_observer"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These tests cover the T3.2 Root observer query routes: registration, the
// Root-only auth matrix, parameter validation, the degraded envelope for
// store failures and timeouts, the 404 for missing resources, and the
// no-secrets response guarantee. The query surface is a fake ObserverRuntime
// (the same injection point main uses), so no database is ever contacted.

// observerQueryRoutes is the T3.2 route set under test. Registration asserts
// use the pattern paths gin reports (with :id); requests use concrete paths.
var observerQueryRoutes = []string{
	"/api/relay-observer/overview",
	"/api/relay-observer/sessions",
	"/api/relay-observer/sessions/:id",
	"/api/relay-observer/sessions/:id/turns",
	"/api/relay-observer/turns/:id/context",
}

// observerQueryRequestPaths is the concrete request form of each route for
// the auth matrix and the unavailable-surface sweep.
var observerQueryRequestPaths = []string{
	"/api/relay-observer/overview",
	"/api/relay-observer/sessions",
	"/api/relay-observer/sessions/00000000-0000-0000-0000-000000000001",
	"/api/relay-observer/sessions/00000000-0000-0000-0000-000000000001/turns",
	"/api/relay-observer/turns/00000000-0000-0000-0000-000000000001/context",
}

// fakeObserverRuntime implements the controller.ObserverRuntime seam: the
// query surface and HMAC key are scripted, so every controller failure path
// runs without a real store.
type fakeObserverRuntime struct {
	qs      relayobserver.QueryStore
	timeout time.Duration
	ok      bool
	hmacKey string
}

func (f *fakeObserverRuntime) Status() relayobserver.Status {
	return relayobserver.Status{Enabled: true}
}

func (f *fakeObserverRuntime) QuerySurface() (relayobserver.QueryStore, time.Duration, bool) {
	return f.qs, f.timeout, f.ok
}

func (f *fakeObserverRuntime) HMACKey() string { return f.hmacKey }

// fakeObserverQueryStore implements QueryStore with scripted results and
// errors per method, so each handler path is driven deterministically.
type fakeObserverQueryStore struct {
	overviewResult relayobserver.OverviewResult
	overviewErr    error
	sessionPage    relayobserver.SessionPage
	sessionsErr    error
	sessionRow     relayobserver.SessionSummary
	sessionErr     error
	turnPage       relayobserver.TurnPage
	turnsErr       error
	contextResult  relayobserver.TurnContextResult
	contextErr     error
}

func (f *fakeObserverQueryStore) Overview(ctx context.Context, query relayobserver.OverviewQuery) (relayobserver.OverviewResult, error) {
	return f.overviewResult, f.overviewErr
}

func (f *fakeObserverQueryStore) ListSessions(ctx context.Context, query relayobserver.SessionQuery) (relayobserver.SessionPage, error) {
	return f.sessionPage, f.sessionsErr
}

func (f *fakeObserverQueryStore) GetSession(ctx context.Context, id uuid.UUID) (relayobserver.SessionSummary, error) {
	return f.sessionRow, f.sessionErr
}

func (f *fakeObserverQueryStore) ListTurns(ctx context.Context, query relayobserver.TurnQuery) (relayobserver.TurnPage, error) {
	return f.turnPage, f.turnsErr
}

func (f *fakeObserverQueryStore) TurnContext(ctx context.Context, query relayobserver.ContextQuery) (relayobserver.TurnContextResult, error) {
	return f.contextResult, f.contextErr
}

// relayObserverTestEnv builds the sqlite-backed engine with root/admin/user
// tokens, exactly like the status auth matrix, and restores every package
// global afterwards.
func relayObserverTestEnv(t *testing.T) (engine *gin.Engine, rootToken, adminToken, userToken string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	origRedis := common.RedisEnabled
	origDB := model.DB
	origLogDB := model.LOG_DB
	common.RedisEnabled = false

	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.RedisEnabled = origRedis
		model.DB = origDB
		model.LOG_DB = origLogDB
	})
	require.NoError(t, db.AutoMigrate(&model.User{}))

	rootToken = "root-access-token-000000000000000000000"
	adminToken = "admin-access-token-00000000000000000000"
	userToken = "user-access-token-000000000000000000000"
	for _, u := range []*model.User{
		{Username: "root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AccessToken: &rootToken, AffCode: "r000"},
		{Username: "admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AccessToken: &adminToken, AffCode: "a000"},
		{Username: "user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AccessToken: &userToken, AffCode: "u000"},
	} {
		require.NoError(t, db.Create(u).Error)
	}

	engine = gin.New()
	SetApiRouter(engine)
	return engine, rootToken, adminToken, userToken
}

// rootObserverRequest issues one authenticated Root request against the
// engine and returns the recorder.
func rootObserverRequest(t *testing.T, engine *gin.Engine, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// injectObserverRuntime installs a fake runtime for the test and restores the
// unwired state afterwards (no other test in this package injects a runtime).
func injectObserverRuntime(t *testing.T, rt *fakeObserverRuntime) {
	t.Helper()
	controller.SetRelayObserverRuntime(rt)
	t.Cleanup(func() { controller.SetRelayObserverRuntime(nil) })
}

// TestRelayObserverQueryRoutesRegistered proves all five T3.2 query routes
// are registered as GET on the relay-observer group.
func TestRelayObserverQueryRoutesRegistered(t *testing.T) {
	engine, _, _, _ := relayObserverTestEnv(t)
	registered := map[string]bool{}
	for _, route := range engine.Routes() {
		if route.Method == http.MethodGet {
			registered[route.Path] = true
		}
	}
	for _, path := range observerQueryRoutes {
		assert.True(t, registered[path], "GET %s must be registered", path)
	}
}

// TestRelayObserverQueryAuthMatrix proves every query route sits behind
// middleware.RootAuth: Root succeeds, Admin and User receive 403 even when
// calling the route directly, and anonymous requests are rejected.
func TestRelayObserverQueryAuthMatrix(t *testing.T) {
	engine, rootToken, adminToken, userToken := relayObserverTestEnv(t)
	tests := []struct {
		name  string
		token string
		want  int
	}{
		{name: "root succeeds", token: rootToken, want: http.StatusOK},
		{name: "admin forbidden", token: adminToken, want: http.StatusForbidden},
		{name: "user forbidden", token: userToken, want: http.StatusForbidden},
		{name: "anonymous unauthorized", token: "", want: http.StatusUnauthorized},
	}
	for _, path := range observerQueryRequestPaths {
		for _, tt := range tests {
			t.Run(strings.TrimPrefix(path, "/api/relay-observer/")+"/"+tt.name, func(t *testing.T) {
				rec := rootObserverRequest(t, engine, tt.token, path)
				assert.Equal(t, tt.want, rec.Code, "%s with %s", path, tt.name)
			})
		}
	}
}

// TestRelayObserverPageSizeClamp proves page_size above the hard cap is
// clamped and the response reflects the clamped value.
func TestRelayObserverPageSizeClamp(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs:      &fakeObserverQueryStore{},
		timeout: time.Second,
		ok:      true,
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/sessions?page_size=1000")
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	data, ok := body["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(100), data["page_size"], "the response must reflect the clamped page size")
}

// TestRelayObserverBadParams proves user input errors are 400 with the
// generic bad-request code: unparsable page size, from after to, an
// unparsable ip, an invalid ip_trust, and a malformed path uuid.
func TestRelayObserverBadParams(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs:      &fakeObserverQueryStore{},
		timeout: time.Second,
		ok:      true,
	})
	cases := []struct {
		name string
		path string
	}{
		{name: "page size not a number", path: "/api/relay-observer/sessions?page_size=abc"},
		{name: "from after to", path: "/api/relay-observer/sessions?from=2026-08-02T00:00:00Z&to=2026-08-01T00:00:00Z"},
		{name: "span over 31 days", path: "/api/relay-observer/sessions?from=2026-01-01T00:00:00Z&to=2026-03-01T00:00:00Z"},
		{name: "from not rfc3339", path: "/api/relay-observer/sessions?from=not-a-time"},
		{name: "invalid ip", path: "/api/relay-observer/sessions?ip=999.1.1.1"},
		{name: "invalid ip_trust", path: "/api/relay-observer/sessions?ip_trust=root"},
		{name: "oversized node_scope", path: "/api/relay-observer/sessions?node_scope=" + strings.Repeat("a", 65)},
		{name: "oversized cursor", path: "/api/relay-observer/sessions?cursor=" + strings.Repeat("a", 513)},
		{name: "malformed session uuid", path: "/api/relay-observer/sessions/not-a-uuid"},
		{name: "malformed turn uuid", path: "/api/relay-observer/turns/not-a-uuid/context?session_id=00000000-0000-0000-0000-000000000001"},
		{name: "context without session_id", path: "/api/relay-observer/turns/00000000-0000-0000-0000-000000000001/context"},
		{name: "context with malformed session_id", path: "/api/relay-observer/turns/00000000-0000-0000-0000-000000000001/context?session_id=not-a-uuid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := rootObserverRequest(t, engine, rootToken, tc.path)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			var body map[string]any
			require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
			assert.Equal(t, false, body["success"])
			assert.Equal(t, "RELAY_OBSERVER_BAD_REQUEST", body["code"])
		})
	}
}

// TestRelayObserverMalformedCursorCode proves a store-reported malformed
// cursor maps onto 400 with the dedicated code, not the generic one.
func TestRelayObserverMalformedCursorCode(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			sessionsErr: &relayobserver.QueryError{Kind: relayobserver.QueryErrMalformedCursor, Msg: "cursor is not valid base64url"},
		},
		timeout: time.Second,
		ok:      true,
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/sessions?cursor=%%%bad")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	assert.Equal(t, "RELAY_OBSERVER_MALFORMED_CURSOR", body["code"])
}

// TestRelayObserverNotFound proves a missing session maps onto 404 with the
// not-found code, on both the session summary and the session turns routes.
func TestRelayObserverNotFound(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			sessionErr: &relayobserver.QueryError{Kind: relayobserver.QueryErrNotFound, Msg: "session not found"},
		},
		timeout: time.Second,
		ok:      true,
	})
	for _, path := range []string{
		"/api/relay-observer/sessions/00000000-0000-0000-0000-0000000000aa",
		"/api/relay-observer/sessions/00000000-0000-0000-0000-0000000000aa/turns",
	} {
		rec := rootObserverRequest(t, engine, rootToken, path)
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
		var body map[string]any
		require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
		assert.Equal(t, "RELAY_OBSERVER_NOT_FOUND", body["code"])
	}
}

// TestRelayObserverDegradedTimeout proves a timeout classification (typed
// QueryError or a raw context deadline) produces HTTP 200 with the degraded
// envelope and reason "timeout".
func TestRelayObserverDegradedTimeout(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	cases := []struct {
		name string
		err  error
	}{
		{name: "typed query timeout", err: &relayobserver.QueryError{Kind: relayobserver.QueryErrTimeout, Msg: "query slot busy"}},
		{name: "context deadline", err: context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			injectObserverRuntime(t, &fakeObserverRuntime{
				qs:      &fakeObserverQueryStore{sessionsErr: tc.err},
				timeout: time.Second,
				ok:      true,
			})
			rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/sessions")
			assert.Equal(t, http.StatusOK, rec.Code)
			var body map[string]any
			require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
			assert.Equal(t, true, body["success"])
			data, ok := body["data"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, true, data["degraded"])
			assert.Equal(t, "timeout", data["reason"])
			assert.NotContains(t, rec.Body.String(), tc.err.Error(), "the raw error must never reach the response")
		})
	}
}

// TestRelayObserverDegradedUnavailable proves a store failure (any error that
// is not a typed query error) produces the degraded envelope with reason
// "unavailable", and that the raw error text — including DSN and HMAC
// material — never reaches the response.
func TestRelayObserverDegradedUnavailable(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	raw := "dial tcp 127.0.0.1:55433: connect: connection refused while opening postgres://obs:topsecretpw@127.0.0.1:55433/relay_observer (hmac=hushhush)"
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			overviewErr: fmt.Errorf("%s", raw),
		},
		timeout: time.Second,
		ok:      true,
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/overview")
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	assert.Equal(t, true, body["success"])
	data, ok := body["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, data["degraded"])
	assert.Equal(t, "unavailable", data["reason"])
	bodyText := rec.Body.String()
	assert.NotContains(t, bodyText, "topsecretpw")
	assert.NotContains(t, bodyText, "hushhush")
	assert.NotContains(t, bodyText, "55433")
	assert.NotContains(t, bodyText, "postgres://")
}

// TestRelayObserverDegradedUnavailableSurface proves an unavailable query
// surface (disabled runtime or unwired) also produces the degraded envelope
// with reason "unavailable", before any store call.
func TestRelayObserverDegradedUnavailableSurface(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	injectObserverRuntime(t, &fakeObserverRuntime{qs: nil, timeout: 0, ok: false})
	for _, path := range observerQueryRequestPaths {
		rec := rootObserverRequest(t, engine, rootToken, path)
		assert.Equal(t, http.StatusOK, rec.Code, path)
		var body map[string]any
		require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
		data, ok := body["data"].(map[string]any)
		require.True(t, ok, path)
		assert.Equal(t, true, data["degraded"], path)
		assert.Equal(t, "unavailable", data["reason"], path)
	}
}

// TestRelayObserverOverviewSuccess proves the overview success path: window
// metadata with snake_case tags, the bounded window list, and the totals.
func TestRelayObserverOverviewSuccess(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	now := time.Now().UTC()
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			overviewResult: relayobserver.OverviewResult{
				WindowSeconds: 300,
				Windows: []relayobserver.OverviewWindow{
					{Start: now.Add(-300 * time.Second), Turns: 2, Success: 1},
					{Start: now, Turns: 1, Success: 1},
				},
				SessionCount: 9,
				TurnCount:    3,
				GapCount:     1,
			},
		},
		timeout: time.Second,
		ok:      true,
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/overview?window_seconds=300&windows=12")
	assert.Equal(t, http.StatusOK, rec.Code)
	bodyText := rec.Body.String()
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(bodyText), &body))
	data := body["data"].(map[string]any)
	assert.Equal(t, float64(300), data["window_seconds"])
	assert.Equal(t, float64(9), data["session_count"])
	assert.Equal(t, float64(3), data["turn_count"])
	assert.Equal(t, float64(1), data["gap_count"])
	windows := data["windows"].([]any)
	require.Len(t, windows, 2)
	first := windows[0].(map[string]any)
	assert.Equal(t, float64(2), first["turns"])
	assert.Equal(t, float64(1), first["success"])
	// The DTO must be snake_case, never Go field names.
	assert.NotContains(t, bodyText, "WindowSeconds")
	assert.NotContains(t, bodyText, "SessionCount")
}

// TestRelayObserverSessionsSuccess proves the session list success path: the
// page payload carries items, the keyset meta, and the clamped page size.
func TestRelayObserverSessionsSuccess(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	sid := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			sessionPage: relayobserver.SessionPage{
				Items: []relayobserver.SessionSummary{
					{SessionID: sid, NodeScope: "node-a", UserID: 7, ClientFamily: "codex", TurnCount: 4, GapCount: 1},
				},
				Meta: relayobserver.PageMeta{NextCursor: "next-cursor", HasMore: true},
			},
		},
		timeout: time.Second,
		ok:      true,
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/sessions?page_size=50")
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	data := body["data"].(map[string]any)
	assert.Equal(t, float64(50), data["page_size"])
	items := data["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	assert.Equal(t, sid.String(), item["session_id"])
	assert.Equal(t, "node-a", item["node_scope"])
	assert.Equal(t, float64(4), item["turn_count"])
	meta := data["meta"].(map[string]any)
	assert.Equal(t, "next-cursor", meta["next_cursor"])
	assert.Equal(t, true, meta["has_more"])
}

// TestRelayObserverSessionSummarySuccess proves GET /sessions/:id returns the
// single metadata row.
func TestRelayObserverSessionSummarySuccess(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	sid := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			sessionRow: relayobserver.SessionSummary{SessionID: sid, NodeScope: "node-a", UserID: 7, ClientFamily: "claude", TurnCount: 2},
		},
		timeout: time.Second,
		ok:      true,
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/sessions/"+sid.String())
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	item := body["data"].(map[string]any)
	assert.Equal(t, sid.String(), item["session_id"])
	assert.Equal(t, "claude", item["client_family"])
	assert.Equal(t, float64(2), item["turn_count"])
}

// TestRelayObserverTurnContextSuccess proves GET /turns/:id/context returns
// the bounded reconstruction with its canonical items.
func TestRelayObserverTurnContextSuccess(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	turnID := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			contextResult: relayobserver.TurnContextResult{
				TurnID:  turnID,
				Ordinal: 3,
				Items: []relayobserver.CanonicalItem{
					{Kind: "text", Role: "user", Content: []relayobserver.CanonicalPart{{Type: "text", Text: "hello"}}, LogicalBytes: 5, Hmac: "aa"},
				},
			},
		},
		timeout: time.Second,
		ok:      true,
		hmacKey: "test-hmac-key",
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/turns/"+turnID.String()+"/context?session_id=00000000-0000-0000-0000-0000000000bb")
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	data := body["data"].(map[string]any)
	assert.Equal(t, turnID.String(), data["turn_id"])
	assert.Equal(t, float64(3), data["ordinal"])
	items := data["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	assert.Equal(t, "text", item["kind"])
	assert.Equal(t, "hello", item["content"].([]any)[0].(map[string]any)["text"])
}

// TestRelayObserverTurnContextMissing proves a turn with no context row maps
// onto 404 with the not-found code.
func TestRelayObserverTurnContextMissing(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs: &fakeObserverQueryStore{
			contextErr: &relayobserver.ContentError{Code: relayobserver.ContentErrMissingContext, Msg: "turn has no context row"},
		},
		timeout: time.Second,
		ok:      true,
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/turns/00000000-0000-0000-0000-0000000000aa/context?session_id=00000000-0000-0000-0000-0000000000bb")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	assert.Equal(t, "RELAY_OBSERVER_NOT_FOUND", body["code"])
}

// TestRelayObserverTurnContextCorruptDegraded proves a corrupt content error
// (a store-side problem, not a missing resource) degrades like any other
// store failure: HTTP 200 with reason "unavailable" and no raw error text.
func TestRelayObserverTurnContextCorruptDegraded(t *testing.T) {
	engine, rootToken, _, _ := relayObserverTestEnv(t)
	corrupt := &relayobserver.ContentError{Code: relayobserver.ContentErrCorrupt, Msg: "full checkpoint declares 2 digests, row says 1"}
	injectObserverRuntime(t, &fakeObserverRuntime{
		qs:      &fakeObserverQueryStore{contextErr: corrupt},
		timeout: time.Second,
		ok:      true,
	})
	rec := rootObserverRequest(t, engine, rootToken, "/api/relay-observer/turns/00000000-0000-0000-0000-0000000000aa/context?session_id=00000000-0000-0000-0000-0000000000bb")
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, common.Unmarshal([]byte(rec.Body.String()), &body))
	data := body["data"].(map[string]any)
	assert.Equal(t, true, data["degraded"])
	assert.Equal(t, "unavailable", data["reason"])
	assert.NotContains(t, rec.Body.String(), "digests")
}
