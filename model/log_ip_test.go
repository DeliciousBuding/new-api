package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

// TestLogIPRecordingUsesGlobalControlPlane locks the fork contract that
// LogRecordIpEnabled is the only IP recording control plane. Upstream's
// per-user record_ip_log opt-in is not exposed by this fork UI and must not
// silently disable audit IPs after a sync.
func TestLogIPRecordingUsesGlobalControlPlane(t *testing.T) {
	original := common.LogRecordIpEnabled
	t.Cleanup(func() { common.LogRecordIpEnabled = original })

	common.LogRecordIpEnabled = false
	require.False(t, shouldRecordLogIP())

	common.LogRecordIpEnabled = true
	require.True(t, shouldRecordLogIP())
}
