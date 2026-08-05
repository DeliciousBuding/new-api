package relayobserver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearObserverEnv removes every RELAY_OBSERVER_* variable so tests observe
// only the variables they set.
func clearObserverEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{
		"RELAY_OBSERVER_ENABLED",
		"RELAY_OBSERVER_SQL_DSN",
		"RELAY_OBSERVER_SCHEMA_MODE",
		"RELAY_OBSERVER_HMAC_KEY",
		"RELAY_OBSERVER_HMAC_KEY_VERSION",
		"RELAY_OBSERVER_PREVIOUS_HMAC_KEY",
		"RELAY_OBSERVER_PREVIOUS_HMAC_KEY_VERSION",
		"RELAY_OBSERVER_RECORD_IP",
		"RELAY_OBSERVER_QUEUE_SIZE",
		"RELAY_OBSERVER_QUEUE_BYTES",
		"RELAY_OBSERVER_MAX_REQUEST_BYTES",
		"RELAY_OBSERVER_BATCH_SIZE",
		"RELAY_OBSERVER_FLUSH_MS",
		"RELAY_OBSERVER_WRITE_TIMEOUT_MS",
		"RELAY_OBSERVER_QUERY_TIMEOUT_MS",
		"RELAY_OBSERVER_RETENTION_TURN_DAYS",
		"RELAY_OBSERVER_RETENTION_CONTENT_DAYS",
		"RELAY_OBSERVER_MAX_CAPTURE_BYTES_PER_TURN",
	} {
		t.Setenv(e, "")
	}
}

// TestDefaultConfig locks every default to the architecture SSOT
// (Configuration section and Runtime Limits table).
func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.False(t, cfg.Enabled)
	assert.Equal(t, "", cfg.SQLDSN)
	assert.Equal(t, SchemaModeVerify, cfg.SchemaMode)
	assert.Equal(t, "", cfg.HMACKey)
	assert.Equal(t, 1, cfg.HMACKeyVersion)
	assert.Equal(t, "", cfg.PreviousHMACKey)
	assert.Equal(t, 0, cfg.PreviousHMACKeyVersion)
	assert.False(t, cfg.RecordIP)

	assert.Equal(t, 512, cfg.QueueSize)
	assert.Equal(t, int64(16*1024*1024), cfg.QueueBytes)
	assert.Equal(t, int64(8*1024*1024), cfg.MaxRequestBytes)
	assert.Equal(t, 32, cfg.BatchSize)
	assert.Equal(t, time.Second, cfg.FlushInterval)
	assert.Equal(t, 2*time.Second, cfg.WriteTimeout)
	assert.Equal(t, 500*time.Millisecond, cfg.QueryTimeout)
	assert.Equal(t, 30, cfg.RetentionTurnDays)
	assert.Equal(t, 14, cfg.RetentionContentDays)
}

// TestConfigFromEnvEmptyEnv verifies that an unset environment yields exactly
// the defaults.
func TestConfigFromEnvEmptyEnv(t *testing.T) {
	clearObserverEnv(t)

	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, DefaultConfig(), cfg)
}

// TestConfigFromEnvValues verifies valid environment values pass through
// unchanged (no clamping on in-range values).
func TestConfigFromEnvValues(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_ENABLED", "true")
	t.Setenv("RELAY_OBSERVER_SQL_DSN", "postgres://observer:secret@localhost:5432/audit")
	t.Setenv("RELAY_OBSERVER_SCHEMA_MODE", "bootstrap")
	t.Setenv("RELAY_OBSERVER_HMAC_KEY", "k1")
	t.Setenv("RELAY_OBSERVER_HMAC_KEY_VERSION", "2")
	t.Setenv("RELAY_OBSERVER_PREVIOUS_HMAC_KEY", "k0")
	t.Setenv("RELAY_OBSERVER_PREVIOUS_HMAC_KEY_VERSION", "1")
	t.Setenv("RELAY_OBSERVER_RECORD_IP", "true")
	t.Setenv("RELAY_OBSERVER_QUEUE_SIZE", "100")
	t.Setenv("RELAY_OBSERVER_QUEUE_BYTES", "1048576")
	t.Setenv("RELAY_OBSERVER_MAX_REQUEST_BYTES", "524288")
	t.Setenv("RELAY_OBSERVER_MAX_CAPTURE_BYTES_PER_TURN", "262144")
	t.Setenv("RELAY_OBSERVER_BATCH_SIZE", "16")
	t.Setenv("RELAY_OBSERVER_FLUSH_MS", "250")
	t.Setenv("RELAY_OBSERVER_WRITE_TIMEOUT_MS", "1000")
	t.Setenv("RELAY_OBSERVER_QUERY_TIMEOUT_MS", "100")
	t.Setenv("RELAY_OBSERVER_RETENTION_TURN_DAYS", "7")
	t.Setenv("RELAY_OBSERVER_RETENTION_CONTENT_DAYS", "3")

	cfg, err := ConfigFromEnv()
	require.NoError(t, err)

	assert.True(t, cfg.Enabled)
	assert.Equal(t, "postgres://observer:secret@localhost:5432/audit", cfg.SQLDSN)
	assert.Equal(t, SchemaModeBootstrap, cfg.SchemaMode)
	assert.Equal(t, "k1", cfg.HMACKey)
	assert.Equal(t, 2, cfg.HMACKeyVersion)
	assert.Equal(t, "k0", cfg.PreviousHMACKey)
	assert.Equal(t, 1, cfg.PreviousHMACKeyVersion)
	assert.True(t, cfg.RecordIP)
	assert.Equal(t, 100, cfg.QueueSize)
	assert.Equal(t, int64(1048576), cfg.QueueBytes)
	assert.Equal(t, int64(524288), cfg.MaxRequestBytes)
	assert.Equal(t, int64(262144), cfg.MaxCaptureBytesPerTurn)
	assert.Equal(t, 16, cfg.BatchSize)
	assert.Equal(t, 250*time.Millisecond, cfg.FlushInterval)
	assert.Equal(t, time.Second, cfg.WriteTimeout)
	assert.Equal(t, 100*time.Millisecond, cfg.QueryTimeout)
	assert.Equal(t, 7, cfg.RetentionTurnDays)
	assert.Equal(t, 3, cfg.RetentionContentDays)
}

// TestConfigClampsUpper verifies every numeric field with a hard maximum
// clamps to that maximum (Runtime Limits table).
func TestConfigClampsUpper(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		value string
		check func(t *testing.T, cfg Config)
	}{
		{"queue size", "RELAY_OBSERVER_QUEUE_SIZE", "10000", func(t *testing.T, c Config) {
			assert.Equal(t, MaxQueueSize, c.QueueSize)
		}},
		{"queue bytes", "RELAY_OBSERVER_QUEUE_BYTES", "1073741824", func(t *testing.T, c Config) {
			assert.Equal(t, int64(MaxQueueBytes), c.QueueBytes)
		}},
		{"max request bytes", "RELAY_OBSERVER_MAX_REQUEST_BYTES", "1073741824", func(t *testing.T, c Config) {
			assert.Equal(t, int64(MaxMaxRequestBytes), c.MaxRequestBytes)
		}},
		{"batch size", "RELAY_OBSERVER_BATCH_SIZE", "999", func(t *testing.T, c Config) {
			assert.Equal(t, MaxBatchSize, c.BatchSize)
		}},
		{"flush interval", "RELAY_OBSERVER_FLUSH_MS", "60000", func(t *testing.T, c Config) {
			assert.Equal(t, MaxFlushInterval, c.FlushInterval)
		}},
		{"write timeout", "RELAY_OBSERVER_WRITE_TIMEOUT_MS", "60000", func(t *testing.T, c Config) {
			assert.Equal(t, MaxWriteTimeout, c.WriteTimeout)
		}},
		{"query timeout", "RELAY_OBSERVER_QUERY_TIMEOUT_MS", "60000", func(t *testing.T, c Config) {
			assert.Equal(t, MaxQueryTimeout, c.QueryTimeout)
		}},
		{"hmac key version", "RELAY_OBSERVER_HMAC_KEY_VERSION", "1000000", func(t *testing.T, c Config) {
			assert.Equal(t, MaxHMACKeyVersion, c.HMACKeyVersion)
		}},
		{"previous hmac key version", "RELAY_OBSERVER_PREVIOUS_HMAC_KEY_VERSION", "1000000", func(t *testing.T, c Config) {
			assert.Equal(t, MaxHMACKeyVersion, c.PreviousHMACKeyVersion)
		}},
		{"retention turn days", "RELAY_OBSERVER_RETENTION_TURN_DAYS", "1000000", func(t *testing.T, c Config) {
			assert.Equal(t, MaxRetentionDays, c.RetentionTurnDays)
		}},
		{"retention content days", "RELAY_OBSERVER_RETENTION_CONTENT_DAYS", "1000000", func(t *testing.T, c Config) {
			assert.Equal(t, MaxRetentionDays, c.RetentionContentDays)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearObserverEnv(t)
			t.Setenv(tc.env, tc.value)

			cfg, err := ConfigFromEnv()
			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}

// TestConfigClampsLower verifies the deterministic floors: every capacity and
// timeout is at least 1 (1 ms for durations), retention days at least 1, and
// key versions at least 0. The SSOT defines only hard maximums; the floors
// prevent pathological zero-capacity configurations.
func TestConfigClampsLower(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		value string
		check func(t *testing.T, cfg Config)
	}{
		{"queue size", "RELAY_OBSERVER_QUEUE_SIZE", "0", func(t *testing.T, c Config) {
			assert.Equal(t, 1, c.QueueSize)
		}},
		{"queue size negative", "RELAY_OBSERVER_QUEUE_SIZE", "-5", func(t *testing.T, c Config) {
			assert.Equal(t, 1, c.QueueSize)
		}},
		{"queue bytes", "RELAY_OBSERVER_QUEUE_BYTES", "0", func(t *testing.T, c Config) {
			assert.Equal(t, int64(1), c.QueueBytes)
		}},
		{"max request bytes", "RELAY_OBSERVER_MAX_REQUEST_BYTES", "0", func(t *testing.T, c Config) {
			assert.Equal(t, int64(1), c.MaxRequestBytes)
		}},
		{"batch size", "RELAY_OBSERVER_BATCH_SIZE", "0", func(t *testing.T, c Config) {
			assert.Equal(t, 1, c.BatchSize)
		}},
		{"flush interval", "RELAY_OBSERVER_FLUSH_MS", "0", func(t *testing.T, c Config) {
			assert.Equal(t, time.Millisecond, c.FlushInterval)
		}},
		{"write timeout", "RELAY_OBSERVER_WRITE_TIMEOUT_MS", "0", func(t *testing.T, c Config) {
			assert.Equal(t, time.Millisecond, c.WriteTimeout)
		}},
		{"query timeout", "RELAY_OBSERVER_QUERY_TIMEOUT_MS", "0", func(t *testing.T, c Config) {
			assert.Equal(t, time.Millisecond, c.QueryTimeout)
		}},
		{"retention turn days", "RELAY_OBSERVER_RETENTION_TURN_DAYS", "0", func(t *testing.T, c Config) {
			assert.Equal(t, 1, c.RetentionTurnDays)
		}},
		{"retention content days", "RELAY_OBSERVER_RETENTION_CONTENT_DAYS", "-3", func(t *testing.T, c Config) {
			assert.Equal(t, 1, c.RetentionContentDays)
		}},
		{"hmac key version", "RELAY_OBSERVER_HMAC_KEY_VERSION", "-1", func(t *testing.T, c Config) {
			assert.Equal(t, 0, c.HMACKeyVersion)
		}},
		{"previous hmac key version", "RELAY_OBSERVER_PREVIOUS_HMAC_KEY_VERSION", "-1", func(t *testing.T, c Config) {
			assert.Equal(t, 0, c.PreviousHMACKeyVersion)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearObserverEnv(t)
			t.Setenv(tc.env, tc.value)

			cfg, err := ConfigFromEnv()
			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}

// TestConfigFromEnvErrors verifies that unparseable values return an error so
// the caller can disable the observer with a reason (fail-open).
func TestConfigFromEnvErrors(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"enabled not a bool", "RELAY_OBSERVER_ENABLED"},
		{"record ip not a bool", "RELAY_OBSERVER_RECORD_IP"},
		{"queue size not a number", "RELAY_OBSERVER_QUEUE_SIZE"},
		{"queue bytes overflow", "RELAY_OBSERVER_QUEUE_BYTES"},
		{"batch size not a number", "RELAY_OBSERVER_BATCH_SIZE"},
		{"flush interval not a number", "RELAY_OBSERVER_FLUSH_MS"},
		{"write timeout not a number", "RELAY_OBSERVER_WRITE_TIMEOUT_MS"},
		{"query timeout not a number", "RELAY_OBSERVER_QUERY_TIMEOUT_MS"},
		{"retention not a number", "RELAY_OBSERVER_RETENTION_TURN_DAYS"},
		{"hmac key version not a number", "RELAY_OBSERVER_HMAC_KEY_VERSION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearObserverEnv(t)
			t.Setenv(tc.env, "not-a-valid-value")

			_, err := ConfigFromEnv()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.env)
		})
	}

	t.Run("unknown schema mode", func(t *testing.T) {
		clearObserverEnv(t)
		t.Setenv("RELAY_OBSERVER_SCHEMA_MODE", "create-drop")

		_, err := ConfigFromEnv()
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "RELAY_OBSERVER_SCHEMA_MODE"))
	})
}

// TestConfigFromEnvSchemaMode verifies schema mode parsing.
func TestConfigFromEnvSchemaMode(t *testing.T) {
	for _, mode := range []SchemaMode{SchemaModeVerify, SchemaModeBootstrap} {
		t.Run(string(mode), func(t *testing.T) {
			clearObserverEnv(t)
			t.Setenv("RELAY_OBSERVER_SCHEMA_MODE", string(mode))

			cfg, err := ConfigFromEnv()
			require.NoError(t, err)
			assert.Equal(t, mode, cfg.SchemaMode)
		})
	}
}

// TestConfigParseDoesNotAffectNeighbors verifies that clamping one field
// leaves the others at their defaults.
func TestConfigParseDoesNotAffectNeighbors(t *testing.T) {
	clearObserverEnv(t)
	t.Setenv("RELAY_OBSERVER_QUEUE_SIZE", "10000") // clamps to 4096

	cfg, err := ConfigFromEnv()
	require.NoError(t, err)

	def := DefaultConfig()
	assert.Equal(t, MaxQueueSize, cfg.QueueSize)
	assert.Equal(t, def.QueueBytes, cfg.QueueBytes)
	assert.Equal(t, def.MaxRequestBytes, cfg.MaxRequestBytes)
	assert.Equal(t, def.BatchSize, cfg.BatchSize)
	assert.Equal(t, def.FlushInterval, cfg.FlushInterval)
	assert.Equal(t, def.WriteTimeout, cfg.WriteTimeout)
	assert.Equal(t, def.QueryTimeout, cfg.QueryTimeout)
	assert.Equal(t, def.RetentionTurnDays, cfg.RetentionTurnDays)
	assert.Equal(t, def.RetentionContentDays, cfg.RetentionContentDays)
}

// TestConfigOutputRedactsSecrets constructs a Config holding unique sentinel
// secrets and asserts that no serialization or formatting view leaks them:
// common.Marshal (JSON), fmt.Sprintf("%s"/"%v"/"%+v") (Stringer), and
// fmt.Sprintf("%#v") (GoStringer) must all hide the real DSN and both HMAC
// keys behind [redacted]. The production fields themselves must stay directly
// readable.
func TestConfigOutputRedactsSecrets(t *testing.T) {
	const (
		sentinelDSN  = "postgres://sentinel-user:sentinel-pass@sentinel-host:5432/sentinel-db?sslmode=require"
		sentinelHMAC = "sentinel-hmac-current-0123456789abcdef"
		sentinelPrev = "sentinel-hmac-prev-fedcba9876543210"
	)
	secrets := []string{
		sentinelDSN, "sentinel-user", "sentinel-pass", "sentinel-host", "sentinel-db",
		sentinelHMAC, sentinelPrev,
	}

	cfg := Config{
		Enabled:                true,
		SQLDSN:                 sentinelDSN,
		SchemaMode:             SchemaModeBootstrap,
		HMACKey:                sentinelHMAC,
		HMACKeyVersion:         2,
		PreviousHMACKey:        sentinelPrev,
		PreviousHMACKeyVersion: 1,
		RecordIP:               true,
		QueueSize:              100,
		QueueBytes:             1048576,
		MaxRequestBytes:        524288,
		BatchSize:              16,
		FlushInterval:          250 * time.Millisecond,
		WriteTimeout:           time.Second,
		QueryTimeout:           100 * time.Millisecond,
		RetentionTurnDays:      7,
		RetentionContentDays:   3,
	}

	// Production code reads the real fields directly.
	assert.Equal(t, sentinelDSN, cfg.SQLDSN)
	assert.Equal(t, sentinelHMAC, cfg.HMACKey)
	assert.Equal(t, sentinelPrev, cfg.PreviousHMACKey)

	data, err := common.Marshal(cfg)
	require.NoError(t, err)

	outputs := map[string]string{
		"common.Marshal":        string(data),
		"fmt.Sprintf(%s, cfg)":  fmt.Sprintf("%s", cfg),
		"fmt.Sprintf(%v, cfg)":  fmt.Sprintf("%v", cfg),
		"fmt.Sprintf(%+v, cfg)": fmt.Sprintf("%+v", cfg),
		"fmt.Sprintf(%#v, cfg)": fmt.Sprintf("%#v", cfg),
	}
	for name, out := range outputs {
		for _, secret := range secrets {
			assert.NotContains(t, out, secret, "%s must not leak %q", name, secret)
		}
		assert.Contains(t, out, "[redacted]", "%s must mark redacted fields", name)
	}
}
