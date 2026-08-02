package relayobserver

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/klauspost/compress/zstd"
)

// This file implements the canonical content codec (T2.3): the zstd payload
// codec and its fail-closed decode validation. Every stored content object is
// the zstd frame of the item's canonical JSON, and every decode re-verifies
// the frame, the declared logical byte count, and (when the HMAC key is
// available) the item's content-layer digest. A truncated, bit-flipped,
// oversized, or digest-mismatched payload is rejected with a classified
// ContentError — a decode never silently produces wrong content.
//
// Zstd uses a single encoder and decoder instance with frame checksums on and
// one compression concurrency, per the architecture SSOT ("Zstd uses
// concurrency one"). The decoder memory cap implements the SSOT object bound:
// no canonical item may exceed the per-request canonical byte cap, so a
// declared logical length above it is rejected before any proportional
// allocation.

// MaxItemBytes is the SSOT per-normalized-item hard maximum from the Runtime
// Limits table ("normalized item | 256 KiB | 1 MiB"). It is the single source
// of truth for every per-item byte bound: the codec decode cap and the zstd
// decoder memory limit align to it.
const MaxItemBytes = 1 << 20

// maxItemDecodeBytes is the per-content-object decode cap: the SSOT per-item
// hard maximum above. The cap deliberately does not follow the per-event
// canonical cap (RELAY_OBSERVER_MAX_REQUEST_BYTES, up to 16 MiB): a stored
// item can never exceed the item maximum, so admitting a larger declared
// length would let a corrupt payload amplify query-path memory 16x.
const maxItemDecodeBytes = int64(MaxItemBytes)

// decodeSlackBytes covers zstd frame overhead and incompressible expansion on
// top of the declared logical bound for the decoder memory cap.
const decodeSlackBytes = 1 << 20

// ContentErrorCode is the stable, secret-free classification of a content
// persistence or reconstruction failure. Codes cross the observer boundary;
// raw errors never do.
type ContentErrorCode string

// Content failure classifications (T2.3 acceptance: corrupt/truncated bases
// and deltas are rejected with a classified error, never silently decoded).
const (
	// ContentErrCodec marks a payload that cannot decompress (truncated frame,
	// bad magic, bit-flipped frame checksum).
	ContentErrCodec ContentErrorCode = "codec_error"
	// ContentErrCorrupt marks a payload that decompresses but fails a
	// structural check (wrong declared length, unparseable canonical JSON,
	// declared length above the decode cap).
	ContentErrCorrupt ContentErrorCode = "corrupt"
	// ContentErrTruncated marks a payload shorter than its declared logical
	// length.
	ContentErrTruncated ContentErrorCode = "truncated"
	// ContentErrDigestMismatch marks a payload whose re-computed content-layer
	// digest does not match the stored digest.
	ContentErrDigestMismatch ContentErrorCode = "digest_mismatch"
	// ContentErrMissingBase marks a delta whose full checkpoint row is absent.
	ContentErrMissingBase ContentErrorCode = "missing_base"
	// ContentErrChainBase marks a delta whose checkpoint row is itself a delta:
	// one-hop reconstruction never follows a chain.
	ContentErrChainBase ContentErrorCode = "chain_base"
	// ContentErrMissingContent marks a digest referenced by a context with no
	// stored content object.
	ContentErrMissingContent ContentErrorCode = "missing_content"
	// ContentErrCorruptDelta marks a delta row that cannot derive from its
	// base (prefix overflow or item-count mismatch).
	ContentErrCorruptDelta ContentErrorCode = "corrupt_delta"
	// ContentErrMissingContext marks a reconstruction target with no context
	// row (for example a turn deleted with its group).
	ContentErrMissingContext ContentErrorCode = "missing_context"
)

// ContentError is a classified content persistence or reconstruction failure.
// Code is stable and secret-free; Msg and Err carry context for logs.
type ContentError struct {
	Code ContentErrorCode
	Msg  string
	Err  error
}

func (e *ContentError) Error() string {
	switch {
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("relayobserver: content %s: %s: %v", e.Code, e.Msg, e.Err)
	case e.Msg != "":
		return fmt.Sprintf("relayobserver: content %s: %s", e.Code, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("relayobserver: content %s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("relayobserver: content %s", e.Code)
}

// Unwrap exposes the wrapped cause for errors.Is/As chains.
func (e *ContentError) Unwrap() error { return e.Err }

// classifiedError builds a ContentError without a cause.
func classifiedError(code ContentErrorCode, format string, args ...any) error {
	return &ContentError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// classifiedErrorWrap builds a ContentError around a cause.
func classifiedErrorWrap(code ContentErrorCode, msg string, err error) error {
	return &ContentError{Code: code, Msg: msg, Err: err}
}

// ContentErrorOf extracts the classification code of err; it reports false
// for every non-ContentError value.
func ContentErrorOf(err error) (ContentErrorCode, bool) {
	var ce *ContentError
	if errors.As(err, &ce) {
		return ce.Code, true
	}
	return "", false
}

// zstd instances. They are created lazily so a codec path that never runs
// allocates nothing; construction only fails on invalid options, which are
// fixed constants.
var (
	zstdEncoderOnce sync.Once
	zstdEncoderInst *zstd.Encoder
	zstdEncoderErr  error
	zstdDecoderOnce sync.Once
	zstdDecoderInst *zstd.Decoder
	zstdDecoderErr  error
)

// zstdCompress compresses src into a new zstd frame with a frame checksum.
func zstdCompress(src []byte) ([]byte, error) {
	zstdEncoderOnce.Do(func() {
		zstdEncoderInst, zstdEncoderErr = zstd.NewWriter(nil,
			zstd.WithEncoderCRC(true),
			zstd.WithEncoderConcurrency(1),
		)
	})
	if zstdEncoderErr != nil {
		return nil, fmt.Errorf("relayobserver: init zstd encoder: %w", zstdEncoderErr)
	}
	return zstdEncoderInst.EncodeAll(src, nil), nil
}

// zstdDecompress decompresses src, refusing frames whose decoded size would
// exceed the per-object cap plus slack.
func zstdDecompress(src []byte) ([]byte, error) {
	zstdDecoderOnce.Do(func() {
		zstdDecoderInst, zstdDecoderErr = zstd.NewReader(nil,
			zstd.WithDecoderMaxMemory(uint64(maxItemDecodeBytes+decodeSlackBytes)),
		)
	})
	if zstdDecoderErr != nil {
		return nil, fmt.Errorf("relayobserver: init zstd decoder: %w", zstdDecoderErr)
	}
	return zstdDecoderInst.DecodeAll(src, nil)
}

// encodeItem renders the canonical item JSON and compresses it into the
// stored payload. It returns the payload and its logical byte count (the
// uncompressed JSON length, the value the content row's logical_bytes
// column and every decode validation use). The payload is a stable function
// of the item (write/reconstruct symmetry): identical items produce
// identical payloads.
func encodeItem(item CanonicalItem) (payload []byte, logical int64, err error) {
	raw, err := common.Marshal(item)
	if err != nil {
		return nil, 0, fmt.Errorf("relayobserver: encode content item: %w", err)
	}
	compressed, err := zstdCompress(raw)
	if err != nil {
		return nil, 0, fmt.Errorf("relayobserver: compress content item: %w", err)
	}
	return compressed, int64(len(raw)), nil
}

// decodeItem decompresses and validates one stored content payload. The
// declared logical byte count and the stored digest come from the content
// row; the HMAC key re-verifies the item's content-layer digest. Gap markers
// skip the re-digest step: v1 rows may hold markers keyed by a dropped item's
// digest (the pre-self-checksum scheme), so re-verifying them would fail
// closed on legitimate old data; the structural checks (frame, length, JSON)
// still apply. Without the key, the structural checks also apply. Every
// failure is classified.
func decodeItem(payload []byte, wantDigest string, wantLogical int64, key string) (CanonicalItem, error) {
	if wantLogical > maxItemDecodeBytes {
		return CanonicalItem{}, classifiedError(ContentErrCorrupt, "declared logical bytes %d exceed decode cap %d", wantLogical, maxItemDecodeBytes)
	}
	if len(payload) == 0 {
		return CanonicalItem{}, classifiedError(ContentErrCodec, "empty stored payload")
	}
	raw, err := zstdDecompress(payload)
	if err != nil {
		return CanonicalItem{}, classifiedErrorWrap(ContentErrCodec, "decompress payload", err)
	}
	if int64(len(raw)) != wantLogical {
		if int64(len(raw)) < wantLogical {
			return CanonicalItem{}, classifiedError(ContentErrTruncated, "decoded %d bytes, declared %d", len(raw), wantLogical)
		}
		return CanonicalItem{}, classifiedError(ContentErrCorrupt, "decoded %d bytes, declared %d", len(raw), wantLogical)
	}
	var item CanonicalItem
	if err := common.Unmarshal(raw, &item); err != nil {
		return CanonicalItem{}, classifiedErrorWrap(ContentErrCorrupt, "decode canonical item JSON", err)
	}
	if item.Kind != CanonicalKindGap && key != "" && wantDigest != "" {
		// Re-compute the digest over the already-parsed item with the digest
		// field cleared. The old path re-unmarshaled and re-marshaled the raw
		// bytes just to do this; the item is already in hand, so clearing the
		// field and marshaling once is byte-identical (Marshal of the parsed
		// item is deterministic) and skips a full second JSON parse (P0-1).
		recomputed := item
		recomputed.Hmac = ""
		payload, err := common.Marshal(recomputed)
		if err != nil {
			return CanonicalItem{}, classifiedErrorWrap(ContentErrCorrupt, "re-compute content-layer digest", err)
		}
		computed := hmacDigest(payload, key)
		if computed != wantDigest {
			return CanonicalItem{}, classifiedError(ContentErrDigestMismatch, "stored digest %s does not match decoded item", wantDigest)
		}
	}
	return item, nil
}

// itemDigestBytes decodes a 64-hex content digest into the 32-byte column
// value. Malformed digests are rejected: a digest column must be a full
// HMAC-SHA-256 value, never a partial or non-hex string.
func itemDigestBytes(digest string) ([]byte, error) {
	if len(digest) != hex.EncodedLen(sha256.Size) {
		return nil, fmt.Errorf("relayobserver: content digest must be %d hex chars, got %d", hex.EncodedLen(sha256.Size), len(digest))
	}
	raw, err := hex.DecodeString(digest)
	if err != nil {
		return nil, fmt.Errorf("relayobserver: content digest is not hex: %w", err)
	}
	return raw, nil
}
