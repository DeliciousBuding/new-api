package relayobserver

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// This file wires the frozen components into the single runtime face that
// NewAPI startup, shutdown, and the Root status endpoint use: Config from the
// environment, the dedicated PostgreSQL Store, and the bounded Dispatcher.
// The runtime is disabled by default and strictly fail-open — every failure
// of config parsing, store opening, or startup turns it into a disabled state
// with a safe ReasonCode and never surfaces a fatal error to main. Status is
// assembled from memory only and never touches the database.

// runtimeState is the composition state of the Runtime.
type runtimeState int

const (
	// stateUninitialized is the state before Init: Status already reads
	// stable disabled.
	stateUninitialized runtimeState = iota
	// stateEnabled is the healthy running state: dispatcher started.
	stateEnabled
	// stateDisabled is a fail-open state reached from an init failure or an
	// explicitly disabled configuration.
	stateDisabled
	// stateClosed is the state after Close: worker stopped, store released.
	stateClosed
)

// storeOpenBudget bounds a single store open during Init. NewAPI calls Init
// synchronously before it starts listening, so a hung DNS or TCP connection
// must never stall process startup beyond this budget (SSOT: observer failures
// never alter process startup).
const storeOpenBudget = 5 * time.Second

// storeOpener is the store construction seam of the runtime. Production uses
// defaultStoreOpener (which adapts OpenPGStore); tests inject a controlled
// opener so no real database is ever contacted.
type storeOpener func(ctx context.Context, cfg Config) (Store, error)

// defaultStoreOpener adapts the frozen adapter signature to the seam. It does
// not duplicate adapter logic: all validation, pool, and schema work stays in
// OpenPGStore. It also wires the previous-generation content HMAC key into the
// adapter so reconstruction can decode items written before a rotation.
func defaultStoreOpener(ctx context.Context, cfg Config) (Store, error) {
	store, err := OpenPGStore(ctx, cfg.SQLDSN, cfg.SchemaMode)
	if err != nil {
		return nil, err
	}
	if ps, ok := store.(*pgStore); ok {
		ps.SetPreviousHMACKey(cfg.PreviousHMACKey)
	}
	return store, nil
}

// Runtime composes Config, Store, and Dispatcher into the single observer
// runtime face. NewAPI startup calls Init (which never fails fatally),
// shutdown calls Close with a bounded context, and the Root status endpoint
// reads Status. All methods are idempotent and concurrency-safe; Status is
// stable in the uninitialized, disabled, and enabled states.
type Runtime struct {
	mu     sync.Mutex
	state  runtimeState
	reason ReasonCode
	disp   *Dispatcher
	store  Store

	// publishable is the lock-free fast path of the enabled state: true only
	// while the runtime is stateEnabled with a started dispatcher. The
	// request-path hooks read it via CanPublish before allocating any
	// request-local observer state, so the fail-open disabled path stays
	// zero-allocation. Written under r.mu by Init / disableLocked / Close.
	publishable atomic.Bool

	openStore storeOpener

	// openTimeout bounds the store open during Init; <= 0 uses storeOpenBudget.
	// It is a test seam: production never sets it.
	openTimeout time.Duration

	// queryTimeout is the Root query budget stored from the init configuration
	// (Config.QueryTimeout, which ConfigFromEnv clamps into [1ms, 2s]). The
	// Root controllers read it through QuerySurface and bound every
	// database-backed query with it.
	queryTimeout time.Duration
	// hmacKey is the content HMAC key of the running configuration; it is a
	// secret and must never appear in status output, logs, or API responses.
	// An empty key skips the per-item digest re-verification of the turn
	// context reconstruction (T2.3 semantics).
	hmacKey string
	// queryStore lazily caches the bounded query surface wrapper created from
	// the enabled store on first QuerySurface call.
	queryStore QueryStore
	// querySurface is the query-surface seam of the runtime. Production leaves
	// it nil, so QuerySurface uses the default lazy wrapper over the enabled
	// store; tests inject a controlled surface (same pattern as openStore) so
	// no real database is ever contacted. The seam must not call back into
	// QuerySurface while the runtime lock is held.
	querySurface func() (QueryStore, time.Duration, bool)
}

// NewRuntime returns a disabled-by-default runtime composition. Status can be
// read at any time: before Init it reports disabled with ReasonDisabled.
func NewRuntime() *Runtime {
	return &Runtime{
		state:     stateUninitialized,
		reason:    ReasonDisabled,
		openStore: defaultStoreOpener,
	}
}

// Init loads the observer configuration from the environment and starts the
// observer. Any failure of ConfigFromEnv, store opening, or dispatcher
// startup is absorbed internally: the runtime becomes disabled with a safe
// ReasonCode and Init never returns an error, so NewAPI startup is never
// affected. Init is idempotent (later calls are no-ops) and concurrency-safe.
// The store open runs under the bounded open budget, and a panicking opener
// disables the observer instead of crashing the process (SSOT: all observer
// entry points recover panics).
func (r *Runtime) Init() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != stateUninitialized {
		return
	}
	// Fail-open wrapper: any panic from the opener seam, the dispatcher
	// constructor, or Start disables the observer. Recover runs while the
	// lock is still held, so disableLocked stays consistent.
	defer func() {
		if recover() != nil {
			r.disableLocked(ReasonStoreInitFailed)
		}
	}()
	cfg, err := ConfigFromEnv()
	if err != nil {
		r.disableLocked(ReasonConfigInvalid)
		return
	}
	if !cfg.Enabled {
		r.disableLocked(ReasonDisabled)
		return
	}
	timeout := r.openTimeout
	if timeout <= 0 {
		timeout = storeOpenBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	store, err := r.openStore(ctx, cfg)
	if err != nil {
		r.disableLocked(storeInitReason(err))
		return
	}
	if store == nil {
		// A nil store would panic the shutdown path (Store.Close inside
		// Dispatcher.Stop); fail open instead of wiring it.
		r.disableLocked(ReasonStoreInitFailed)
		return
	}
	disp := NewDispatcher(cfg, store)
	disp.SetIPTrust(ipTrustFor(cfg))
	disp.Start()
	r.disp = disp
	r.store = store
	r.queryTimeout = cfg.QueryTimeout
	r.hmacKey = cfg.HMACKey
	r.state = stateEnabled
	r.publishable.Store(true)
}

// Close stops the worker and releases the store inside the caller's budget
// (main passes a hard two-second context). It is idempotent and
// concurrency-safe; the dispatcher absorbs its own stop and store-close
// failures, so Close never changes the main shutdown outcome. A store Close
// that panics is absorbed here for the same reason. A runtime that was never
// initialized or already closed is a no-op.
func (r *Runtime) Close(ctx context.Context) {
	r.mu.Lock()
	if r.state != stateEnabled {
		r.mu.Unlock()
		return
	}
	disp := r.disp
	r.state = stateClosed
	r.reason = ReasonDisabled
	r.publishable.Store(false)
	r.mu.Unlock()
	// Dispatcher.Stop owns the store release: it passes the remaining budget
	// to Store.Close (the adapter's Close is idempotent). A panic from either
	// must never change the main shutdown outcome.
	defer func() { _ = recover() }()
	disp.Stop(ctx)
}

// Status returns a point-in-time snapshot assembled from in-memory state only:
// it never queries the database or the network, and it never contains secrets.
// An enabled runtime reports the live dispatcher snapshot; uninitialized,
// disabled, and closed runtimes report a stable disabled snapshot with the
// safe ReasonCode of the current state. Safe to call concurrently with any
// other method.
func (r *Runtime) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateEnabled {
		return r.disp.Status()
	}
	return Status{
		Enabled:    false,
		ReasonCode: r.reason,
		IPTrust:    IPTrustNone,
	}
}

// QuerySurface exposes the bounded Root query port of an enabled runtime:
// the QueryStore, the stored query timeout, and availability. A disabled,
// uninitialized, closed, or nil-store runtime reports (nil, 0, false) so the
// Root controllers emit the degraded envelope instead of touching a query
// surface that cannot run. The QueryStore wrapper is created lazily on the
// first call and cached; a store that cannot be wrapped (any non-PostgreSQL
// adapter) also reports unavailable, matching the fail-open contract. The
// injected querySurface seam replaces the whole default behavior. Safe to
// call concurrently with any other method.
func (r *Runtime) QuerySurface() (QueryStore, time.Duration, bool) {
	r.mu.Lock()
	seam := r.querySurface
	timeout := r.queryTimeout
	if seam == nil {
		// Default path: the wrapper creation stays under the lock (it is
		// idempotent and cheap), but the seam — a test-injected callback that
		// may re-enter the runtime — must never run while r.mu is held.
		if r.state != stateEnabled || r.store == nil {
			r.mu.Unlock()
			return nil, 0, false
		}
		if r.queryStore == nil {
			qs, err := NewQueryStore(r.store)
			if err != nil {
				r.mu.Unlock()
				return nil, 0, false
			}
			r.queryStore = qs
		}
		qs := r.queryStore
		r.mu.Unlock()
		return qs, timeout, true
	}
	r.mu.Unlock()
	// The seam runs outside the lock: an injected callback that calls back
	// into the runtime (QuerySurface, HMACKey, Status) can never deadlock on
	// r.mu (P2-2 hardening).
	return seam()
}

// HMACKey returns the content HMAC key of the running configuration. An empty
// key skips the per-item digest re-verification of the turn context
// reconstruction (T3.1 semantics); the value is a secret and must never cross
// the status, API, or log boundary.
func (r *Runtime) HMACKey() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hmacKey
}

// disableLocked moves the runtime into the fail-open disabled state with a
// safe reason. Caller holds r.mu. The observer is strictly fail-open (NewAPI
// still starts), but a disable for any reason other than an explicit off
// switch is an operational signal that must not be silent — log it exactly
// once here, with the safe reason only (never the raw error, which may
// contain secrets).
func (r *Runtime) disableLocked(reason ReasonCode) {
	r.state = stateDisabled
	r.reason = reason
	r.publishable.Store(false)
	if reason != ReasonDisabled {
		common.SysError("relayobserver: disabled (reason=" + string(reason) + ")")
	}
}

// CanPublish reports whether the request-path hooks may allocate observer
// state and publish turn events. It is the lock-free fast path of the
// fail-open contract: nil-runtime checks stay in the hooks, and this check
// keeps the disabled/closed paths from constructing request-local state,
// events, or body reads. Safe to call concurrently with any other method.
func (r *Runtime) CanPublish() bool {
	return r.publishable.Load()
}

// storeInitReason classifies a store open failure into a safe ReasonCode.
// The error text comes from this package (store_pg.go) and is stable: schema
// verify/bootstrap failures map to ReasonSchemaMismatch, everything else
// (unsupported DSN, pool open or ping failure) maps to ReasonStoreInitFailed.
// The verify sub-path errors (information_schema listing and scanning) are
// schema check failures too, returned without the "schema verify:" wrapper,
// so they are matched explicitly. The raw error never crosses the status,
// API, or log boundary.
func storeInitReason(err error) ReasonCode {
	if err == nil {
		return ReasonStoreInitFailed
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "relayobserver: schema verify:") ||
		strings.HasPrefix(msg, "relayobserver: schema bootstrap:") ||
		strings.HasPrefix(msg, "relayobserver: list observer tables:") ||
		strings.HasPrefix(msg, "relayobserver: scan observer table:") {
		return ReasonSchemaMismatch
	}
	return ReasonStoreInitFailed
}

// ipTrustFor derives the effective IP trust tier of the running
// configuration, mirroring the SSOT semantics: capture is on only when both
// opt-ins hold — the NewAPI system setting (common.LogRecordIpEnabled) and
// RELAY_OBSERVER_RECORD_IP — and with capture on, the tier is "direct" only
// under TRUSTED_PROXIES=none strict direct-connect. Under the default
// trusted-CIDR configuration or an explicit proxy list, gin.ClientIP() may
// derive the peer from X-Forwarded-For, so the tier is "proxy".
func ipTrustFor(cfg Config) IPTrust {
	if !cfg.RecordIP || !common.LogRecordIpEnabled {
		return IPTrustNone
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")), "none") {
		return IPTrustDirect
	}
	return IPTrustProxy
}
