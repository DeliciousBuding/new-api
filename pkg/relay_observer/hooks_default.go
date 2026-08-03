//go:build !relay_observer_pg_integration

package relayobserver

// runAppendHook and runRetentionHook are the production-build stubs of the
// test-only pause hooks. They are empty and get inlined away by the compiler,
// so the production binary carries zero test surface. The real hook wiring
// lives in hooks_pg_integration.go, compiled only under the
// relay_observer_pg_integration build tag.
func runAppendHook()    {}
func runRetentionHook() {}
