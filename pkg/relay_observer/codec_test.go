package relayobserver

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the canonical content codec (T2.3): the zstd payload codec
// and its fail-closed decode validation. Every decode path rejects truncated,
// flipped, oversized, length-mismatched, or digest-mismatched payloads with a
// classified ContentError; a decode never silently produces wrong content.
// The digest rule mirrors the normalizer contract: an item's Hmac covers its
// content layer (all fields except the digest itself), and a gap marker's
// Hmac is the digest of the dropped tail start, not a self-checksum, so gap
// markers skip the re-digest step.

// sampleItem returns a canonical message item with a valid content-layer
// digest for the test key.
func sampleItem(t *testing.T) CanonicalItem {
	t.Helper()
	item := CanonicalItem{
		Kind:    CanonicalKindMessage,
		Role:    "user",
		Content: []CanonicalPart{{Type: partTypeText, Text: "hello observer"}},
	}
	item.Hmac = digestOfItem(t, item)
	return item
}

// digestOfItem computes the content-layer digest of item exactly like
// withHmac does: the marshaled item with the digest field cleared.
func digestOfItem(t *testing.T, item CanonicalItem) string {
	t.Helper()
	clear := item
	clear.Hmac = ""
	payload, err := common.Marshal(clear)
	require.NoError(t, err)
	return hmacDigest(payload, testHMACKey)
}

// contentItemWith builds a canonical message item with a valid content-layer
// digest for the shared test key. It lives in a non-tagged test file so the
// fake orchestration tests and the PG integration tests share it.
func contentItemWith(t *testing.T, text string) CanonicalItem {
	t.Helper()
	it := CanonicalItem{
		Kind:    CanonicalKindMessage,
		Role:    "user",
		Content: []CanonicalPart{{Type: partTypeText, Text: text}},
	}
	it.Hmac = digestOfItem(t, it)
	return it
}

// itemJSONBytes returns the marshaled item length, the logical byte count the
// codec must validate against.
func itemJSONBytes(t *testing.T, item CanonicalItem) int64 {
	t.Helper()
	payload, err := common.Marshal(item)
	require.NoError(t, err)
	return int64(len(payload))
}

// testHMACKey is shared with the normalizer tests (normalizer_test.go).

// TestEncodeDecodeRoundtrip locks the codec contract: a payload encoded by
// encodeItem decodes back to the exact item, compressible text shrinks
// (zstd frames carry header overhead, so only larger inputs are guaranteed
// smaller), and the digest validates.
func TestEncodeDecodeRoundtrip(t *testing.T) {
	t.Run("small item", func(t *testing.T) {
		item := sampleItem(t)
		logical := itemJSONBytes(t, item)

		payload, gotLogical, err := encodeItem(item)
		require.NoError(t, err)
		assert.Equal(t, logical, gotLogical, "logical byte count is the canonical JSON length")

		got, err := decodeItem(payload, item.Hmac, logical, testHMACKey)
		require.NoError(t, err)
		assert.Equal(t, item, got, "decode must return the exact stored item")
	})
	t.Run("compressible text shrinks", func(t *testing.T) {
		item := CanonicalItem{
			Kind:    CanonicalKindMessage,
			Role:    "user",
			Content: []CanonicalPart{{Type: partTypeText, Text: strings.Repeat("the same phrase repeats ", 200)}},
		}
		item.Hmac = digestOfItem(t, item)
		logical := itemJSONBytes(t, item)

		payload, _, err := encodeItem(item)
		require.NoError(t, err)
		assert.Less(t, len(payload), int(logical), "zstd payload must compress compressible text")

		got, err := decodeItem(payload, item.Hmac, logical, testHMACKey)
		require.NoError(t, err)
		assert.Equal(t, item, got)
	})
}

// TestEncodeIsDeterministic locks the write/reconstruct symmetry: the same
// item always encodes to the same payload, so a stored object is a stable
// function of the item.
func TestEncodeIsDeterministic(t *testing.T) {
	item := sampleItem(t)
	a, _, err := encodeItem(item)
	require.NoError(t, err)
	b, _, err := encodeItem(item)
	require.NoError(t, err)
	assert.Equal(t, a, b, "encoding must be deterministic")
}

// TestDecodeRejectsTruncatedPayload proves a truncated payload is rejected
// with a classified error: the frame cannot decompress.
func TestDecodeRejectsTruncatedPayload(t *testing.T) {
	item := sampleItem(t)
	logical := itemJSONBytes(t, item)
	payload, _, err := encodeItem(item)
	require.NoError(t, err)

	_, err = decodeItem(payload[:len(payload)-5], item.Hmac, logical, testHMACKey)
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok, "truncated payload must return a classified ContentError")
	assert.NotEqual(t, "", code, "classification must be non-empty")
}

// TestDecodeRejectsBitFlip proves a single flipped payload byte is rejected:
// the zstd frame checksum fails, so corrupted content can never decode
// silently into wrong content.
func TestDecodeRejectsBitFlip(t *testing.T) {
	item := sampleItem(t)
	logical := itemJSONBytes(t, item)
	payload, _, err := encodeItem(item)
	require.NoError(t, err)

	flipped := append([]byte(nil), payload...)
	flipped[len(flipped)/2] ^= 0xff
	_, err = decodeItem(flipped, item.Hmac, logical, testHMACKey)
	require.Error(t, err)
	_, ok := ContentErrorOf(err)
	require.True(t, ok, "bit-flipped payload must return a classified ContentError")
}

// TestDecodeRejectsLengthMismatch proves the declared logical byte count is
// enforced: a short declared length is rejected as truncated, a longer one as
// corrupt. The declared count comes from the stored row, so a tampered row
// cannot change the decode outcome.
func TestDecodeRejectsLengthMismatch(t *testing.T) {
	item := sampleItem(t)
	logical := itemJSONBytes(t, item)
	payload, _, err := encodeItem(item)
	require.NoError(t, err)

	t.Run("declared too long", func(t *testing.T) {
		_, err := decodeItem(payload, item.Hmac, logical+10, testHMACKey)
		require.Error(t, err)
		code, ok := ContentErrorOf(err)
		require.True(t, ok)
		assert.Equal(t, ContentErrTruncated, code, "decoded bytes below the declared count classify as truncated")
	})
	t.Run("declared too short", func(t *testing.T) {
		_, err := decodeItem(payload, item.Hmac, logical-10, testHMACKey)
		require.Error(t, err)
		code, ok := ContentErrorOf(err)
		require.True(t, ok)
		assert.Equal(t, ContentErrCorrupt, code, "decoded bytes above the declared count classify as corrupt")
	})
}

// TestDecodeRejectsDigestMismatch proves the content-layer digest is
// re-verified on decode: a stored digest that does not match the decoded item
// is rejected, so a tampered digest column cannot launder wrong content.
func TestDecodeRejectsDigestMismatch(t *testing.T) {
	item := sampleItem(t)
	logical := itemJSONBytes(t, item)
	payload, _, err := encodeItem(item)
	require.NoError(t, err)

	other := sampleItem(t)
	other.Content = []CanonicalPart{{Type: partTypeText, Text: "tampered"}}
	other.Hmac = digestOfItem(t, other)

	_, err = decodeItem(payload, other.Hmac, logical, testHMACKey)
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrDigestMismatch, code)
}

// TestDecodeSkipsDigestForGapMarker locks the gap-marker rule: decode
// validates a gap item's shape but does not re-digest it, because v1 rows
// may hold markers keyed by a dropped item's digest (the pre-self-checksum
// scheme) and re-verifying those would fail closed on legitimate old data.
// The marker itself still roundtrips.
func TestDecodeSkipsDigestForGapMarker(t *testing.T) {
	gap := CanonicalItem{
		Kind:         CanonicalKindGap,
		LogicalBytes: 42,
		Hmac:         "a1b2c3", // short non-self value: still accepted, shape-only check
		Truncated:    true,
	}
	logical := itemJSONBytes(t, gap)
	payload, _, err := encodeItem(gap)
	require.NoError(t, err)

	got, err := decodeItem(payload, gap.Hmac, logical, testHMACKey)
	require.NoError(t, err)
	assert.Equal(t, gap, got)
}

// TestDecodeWithoutKeySkipsDigest locks the keyless decode path: without the
// HMAC key the digest cannot be re-verified, so decode falls back to the
// structural checks (frame integrity and logical length) instead of failing
// closed on an unavailable secret.
func TestDecodeWithoutKeySkipsDigest(t *testing.T) {
	item := sampleItem(t)
	logical := itemJSONBytes(t, item)
	payload, _, err := encodeItem(item)
	require.NoError(t, err)

	got, err := decodeItem(payload, item.Hmac, logical, "")
	require.NoError(t, err)
	assert.Equal(t, item, got)
}

// TestDecodeRejectsOversizedDeclaredLength proves the decode bomb guard: a
// declared logical byte count above the per-item hard maximum is rejected
// before any allocation proportional to it.
func TestDecodeRejectsOversizedDeclaredLength(t *testing.T) {
	item := sampleItem(t)
	payload, _, err := encodeItem(item)
	require.NoError(t, err)

	_, err = decodeItem(payload, item.Hmac, maxItemDecodeBytes+1, testHMACKey)
	require.Error(t, err)
	_, ok := ContentErrorOf(err)
	require.True(t, ok, "oversized declared length must return a classified ContentError")
}

// TestItemDigestBytes locks the digest representation: the 64-hex HMAC string
// becomes the 32-byte column value, and malformed digests are rejected.
func TestItemDigestBytes(t *testing.T) {
	item := sampleItem(t)
	raw, err := itemDigestBytes(item.Hmac)
	require.NoError(t, err)
	assert.Len(t, raw, 32, "a valid HMAC-SHA-256 hex digest is 32 bytes")

	_, err = itemDigestBytes("not-hex")
	require.Error(t, err)
	_, err = itemDigestBytes("abcd")
	require.Error(t, err, "a short hex string is not a full digest")
}

// TestContentErrorClassification locks the classified error helpers: errors
// carry a stable code, wrap the cause, and ContentErrorOf only matches
// ContentError values.
func TestContentErrorClassification(t *testing.T) {
	base := &ContentError{Code: ContentErrMissingBase, Msg: "no base row"}
	assert.Equal(t, ContentErrMissingBase, base.Code)

	plain := &ContentError{Code: ContentErrCorrupt, Msg: "bad", Err: assert.AnError}
	var ce *ContentError
	require.ErrorAs(t, plain, &ce)
	assert.Equal(t, ContentErrCorrupt, ce.Code)
	assert.ErrorIs(t, plain, assert.AnError)

	code, ok := ContentErrorOf(plain)
	require.True(t, ok)
	assert.Equal(t, ContentErrCorrupt, code)
	_, ok = ContentErrorOf(assert.AnError)
	assert.False(t, ok, "non-ContentError values never classify")
}

// TestDecodeRejectsEmptyPayload locks the empty-payload guard: a stored
// payload of zero bytes is rejected as a codec error.
func TestDecodeRejectsEmptyPayload(t *testing.T) {
	_, err := decodeItem(nil, strings.Repeat("d", 64), 10, testHMACKey)
	require.Error(t, err)
	code, ok := ContentErrorOf(err)
	require.True(t, ok)
	assert.Equal(t, ContentErrCodec, code)
}
