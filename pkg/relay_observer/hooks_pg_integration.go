//go:build relay_observer_pg_integration

package relayobserver

// appendHook and retentionHook are the integration-test-only pause points
// inside the append and retention transactions. When non-nil they run after
// the claim (append) or after the session row lock and last_seen re-check
// (retention), while the transaction still holds its locks and has committed
// nothing. The integration suite sets them to coordinate two-connection
// concurrency scenarios; production builds compile the empty stubs from
// hooks_default.go instead, so no hook symbol exists outside the tagged
// build.
var appendHook func()
var retentionHook func()

func runAppendHook() {
	if appendHook != nil {
		appendHook()
	}
}

func runRetentionHook() {
	if retentionHook != nil {
		retentionHook()
	}
}
