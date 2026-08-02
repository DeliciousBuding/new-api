package controller

import (
	"net/http"
	"sync"
	"time"

	relayobserver "github.com/QuantumNous/new-api/pkg/relay_observer"

	"github.com/gin-gonic/gin"
)

// This file hosts the Root-only observer status surface. It returns only the
// safe in-memory Status snapshot: no DSN, no HMAC keys, no event content, and
// no database query. The route sits behind middleware.RootAuth in
// router/api-router.go, so Admin and User receive 403 even when calling the
// route directly; frontend hiding is convenience, not authorization.

// ObserverRuntime is the minimal runtime surface the Root observer
// controllers need: the in-memory status snapshot, the bounded query surface
// with its query timeout, and the content HMAC key. *relayobserver.Runtime
// satisfies it (main wires the real runtime); tests inject fakes to cover
// every failure path, exactly like the store opener seam.
type ObserverRuntime interface {
	// Status returns the safe in-memory observer status snapshot.
	Status() relayobserver.Status
	// QuerySurface exposes the bounded Root query port, its query timeout,
	// and availability: (nil, 0, false) when the observer cannot run queries.
	QuerySurface() (relayobserver.QueryStore, time.Duration, bool)
	// HMACKey returns the content HMAC key; an empty key skips the digest
	// re-verification of the turn context reconstruction.
	HMACKey() string
}

var (
	relayObserverMu sync.RWMutex
	relayObserverRT ObserverRuntime
)

// SetRelayObserverRuntime wires the process-level observer runtime into the
// controller. main calls it once after Init; tests inject controlled runtimes
// (and nil to restore the unwired state). Concurrency-safe.
func SetRelayObserverRuntime(rt ObserverRuntime) {
	relayObserverMu.Lock()
	defer relayObserverMu.Unlock()
	relayObserverRT = rt
}

// GetRelayObserverStatus returns the observer's in-memory status snapshot.
// An unwired observer (before main injects the runtime, or in tests) reports
// a stable disabled status. The handler itself performs no authentication —
// the route's middleware.RootAuth does that.
func GetRelayObserverStatus(c *gin.Context) {
	relayObserverMu.RLock()
	rt := relayObserverRT
	relayObserverMu.RUnlock()
	st := relayobserver.Status{Enabled: false, ReasonCode: relayobserver.ReasonDisabled}
	if rt != nil {
		st = rt.Status()
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    st,
	})
}
