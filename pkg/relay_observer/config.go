// Package relayobserver defines the frozen contracts of the optional native
// relay observer: configuration, the turn event, the status snapshot, and the
// store port. The package must stay free of database driver imports: all
// PostgreSQL code lives in the adapter (store_pg*.go) that implements the
// Store port. The observer is strictly fail-open: every failure disables or
// degrades the observer and never changes relay responses, billing, or NewAPI
// startup.
package relayobserver

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// SchemaMode controls how the observer schema is handled at startup.
type SchemaMode string

const (
	// SchemaModeVerify checks the schema version and disables the observer on
	// mismatch. This is the default.
	SchemaModeVerify SchemaMode = "verify"
	// SchemaModeBootstrap may create the empty v1 schema under short
	// advisory-lock, lock, and statement timeouts.
	SchemaModeBootstrap SchemaMode = "bootstrap"
)

// Config is the frozen environment-owned configuration of the observer.
// Defaults and hard maximums mirror the architecture SSOT (Runtime Limits
// table and Configuration section). Numeric fields parsed from the environment
// are clamped into [1, hardMaximum]; key versions are clamped to >= 0.
// Unknown schema modes and non-numeric values produce an error, and the
// caller must then disable the observer with that error as the reason.
type Config struct {
	Enabled    bool
	SQLDSN     string
	SchemaMode SchemaMode

	// HMAC keys are secrets. They must never appear in status output, logs,
	// or API responses.
	HMACKey                string
	HMACKeyVersion         int
	PreviousHMACKey        string
	PreviousHMACKeyVersion int

	RecordIP bool

	// QueueSize bounds queued events; QueueBytes bounds the reserved request
	// bytes of queued events. A request larger than MaxRequestBytes becomes
	// metadata-only instead of entering the content path.
	QueueSize       int
	QueueBytes      int64
	MaxRequestBytes int64

	// MaxCaptureBytesPerTurn bounds the canonical content a worker may
	// produce for one turn — the capture budget, decoupled from the queue
	// admission estimate (P0-B). A request body below this cap captures
	// fully; canonical JSON is larger than the raw body (per-item HMAC +
	// structure overhead), so the capture cap is an upper bound on the
	// persisted content, not a raw-body proxy. There is no gap-marker
	// envelope knob: the normalizer charges the marker its exact serialized
	// size only when truncation actually happens, and content that fits the
	// cap is never truncated.
	MaxCaptureBytesPerTurn int64

	// BatchSize and FlushInterval bound the worker write batch; WriteTimeout
	// bounds a single batch write; QueryTimeout bounds Root database queries;
	// RetentionTimeout bounds one retention segment (independent of
	// QueryTimeout, since retention deletes at scale on its own goroutine).
	BatchSize        int
	FlushInterval    time.Duration
	WriteTimeout     time.Duration
	QueryTimeout     time.Duration
	RetentionTimeout time.Duration

	RetentionTurnDays    int
	RetentionContentDays int
}

// Redacted is the placeholder that replaces secrets (SQLDSN, HMAC keys) in
// every serialized or formatted view of Config. Production code reads the real
// fields directly; only output paths see Redacted.
const Redacted = "[redacted]"

// redactedConfig is Config with every secret field replaced by Redacted. The
// named type (not Config) stops MarshalJSON/String/GoString from recursing
// through their own method set.
type redactedConfig Config

func (c Config) redacted() redactedConfig {
	r := redactedConfig(c)
	r.SQLDSN = Redacted
	r.HMACKey = Redacted
	r.PreviousHMACKey = Redacted
	return r
}

// MarshalJSON implements json.Marshaler: the JSON view hides the real DSN and
// HMAC keys behind Redacted while keeping the default field names and shapes.
func (c Config) MarshalJSON() ([]byte, error) {
	return common.Marshal(c.redacted())
}

// String implements fmt.Stringer: %s and %v views hide the real DSN and HMAC
// keys behind Redacted.
func (c Config) String() string {
	return fmt.Sprintf("%+v", c.redacted())
}

// GoString implements fmt.GoStringer: %#v views hide the real DSN and HMAC
// keys behind Redacted.
func (c Config) GoString() string {
	return fmt.Sprintf("%#v", c.redacted())
}

// Defaults and hard maximums. Keep in sync with the architecture SSOT:
// Runtime Limits table and Configuration section.
const (
	DefaultQueueSize       = 512
	MaxQueueSize           = 4096
	DefaultQueueBytes      = 16 * 1024 * 1024 // 16 MiB reserved request bytes
	MaxQueueBytes          = 64 * 1024 * 1024 // 64 MiB
	DefaultMaxRequestBytes = 8 * 1024 * 1024  // 8 MiB one request content
	MaxMaxRequestBytes     = 16 * 1024 * 1024 // 16 MiB
	// DefaultMaxCaptureBytesPerTurn equals the request cap: the capture
	// budget decouples from the queue admission, so ordinary requests
	// capture fully (canonical overhead included) and only the request cap
	// limits oversized bodies.
	DefaultMaxCaptureBytesPerTurn = 8 * 1024 * 1024  // 8 MiB canonical per turn
	MaxMaxCaptureBytesPerTurn     = 16 * 1024 * 1024 // 16 MiB

	DefaultBatchSize = 32
	MaxBatchSize     = 128

	DefaultFlushInterval    = time.Second
	MaxFlushInterval        = 5 * time.Second
	DefaultWriteTimeout     = 2 * time.Second
	MaxWriteTimeout         = 5 * time.Second
	DefaultQueryTimeout     = 500 * time.Millisecond
	MaxQueryTimeout         = 2 * time.Second
	DefaultRetentionTimeout = 30 * time.Second
	MaxRetentionTimeout     = 5 * time.Minute
	// MaxHMACKeyVersion matches observer_session_aliases.key_version
	// (SMALLINT). Config must never admit a value PostgreSQL cannot store.
	MaxHMACKeyVersion = math.MaxInt16
	// MaxRetentionDays is a practical and overflow-safe upper bound. Ten
	// years already exceeds the observer's intended operational horizon.
	MaxRetentionDays = 3650

	DefaultRetentionTurnDays    = 30
	DefaultRetentionContentDays = 14
)

// Runtime-tunable option keys. These are read from common.OptionMap at each
// use point (hot reload, NewAPI convention) and fall back to the startup
// Config when unset.
const (
	optQueryTimeoutMs       = "relay_observer.query_timeout_ms"
	optRetentionTurnDays    = "relay_observer.retention_turn_days"
	optRetentionContentDays = "relay_observer.retention_content_days"
)

// RuntimeTunable is the hot-reloadable runtime tuning snapshot of the
// observer. It is deliberately separate from Config (the startup,
// environment-owned configuration): Config owns DSN, schema mode, HMAC keys,
// and the enabled switch, which are fixed at startup; RuntimeTunable owns
// parameters an operator may retune at runtime through the options table
// without restarting the process.
type RuntimeTunable struct {
	QueryTimeout         time.Duration
	RetentionTurnDays    int
	RetentionContentDays int
}

// GetRuntimeTunable reads the hot-reloadable runtime snapshot from the option
// map. Each field falls back to its startup Config value when the option is
// unset, empty, or invalid, and clamps to the same bounds as ConfigFromEnv.
// Callers read it at each use point so a changed option takes effect without
// a restart. Safe to call concurrently.
func GetRuntimeTunable(cfg Config) RuntimeTunable {
	return RuntimeTunable{
		QueryTimeout:         tunableDuration(optQueryTimeoutMs, cfg.QueryTimeout, MaxQueryTimeout),
		RetentionTurnDays:    tunableInt(optRetentionTurnDays, cfg.RetentionTurnDays, 1, MaxRetentionDays),
		RetentionContentDays: tunableInt(optRetentionContentDays, cfg.RetentionContentDays, 1, MaxRetentionDays),
	}
}

// tunableDuration resolves one millisecond-valued runtime option.
func tunableDuration(key string, fallback, max time.Duration) time.Duration {
	common.OptionMapRWMutex.RLock()
	raw := common.OptionMap[key]
	common.OptionMapRWMutex.RUnlock()
	if raw == "" {
		return fallback
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms < 1 {
		return fallback
	}
	d := time.Duration(ms) * time.Millisecond
	if d > max {
		return max
	}
	return d
}

// tunableInt resolves one integer-valued runtime option.
func tunableInt(key string, fallback, min, max int) int {
	common.OptionMapRWMutex.RLock()
	raw := common.OptionMap[key]
	common.OptionMapRWMutex.RUnlock()
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return clampInt(v, min, max)
}

// DefaultConfig returns the SSOT defaults for every field.
func DefaultConfig() Config {
	return Config{
		Enabled:                false,
		SchemaMode:             SchemaModeVerify,
		HMACKeyVersion:         1,
		QueueSize:              DefaultQueueSize,
		QueueBytes:             DefaultQueueBytes,
		MaxRequestBytes:        DefaultMaxRequestBytes,
		MaxCaptureBytesPerTurn: DefaultMaxCaptureBytesPerTurn,
		BatchSize:              DefaultBatchSize,
		FlushInterval:          DefaultFlushInterval,
		WriteTimeout:           DefaultWriteTimeout,
		QueryTimeout:           DefaultQueryTimeout,
		RetentionTimeout:       DefaultRetentionTimeout,
		RetentionTurnDays:      DefaultRetentionTurnDays,
		RetentionContentDays:   DefaultRetentionContentDays,
	}
}

// ConfigFromEnv parses the RELAY_OBSERVER_* environment. Missing or empty
// variables keep the defaults; numeric values are clamped into their bounds.
// Non-numeric values and unknown schema modes return an error.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()

	var err error
	if cfg.Enabled, err = envBool("RELAY_OBSERVER_ENABLED", cfg.Enabled); err != nil {
		return Config{}, err
	}
	cfg.SQLDSN = os.Getenv("RELAY_OBSERVER_SQL_DSN")

	if v := os.Getenv("RELAY_OBSERVER_SCHEMA_MODE"); v != "" {
		mode := SchemaMode(v)
		if mode != SchemaModeVerify && mode != SchemaModeBootstrap {
			return Config{}, fmt.Errorf("relayobserver: invalid RELAY_OBSERVER_SCHEMA_MODE %q", v)
		}
		cfg.SchemaMode = mode
	}

	cfg.HMACKey = os.Getenv("RELAY_OBSERVER_HMAC_KEY")
	if cfg.HMACKeyVersion, err = envInt("RELAY_OBSERVER_HMAC_KEY_VERSION", cfg.HMACKeyVersion, 0, MaxHMACKeyVersion); err != nil {
		return Config{}, err
	}
	cfg.PreviousHMACKey = os.Getenv("RELAY_OBSERVER_PREVIOUS_HMAC_KEY")
	if cfg.PreviousHMACKeyVersion, err = envInt("RELAY_OBSERVER_PREVIOUS_HMAC_KEY_VERSION", 0, 0, MaxHMACKeyVersion); err != nil {
		return Config{}, err
	}
	if cfg.RecordIP, err = envBool("RELAY_OBSERVER_RECORD_IP", cfg.RecordIP); err != nil {
		return Config{}, err
	}

	if cfg.QueueSize, err = envInt("RELAY_OBSERVER_QUEUE_SIZE", cfg.QueueSize, 1, MaxQueueSize); err != nil {
		return Config{}, err
	}
	if cfg.QueueBytes, err = envInt64("RELAY_OBSERVER_QUEUE_BYTES", cfg.QueueBytes, 1, MaxQueueBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBytes, err = envInt64("RELAY_OBSERVER_MAX_REQUEST_BYTES", cfg.MaxRequestBytes, 1, MaxMaxRequestBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxCaptureBytesPerTurn, err = envInt64("RELAY_OBSERVER_MAX_CAPTURE_BYTES_PER_TURN", cfg.MaxCaptureBytesPerTurn, 1, MaxMaxCaptureBytesPerTurn); err != nil {
		return Config{}, err
	}
	if cfg.BatchSize, err = envInt("RELAY_OBSERVER_BATCH_SIZE", cfg.BatchSize, 1, MaxBatchSize); err != nil {
		return Config{}, err
	}
	if cfg.FlushInterval, err = envDurationMS("RELAY_OBSERVER_FLUSH_MS", cfg.FlushInterval, time.Millisecond, MaxFlushInterval); err != nil {
		return Config{}, err
	}
	if cfg.WriteTimeout, err = envDurationMS("RELAY_OBSERVER_WRITE_TIMEOUT_MS", cfg.WriteTimeout, time.Millisecond, MaxWriteTimeout); err != nil {
		return Config{}, err
	}
	if cfg.QueryTimeout, err = envDurationMS("RELAY_OBSERVER_QUERY_TIMEOUT_MS", cfg.QueryTimeout, time.Millisecond, MaxQueryTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RetentionTimeout, err = envDurationMS("RELAY_OBSERVER_RETENTION_TIMEOUT_MS", cfg.RetentionTimeout, time.Millisecond, MaxRetentionTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RetentionTurnDays, err = envInt("RELAY_OBSERVER_RETENTION_TURN_DAYS", cfg.RetentionTurnDays, 1, MaxRetentionDays); err != nil {
		return Config{}, err
	}
	if cfg.RetentionContentDays, err = envInt("RELAY_OBSERVER_RETENTION_CONTENT_DAYS", cfg.RetentionContentDays, 1, MaxRetentionDays); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func envBool(name string, def bool) (bool, error) {
	s := os.Getenv(name)
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("relayobserver: invalid %s %q", name, s)
	}
	return v, nil
}

// envInt parses name into [lo, hi]; an empty variable keeps def.
func envInt(name string, def, lo, hi int) (int, error) {
	s := os.Getenv(name)
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("relayobserver: invalid %s %q", name, s)
	}
	return clampInt(v, lo, hi), nil
}

// envInt64 parses name into [lo, hi]; an empty variable keeps def.
func envInt64(name string, def, lo, hi int64) (int64, error) {
	s := os.Getenv(name)
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("relayobserver: invalid %s %q", name, s)
	}
	return clampInt64(v, lo, hi), nil
}

// envDurationMS parses name as milliseconds into [lo, hi]; an empty variable
// keeps def. The raw millisecond count is clamped before conversion so a huge
// value cannot overflow the duration multiplication.
func envDurationMS(name string, def, lo, hi time.Duration) (time.Duration, error) {
	s := os.Getenv(name)
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("relayobserver: invalid %s %q", name, s)
	}
	ms := clampInt64(v, 1, int64(hi/time.Millisecond))
	return time.Duration(ms) * time.Millisecond, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
