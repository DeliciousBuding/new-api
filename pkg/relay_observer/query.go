package relayobserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
)

// This file is the T3.1 bounded Root query port and its PostgreSQL
// implementation. The port is an additive extension of the Store surface
// (types.go: "Query and retention surfaces are additive extensions of this
// port added by their owning phases"); the concrete adapter is a wrapper
// around the existing *pgStore so queries share the single dedicated observer
// pool, its 2/1/60s tuning, and the v1 schema with the turn writer. The
// orchestration (keyset decisions, cursor codec, clamping) lives in this file
// as pure functions and seam functions tested against a fake data layer,
// exactly like the dispatcher and the T2.3 append; the SQL itself is
// exercised by the relay_observer_pg_integration suite.
//
// Bounds per the architecture SSOT (Root API And UI, Runtime Limits):
//   - keyset pagination with a cursor, never offset;
//   - page size clamped into [default, 100];
//   - at most one database-backed Root query at a time ("One in-process
//     semaphore admits at most one database-backed Root query");
//   - query timeout and a row cap as the two independent backstops
//     ("query timeout 500ms/2s", LIMIT page_size+1);
//   - list queries (overview/session/turn) never read the content table;
//     only the turn context query reads content objects, bounded to one
//     checkpoint and one suffix row plus their objects (T2.3 reconstruction).

// Page bounds per the Runtime Limits table: list page size default 50,
// hard maximum 100 (reject/clamp — the clamp mirrors common.PageInfo, which
// clamps PageSize above 100 to 100).
const (
	// DefaultPageSize is the page size used when the caller passes none.
	DefaultPageSize = 50
	// MaxPageSize is the hard page-size cap; larger values are clamped.
	MaxPageSize = 100
)

// Overview window bounds: 12 windows of 300 seconds cover the last hour, and
// the window count is clamped to keep the aggregate read bounded.
const (
	// DefaultOverviewWindowSeconds is the default per-window span.
	DefaultOverviewWindowSeconds = 300
	// DefaultOverviewWindows is the default window count.
	DefaultOverviewWindows = 12
	// MaxOverviewWindows is the hard window-count cap.
	MaxOverviewWindows = 48
)

// QueryErrorKind classifies a bounded query failure. The classification is
// stable and secret-free: malformed cursors and timeouts are typed so the
// Root controllers can map them onto the degraded envelope; database errors
// surface unchanged for the controller's store-failure handling.
type QueryErrorKind string

const (
	// QueryErrMalformedCursor marks a cursor that is not valid base64url JSON
	// or does not carry a valid keyset (missing, mistyped, or non-UUID id).
	QueryErrMalformedCursor QueryErrorKind = "malformed_cursor"
	// QueryErrTimeout marks a query that expired its context (caller timeout)
	// before starting or while waiting for the query semaphore.
	QueryErrTimeout QueryErrorKind = "timeout"
	// QueryErrNotFound marks a query whose target row does not exist (for
	// example GET /sessions/:id of an unknown session).
	QueryErrNotFound QueryErrorKind = "not_found"
)

// QueryError is a classified bounded query failure. Kind is stable and
// secret-free; Msg and Err carry context for logs.
type QueryError struct {
	Kind QueryErrorKind
	Msg  string
	Err  error
}

func (e *QueryError) Error() string {
	switch {
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("relayobserver: query %s: %s: %v", e.Kind, e.Msg, e.Err)
	case e.Msg != "":
		return fmt.Sprintf("relayobserver: query %s: %s", e.Kind, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("relayobserver: query %s: %v", e.Kind, e.Err)
	}
	return fmt.Sprintf("relayobserver: query %s", e.Kind)
}

// Unwrap exposes the wrapped cause for errors.Is/As chains.
func (e *QueryError) Unwrap() error { return e.Err }

// classifiedQueryError builds a QueryError around a cause.
func classifiedQueryError(kind QueryErrorKind, msg string, err error) error {
	return &QueryError{Kind: kind, Msg: msg, Err: err}
}

// ---------------------------------------------------------------------------
// frozen query contracts (consumed directly by the T3.2 Root controllers)
// (frozen + T3.2 extensions: GetSession, not_found, and the filter
// dimensions)

// OverviewQuery selects the bounded aggregate windows of GET /overview.
type OverviewQuery struct {
	// WindowSeconds is the per-window span; <= 0 uses DefaultOverviewWindowSeconds.
	WindowSeconds int
	// Windows is the window count; <= 0 uses DefaultOverviewWindows and values
	// above MaxOverviewWindows are clamped.
	Windows int
}

// OverviewWindow is one aggregate window: the turn volume and success count
// of a fixed span.
type OverviewWindow struct {
	// Start is the window start, aligned to the window span.
	Start time.Time
	// Turns is the number of turns in the window.
	Turns int64
	// Success is the number of successful turns in the window.
	Success int64
}

// OverviewResult is the bounded aggregate response of GET /overview. It reads
// metadata columns only, never content objects.
type OverviewResult struct {
	// WindowSeconds echoes the effective window span of the response.
	WindowSeconds int
	// Windows lists the bounded aggregate windows, oldest first.
	Windows []OverviewWindow
	// SessionCount is the total session count.
	SessionCount int64
	// TurnCount is the total turn count.
	TurnCount int64
	// GapCount counts turns whose content capture ended with a gap marker or
	// metadata-only state (the same semantics as Status.ContentGapsTotal).
	GapCount int64
}

// SessionQuery selects one page of GET /sessions. Filters are optional; the
// page is ordered by (last_seen DESC, id DESC) with keyset pagination. The
// turn-derived filters (Model, Success, Country, ASN, IP) are evaluated as
// EXISTS subqueries over observer_turns, reusing the idx_observer_turns_*
// index coverage, because the sessions table carries no per-turn columns.
type SessionQuery struct {
	// NodeScope restricts to one node scope; empty means all.
	NodeScope string
	// UserID restricts to one user; 0 means all.
	UserID int64
	// ClientFamily restricts to one session client family; empty means all.
	ClientFamily string
	// Model restricts to sessions that have at least one turn with this model;
	// empty means all.
	Model string
	// Success restricts to sessions that have at least one successful (or,
	// when false, failed) turn; nil means all.
	Success *bool
	// Country restricts to sessions with at least one turn from this country
	// code; empty means all.
	Country string
	// ASN restricts to sessions with at least one turn from this ASN; 0 means
	// all.
	ASN int64
	// IP restricts to sessions with at least one turn from this client IP;
	// nil means all.
	IP net.IP
	// IPTrust restricts to sessions with at least one turn captured at this
	// trust tier; empty means all (T3.2 extension beyond the T3.1 field set,
	// consumed by the Root controller's ip_trust whitelist).
	IPTrust IPTrust
	// From/To bound the session recency by last_seen; zero means unbounded.
	From time.Time
	To   time.Time
	// PageSize is clamped into [DefaultPageSize, MaxPageSize].
	PageSize int
	// Cursor is the opaque keyset cursor of the next page; empty means the
	// first page.
	Cursor string
}

// SessionSummary is one session row of a list page. It carries metadata
// columns only — never session aliases or content.
type SessionSummary struct {
	SessionID    uuid.UUID
	NodeScope    string
	UserID       int64
	ClientFamily string
	FirstSeen    time.Time
	LastSeen     time.Time
	TurnCount    int64
	GapCount     int64
}

// TurnQuery selects one page of GET /sessions/:id/turns or the global turn
// list. The page is ordered by (occurred_at DESC, id DESC) with keyset
// pagination.
type TurnQuery struct {
	// SessionID restricts the page to one session's turns; nil means all.
	SessionID *uuid.UUID
	// UserID restricts to one user; 0 means all.
	UserID int64
	// Model restricts to one model; empty means all.
	Model string
	// Success restricts to successful (or, when false, failed) turns; nil
	// means all.
	Success *bool
	// ErrorType restricts to one error type; empty means all.
	ErrorType string
	// IPTrust restricts to turns captured at this trust tier; empty means all.
	IPTrust IPTrust
	// PageSize is clamped into [DefaultPageSize, MaxPageSize].
	PageSize int
	// Cursor is the opaque keyset cursor of the next page; empty means the
	// first page.
	Cursor string
}

// TurnSummary is one turn row of a list page. It carries metadata columns
// only — the payload column of observer_content_objects is never read.
type TurnSummary struct {
	TurnID           uuid.UUID
	EventID          string
	SessionID        *uuid.UUID
	OccurredAt       time.Time
	NodeScope        string
	UserID           int64
	TokenID          int64
	Model            string
	UpstreamModel    string
	RelayFormat      string
	Success          bool
	StatusCode       int
	ErrorType        string
	ErrorCode        string
	LatencyMS        int64
	Stream           bool
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Quota            int64
	Attempts         []AttemptSummary
	AttemptsOmitted  int
	ContentState     string
	ClientProfile    string
	FirstResponseMS  int64
}

// PageMeta carries the keyset pagination state of one list page.
type PageMeta struct {
	// NextCursor is the opaque cursor of the next page; empty when the page
	// is the last one.
	NextCursor string
	// HasMore reports whether another page exists after this one.
	HasMore bool
}

// SessionPage is one keyset page of sessions.
type SessionPage struct {
	Items []SessionSummary
	Meta  PageMeta
}

// TurnPage is one keyset page of turns.
type TurnPage struct {
	Items []TurnSummary
	Meta  PageMeta
}

// ContextQuery selects the reconstructed content of one turn (GET
// /turns/:id/context). The read is bounded: one context row plus the content
// objects of its digest list, never a full-session scan.
type ContextQuery struct {
	SessionID uuid.UUID
	TurnID    uuid.UUID
	// HMACKey re-verifies every item's content-layer digest during
	// reconstruction; an empty key skips the digest step but keeps the
	// structural checks (the T2.3 ReconstructTurn contract).
	HMACKey string
}

// TurnContextResult is the bounded reconstruction of one turn: its group
// ordinal and the full ordered canonical items.
type TurnContextResult struct {
	TurnID  uuid.UUID
	Ordinal int
	Items   []CanonicalItem
}

// Transcript directions: "latest" (default) returns the trailing page of the
// session's flattened message stream; "older" returns the page before the
// cursor.
const (
	TranscriptDirLatest = "latest"
	TranscriptDirOlder  = "older"
)

// TranscriptQuery selects one page of GET /sessions/:id/transcript. The
// transcript is the session's conversation flow flattened into one ordered
// message stream — each turn contributes only the messages that are new in
// that turn (its digest list beyond the previous turn's length), so the
// stream is append-only even though every context row stores the full
// history. Cursor is the message index of the oldest message of the
// previously loaded page; the page before it starts at Cursor - PageSize.
type TranscriptQuery struct {
	// SessionID restricts the page to one session's transcript.
	SessionID uuid.UUID
	// Direction selects the trailing page ("latest") or the page before
	// Cursor ("older").
	Direction string
	// Cursor is the message index of the oldest already-loaded message;
	// ignored when Direction is "latest".
	Cursor int64
	// PageSize is clamped into [DefaultPageSize, MaxPageSize].
	PageSize int
	// HMACKey re-verifies every item's content-layer digest during content
	// load; an empty key skips the digest step but keeps the structural
	// checks.
	HMACKey string
}

// TranscriptMessage is one flattened message of a session transcript.
type TranscriptMessage struct {
	// TurnID identifies the turn the message belongs to.
	TurnID uuid.UUID
	// TurnSeq is the 0-based position of the turn within the session.
	TurnSeq int64
	// Seq is the 0-based position of the message within its turn's new
	// messages.
	Seq int64
	// Kind is the canonical item kind (message, tool_call, tool_result,
	// system, gap, unknown).
	Kind string
	// Role is the item role when the kind carries one.
	Role string
	// Content is the whitelisted content parts of the item.
	Content []CanonicalPart
	// Gap carries the gap marker of an over-limit capture, when any.
	Gap *GapInfo
	// LogicalBytes is the item's logical size before capture bounds.
	LogicalBytes int64
	// Hmac is the keyed digest of the item's content layer.
	Hmac string
	// Truncated reports whether the item was cut at the capture limit.
	Truncated bool
}

// TranscriptPage is one page of a session transcript.
type TranscriptPage struct {
	// Items is the ordered page of messages (oldest first).
	Items []TranscriptMessage
	// PrevCursor is the message index of the oldest message of this page;
	// it is the cursor of the next "older" page. Zero when the page starts
	// at the beginning of the stream.
	PrevCursor int64
	// HasOlder reports whether older messages exist before this page.
	HasOlder bool
}

// QueryStore is the bounded Root query port of the observer. All methods are
// read-only, run under the single-query semaphore, and respect the caller's
// context; every list response carries keyset pagination state.
type QueryStore interface {
	// Overview returns the bounded aggregate windows and totals.
	Overview(ctx context.Context, query OverviewQuery) (OverviewResult, error)
	// ListSessions returns one keyset page of sessions, ordered by recency.
	ListSessions(ctx context.Context, query SessionQuery) (SessionPage, error)
	// GetSession returns the metadata row of one session; a session with no
	// row is reported with the not_found classification.
	GetSession(ctx context.Context, id uuid.UUID) (SessionSummary, error)
	// ListTurns returns one keyset page of turns, ordered by time.
	ListTurns(ctx context.Context, query TurnQuery) (TurnPage, error)
	// TurnContext reconstructs one turn's content, bounded to one checkpoint
	// and one suffix row plus their objects.
	TurnContext(ctx context.Context, query ContextQuery) (TurnContextResult, error)
	// Transcript returns one page of a session's flattened conversation
	// stream. Each turn contributes only its new messages, so the stream is
	// append-only; content objects are loaded only for the returned page.
	Transcript(ctx context.Context, query TranscriptQuery) (TranscriptPage, error)
}

// ---------------------------------------------------------------------------
// keyset cursor

// keysetCursor is the JSON payload of an opaque cursor: the tuple position
// (sort value, id). The sort value travels as unix nanoseconds so the
// encoding is deterministic and timezone-free; the id breaks ties.
type keysetCursor struct {
	At int64  `json:"at"`
	ID string `json:"id"`
}

// encodeKeysetCursor renders the opaque cursor of a tuple position.
func encodeKeysetCursor(at time.Time, id uuid.UUID) (string, error) {
	payload, err := common.Marshal(keysetCursor{At: at.UnixNano(), ID: id.String()})
	if err != nil {
		return "", fmt.Errorf("relayobserver: query: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// decodeKeysetCursor parses an opaque cursor back into its tuple position.
// The empty cursor means the first page and is not an error; any other cursor
// that is not valid base64url JSON with a parseable UUID id is rejected with
// the malformed-cursor classification.
func decodeKeysetCursor(s string) (time.Time, uuid.UUID, error) {
	if s == "" {
		return time.Time{}, uuid.Nil, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, classifiedQueryError(QueryErrMalformedCursor, "cursor is not valid base64url", err)
	}
	var c keysetCursor
	if err := common.Unmarshal(payload, &c); err != nil {
		return time.Time{}, uuid.Nil, classifiedQueryError(QueryErrMalformedCursor, "cursor payload is not a keyset", err)
	}
	if c.ID == "" {
		return time.Time{}, uuid.Nil, classifiedQueryError(QueryErrMalformedCursor, "cursor has no id", nil)
	}
	id, err := uuid.Parse(c.ID)
	if err != nil {
		return time.Time{}, uuid.Nil, classifiedQueryError(QueryErrMalformedCursor, "cursor id is not a uuid", err)
	}
	return time.Unix(0, c.At), id, nil
}

// clampPageSize bounds a requested page size into [DefaultPageSize,
// MaxPageSize], mirroring the PageInfo clamp of the dashboard list APIs.
func clampPageSize(n int) int {
	if n <= 0 {
		return DefaultPageSize
	}
	if n > MaxPageSize {
		return MaxPageSize
	}
	return n
}

// ---------------------------------------------------------------------------
// query semaphore

// querySlotCapacity is the SSOT query concurrency bound: at most one
// database-backed Root query runs at a time. A second query waits on the slot
// until its context expires and then fails with the timeout classification —
// it never queues unbounded.
const querySlotCapacity = 1

// acquireQuerySlot takes the query slot or fails with the timeout
// classification when ctx expires first.
func acquireQuerySlot(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return classifiedQueryError(QueryErrTimeout, "query slot busy", ctx.Err())
	}
}

// releaseQuerySlot returns the query slot.
func releaseQuerySlot(sem chan struct{}) {
	<-sem
}

// pgQueryStore implements QueryStore over the existing dedicated pool. It
// wraps the *pgStore so queries share its connections, pool tuning, and
// schema; it owns only the query semaphore.
type pgQueryStore struct {
	store *pgStore
	sem   chan struct{}
}

var _ QueryStore = (*pgQueryStore)(nil)

// NewQueryStore wraps a Store implementation with the bounded query surface.
// Only the PostgreSQL adapter carries the dedicated pool the queries run on,
// so any other Store implementation is rejected.
func NewQueryStore(s Store) (QueryStore, error) {
	pg, ok := s.(*pgStore)
	if !ok {
		return nil, fmt.Errorf("relayobserver: query port requires the PostgreSQL adapter store")
	}
	return &pgQueryStore{store: pg, sem: make(chan struct{}, querySlotCapacity)}, nil
}

// withSlot runs one database-backed query under the single-query semaphore
// and releases the slot on every path.
func (q *pgQueryStore) withSlot(ctx context.Context, run func() error) error {
	if err := acquireQuerySlot(ctx, q.sem); err != nil {
		return err
	}
	defer releaseQuerySlot(q.sem)
	return run()
}

// ---------------------------------------------------------------------------
// query orchestration

// closeRows releases a row set when the concrete adapter owns one; fake data
// layers without Close are left untouched.
func closeRows(rows rowIter) {
	if closer, ok := rows.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// listSessionsQ runs the keyset session list through the query seam. The
// LIMIT is page_size + 1: the extra row proves HasMore without a second
// query, and the row cap is the bounded-read backstop.
func listSessionsQ(ctx context.Context, q contentQuerier, query SessionQuery) (SessionPage, error) {
	if err := ctx.Err(); err != nil {
		return SessionPage{}, classifiedQueryError(QueryErrTimeout, "query context expired", err)
	}
	size := clampPageSize(query.PageSize)

	var conds []string
	var args []any
	if query.NodeScope != "" {
		conds = append(conds, "node_scope = $"+strconv.Itoa(len(args)+1))
		args = append(args, query.NodeScope)
	}
	if query.UserID != 0 {
		conds = append(conds, "user_id = $"+strconv.Itoa(len(args)+1))
		args = append(args, query.UserID)
	}
	if query.ClientFamily != "" {
		conds = append(conds, "client_family = $"+strconv.Itoa(len(args)+1))
		args = append(args, query.ClientFamily)
	}
	// Turn-derived filters are EXISTS subqueries over observer_turns, reusing
	// the idx_observer_turns_session_id/model index coverage: a session is
	// listed only when at least one of its turns matches the filter.
	if query.Model != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM observer_turns t WHERE t.session_id = observer_sessions.id AND t.model = $"+strconv.Itoa(len(args)+1)+")")
		args = append(args, query.Model)
	}
	if query.Success != nil {
		conds = append(conds, "EXISTS (SELECT 1 FROM observer_turns t WHERE t.session_id = observer_sessions.id AND t.success = $"+strconv.Itoa(len(args)+1)+")")
		args = append(args, *query.Success)
	}
	if query.Country != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM observer_turns t WHERE t.session_id = observer_sessions.id AND t.country_code = $"+strconv.Itoa(len(args)+1)+")")
		args = append(args, query.Country)
	}
	if query.ASN != 0 {
		conds = append(conds, "EXISTS (SELECT 1 FROM observer_turns t WHERE t.session_id = observer_sessions.id AND t.asn = $"+strconv.Itoa(len(args)+1)+")")
		args = append(args, query.ASN)
	}
	if query.IP != nil {
		conds = append(conds, "EXISTS (SELECT 1 FROM observer_turns t WHERE t.session_id = observer_sessions.id AND t.client_ip = $"+strconv.Itoa(len(args)+1)+"::inet)")
		args = append(args, query.IP.String())
	}
	if query.IPTrust != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM observer_turns t WHERE t.session_id = observer_sessions.id AND t.ip_trust = $"+strconv.Itoa(len(args)+1)+")")
		args = append(args, string(query.IPTrust))
	}
	if !query.From.IsZero() {
		conds = append(conds, "last_seen >= $"+strconv.Itoa(len(args)+1))
		args = append(args, query.From)
	}
	if !query.To.IsZero() {
		conds = append(conds, "last_seen < $"+strconv.Itoa(len(args)+1))
		args = append(args, query.To)
	}
	if query.Cursor != "" {
		at, id, err := decodeKeysetCursor(query.Cursor)
		if err != nil {
			return SessionPage{}, err
		}
		conds = append(conds, "(last_seen, id) < ($"+strconv.Itoa(len(args)+1)+", $"+strconv.Itoa(len(args)+2)+")")
		args = append(args, at, id.String())
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, size+1)
	sqlText := `SELECT id::text, node_scope, user_id, client_family, first_seen, last_seen, turn_count, gap_count
FROM observer_sessions` + where + ` ORDER BY last_seen DESC, id DESC LIMIT $` + strconv.Itoa(len(args))

	rows, err := q.Query(ctx, sqlText, args...)
	if err != nil {
		return SessionPage{}, fmt.Errorf("relayobserver: query sessions: %w", err)
	}
	defer closeRows(rows)
	var items []SessionSummary
	for rows.Next() {
		var s SessionSummary
		var idText string
		if err := rows.Scan(&idText, &s.NodeScope, &s.UserID, &s.ClientFamily, &s.FirstSeen, &s.LastSeen, &s.TurnCount, &s.GapCount); err != nil {
			return SessionPage{}, fmt.Errorf("relayobserver: query sessions: scan: %w", err)
		}
		s.SessionID, err = uuid.Parse(idText)
		if err != nil {
			return SessionPage{}, fmt.Errorf("relayobserver: query sessions: invalid session id %q: %w", idText, err)
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, fmt.Errorf("relayobserver: query sessions: %w", err)
	}
	if len(items) > size {
		cur, err := encodeKeysetCursor(items[size-1].LastSeen, items[size-1].SessionID)
		if err != nil {
			return SessionPage{}, err
		}
		return SessionPage{Items: items[:size], Meta: PageMeta{NextCursor: cur, HasMore: true}}, nil
	}
	return SessionPage{Items: items}, nil
}

// listTurnsQ runs the keyset turn list through the query seam. Same LIMIT
// page_size + 1 backstop as the session list.
func listTurnsQ(ctx context.Context, q contentQuerier, query TurnQuery) (TurnPage, error) {
	if err := ctx.Err(); err != nil {
		return TurnPage{}, classifiedQueryError(QueryErrTimeout, "query context expired", err)
	}
	size := clampPageSize(query.PageSize)

	var conds []string
	var args []any
	if query.SessionID != nil {
		conds = append(conds, "session_id = $"+strconv.Itoa(len(args)+1))
		args = append(args, query.SessionID.String())
	}
	if query.UserID != 0 {
		conds = append(conds, "user_id = $"+strconv.Itoa(len(args)+1))
		args = append(args, query.UserID)
	}
	if query.Model != "" {
		conds = append(conds, "model = $"+strconv.Itoa(len(args)+1))
		args = append(args, query.Model)
	}
	if query.Success != nil {
		conds = append(conds, "success = $"+strconv.Itoa(len(args)+1))
		args = append(args, *query.Success)
	}
	if query.ErrorType != "" {
		conds = append(conds, "error_type = $"+strconv.Itoa(len(args)+1))
		args = append(args, query.ErrorType)
	}
	if query.IPTrust != "" {
		conds = append(conds, "ip_trust = $"+strconv.Itoa(len(args)+1))
		args = append(args, string(query.IPTrust))
	}
	if query.Cursor != "" {
		at, id, err := decodeKeysetCursor(query.Cursor)
		if err != nil {
			return TurnPage{}, err
		}
		conds = append(conds, "(occurred_at, id) < ($"+strconv.Itoa(len(args)+1)+", $"+strconv.Itoa(len(args)+2)+")")
		args = append(args, at, id.String())
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, size+1)
	sqlText := `SELECT id::text, node_scope, event_id, session_id::text, occurred_at, user_id, token_id,
  client_profile, model, upstream_model, relay_format,
  success, status_code, error_type, error_code,
  latency_ms, first_response_ms, stream,
  prompt_tokens, completion_tokens, cached_tokens, quota,
  attempts, attempts_omitted, content_state
FROM observer_turns` + where + ` ORDER BY occurred_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))

	rows, err := q.Query(ctx, sqlText, args...)
	if err != nil {
		return TurnPage{}, fmt.Errorf("relayobserver: query turns: %w", err)
	}
	defer closeRows(rows)
	var items []TurnSummary
	for rows.Next() {
		var s TurnSummary
		var idText string
		var sidText sql.NullString
		var attemptsRaw []byte
		if err := rows.Scan(&idText, &s.NodeScope, &s.EventID, &sidText, &s.OccurredAt, &s.UserID, &s.TokenID,
			&s.ClientProfile, &s.Model, &s.UpstreamModel, &s.RelayFormat,
			&s.Success, &s.StatusCode, &s.ErrorType, &s.ErrorCode,
			&s.LatencyMS, &s.FirstResponseMS, &s.Stream,
			&s.PromptTokens, &s.CompletionTokens, &s.CachedTokens, &s.Quota,
			&attemptsRaw, &s.AttemptsOmitted, &s.ContentState); err != nil {
			return TurnPage{}, fmt.Errorf("relayobserver: query turns: scan: %w", err)
		}
		s.TurnID, err = uuid.Parse(idText)
		if err != nil {
			return TurnPage{}, fmt.Errorf("relayobserver: query turns: invalid turn id %q: %w", idText, err)
		}
		if sidText.Valid {
			sid, err := uuid.Parse(sidText.String)
			if err != nil {
				return TurnPage{}, fmt.Errorf("relayobserver: query turns: invalid session id %q: %w", sidText.String, err)
			}
			s.SessionID = &sid
		}
		if err := common.Unmarshal(attemptsRaw, &s.Attempts); err != nil {
			return TurnPage{}, fmt.Errorf("relayobserver: query turns: decode attempts: %w", err)
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		return TurnPage{}, fmt.Errorf("relayobserver: query turns: %w", err)
	}
	if len(items) > size {
		cur, err := encodeKeysetCursor(items[size-1].OccurredAt, items[size-1].TurnID)
		if err != nil {
			return TurnPage{}, err
		}
		return TurnPage{Items: items[:size], Meta: PageMeta{NextCursor: cur, HasMore: true}}, nil
	}
	return TurnPage{Items: items}, nil
}

// overviewQ computes the bounded aggregate windows and totals through the
// query seam. The window scan is bounded by the clamped window count and an
// explicit LIMIT; the totals are three count rows.
func overviewQ(ctx context.Context, q contentQuerier, query OverviewQuery) (OverviewResult, error) {
	if err := ctx.Err(); err != nil {
		return OverviewResult{}, classifiedQueryError(QueryErrTimeout, "query context expired", err)
	}
	winSec := query.WindowSeconds
	if winSec <= 0 {
		winSec = DefaultOverviewWindowSeconds
	}
	windows := query.Windows
	if windows <= 0 {
		windows = DefaultOverviewWindows
	}
	if windows > MaxOverviewWindows {
		windows = MaxOverviewWindows
	}
	start := time.Now().Add(-time.Duration(windows) * time.Duration(winSec) * time.Second)
	out := OverviewResult{WindowSeconds: winSec}

	rows, err := q.Query(ctx, `SELECT floor(extract(epoch FROM occurred_at) / $1)::bigint, count(*), count(*) FILTER (WHERE success)
FROM observer_turns
WHERE occurred_at >= $2
GROUP BY 1 ORDER BY 1
LIMIT $3`, float64(winSec), start, windows+1)
	if err != nil {
		return out, fmt.Errorf("relayobserver: query overview windows: %w", err)
	}
	defer closeRows(rows)
	for rows.Next() {
		var bucket, turns, success int64
		if err := rows.Scan(&bucket, &turns, &success); err != nil {
			return out, fmt.Errorf("relayobserver: query overview windows: scan: %w", err)
		}
		out.Windows = append(out.Windows, OverviewWindow{Start: time.Unix(bucket*int64(winSec), 0), Turns: turns, Success: success})
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("relayobserver: query overview windows: %w", err)
	}
	// The response never exceeds the requested window count: the LIMIT is
	// windows+1 only to prove that more data exists, and the extras are
	// trimmed here (P2-1: overview output self-truncates to the request).
	if len(out.Windows) > windows {
		out.Windows = out.Windows[:windows]
	}

	if err := q.QueryRow(ctx, `SELECT count(*) FROM observer_sessions`).Scan(&out.SessionCount); err != nil {
		return out, fmt.Errorf("relayobserver: query overview session count: %w", err)
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM observer_turns`).Scan(&out.TurnCount); err != nil {
		return out, fmt.Errorf("relayobserver: query overview turn count: %w", err)
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM observer_turns WHERE content_state IN ('gap', 'metadata_only')`).Scan(&out.GapCount); err != nil {
		return out, fmt.Errorf("relayobserver: query overview gap count: %w", err)
	}
	return out, nil
}

// turnContextQ reconstructs one turn's content through the query seam. The
// read is bounded by the T2.3 reconstruction: one context row (a delta also
// reads exactly one full checkpoint) plus the content objects of its digest
// list. Content errors pass through with their classification.
func turnContextQ(ctx context.Context, q contentQuerier, query ContextQuery) (TurnContextResult, error) {
	if err := ctx.Err(); err != nil {
		return TurnContextResult{}, classifiedQueryError(QueryErrTimeout, "query context expired", err)
	}
	rt, err := reconstructTurnQ(ctx, q, query.SessionID, query.TurnID, query.HMACKey)
	if err != nil {
		return TurnContextResult{}, err
	}
	return TurnContextResult{TurnID: rt.TurnID, Ordinal: rt.Ordinal, Items: rt.Items}, nil
}

// getSessionQ loads one session's metadata row through the query seam. A
// missing row is classified not_found so the Root controller can map it onto
// a 404; content objects are never read on this path.
func getSessionQ(ctx context.Context, q contentQuerier, id uuid.UUID) (SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return SessionSummary{}, classifiedQueryError(QueryErrTimeout, "query context expired", err)
	}
	var s SessionSummary
	var idText string
	err := q.QueryRow(ctx, `SELECT id::text, node_scope, user_id, client_family, first_seen, last_seen, turn_count, gap_count
FROM observer_sessions WHERE id = $1`, id.String()).Scan(&idText, &s.NodeScope, &s.UserID, &s.ClientFamily, &s.FirstSeen, &s.LastSeen, &s.TurnCount, &s.GapCount)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionSummary{}, classifiedQueryError(QueryErrNotFound, "session not found", err)
	}
	if err != nil {
		return SessionSummary{}, fmt.Errorf("relayobserver: query session: %w", err)
	}
	s.SessionID, err = uuid.Parse(idText)
	if err != nil {
		return SessionSummary{}, fmt.Errorf("relayobserver: query session: invalid session id %q: %w", idText, err)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// QueryStore methods: semaphore-gated adapters over the seam functions.

// Overview implements QueryStore.
func (q *pgQueryStore) Overview(ctx context.Context, query OverviewQuery) (OverviewResult, error) {
	var out OverviewResult
	err := q.withSlot(ctx, func() error {
		var err error
		out, err = overviewQ(ctx, sqlDBAdapter{db: q.store.db}, query)
		return err
	})
	return out, err
}

// ListSessions implements QueryStore.
func (q *pgQueryStore) ListSessions(ctx context.Context, query SessionQuery) (SessionPage, error) {
	var out SessionPage
	err := q.withSlot(ctx, func() error {
		var err error
		out, err = listSessionsQ(ctx, sqlDBAdapter{db: q.store.db}, query)
		return err
	})
	return out, err
}

// GetSession implements QueryStore.
func (q *pgQueryStore) GetSession(ctx context.Context, id uuid.UUID) (SessionSummary, error) {
	var out SessionSummary
	err := q.withSlot(ctx, func() error {
		var err error
		out, err = getSessionQ(ctx, sqlDBAdapter{db: q.store.db}, id)
		return err
	})
	return out, err
}

// ListTurns implements QueryStore.
func (q *pgQueryStore) ListTurns(ctx context.Context, query TurnQuery) (TurnPage, error) {
	var out TurnPage
	err := q.withSlot(ctx, func() error {
		var err error
		out, err = listTurnsQ(ctx, sqlDBAdapter{db: q.store.db}, query)
		return err
	})
	return out, err
}

// TurnContext implements QueryStore.
func (q *pgQueryStore) TurnContext(ctx context.Context, query ContextQuery) (TurnContextResult, error) {
	var out TurnContextResult
	err := q.withSlot(ctx, func() error {
		var err error
		out, err = turnContextQ(ctx, sqlDBAdapter{db: q.store.db}, query)
		return err
	})
	return out, err
}

// Transcript implements QueryStore.
func (q *pgQueryStore) Transcript(ctx context.Context, query TranscriptQuery) (TranscriptPage, error) {
	var out TranscriptPage
	err := q.withSlot(ctx, func() error {
		var err error
		out, err = transcriptQ(ctx, sqlDBAdapter{db: q.store.db}, query)
		return err
	})
	return out, err
}

// transcriptFlatRef is one message of the flattened stream before content
// decode: the digest reference is enough to page the stream, and the content
// objects are loaded only for the returned page.
type transcriptFlatRef struct {
	turnID  uuid.UUID
	turnSeq int64
	seq     int64
	digest  string
}

// transcriptQ flattens a session's context rows into one ordered message
// stream and returns one page. Every context row stores the turn's complete
// history; a turn's new messages are its digest list beyond the previous
// turn's list length (the append-only conversation view). A history
// compaction (a shorter list than the previous turn) restarts the window so
// the whole compacted view is shown once instead of dropping messages.
func transcriptQ(ctx context.Context, q contentQuerier, query TranscriptQuery) (TranscriptPage, error) {
	rows, err := q.Query(ctx, `SELECT id, turn_id::text, checkpoint_id, group_ordinal, common_prefix_count, item_count, item_digests, logical_bytes FROM observer_contexts WHERE session_id = $1 ORDER BY id`, query.SessionID.String())
	if err != nil {
		return TranscriptPage{}, fmt.Errorf("relayobserver: transcript: read context rows: %w", err)
	}
	defer func() {
		if closer, ok := rows.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	var flat []transcriptFlatRef
	var prevCount int64
	var turnSeq int64
	for rows.Next() {
		var row contextRow
		var turnRaw string
		if err := rows.Scan(&row.id, &turnRaw, &row.checkpointID, &row.groupOrdinal, &row.commonPrefix, &row.itemCount, &row.itemDigests, &row.logicalBytes); err != nil {
			return TranscriptPage{}, fmt.Errorf("relayobserver: transcript: scan context row: %w", err)
		}
		turnID, err := uuid.Parse(turnRaw)
		if err != nil {
			return TranscriptPage{}, classifiedErrorWrap(ContentErrCorrupt, "invalid turn id in context row", err)
		}
		digests, err := reconstructDigests(row, func(id int64) (contextRow, error) {
			return loadContextRowByIDQ(ctx, q, query.SessionID, id)
		})
		if err != nil {
			return TranscriptPage{}, err
		}
		start := prevCount
		if start > int64(len(digests)) {
			start = 0
		}
		for i := start; i < int64(len(digests)); i++ {
			flat = append(flat, transcriptFlatRef{turnID: turnID, turnSeq: turnSeq, seq: i - start, digest: digests[i]})
		}
		prevCount = int64(len(digests))
		turnSeq++
	}
	if err := rows.Err(); err != nil {
		return TranscriptPage{}, fmt.Errorf("relayobserver: transcript: read context rows: %w", err)
	}

	total := int64(len(flat))
	var start, end int64
	if query.Direction == TranscriptDirOlder && query.Cursor > 0 {
		end = query.Cursor
		if end > total {
			end = total
		}
		start = end - int64(query.PageSize)
		if start < 0 {
			start = 0
		}
	} else {
		end = total
		start = total - int64(query.PageSize)
		if start < 0 {
			start = 0
		}
	}
	page := flat[start:end]

	out := TranscriptPage{
		PrevCursor: start,
		HasOlder:   start > 0,
	}
	if len(page) == 0 {
		return out, nil
	}
	digests := make([]string, 0, len(page))
	for _, ref := range page {
		digests = append(digests, ref.digest)
	}
	items, err := loadContentItemsQ(ctx, q, query.SessionID, digests, query.HMACKey)
	if err != nil {
		return TranscriptPage{}, err
	}
	out.Items = make([]TranscriptMessage, 0, len(page))
	for i, ref := range page {
		item := items[i]
		out.Items = append(out.Items, TranscriptMessage{
			TurnID:       ref.turnID,
			TurnSeq:      ref.turnSeq,
			Seq:          ref.seq,
			Kind:         item.Kind,
			Role:         item.Role,
			Content:      item.Content,
			Gap:          item.Gap,
			LogicalBytes: item.LogicalBytes,
			Hmac:         item.Hmac,
			Truncated:    item.Truncated,
		})
	}
	return out, nil
}
