package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFormatUserLogsStripsQuotaSaturation verifies the admin-only quota
// saturation marker (nested under other.admin_info) is removed for non-admin
// log views, since formatUserLogs strips the whole admin_info object.
func TestFormatUserLogsStripsQuotaSaturation(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"model_price": 0.004,
		"admin_info": map[string]interface{}{
			"quota_saturation": map[string]interface{}{
				"op":      "QuotaFromDecimal",
				"kind":    "overflow",
				"clamped": common.MaxQuota,
			},
		},
	})
	logs := []*Log{{Other: other}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	_, hasAdminInfo := parsed["admin_info"]
	require.False(t, hasAdminInfo, "admin_info (and nested quota_saturation) must be stripped for non-admin views")
	// Non-admin billing fields remain visible.
	require.Contains(t, parsed, "model_price")
}

// TestAttachGeoInfoToOtherMergesIntoExistingAdminInfo verifies locality
// hints nest into admin_info without clobbering existing fields (the
// RecordOperationAuditLog / RecordLoginLog / RecordTopupLog path), and
// degrade to a full no-op without mmdb files or for empty IPs (same degraded
// philosophy as pkg/geoip). admin_info is only created when a lookup
// actually resolves — with the CI-downloaded mmdb the create path is covered
// by the geoip integration tests.
func TestAttachGeoInfoToOtherMergesIntoExistingAdminInfo(t *testing.T) {
	cases := []struct {
		name  string
		ip    string
		other map[string]interface{}
	}{
		{
			name: "empty ip is no-op",
			ip:   "",
			other: map[string]interface{}{
				"admin_info": map[string]interface{}{"admin_username": "op"},
			},
		},
		{
			name: "unresolvable ip is no-op without mmdb",
			ip:   "8.8.8.8",
			other: map[string]interface{}{
				"admin_info": map[string]interface{}{"admin_username": "op"},
			},
		},
		{
			name:  "no admin_info created when lookup degrades",
			ip:    "8.8.8.8",
			other: map[string]interface{}{"op": map[string]interface{}{"action": "channel.status_update"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attachGeoInfoToOther(tc.other, tc.ip)
			adminInfo, ok := tc.other["admin_info"].(map[string]interface{})
			if tc.name == "no admin_info created when lookup degrades" {
				require.False(t, ok, "degraded lookup must not create an empty admin_info")
				return
			}
			require.True(t, ok, "admin_info must survive attach untouched")
			if u, ok := adminInfo["admin_username"]; ok {
				require.Equal(t, "op", u, "existing admin_info fields must survive")
			}
			_, hasGeo := adminInfo["geo"]
			require.False(t, hasGeo, "degraded mode (no mmdb in test env) must not attach geo")
		})
	}
}

func TestTaskPluginLogVisibilityIsRoleSeparated(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"model_price": 1.25,
		"admin_info": map[string]interface{}{
			"task_plugin": map[string]interface{}{
				"key":     "document-parser",
				"name":    "Document Parser",
				"version": "1.2.3",
			},
		},
		"root_info": map[string]interface{}{
			"upstream_task_id": "upstream-private",
			"task_plugin": map[string]interface{}{
				"generation": 42,
			},
		},
	})

	t.Run("user", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		formatUserLogs(logs, 0)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.NotContains(t, parsed, "admin_info")
		assert.NotContains(t, parsed, "root_info")
		assert.Equal(t, 1.25, parsed["model_price"])
	})

	t.Run("admin", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		FormatAdminLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Contains(t, parsed, "admin_info")
		assert.NotContains(t, parsed, "root_info")
	})

	t.Run("root", func(t *testing.T) {
		parsed, err := common.StrToMap(other)
		require.NoError(t, err)
		assert.Contains(t, parsed, "admin_info")
		assert.Contains(t, parsed, "root_info")
	})
}
