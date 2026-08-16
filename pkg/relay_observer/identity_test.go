package relayobserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hmacHex recomputes the expected digest independently of the code under
// test, so the tests verify the HMAC scheme itself rather than mirroring the
// implementation.
func hmacHex(key, value string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))
}

func testKM(key string, version int) KeyMaterial {
	return KeyMaterial{CurrentKey: key, CurrentVersion: version}
}

func codexHeaders(metadata string) http.Header {
	h := http.Header{}
	h.Set("X-Codex-Turn-State", "started")
	if metadata != "" {
		h.Set("X-Codex-Turn-Metadata", metadata)
	}
	return h
}

func claudeHeaders(sessionID string) http.Header {
	h := http.Header{}
	h.Set("X-App", "claude-cli/1.2.3")
	if sessionID != "" {
		h.Set("X-Claude-Code-Session-Id", sessionID)
	}
	return h
}

// TestGoldenDigestLocksHMACSHA256 locks the alias digest scheme: the digest
// must be the full 64-hex HMAC-SHA-256 of the raw value with the current key.
// A shorter scheme (for example the 32-bit affinity fingerprint) fails here.
func TestGoldenDigestLocksHMACSHA256(t *testing.T) {
	km := testKM("observer-golden-key", 7)
	a, err := GenerateAlias("golden-session", SourceTurnThread, ScopeCodexCLI, km)
	require.NoError(t, err)
	assert.Equal(t, "b36888d623822c7904b8e5f6882588e2d255bf933cc1c2539c0e6666bf6acec3", a.Digest)
	assert.Equal(t, 7, a.Version)
	assert.Equal(t, ScopeCodexCLI, a.Scope)
	assert.Equal(t, SourceTurnThread, a.Source)
}

// TestResolveCodexTurnMetadataThreadPrimary covers the primary Codex chain:
// X-Codex-Turn-Metadata's thread_id is the primary alias and session_id the
// auxiliary one, both versioned digests of the raw values.
func TestResolveCodexTurnMetadataThreadPrimary(t *testing.T) {
	km := testKM("observer-test-key", 1)
	in := IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: codexHeaders(`{"thread_id":"t-1","session_id":"s-1"}`),
	}
	res, err := ResolveIdentity(in, km)
	require.NoError(t, err)
	assert.Equal(t, ScopeCodexCLI, res.Scope)
	require.NotNil(t, res.Primary)
	assert.Equal(t, SourceTurnThread, res.Primary.Source)
	assert.Equal(t, hmacHex("observer-test-key", "t-1"), res.Primary.Digest)
	assert.Equal(t, 1, res.Primary.Version)
	require.Len(t, res.Auxiliary, 1)
	assert.Equal(t, SourceTurnSession, res.Auxiliary[0].Source)
	assert.Equal(t, hmacHex("observer-test-key", "s-1"), res.Auxiliary[0].Digest)
}

// TestResolveCodexTurnMetadataSessionOnly covers a Turn-Metadata that carries
// only session_id: it becomes the primary alias.
func TestResolveCodexTurnMetadataSessionOnly(t *testing.T) {
	km := testKM("observer-test-key", 1)
	res, err := ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: codexHeaders(`{"session_id":"s-only"}`)}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceTurnSession, res.Primary.Source)
	assert.Equal(t, hmacHex("observer-test-key", "s-only"), res.Primary.Digest)
	assert.Empty(t, res.Auxiliary)
}

// TestResolveClaudeHeaderPrimary covers the primary Claude chain: the
// X-Claude-Code-Session-Id header wins over both metadata forms.
func TestResolveClaudeHeaderPrimary(t *testing.T) {
	km := testKM("observer-test-key", 1)
	res, err := ResolveIdentity(IdentityInput{
		Scope:   ScopeClaudeCLI,
		Headers: claudeHeaders("c-1"),
		Body:    []byte(`{"metadata":{"user_id":{"session_id":"u-1"},"session_id":"m-1"}}`),
	}, km)
	require.NoError(t, err)
	assert.Equal(t, ScopeClaudeCLI, res.Scope)
	assert.Equal(t, SourceClaudeHeader, res.Primary.Source)
	assert.Equal(t, hmacHex("observer-test-key", "c-1"), res.Primary.Digest)
	require.Len(t, res.Auxiliary, 2)
	assert.Equal(t, SourceClaudeMetaUser, res.Auxiliary[0].Source)
	assert.Equal(t, hmacHex("observer-test-key", "u-1"), res.Auxiliary[0].Digest)
	assert.Equal(t, SourceClaudeMetaSession, res.Auxiliary[1].Source)
	assert.Equal(t, hmacHex("observer-test-key", "m-1"), res.Auxiliary[1].Digest)
}

// TestResolveClaudeMetadataChainPriority covers the metadata chain order
// without the header: metadata.user_id.session_id beats metadata.session_id.
func TestResolveClaudeMetadataChainPriority(t *testing.T) {
	km := testKM("observer-test-key", 1)
	res, err := ResolveIdentity(IdentityInput{
		Scope:   ScopeClaudeCLI,
		Headers: claudeHeaders(""),
		Body:    []byte(`{"metadata":{"user_id":{"session_id":"u-1"},"session_id":"m-1"}}`),
	}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceClaudeMetaUser, res.Primary.Source)
	require.Len(t, res.Auxiliary, 1)
	assert.Equal(t, SourceClaudeMetaSession, res.Auxiliary[0].Source)

	res, err = ResolveIdentity(IdentityInput{
		Scope:   ScopeClaudeCLI,
		Headers: claudeHeaders(""),
		Body:    []byte(`{"metadata":{"session_id":"m-1"}}`),
	}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceClaudeMetaSession, res.Primary.Source)
	assert.Empty(t, res.Auxiliary)
}

// TestResolveCodexHeaderSources covers the standalone Session_id/Thread_id
// headers, thread first, as auxiliary Codex sources.
func TestResolveCodexHeaderSources(t *testing.T) {
	km := testKM("observer-test-key", 1)
	h := codexHeaders("")
	h.Set("Thread_id", "ht-1")
	h.Set("Session_id", "hs-1")
	res, err := ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: h}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceHeaderThread, res.Primary.Source)
	require.Len(t, res.Auxiliary, 1)
	assert.Equal(t, SourceHeaderSession, res.Auxiliary[0].Source)

	h = codexHeaders("")
	h.Set("Session-Id", "hs-2")
	res, err = ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: h}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceHeaderSession, res.Primary.Source)
	assert.Equal(t, hmacHex("observer-test-key", "hs-2"), res.Primary.Digest)
}

// TestResolveCacheKeyFallbackPriority covers prompt_cache_key as the lowest-
// priority Codex fallback: it never overrides an explicit thread.
func TestResolveCacheKeyFallbackPriority(t *testing.T) {
	km := testKM("observer-test-key", 1)

	// Fallback only: no explicit thread/session, cache key becomes primary.
	res, err := ResolveIdentity(IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: codexHeaders(""),
		Body:    []byte(`{"prompt_cache_key":"ck-1"}`),
	}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceCacheKey, res.Primary.Source)
	assert.Equal(t, hmacHex("observer-test-key", "ck-1"), res.Primary.Digest)

	// Explicit thread present: cache key stays auxiliary and never overrides.
	res, err = ResolveIdentity(IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: codexHeaders(`{"thread_id":"t-1"}`),
		Body:    []byte(`{"prompt_cache_key":"ck-1"}`),
	}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceTurnThread, res.Primary.Source)
	require.Len(t, res.Auxiliary, 1)
	assert.Equal(t, SourceCacheKey, res.Auxiliary[0].Source)
}

// TestResolveCacheKeyScopeRules covers the scope rules of prompt_cache_key:
// read only for Codex-profile or unknown-profile requests, never for Claude.
func TestResolveCacheKeyScopeRules(t *testing.T) {
	km := testKM("observer-test-key", 1)
	body := []byte(`{"prompt_cache_key":"ck-1"}`)

	// Unknown profile: cache key is the auxiliary key of SSOT's "unknown
	// clients" rule; with no other source it becomes the primary alias.
	res, err := ResolveIdentity(IdentityInput{Scope: ScopeUnknown, Body: body}, km)
	require.NoError(t, err)
	assert.Equal(t, ScopeUnknown, res.Scope)
	assert.Equal(t, SourceCacheKey, res.Primary.Source)

	// Claude profile: cache key produces no alias.
	res, err = ResolveIdentity(IdentityInput{Scope: ScopeClaudeCLI, Headers: claudeHeaders("c-1"), Body: body}, km)
	require.NoError(t, err)
	assert.Equal(t, ScopeClaudeCLI, res.Scope)
	assert.Equal(t, SourceClaudeHeader, res.Primary.Source)
	for _, a := range res.Auxiliary {
		assert.NotEqual(t, SourceCacheKey, a.Source)
	}
}

// TestResolveCacheKeyNonStringSkipped covers a non-string prompt_cache_key
// (responses DTO carries it as RawMessage): not a valid candidate, skipped.
func TestResolveCacheKeyNonStringSkipped(t *testing.T) {
	km := testKM("observer-test-key", 1)
	res, err := ResolveIdentity(IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: codexHeaders(""),
		Body:    []byte(`{"prompt_cache_key":{"a":1}}`),
	}, km)
	require.NoError(t, err)
	assert.Empty(t, res.Primary)
	assert.Empty(t, res.Auxiliary)
}

// TestVersionedRotationOldAliasResolvable covers key rotation: an alias
// produced under the old key stays verifiable through the previous-key slot,
// and new aliases carry the new version.
func TestVersionedRotationOldAliasResolvable(t *testing.T) {
	km1 := testKM("old-key", 1)
	old, err := GenerateAlias("t-1", SourceTurnThread, ScopeCodexCLI, km1)
	require.NoError(t, err)
	assert.Equal(t, 1, old.Version)

	km2 := KeyMaterial{CurrentKey: "new-key", CurrentVersion: 2, PreviousKey: "old-key", PreviousVersion: 1}
	require.True(t, VerifyAlias(old, "t-1", km2), "old alias must resolve through the previous key")
	assert.False(t, VerifyAlias(old, "t-2", km2), "wrong raw value must not verify")

	next, err := GenerateAlias("t-1", SourceTurnThread, ScopeCodexCLI, km2)
	require.NoError(t, err)
	assert.Equal(t, 2, next.Version)
	assert.Equal(t, hmacHex("new-key", "t-1"), next.Digest)
	assert.NotEqual(t, old.Digest, next.Digest)

	// Two generations later the old alias is out of scope, the middle one stays.
	km3 := KeyMaterial{CurrentKey: "newest-key", CurrentVersion: 3, PreviousKey: "new-key", PreviousVersion: 2}
	assert.False(t, VerifyAlias(old, "t-1", km3))
	assert.True(t, VerifyAlias(next, "t-1", km3))
}

// TestVerifyRejectsUnknownVersionAndEmptyKeys covers the deterministic reject
// cases of VerifyAlias: unknown versions, empty key slots, malformed digests.
func TestVerifyRejectsUnknownVersionAndEmptyKeys(t *testing.T) {
	km := KeyMaterial{CurrentKey: "k", CurrentVersion: 2, PreviousKey: "p", PreviousVersion: 1}
	assert.False(t, VerifyAlias(Alias{Version: 9, Digest: hmacHex("k", "v")}, "v", km))

	emptyKM := KeyMaterial{}
	assert.False(t, VerifyAlias(Alias{Version: 0, Digest: hmacHex("", "v")}, "v", emptyKM))
	assert.False(t, VerifyAlias(Alias{Version: 1, Digest: hmacHex("k", "v")}, "v", emptyKM))

	a := Alias{Version: 1, Digest: "zz-not-hex", Scope: ScopeCodexCLI, Source: SourceTurnThread}
	assert.False(t, VerifyAlias(a, "v", km))
}

// TestResolveNoCrossScopeMerge covers the scoped resolution rule: the same
// raw value under different client profiles yields distinct aliases, because
// the scope is part of the alias identity.
func TestResolveNoCrossScopeMerge(t *testing.T) {
	km := testKM("observer-test-key", 1)

	codexRes, err := ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: codexHeaders(`{"thread_id":"same-id"}`)}, km)
	require.NoError(t, err)
	claudeRes, err := ResolveIdentity(IdentityInput{Scope: ScopeClaudeCLI, Headers: claudeHeaders("same-id")}, km)
	require.NoError(t, err)

	assert.Equal(t, codexRes.Primary.Scope, ScopeCodexCLI)
	assert.Equal(t, claudeRes.Primary.Scope, ScopeClaudeCLI)
	assert.Equal(t, codexRes.Primary.Digest, claudeRes.Primary.Digest, "digests may coincide")
	assert.NotEqual(t, codexRes.Primary, claudeRes.Primary, "scope is part of the alias identity")

	// A request cannot belong to two scopes: Codex markers win over X-App.
	mixed := codexHeaders(`{"thread_id":"t-1"}`)
	mixed.Set("X-App", "claude-cli/1.0")
	mixed.Set("X-Claude-Code-Session-Id", "c-1")
	res, err := ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: mixed}, km)
	require.NoError(t, err)
	assert.Equal(t, ScopeCodexCLI, res.Scope)
	for _, a := range res.Auxiliary {
		assert.NotEqual(t, SourceClaudeHeader, a.Source)
	}
}

// TestSessionScopeForClientProfile locks the value-only bridge between the
// request-path profile detector and observer identity families. Variant
// labels share a scope when they share the same protocol session chain;
// unknown labels remain fail-open and do not gain header parsing here.
func TestSessionScopeForClientProfile(t *testing.T) {
	cases := []struct {
		profile string
		want    SessionScope
	}{
		{profile: "codex_cli", want: ScopeCodexCLI},
		{profile: "codex_app", want: ScopeCodexCLI},
		{profile: "codex_vscode", want: ScopeCodexCLI},
		{profile: "codex_browser", want: ScopeCodexCLI},
		{profile: "codex_desktop", want: ScopeCodexDesktop},
		{profile: "claude_cli", want: ScopeClaudeCLI},
		{profile: "claude_vscode", want: ScopeClaudeCLI},
		{profile: "claude_desktop_3p", want: ScopeClaudeCLI},
		{profile: "claude_plugin", want: ScopeUnknown},
		{profile: "chat", want: ScopeUnknown},
		{profile: "", want: ScopeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			assert.Equal(t, tc.want, SessionScopeForClientProfile(tc.profile))
		})
	}
	assert.Equal(t, ScopeCodexCLI, SessionScopeForClientProfile(" CODEX_VSCODE "))
}

// TestResolveDesktopScopeHasChain covers the codex_desktop scope resolving
// the same primary chain as codex_cli, with the desktop scope retained.
func TestResolveDesktopScopeHasChain(t *testing.T) {
	km := testKM("observer-test-key", 1)
	h := http.Header{}
	h.Set("X-Codex-Turn-Metadata", `{"thread_id":"dt-1"}`)
	h.Set("User-Agent", "codex-desktop/0.2")
	res, err := ResolveIdentity(IdentityInput{Scope: ScopeCodexDesktop, Headers: h}, km)
	require.NoError(t, err)
	assert.Equal(t, ScopeCodexDesktop, res.Scope)
	assert.Equal(t, SourceTurnThread, res.Primary.Source)
}

// TestRawIDsNeverInOutput covers the central privacy contract: no raw session
// identifier appears in any output string of the resolution.
func TestRawIDsNeverInOutput(t *testing.T) {
	km := testKM("observer-test-key", 1)
	in := IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: codexHeaders(`{"thread_id":"t-1","session_id":"s-1"}`),
		Body:    []byte(`{"prompt_cache_key":"ck-1"}`),
	}
	res, err := ResolveIdentity(in, km)
	require.NoError(t, err)

	out := fmt.Sprintf("%+v", res)
	aliases := append([]Alias{res.Primary}, res.Auxiliary...)
	for _, raw := range []string{"t-1", "s-1", "ck-1"} {
		assert.False(t, strings.Contains(out, raw), "raw %q leaked into output", raw)
		for _, a := range aliases {
			assert.NotContains(t, a.Digest, raw, "raw %q leaked into a digest", raw)
		}
	}
	// Every digest is exactly the full 64-hex HMAC, not a fingerprint.
	for _, a := range aliases {
		assert.Len(t, a.Digest, 64)
	}
}

// TestResolveMalformedOrMissingDeterministic covers the deterministic
// behavior for malformed or missing sources: the bad source is skipped, the
// rest still resolves, and nothing panics.
func TestResolveMalformedOrMissingDeterministic(t *testing.T) {
	km := testKM("observer-test-key", 1)

	// Bad JSON in Turn-Metadata: skipped, header thread still resolves.
	res, err := ResolveIdentity(IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: codexHeaders(`{"thread_id":`),
	}, km)
	require.NoError(t, err)
	assert.Empty(t, res.Primary)
	assert.Empty(t, res.Auxiliary)

	res, err = ResolveIdentity(IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: func() http.Header {
			h := codexHeaders(`{"thread_id":`)
			h.Set("Thread_id", "ht-1")
			return h
		}(),
	}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceHeaderThread, res.Primary.Source)

	// Non-string thread_id: skipped.
	res, err = ResolveIdentity(IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: codexHeaders(`{"thread_id":{"x":1}}`),
	}, km)
	require.NoError(t, err)
	assert.Empty(t, res.Primary)

	// No identity material at all: empty result, no error.
	res, err = ResolveIdentity(IdentityInput{}, km)
	require.NoError(t, err)
	assert.Equal(t, ScopeUnknown, res.Scope)
	assert.Empty(t, res.Primary)
	assert.Empty(t, res.Auxiliary)

	// Empty metadata header value: skipped.
	res, err = ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: codexHeaders("")}, km)
	require.NoError(t, err)
	assert.Empty(t, res.Primary)

	// Missing HMAC key: an error, and the raw identifiers stay out of it.
	_, err = ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: codexHeaders(`{"thread_id":"t-1"}`)}, KeyMaterial{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "t-1")
}

// TestResolveOversizedSourcesSkipped covers the byte budgets: an oversized
// Turn-Metadata header yields no turn aliases while other sources still
// resolve; an oversized candidate value is skipped.
func TestResolveOversizedSourcesSkipped(t *testing.T) {
	km := testKM("observer-test-key", 1)

	big := `{"thread_id":"t-1"}` + strings.Repeat("x", MaxTurnMetadataHeaderBytes)
	res, err := ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: codexHeaders(big)}, km)
	require.NoError(t, err)
	assert.Empty(t, res.Primary, "oversized Turn-Metadata must be skipped whole")

	// Same oversized header, but the standalone header thread resolves.
	h := codexHeaders(big)
	h.Set("Thread_id", "ht-1")
	res, err = ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: h}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceHeaderThread, res.Primary.Source)

	// Oversized extracted value: skipped.
	h = codexHeaders("")
	h.Set("Thread_id", strings.Repeat("y", MaxAliasValueBytes+1))
	res, err = ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: h}, km)
	require.NoError(t, err)
	assert.Empty(t, res.Primary)
}

// TestResolveInstallationIDNotSessionKey covers the lifecycle-only rule:
// installation-level identifiers never enter the session primary key.
func TestResolveInstallationIDNotSessionKey(t *testing.T) {
	km := testKM("observer-test-key", 1)
	res, err := ResolveIdentity(IdentityInput{
		Scope:   ScopeCodexCLI,
		Headers: codexHeaders(`{"installation_id":"inst-1","window_id":"w-1"}`),
	}, km)
	require.NoError(t, err)
	assert.Empty(t, res.Primary)
	assert.Empty(t, res.Auxiliary)
}

// TestResolveConflictsBounded covers the conflict rule: several disagreeing
// sources yield the priority-ordered primary plus at most one auxiliary per
// source — a bounded set, never an unbounded merge.
func TestResolveConflictsBounded(t *testing.T) {
	km := testKM("observer-test-key", 1)
	h := codexHeaders(`{"thread_id":"t-1","session_id":"s-1"}`)
	h.Set("Thread_id", "ht-1")
	h.Set("Session_id", "hs-1")
	res, err := ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: h, Body: []byte(`{"prompt_cache_key":"ck-1"}`)}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceTurnThread, res.Primary.Source)
	require.Len(t, res.Auxiliary, 4)
	order := []AliasSource{SourceTurnSession, SourceHeaderThread, SourceHeaderSession, SourceCacheKey}
	for i, a := range res.Auxiliary {
		assert.Equal(t, order[i], a.Source, "auxiliary order must follow the priority chain")
	}

	// Duplicate raw value across sources dedupes to a single alias.
	h = codexHeaders(`{"thread_id":"dup-1"}`)
	h.Set("Thread_id", "dup-1")
	res, err = ResolveIdentity(IdentityInput{Scope: ScopeCodexCLI, Headers: h}, km)
	require.NoError(t, err)
	assert.Equal(t, SourceTurnThread, res.Primary.Source)
	assert.Empty(t, res.Auxiliary, "duplicate values must not create duplicate aliases")
}

// TestGenerateAliasErrors covers the generator's deterministic failure
// modes: missing key and oversized value.
func TestGenerateAliasErrors(t *testing.T) {
	_, err := GenerateAlias("v", SourceTurnThread, ScopeCodexCLI, KeyMaterial{})
	require.Error(t, err)

	km := testKM("observer-test-key", 1)
	_, err = GenerateAlias(strings.Repeat("z", MaxAliasValueBytes+1), SourceTurnThread, ScopeCodexCLI, km)
	require.Error(t, err)
}

// TestVerifyAliasMatchesCurrentAndPrevious covers VerifyAlias on both key
// slots and constant-time comparison of a full-width digest.
func TestVerifyAliasMatchesCurrentAndPrevious(t *testing.T) {
	km := KeyMaterial{CurrentKey: "k2", CurrentVersion: 2, PreviousKey: "k1", PreviousVersion: 1}
	a2 := Alias{Version: 2, Digest: hmacHex("k2", "v"), Scope: ScopeCodexCLI, Source: SourceTurnThread}
	a1 := Alias{Version: 1, Digest: hmacHex("k1", "v"), Scope: ScopeCodexCLI, Source: SourceTurnThread}
	assert.True(t, VerifyAlias(a2, "v", km))
	assert.True(t, VerifyAlias(a1, "v", km))
	assert.False(t, VerifyAlias(a2, "other", km))
	assert.False(t, VerifyAlias(a1, "other", km))
}
