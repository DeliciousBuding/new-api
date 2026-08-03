package relayobserver

import "net"

// This file hosts the request-path publish face of the final turn event: the
// single entry point the settlement hooks call to hand an event to the
// bounded dispatcher. The event is constructed and published only after the
// last write to the parsed request on that path (final attempt and
// settlement finish first); the channel send of the event establishes
// happens-before with the worker, and the request path must not write the
// parsed request after the send (SSOT Isolation Contract).

// TryPublishTurn publishes the final turn event to the bounded dispatcher. It
// is the only request-path entry point of the settlement hooks: it never
// blocks, marshals, copies the request, or touches the database, and it is
// strictly fail-open — every failure drops the event and never changes relay
// responses, billing, or NewAPI startup.
//
// ev is taken by value: the publish snapshots the caller's event before the
// channel send, so a write to the caller's event after TryPublishTurn returns
// can never race the worker's read (the send establishes happens-before, and
// the caller keeps its own copy). A reservation above the per-request cap is
// clamped at the cap instead of rejected — SSOT: an oversized request becomes
// metadata-only instead of being dropped — while a negative reservation is
// rejected. Returns false when the event was dropped (dispatcher stopped,
// circuit open, queue or byte budget full).
func (r *Runtime) TryPublishTurn(ev Event, reservation int64) (ok bool) {
	var disp *Dispatcher
	defer func() {
		if recover() != nil {
			// Defensive, mirroring Dispatcher.TryEnqueue: the request path
			// must stay fail-open even if a bug panics here. Once the
			// dispatcher snapshot is taken, count the drop on its admission
			// counters; a panic before that snapshot never entered any
			// admission budget.
			ok = false
			if disp != nil {
				disp.droppedTotal.Add(1)
			}
		}
	}()
	r.mu.Lock()
	if r.state != stateEnabled || r.disp == nil {
		r.mu.Unlock()
		return false
	}
	disp = r.disp
	r.mu.Unlock()
	if reservation < 0 {
		return false
	}
	if reservation > disp.cfg.MaxRequestBytes {
		reservation = disp.cfg.MaxRequestBytes
	}
	// local is a second value snapshot made only on the enabled path: the
	// caller's ev parameter stays stack-allocated (the disabled path above
	// allocates nothing), and the dispatcher holds a pointer to this local
	// copy, so a write to the caller's event after TryPublishTurn returns can
	// never race the worker's read.
	local := ev
	return disp.TryEnqueue(&local, reservation)
}

// CaptureClientIP applies the dual-opt-in capture policy to one peer string
// (SSOT IP And GeoIP): a "none" trust tier — either opt-in off, as derived by
// the runtime's effective-tier logic — yields no IP at all; otherwise the
// peer is parsed and returned with its tier. An unparseable peer yields a nil
// IP without changing the tier: capture is best-effort and never alters the
// relay response. The peer string is gin.Context.ClientIP() under NewAPI's
// existing trusted-proxy configuration; this function only parses it and
// never loads or duplicates any GeoIP database.
func CaptureClientIP(trust IPTrust, clientIP string) (net.IP, IPTrust) {
	if trust == IPTrustNone || clientIP == "" {
		return nil, IPTrustNone
	}
	return net.ParseIP(clientIP), trust
}
