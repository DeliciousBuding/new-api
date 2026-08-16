package relayobserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// This file freezes the versioned session identity contract of the observer
// (T2.1): the HMAC alias, its scopes and sources, and the request-side
// resolution entry points. Raw session identifiers never cross this boundary:
// every value is normalized and hashed with a versioned HMAC key, and only
// (digest, key version, scope, source) is retained. The structs here are the
// stable output consumed by T2.3 session persistence; contract tests lock the
// shapes and the resolution rules below.

// SessionScope is the frozen identity-family scope of a session. Aliases from
// different scopes never merge: the scope is part of the alias identity, so a
// session observed by Codex and Claude stays two separate sessions even if
// their raw identifiers collide. The request path supplies the already
// detected client profile; this package only maps that low-cardinality value
// to the identity family and never re-parses request headers.
type SessionScope string

const (
	// ScopeCodexCLI marks the Codex CLI profile.
	ScopeCodexCLI SessionScope = "codex_cli"
	// ScopeCodexDesktop marks the Codex desktop profile.
	ScopeCodexDesktop SessionScope = "codex_desktop"
	// ScopeClaudeCLI marks the Claude CLI profile.
	ScopeClaudeCLI SessionScope = "claude_cli"
	// ScopeUnknown marks a request whose profile has no session identity
	// chain; it resolves to an empty result unless a body prompt_cache_key
	// is present (auxiliary rule, never merges sessions by itself).
	ScopeUnknown SessionScope = "unknown"
)

// AliasSource is the frozen source type of one alias, mirroring the SSOT
// "provider and source type" retention. It records which request channel the
// raw value came from, so operators can audit where a session key was born.
type AliasSource string

const (
	// SourceTurnThread is the thread_id field of the X-Codex-Turn-Metadata
	// JSON header. Highest-priority Codex primary alias.
	SourceTurnThread AliasSource = "turn_thread"
	// SourceTurnSession is the session_id field of the X-Codex-Turn-Metadata
	// JSON header; second in the Codex primary chain.
	SourceTurnSession AliasSource = "turn_session"
	// SourceHeaderThread is the standalone Thread_id / Thread-Id request
	// header (Codex 7c7b4861); auxiliary Codex source.
	SourceHeaderThread AliasSource = "header_thread"
	// SourceHeaderSession is the standalone Session_id / Session-Id request
	// header (Codex 7c7b4861); auxiliary Codex source.
	SourceHeaderSession AliasSource = "header_session"
	// SourceCacheKey is the body prompt_cache_key field. Auxiliary fallback:
	// it never overrides an explicit thread or session, and it is only read
	// for Codex-profile or unknown-profile requests.
	SourceCacheKey AliasSource = "prompt_cache_key"
	// SourceClaudeHeader is the X-Claude-Code-Session-Id request header;
	// highest-priority Claude primary alias.
	SourceClaudeHeader AliasSource = "claude_session_header"
	// SourceClaudeMetaUser is the body metadata.user_id.session_id field;
	// second in the Claude primary chain.
	SourceClaudeMetaUser AliasSource = "claude_meta_user_session"
	// SourceClaudeMetaSession is the body metadata.session_id field; third in
	// the Claude primary chain.
	SourceClaudeMetaSession AliasSource = "claude_meta_session"
	// SourceCredential marks an alias derived from a credential (used as the
	// session-scope user dimension when the user id is absent, per SSOT:
	// a versioned credential HMAC, never the raw key).
	SourceCredential AliasSource = "credential"
)

// Alias is the frozen, versioned HMAC alias of one session identifier. It is
// the only form in which a session identifier may be persisted or logged:
// Digest is the full 64-hex HMAC-SHA-256 of the raw value, Version is the
// HMAC key version that produced it, and Scope/Source record the profile and
// request channel. Two aliases are equal only when all four fields match, so
// scopes cannot merge and key generations stay distinguishable.
type Alias struct {
	Version int
	Digest  string
	Scope   SessionScope
	Source  AliasSource
}

// KeyMaterial carries the current and previous HMAC key generations. The
// current key creates new aliases; the previous key only verifies aliases
// produced before a rotation, so old digests stay resolvable for exactly one
// rotation generation. Raw keys never appear in any output of this package.
type KeyMaterial struct {
	CurrentKey      string
	CurrentVersion  int
	PreviousKey     string
	PreviousVersion int
}

// IdentityInput is one request's identity material: headers (canonical form,
// as net/http normalizes) and the raw request body used for gjson extraction
// of prompt_cache_key and Claude metadata. The body is read-only and is
// expected to come from the bounded BodyStorage of the request path.
type IdentityInput struct {
	// Scope is mapped from service.DetectClientProfile on the request path. It
	// selects one bounded identity chain but is not itself an identity
	// credential. The observer worker never re-parses request headers to infer
	// it.
	Scope   SessionScope
	Headers http.Header
	Body    []byte
}

// IdentityResult is the bounded resolution output of one request. Primary is
// the highest-priority available alias; Auxiliary holds every other available
// alias with a distinct digest, at most one per source. The set is finite by
// construction (the source chain has a fixed length), so conflicting sources
// never unbounded-merge. ScopeUnknown with an empty Primary means no session
// identity was resolvable.
//
// PreviousPrimary / PreviousAuxiliary hold the same raw values re-keyed under
// the previous generation (empty when no previous key is configured). A
// rotation window uses them to adopt a session bound under the old key
// instead of orphaning it.
type IdentityResult struct {
	Scope             SessionScope
	Primary           Alias
	Auxiliary         []Alias
	PreviousPrimary   Alias
	PreviousAuxiliary []Alias
}

// Budget constants for the identity extraction paths. A header or extracted
// value beyond its budget is skipped deterministically (fail-open), never
// parsed partially.
const (
	// MaxTurnMetadataHeaderBytes bounds the X-Codex-Turn-Metadata header
	// value before JSON extraction; oversized values yield no turn aliases.
	MaxTurnMetadataHeaderBytes = 4096
	// MaxAliasValueBytes bounds every raw candidate value before hashing;
	// oversized values yield no alias for that source.
	MaxAliasValueBytes = 2048
)

// SessionScopeForClientProfile maps the request-path client profile to the
// stable identity family used by alias generation. Profile variants that are
// different display labels but share the same protocol identity chain map to
// one scope, so changing a shell (for example Claude CLI to its VS Code
// extension) does not split a session. Profiles without a supported identity
// chain remain unknown and fail open.
//
// This is deliberately a value-only mapping. Header and User-Agent parsing
// belongs to service.DetectClientProfile; duplicating that parser here was
// the source of the scope/profile drift fixed by issue #118.
func SessionScopeForClientProfile(clientProfile string) SessionScope {
	switch strings.ToLower(strings.TrimSpace(clientProfile)) {
	case "codex_cli", "codex_vscode":
		return ScopeCodexCLI
	case "codex_desktop":
		return ScopeCodexDesktop
	case "claude_cli", "claude_vscode", "claude_desktop_3p":
		return ScopeClaudeCLI
	default:
		return ScopeUnknown
	}
}

// ResolveIdentity resolves the session aliases of one request: it maps the
// request-path profile to a scope, extracts the bounded candidate values in
// priority order, hashes them with the current key, and returns the primary
// alias plus the bounded auxiliary set. When a previous key is configured it
// also re-keys every candidate under the previous generation: a rotation
// window uses those to adopt a session bound under the old key. Malformed or
// missing sources are skipped deterministically; only an unconfigured HMAC
// key is an error. Raw identifiers never appear in the result.
func ResolveIdentity(in IdentityInput, km KeyMaterial) (IdentityResult, error) {
	if km.CurrentKey == "" {
		return IdentityResult{}, errHMACKeyNotConfigured
	}
	scope := in.Scope
	if scope == "" {
		scope = ScopeUnknown
	}
	candidates := collectCandidates(in, scope)

	seen := make(map[string]bool, len(candidates))
	prevSeen := make(map[string]bool, len(candidates))
	res := IdentityResult{Scope: scope}
	for _, c := range candidates {
		a, err := GenerateAlias(c.value, c.source, scope, km)
		if err != nil {
			continue // bounded value failed: skip the source, fail-open
		}
		if !seen[a.Digest] {
			seen[a.Digest] = true
			if res.Primary.Digest == "" {
				res.Primary = a
			} else {
				res.Auxiliary = append(res.Auxiliary, a)
			}
		}
		//  re-key the same raw value under the previous generation so a
		// rotation window can adopt a session bound under the old key.
		if km.PreviousKey != "" {
			pa, err := generateAliasWith(c.value, c.source, scope, km.PreviousKey, km.PreviousVersion)
			if err == nil && !prevSeen[pa.Digest] {
				prevSeen[pa.Digest] = true
				if res.PreviousPrimary.Digest == "" {
					res.PreviousPrimary = pa
				} else {
					res.PreviousAuxiliary = append(res.PreviousAuxiliary, pa)
				}
			}
		}
	}
	return res, nil
}

// generateAliasWith creates a versioned HMAC alias of one raw value with an
// explicit key/version. GenerateAlias and the previous-generation adoption
// path both use it; the raw value is never retained.
func generateAliasWith(value string, source AliasSource, scope SessionScope, key string, version int) (Alias, error) {
	if key == "" {
		return Alias{}, errHMACKeyNotConfigured
	}
	if value == "" || len(value) > MaxAliasValueBytes {
		return Alias{}, fmt.Errorf("relayobserver: identity: value out of bounds (%d bytes)", len(value))
	}
	digest := hmacSHA256Hex(key, value)
	return Alias{Version: version, Digest: digest, Scope: scope, Source: source}, nil
}

// GenerateAlias creates a versioned HMAC alias of one raw value with the
// current key. It is the shared generator, also used by later phases for the
// credential alias of SSOT's "versioned credential HMAC" user dimension. The
// raw value is not retained anywhere.
func GenerateAlias(value string, source AliasSource, scope SessionScope, km KeyMaterial) (Alias, error) {
	return generateAliasWith(value, source, scope, km.CurrentKey, km.CurrentVersion)
}

// VerifyAlias checks a raw value against an existing alias, selecting the key
// by the alias version (current, or previous for exactly one rotation
// generation). It returns false for unknown versions, empty keys, malformed
// digests, and mismatches.
func VerifyAlias(a Alias, value string, km KeyMaterial) bool {
	key, ok := keyForVersion(a.Version, km)
	if !ok || key == "" {
		return false
	}
	raw, err := hex.DecodeString(a.Digest)
	if err != nil || len(raw) != sha256.Size {
		return false
	}
	computed := hmacSHA256(key, value)
	return hmac.Equal(computed, raw)
}

func keyForVersion(version int, km KeyMaterial) (string, bool) {
	switch version {
	case km.CurrentVersion:
		return km.CurrentKey, true
	case km.PreviousVersion:
		return km.PreviousKey, true
	}
	return "", false
}

func hmacSHA256(key, value string) []byte {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(value))
	return m.Sum(nil)
}

func hmacSHA256Hex(key, value string) string {
	return hex.EncodeToString(hmacSHA256(key, value))
}

var errHMACKeyNotConfigured = errors.New("relayobserver: identity: HMAC key not configured")

// identityCandidate is one bounded raw value and the source it came from,
// collected in SSOT priority order.
type identityCandidate struct {
	source AliasSource
	value  string
}

// collectCandidates extracts the bounded candidate values of one scope in
// priority order. Each candidate is trimmed, budget-checked, and skipped
// deterministically when malformed, non-string, empty, or oversized; a
// failed extraction never panics.
func collectCandidates(in IdentityInput, scope SessionScope) []identityCandidate {
	var out []identityCandidate
	add := func(source AliasSource, value string) {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > MaxAliasValueBytes {
			return
		}
		out = append(out, identityCandidate{source: source, value: value})
	}

	switch scope {
	case ScopeCodexCLI, ScopeCodexDesktop:
		if meta := in.Headers.Get("X-Codex-Turn-Metadata"); meta != "" && len(meta) <= MaxTurnMetadataHeaderBytes {
			metaBytes := []byte(meta)
			add(SourceTurnThread, gjsonString(gjson.GetBytes(metaBytes, "thread_id")))
			add(SourceTurnSession, gjsonString(gjson.GetBytes(metaBytes, "session_id")))
		}
		add(SourceHeaderThread, firstHeader(in.Headers, "Thread_id", "Thread-Id"))
		add(SourceHeaderSession, firstHeader(in.Headers, "Session_id", "Session-Id"))
		add(SourceCacheKey, cacheKeyOf(in.Body))
	case ScopeClaudeCLI:
		add(SourceClaudeHeader, in.Headers.Get("X-Claude-Code-Session-Id"))
		add(SourceClaudeMetaUser, gjsonString(gjson.GetBytes(in.Body, "metadata.user_id.session_id")))
		add(SourceClaudeMetaSession, gjsonString(gjson.GetBytes(in.Body, "metadata.session_id")))
	case ScopeUnknown:
		add(SourceCacheKey, cacheKeyOf(in.Body))
	}
	return out
}

// firstHeader returns the first non-empty of the given header names; the
// standalone Codex headers appear in both underscore and dash spellings.
func firstHeader(h http.Header, names ...string) string {
	for _, name := range names {
		if v := h.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// cacheKeyOf reads the body prompt_cache_key as a string. The responses and
// compaction DTOs carry it as RawMessage, so only a JSON string is a valid
// candidate; objects, arrays, numbers, and nulls are skipped.
func cacheKeyOf(body []byte) string {
	return gjsonString(gjson.GetBytes(body, "prompt_cache_key"))
}

// gjsonString returns the string value only when the result is a JSON string.
// Other types (objects, arrays, numbers) are not valid session identifiers
// and yield nothing, deterministically.
func gjsonString(res gjson.Result) string {
	if !res.Exists() || res.Type != gjson.String {
		return ""
	}
	return res.String()
}
