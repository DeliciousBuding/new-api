package relayobserver

import (
	"context"
	"strings"
	"sync"
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

// storeOpener is the store construction seam of the runtime. Production uses
// defaultStoreOpener (which adapts OpenPGStore); tests inject a controlled
// opener so no real database is ever contacted.
type storeOpener func(ctx context.Context, cfg Config) (Store, error)

// defaultStoreOpener adapts the frozen adapter signature to the seam. It does
// not duplicate adapter logic: all validation, pool, and schema work stays in
// OpenPGStore.
func defaultStoreOpener(ctx context.Context, cfg Config) (Store, error) {
	return OpenPGStore(ctx, cfg.SQLDSN, cfg.SchemaMode)
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

	openStore storeOpener
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
func (r *Runtime) Init() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != stateUninitialized {
		return
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		r.disableLocked(ReasonConfigInvalid)
		return
	}
	if !cfg.Enabled {
		r.disableLocked(ReasonDisabled)
		return
	}
	store, err := r.openStore(context.Background(), cfg)
	if err != nil {
		r.disableLocked(storeInitReason(err))
		return
	}
	disp := NewDispatcher(cfg, store)
	disp.SetIPTrust(ipTrustFor(cfg))
	disp.Start()
	r.disp = disp
	r.store = store
	r.state = stateEnabled
}

// Close stops the worker and releases the store inside the caller's budget
// (main passes a hard two-second context). It is idempotent and
// concurrency-safe; the dispatcher absorbs its own stop and store-close
// failures, so Close never changes the main shutdown outcome. A runtime that
// was never initialized or already closed is a no-op.
func (r *Runtime) Close(ctx context.Context) {
	r.mu.Lock()
	if r.state != stateEnabled {
		r.mu.Unlock()
		return
	}
	disp := r.disp
	r.state = stateClosed
	r.reason = ReasonDisabled
	r.mu.Unlock()
	// Dispatcher.Stop owns the store release: it passes the remaining budget
	// to Store.Close (the adapter's Close is idempotent).
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

// disableLocked moves the runtime into the fail-open disabled state with a
// safe reason. Caller holds r.mu.
func (r *Runtime) disableLocked(reason ReasonCode) {
	r.state = stateDisabled
	r.reason = reason
}

// storeInitReason classifies a store open failure into a safe ReasonCode.
// The error text comes from this package (store_pg.go) and is stable: schema
// verify/bootstrap failures map to ReasonSchemaMismatch, everything else
// (unsupported DSN, pool open or ping failure) maps to ReasonStoreInitFailed.
// The raw error never crosses the status, API, or log boundary.
func storeInitReason(err error) ReasonCode {
	if err == nil {
		return ReasonStoreInitFailed
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "relayobserver: schema verify:") ||
		strings.HasPrefix(msg, "relayobserver: schema bootstrap:") {
		return ReasonSchemaMismatch
	}
	return ReasonStoreInitFailed
}

// ipTrustFor derives the effective IP trust tier of the running configuration:
// "none" while the dual-opt-in RecordIP policy is off, "direct" when capture
// is on (the proxy tier is decided by later phases from the trusted-proxy
// configuration).
func ipTrustFor(cfg Config) IPTrust {
	if !cfg.RecordIP {
		return IPTrustNone
	}
	return IPTrustDirect
}
