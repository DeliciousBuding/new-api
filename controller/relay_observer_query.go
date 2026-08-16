package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	relayobserver "github.com/QuantumNous/new-api/pkg/relay_observer"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// This file hosts the Root-only observer query surface: overview, session
// list, session summary, session turns, and turn context. Every handler runs
// behind middleware.RootAuth (Admin and User receive 403), bounds the query
// with the runtime's stored query timeout, and maps failures onto the SSOT
// envelope:
//   - user input errors: 400 + {"success":false,"code":"...","message":"..."}
//     (RELAY_OBSERVER_MALFORMED_CURSOR for a malformed cursor, otherwise
//     RELAY_OBSERVER_BAD_REQUEST);
//   - missing session/turn: 404 + RELAY_OBSERVER_NOT_FOUND;
//   - store failures and timeouts: 200 + the degraded envelope
//     {"success":true,"message":"","data":{"degraded":true,"reason":...}}.
//
// Internal error text never crosses the API boundary: it goes to the log
// only. Secrets (DSN, HMAC keys, session ids) never appear in responses.

// Observer query error codes of the Root API (SSOT: explicit machine-readable
// codes, never free-form text).
const (
	relayObserverErrBadRequest      = "RELAY_OBSERVER_BAD_REQUEST"
	relayObserverErrMalformedCursor = "RELAY_OBSERVER_MALFORMED_CURSOR"
	relayObserverErrNotFound        = "RELAY_OBSERVER_NOT_FOUND"
)

// Parameter bounds of the Root query API (SSOT: explicit length, enum,
// time-range, and page-size validation). The page-size clamp mirrors the
// query port's [DefaultPageSize, MaxPageSize]; the overview window clamp is
// the second line of defense behind the port's own caps.
const (
	relayObserverMaxNodeScopeLen = 64
	relayObserverMaxCursorLen    = 512
	relayObserverMaxNameLen      = 128
	relayObserverMaxCountryLen   = 16
	relayObserverMaxWindowSpan   = 31 * 24 * time.Hour
	relayObserverMaxWindowSecs   = 3600
	relayObserverMaxWindowCount  = 48
)

// writeRelayObserverDegraded emits the SSOT degraded envelope: HTTP 200 with
// a short human message and a stable reason ("timeout" or "unavailable").
// Internal error text is never included — it goes to the log only.
func writeRelayObserverDegraded(c *gin.Context, reason, message string) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"degraded": true,
			"reason":   reason,
			"message":  message,
		},
	})
}

// writeRelayObserverBadRequest emits a 400 user-input error with the generic
// bad-request code.
func writeRelayObserverBadRequest(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{
		"success": false,
		"code":    relayObserverErrBadRequest,
		"message": message,
	})
}

// relayObserverQueryError maps one store-level query failure onto the API
// contract: malformed cursors are 400 with their dedicated code, missing rows
// are 404, timeouts and store failures are the degraded envelope. The raw
// error text is logged (common.SysError) and never appears in the response.
func relayObserverQueryError(c *gin.Context, op string, err error) {
	var qe *relayobserver.QueryError
	if errors.As(err, &qe) {
		switch qe.Kind {
		case relayobserver.QueryErrMalformedCursor:
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"code":    relayObserverErrMalformedCursor,
				"message": "malformed cursor",
			})
			return
		case relayobserver.QueryErrNotFound:
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"code":    relayObserverErrNotFound,
				"message": "not found",
			})
			return
		case relayobserver.QueryErrTimeout:
			writeRelayObserverDegraded(c, "timeout", "the observer query timed out")
			return
		case relayobserver.QueryErrResultTooLarge:
			writeRelayObserverDegraded(c, "unavailable", "the observer query result exceeded its bounds")
			return
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeRelayObserverDegraded(c, "timeout", "the observer query timed out")
		return
	}
	// A turn with no context row is a missing resource, not a store failure.
	if code, ok := relayobserver.ContentErrorOf(err); ok && code == relayobserver.ContentErrMissingContext {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"code":    relayObserverErrNotFound,
			"message": "not found",
		})
		return
	}
	common.SysError("relay observer " + op + " failed: " + err.Error())
	writeRelayObserverDegraded(c, "unavailable", "the observer query service is unavailable")
}

// observerQuerySurface resolves the bounded query surface of the current
// runtime. An unwired runtime or an unavailable surface writes the degraded
// envelope and reports false, so the caller returns without touching the
// store.
func observerQuerySurface(c *gin.Context) (relayobserver.QueryStore, time.Duration, bool) {
	relayObserverMu.RLock()
	rt := relayObserverRT
	relayObserverMu.RUnlock()
	if rt == nil {
		writeRelayObserverDegraded(c, "unavailable", "the observer is not configured")
		return nil, 0, false
	}
	qs, timeout, ok := rt.QuerySurface()
	if !ok || qs == nil {
		writeRelayObserverDegraded(c, "unavailable", "the observer query service is unavailable")
		return nil, 0, false
	}
	return qs, timeout, true
}

// ---------------------------------------------------------------------------
// parameter parsing

// parseBoundedLenParam reads one string parameter and enforces its explicit
// length cap; an oversized value is a 400.
func parseBoundedLenParam(c *gin.Context, name string, max int) (string, bool) {
	raw := c.Query(name)
	if len(raw) > max {
		writeRelayObserverBadRequest(c, name+" must not exceed "+strconv.Itoa(max)+" characters")
		return "", false
	}
	return raw, true
}

// parsePageSize reads page_size and clamps it into the query port's
// [DefaultPageSize, MaxPageSize] range. Empty means the default; an
// unparsable or negative value is a 400.
func parsePageSize(c *gin.Context) (int, bool) {
	raw := strings.TrimSpace(c.Query("page_size"))
	if raw == "" {
		return relayobserver.DefaultPageSize, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		writeRelayObserverBadRequest(c, "page_size must be a non-negative integer")
		return 0, false
	}
	if n == 0 {
		return relayobserver.DefaultPageSize, true
	}
	if n > relayobserver.MaxPageSize {
		n = relayobserver.MaxPageSize
	}
	return n, true
}

// parseCursor reads the opaque keyset cursor and enforces its length cap. The
// cursor format itself is validated by the query port, which reports a
// malformed cursor with its dedicated code.
func parseCursor(c *gin.Context) (string, bool) {
	raw := c.Query("cursor")
	if len(raw) > relayObserverMaxCursorLen {
		writeRelayObserverBadRequest(c, "cursor must not exceed "+strconv.Itoa(relayObserverMaxCursorLen)+" characters")
		return "", false
	}
	return raw, true
}

// parseTimeRange reads the optional from/to RFC3339 bounds. Both empty means
// unbounded; an unparsable value, from > to, or a span above 31 days is a
// 400.
func parseTimeRange(c *gin.Context) (time.Time, time.Time, bool) {
	fromRaw := strings.TrimSpace(c.Query("from"))
	toRaw := strings.TrimSpace(c.Query("to"))
	if fromRaw == "" && toRaw == "" {
		return time.Time{}, time.Time{}, true
	}
	var from, to time.Time
	var err error
	if fromRaw != "" {
		from, err = time.Parse(time.RFC3339, fromRaw)
		if err != nil {
			writeRelayObserverBadRequest(c, "from must be an RFC3339 timestamp")
			return time.Time{}, time.Time{}, false
		}
	}
	if toRaw != "" {
		to, err = time.Parse(time.RFC3339, toRaw)
		if err != nil {
			writeRelayObserverBadRequest(c, "to must be an RFC3339 timestamp")
			return time.Time{}, time.Time{}, false
		}
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		writeRelayObserverBadRequest(c, "from must not be after to")
		return time.Time{}, time.Time{}, false
	}
	if !from.IsZero() && !to.IsZero() && to.Sub(from) > relayObserverMaxWindowSpan {
		writeRelayObserverBadRequest(c, "the from/to span must not exceed 31 days")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// parseBoolParam reads one optional boolean filter; an unparsable value is a
// 400.
func parseBoolParam(c *gin.Context, name string) (*bool, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		writeRelayObserverBadRequest(c, name+" must be true or false")
		return nil, false
	}
	return &v, true
}

// parsePositiveInt64Param reads one optional non-negative integer filter.
func parsePositiveInt64Param(c *gin.Context, name string) (int64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		writeRelayObserverBadRequest(c, name+" must be a non-negative integer")
		return 0, false
	}
	return n, true
}

// parseIPParam reads one optional client IP filter; an unparsable value is a
// 400.
func parseIPParam(c *gin.Context, name string) (net.IP, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		writeRelayObserverBadRequest(c, name+" must be a valid IP address")
		return nil, false
	}
	return ip, true
}

// parseIPTrustParam reads the optional ip_trust filter against the IPTrust
// whitelist; anything else is a 400.
func parseIPTrustParam(c *gin.Context) (relayobserver.IPTrust, bool) {
	raw := strings.TrimSpace(c.Query("ip_trust"))
	if raw == "" {
		return "", true
	}
	switch relayobserver.IPTrust(raw) {
	case relayobserver.IPTrustDirect, relayobserver.IPTrustProxy, relayobserver.IPTrustNone:
		return relayobserver.IPTrust(raw), true
	}
	writeRelayObserverBadRequest(c, "ip_trust must be one of direct, proxy, none")
	return "", false
}

// parseUUIDParam reads one path id and validates it as a UUID.
func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	raw := strings.TrimSpace(c.Param(name))
	id, err := uuid.Parse(raw)
	if err != nil {
		writeRelayObserverBadRequest(c, name+" must be a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

// parseSessionQuery assembles the bounded SessionQuery from validated query
// parameters; any validation failure writes the 400 and reports false.
func parseSessionQuery(c *gin.Context) (relayobserver.SessionQuery, bool) {
	nodeScope, ok := parseBoundedLenParam(c, "node_scope", relayObserverMaxNodeScopeLen)
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	userID, ok := parsePositiveInt64Param(c, "user_id")
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	clientFamily, ok := parseBoundedLenParam(c, "client_family", relayObserverMaxNameLen)
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	model, ok := parseBoundedLenParam(c, "model", relayObserverMaxNameLen)
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	success, ok := parseBoolParam(c, "success")
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	country, ok := parseBoundedLenParam(c, "country", relayObserverMaxCountryLen)
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	asn, ok := parsePositiveInt64Param(c, "asn")
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	ip, ok := parseIPParam(c, "ip")
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	ipTrust, ok := parseIPTrustParam(c)
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	from, to, ok := parseTimeRange(c)
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	pageSize, ok := parsePageSize(c)
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	cursor, ok := parseCursor(c)
	if !ok {
		return relayobserver.SessionQuery{}, false
	}
	return relayobserver.SessionQuery{
		NodeScope:    nodeScope,
		UserID:       userID,
		ClientFamily: clientFamily,
		Model:        model,
		Success:      success,
		Country:      country,
		ASN:          asn,
		IP:           ip,
		IPTrust:      ipTrust,
		From:         from,
		To:           to,
		PageSize:     pageSize,
		Cursor:       cursor,
	}, true
}

// parseTurnQuery assembles the bounded TurnQuery from validated query
// parameters; any validation failure writes the 400 and reports false.
func parseTurnQuery(c *gin.Context, sessionID *uuid.UUID) (relayobserver.TurnQuery, bool) {
	userID, ok := parsePositiveInt64Param(c, "user_id")
	if !ok {
		return relayobserver.TurnQuery{}, false
	}
	model, ok := parseBoundedLenParam(c, "model", relayObserverMaxNameLen)
	if !ok {
		return relayobserver.TurnQuery{}, false
	}
	success, ok := parseBoolParam(c, "success")
	if !ok {
		return relayobserver.TurnQuery{}, false
	}
	errorType, ok := parseBoundedLenParam(c, "error_type", relayObserverMaxNameLen)
	if !ok {
		return relayobserver.TurnQuery{}, false
	}
	ipTrust, ok := parseIPTrustParam(c)
	if !ok {
		return relayobserver.TurnQuery{}, false
	}
	pageSize, ok := parsePageSize(c)
	if !ok {
		return relayobserver.TurnQuery{}, false
	}
	cursor, ok := parseCursor(c)
	if !ok {
		return relayobserver.TurnQuery{}, false
	}
	return relayobserver.TurnQuery{
		SessionID: sessionID,
		UserID:    userID,
		Model:     model,
		Success:   success,
		ErrorType: errorType,
		IPTrust:   ipTrust,
		PageSize:  pageSize,
		Cursor:    cursor,
	}, true
}

// parseOverviewWindow reads the two bounded overview parameters: empty keeps
// the port defaults, unparsable or negative values are a 400, and values
// above the hard maximum are clamped (the second line of defense behind the
// port's own caps).
func parseOverviewWindow(c *gin.Context) (windowSeconds, windows int, ok bool) {
	raw := strings.TrimSpace(c.Query("window_seconds"))
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeRelayObserverBadRequest(c, "window_seconds must be a positive integer")
			return 0, 0, false
		}
		if n > relayObserverMaxWindowSecs {
			n = relayObserverMaxWindowSecs
		}
		windowSeconds = n
	}
	raw = strings.TrimSpace(c.Query("windows"))
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeRelayObserverBadRequest(c, "windows must be a positive integer")
			return 0, 0, false
		}
		if n > relayObserverMaxWindowCount {
			n = relayObserverMaxWindowCount
		}
		windows = n
	}
	return windowSeconds, windows, true
}

// ---------------------------------------------------------------------------
// response DTOs (snake_case JSON tags; the query port stays JSON-free)

type observerOverviewWindowDTO struct {
	Start   time.Time `json:"start"`
	Turns   int64     `json:"turns"`
	Success int64     `json:"success"`
}

type observerOverviewDTO struct {
	WindowSeconds int                         `json:"window_seconds"`
	Windows       []observerOverviewWindowDTO `json:"windows"`
	SessionCount  int64                       `json:"session_count"`
	TurnCount     int64                       `json:"turn_count"`
	GapCount      int64                       `json:"gap_count"`
}

type observerSessionDTO struct {
	SessionID    string    `json:"session_id"`
	NodeScope    string    `json:"node_scope"`
	UserID       int64     `json:"user_id"`
	Username     string    `json:"username,omitempty"`
	ClientFamily string    `json:"client_family"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	TurnCount    int64     `json:"turn_count"`
	GapCount     int64     `json:"gap_count"`
}

// resolveSessionUsernames maps user ids of one session page onto their
// usernames from the primary users table. The observer store lives in its
// own database (no cross-database join), and a user that was deleted since
// the session was captured simply stays absent — the caller renders the
// numeric id in that case.
func resolveSessionUsernames(sessions []relayobserver.SessionSummary) map[int64]string {
	out := make(map[int64]string, len(sessions))
	ids := make([]int64, 0, len(sessions))
	seen := make(map[int64]bool, len(sessions))
	for _, s := range sessions {
		if s.UserID <= 0 || seen[s.UserID] {
			continue
		}
		seen[s.UserID] = true
		ids = append(ids, s.UserID)
	}
	if len(ids) == 0 {
		return out
	}
	var users []model.User
	if err := model.DB.Select("id", "username").Where("id IN ?", ids).Find(&users).Error; err != nil {
		// Usernames are a display convenience; a lookup failure must not
		// degrade the session list.
		common.SysError("relay observer resolve usernames: " + err.Error())
		return out
	}
	for _, u := range users {
		out[int64(u.Id)] = u.Username
	}
	return out
}

type observerTurnDTO struct {
	TurnID           string                         `json:"turn_id"`
	EventID          string                         `json:"event_id"`
	SessionID        *string                        `json:"session_id"`
	OccurredAt       time.Time                      `json:"occurred_at"`
	NodeScope        string                         `json:"node_scope"`
	UserID           int64                          `json:"user_id"`
	Model            string                         `json:"model"`
	UpstreamModel    string                         `json:"upstream_model"`
	RelayFormat      string                         `json:"relay_format"`
	Success          bool                           `json:"success"`
	StatusCode       int                            `json:"status_code"`
	ErrorType        string                         `json:"error_type"`
	ErrorCode        string                         `json:"error_code"`
	LatencyMS        int64                          `json:"latency_ms"`
	Stream           bool                           `json:"stream"`
	PromptTokens     int64                          `json:"prompt_tokens"`
	CompletionTokens int64                          `json:"completion_tokens"`
	CachedTokens     int64                          `json:"cached_tokens"`
	Quota            int64                          `json:"quota"`
	Attempts         []relayobserver.AttemptSummary `json:"attempts"`
	ContentState     string                         `json:"content_state"`
}

type observerPageMetaDTO struct {
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

type observerTurnContextDTO struct {
	TurnID  string                        `json:"turn_id"`
	Ordinal int                           `json:"ordinal"`
	Items   []relayobserver.CanonicalItem `json:"items"`
}

// ---------------------------------------------------------------------------
// handlers

// GetRelayObserverOverview serves GET /api/relay-observer/overview: the
// bounded aggregate windows and totals of the last covered span.
func GetRelayObserverOverview(c *gin.Context) {
	qs, timeout, ok := observerQuerySurface(c)
	if !ok {
		return
	}
	windowSeconds, windows, ok := parseOverviewWindow(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	out, err := qs.Overview(ctx, relayobserver.OverviewQuery{WindowSeconds: windowSeconds, Windows: windows})
	if err != nil {
		relayObserverQueryError(c, "overview", err)
		return
	}
	winDTOs := make([]observerOverviewWindowDTO, 0, len(out.Windows))
	for _, w := range out.Windows {
		winDTOs = append(winDTOs, observerOverviewWindowDTO{Start: w.Start, Turns: w.Turns, Success: w.Success})
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": observerOverviewDTO{
			WindowSeconds: out.WindowSeconds,
			Windows:       winDTOs,
			SessionCount:  out.SessionCount,
			TurnCount:     out.TurnCount,
			GapCount:      out.GapCount,
		},
	})
}

// GetRelayObserverSessions serves GET /api/relay-observer/sessions: one
// keyset page of session summaries with the bounded filters.
func GetRelayObserverSessions(c *gin.Context) {
	qs, timeout, ok := observerQuerySurface(c)
	if !ok {
		return
	}
	query, ok := parseSessionQuery(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	page, err := qs.ListSessions(ctx, query)
	if err != nil {
		relayObserverQueryError(c, "sessions", err)
		return
	}
	// The observer store lives in its own database, so usernames cannot be
	// joined in SQL: resolve them here against the primary users table and
	// attach them to the DTOs (missing users render as their numeric id).
	usernames := resolveSessionUsernames(page.Items)
	items := make([]observerSessionDTO, 0, len(page.Items))
	for _, s := range page.Items {
		items = append(items, observerSessionDTO{
			SessionID:    s.SessionID.String(),
			NodeScope:    s.NodeScope,
			UserID:       s.UserID,
			Username:     usernames[s.UserID],
			ClientFamily: s.ClientFamily,
			FirstSeen:    s.FirstSeen,
			LastSeen:     s.LastSeen,
			TurnCount:    s.TurnCount,
			GapCount:     s.GapCount,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"page_size": query.PageSize,
			"items":     items,
			"meta": observerPageMetaDTO{
				NextCursor: page.Meta.NextCursor,
				HasMore:    page.Meta.HasMore,
			},
		},
	})
}

// GetRelayObserverSession serves GET /api/relay-observer/sessions/:id: the
// metadata row of one session; an unknown session is a 404.
func GetRelayObserverSession(c *gin.Context) {
	qs, timeout, ok := observerQuerySurface(c)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	s, err := qs.GetSession(ctx, id)
	if err != nil {
		relayObserverQueryError(c, "session", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": observerSessionDTO{
			SessionID:    s.SessionID.String(),
			NodeScope:    s.NodeScope,
			UserID:       s.UserID,
			ClientFamily: s.ClientFamily,
			FirstSeen:    s.FirstSeen,
			LastSeen:     s.LastSeen,
			TurnCount:    s.TurnCount,
			GapCount:     s.GapCount,
		},
	})
}

// GetRelayObserverSessionTurns serves GET /api/relay-observer/sessions/:id/
// turns: one keyset page of one session's turns. An unknown session is a 404
// even when no turns match, so the resource itself is what the client asked
// for.
func GetRelayObserverSessionTurns(c *gin.Context) {
	qs, timeout, ok := observerQuerySurface(c)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	query, ok := parseTurnQuery(c, &id)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	if _, err := qs.GetSession(ctx, id); err != nil {
		relayObserverQueryError(c, "session", err)
		return
	}
	page, err := qs.ListTurns(ctx, query)
	if err != nil {
		relayObserverQueryError(c, "session turns", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"page_size": query.PageSize,
			"items":     observerTurnsDTO(page.Items),
			"meta": observerPageMetaDTO{
				NextCursor: page.Meta.NextCursor,
				HasMore:    page.Meta.HasMore,
			},
		},
	})
}

// parseTranscriptQuery assembles the bounded TranscriptQuery: direction is
// whitelisted ("latest" default, "older"), cursor is a non-negative message
// index, and page_size reuses the shared clamp.
func parseTranscriptQuery(c *gin.Context, sessionID uuid.UUID) (relayobserver.TranscriptQuery, bool) {
	direction := strings.ToLower(strings.TrimSpace(c.Query("direction")))
	if direction == "" {
		direction = relayobserver.TranscriptDirLatest
	}
	if direction != relayobserver.TranscriptDirLatest && direction != relayobserver.TranscriptDirOlder {
		writeRelayObserverBadRequest(c, "direction must be one of latest, older")
		return relayobserver.TranscriptQuery{}, false
	}
	var cursor int64
	raw := strings.TrimSpace(c.Query("cursor"))
	if raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeRelayObserverBadRequest(c, "cursor must be a non-negative integer")
			return relayobserver.TranscriptQuery{}, false
		}
		cursor = n
	}
	pageSize, ok := parsePageSize(c)
	if !ok {
		return relayobserver.TranscriptQuery{}, false
	}
	return relayobserver.TranscriptQuery{
		SessionID: sessionID,
		Direction: direction,
		Cursor:    cursor,
		PageSize:  pageSize,
		HMACKey:   relayObserverHMACKey(),
	}, true
}

// observerTranscriptMessageDTO is one flattened message of a session
// transcript.
type observerTranscriptMessageDTO struct {
	TurnID       string                        `json:"turn_id"`
	TurnSeq      int64                         `json:"turn_seq"`
	Seq          int64                         `json:"seq"`
	Kind         string                        `json:"kind"`
	Role         string                        `json:"role,omitempty"`
	Content      []relayobserver.CanonicalPart `json:"content,omitempty"`
	Gap          *relayobserver.GapInfo        `json:"gap,omitempty"`
	LogicalBytes int64                         `json:"logical_bytes"`
	Hmac         string                        `json:"hmac"`
	Truncated    bool                          `json:"truncated,omitempty"`
}

// GetRelayObserverSessionTranscript serves GET /api/relay-observer/sessions/
// :id/transcript: one page of the session's flattened conversation stream.
// An unknown session is a 404 even when the stream is empty.
func GetRelayObserverSessionTranscript(c *gin.Context) {
	qs, timeout, ok := observerQuerySurface(c)
	if !ok {
		return
	}
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	query, ok := parseTranscriptQuery(c, id)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	if _, err := qs.GetSession(ctx, id); err != nil {
		relayObserverQueryError(c, "session", err)
		return
	}
	page, err := qs.Transcript(ctx, query)
	if err != nil {
		relayObserverQueryError(c, "session transcript", err)
		return
	}
	items := make([]observerTranscriptMessageDTO, 0, len(page.Items))
	for _, m := range page.Items {
		items = append(items, observerTranscriptMessageDTO{
			TurnID:       m.TurnID.String(),
			TurnSeq:      m.TurnSeq,
			Seq:          m.Seq,
			Kind:         m.Kind,
			Role:         m.Role,
			Content:      m.Content,
			Gap:          m.Gap,
			LogicalBytes: m.LogicalBytes,
			Hmac:         m.Hmac,
			Truncated:    m.Truncated,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"page_size": query.PageSize,
			"items":     items,
			"meta": gin.H{
				"prev_cursor": page.PrevCursor,
				"has_older":   page.HasOlder,
			},
		},
	})
}

// GetRelayObserverTurnContext serves GET /api/relay-observer/turns/:id/
// context: the bounded reconstruction of one turn's content. The session_id
// query parameter is mandatory (the reconstruction is keyed by session).
func GetRelayObserverTurnContext(c *gin.Context) {
	qs, timeout, ok := observerQuerySurface(c)
	if !ok {
		return
	}
	turnID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	sessionRaw := strings.TrimSpace(c.Query("session_id"))
	if sessionRaw == "" {
		writeRelayObserverBadRequest(c, "session_id query parameter is required")
		return
	}
	sessionID, err := uuid.Parse(sessionRaw)
	if err != nil {
		writeRelayObserverBadRequest(c, "session_id must be a valid UUID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	out, err := qs.TurnContext(ctx, relayobserver.ContextQuery{SessionID: sessionID, TurnID: turnID, HMACKey: relayObserverHMACKey()})
	if err != nil {
		relayObserverQueryError(c, "turn context", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": observerTurnContextDTO{
			TurnID:  out.TurnID.String(),
			Ordinal: out.Ordinal,
			Items:   out.Items,
		},
	})
}

// relayObserverHMACKey returns the content HMAC key of the current runtime
// (empty skips the digest re-verification).
func relayObserverHMACKey() string {
	relayObserverMu.RLock()
	rt := relayObserverRT
	relayObserverMu.RUnlock()
	if rt == nil {
		return ""
	}
	return rt.HMACKey()
}

// observerTurnsDTO converts one page of turn summaries into the response DTO
// shape.
func observerTurnsDTO(items []relayobserver.TurnSummary) []observerTurnDTO {
	out := make([]observerTurnDTO, 0, len(items))
	for _, t := range items {
		dto := observerTurnDTO{
			TurnID:           t.TurnID.String(),
			EventID:          t.EventID,
			OccurredAt:       t.OccurredAt,
			NodeScope:        t.NodeScope,
			UserID:           t.UserID,
			Model:            t.Model,
			UpstreamModel:    t.UpstreamModel,
			RelayFormat:      t.RelayFormat,
			Success:          t.Success,
			StatusCode:       t.StatusCode,
			ErrorType:        t.ErrorType,
			ErrorCode:        t.ErrorCode,
			LatencyMS:        t.LatencyMS,
			Stream:           t.Stream,
			PromptTokens:     t.PromptTokens,
			CompletionTokens: t.CompletionTokens,
			CachedTokens:     t.CachedTokens,
			Quota:            t.Quota,
			Attempts:         t.Attempts,
			ContentState:     t.ContentState,
		}
		if t.SessionID != nil {
			sid := t.SessionID.String()
			dto.SessionID = &sid
		}
		out = append(out, dto)
	}
	return out
}
