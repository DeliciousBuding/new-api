package relayobserver

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveIdentityEmitsPreviousAliases locks the rotation identity contract: when
// a previous key is configured, ResolveIdentity re-keys every candidate under
// the previous generation in parallel with the current generation, so a
// rotation window can adopt a session bound under the old key.
func TestResolveIdentityEmitsPreviousAliases(t *testing.T) {
	km := KeyMaterial{CurrentKey: "current-key", CurrentVersion: 2, PreviousKey: "previous-key", PreviousVersion: 1}
	in := IdentityInput{
		Headers: map[string][]string{"X-Codex-Turn-Metadata": {`{"thread_id":"thr-rotated"}`}},
	}

	res, err := ResolveIdentity(in, km)
	require.NoError(t, err)

	require.NotEmpty(t, res.Primary.Digest)
	assert.Equal(t, km.CurrentVersion, res.Primary.Version)
	require.NotEmpty(t, res.PreviousPrimary.Digest, "a configured previous key must re-key the candidate")
	assert.Equal(t, km.PreviousVersion, res.PreviousPrimary.Version)
	assert.NotEqual(t, res.Primary.Digest, res.PreviousPrimary.Digest, "distinct keys produce distinct digests")

	// Without a previous key there is no previous alias.
	resNoPrev, err := ResolveIdentity(in, KeyMaterial{CurrentKey: "current-key", CurrentVersion: 2})
	require.NoError(t, err)
	assert.Empty(t, resNoPrev.PreviousPrimary.Digest)
	assert.Empty(t, resNoPrev.PreviousAuxiliary)
}

// TestAppendTurnAdoptsSessionUnderPreviousKey locks the rotation adoption contract:
// a turn whose current-key primary alias misses but whose previous-key alias
// resolves to an existing session is adopted into that session — the current
// alias is re-bound so future turns resolve directly, and no orphan session is
// created.
func TestAppendTurnAdoptsSessionUnderPreviousKey(t *testing.T) {
	const previousKey = "previous-generation-key"
	curAlias, err := generateAliasWith("shared-value", SourceTurnThread, ScopeCodexCLI, "current-generation-key", 2)
	require.NoError(t, err)
	prevAlias, err := generateAliasWith("shared-value", SourceTurnThread, ScopeCodexCLI, previousKey, 1)
	require.NoError(t, err)

	// A session already bound under the previous key (simulating pre-rotation
	// state).
	tx := newFakeContentTx()
	oldSessionID := uuid.New()
	prevRaw, err := itemDigestBytes(prevAlias.Digest)
	require.NoError(t, err)
	tx.aliases[aliasKey("n", 7, prevAlias.Version, prevRaw, string(prevAlias.Scope))] = oldSessionID.String()
	tx.sessions[oldSessionID.String()] = true

	in := ContentInput{
		NodeScope:       "n",
		UserID:          7,
		Aliases:         []Alias{curAlias},
		PreviousAliases: []Alias{prevAlias},
		TurnID:          uuid.New(),
		ContentState:    ContentStateGap,
	}
	require.NoError(t, appendTurnTx(context.Background(), tx, &in))

	// The turn is claimed by the adopted (old) session, not a fresh one.
	assert.Equal(t, oldSessionID.String(), tx.turnSessions[in.TurnID.String()])
	assert.Len(t, tx.sessions, 1, "no orphan session may be created on adoption")
	require.True(t, tx.sessions[oldSessionID.String()])

	// The current alias is re-bound to the adopted session so future turns
	// resolve directly through the current key.
	curRaw, err := itemDigestBytes(curAlias.Digest)
	require.NoError(t, err)
	assert.Equal(t, oldSessionID.String(), tx.aliases[aliasKey("n", 7, curAlias.Version, curRaw, string(curAlias.Scope))])
}

// TestAppendTurnWithoutPreviousAliasStillCreatesSession locks the negative
// negative case: a turn whose current alias misses and has no previous alias still
// creates a fresh session (the pre-rotation behavior is unchanged when no rotation
// is configured).
func TestAppendTurnWithoutPreviousAliasStillCreatesSession(t *testing.T) {
	f := newFakeFixture(t, "no-rotation")
	turn := f.turn("a")
	require.NoError(t, appendTurnTx(context.Background(), f.tx, &turn))

	sid := f.sessionID()
	require.True(t, f.tx.sessions[sid.String()], "a fresh session must be created")
	assert.Len(t, f.tx.sessions, 1)
}
