package service

import (
	"strings"

	"github.com/QuantumNous/new-api/relaykit/types"
)

// UpstreamErrorClass is a coarse category for a structured provider error code.
// It drives retry and channel-health policy; only codes observed in production
// or from well-known provider contracts are classified, so an unknown code
// falls through to the existing status-code / keyword logic unchanged.
type UpstreamErrorClass int

const (
	UpstreamErrorClassUnknown UpstreamErrorClass = iota
	// UpstreamErrorClassAccountFatal marks errors caused by the upstream
	// account's billing or status (arrearage, insufficient balance, disabled).
	// These will never succeed on retry, and the channel should be auto-disabled.
	UpstreamErrorClassAccountFatal
	// UpstreamErrorClassAuthFatal marks invalid or revoked credentials. Retrying
	// the same channel with the same key cannot help, so the request should not
	// retry, but the channel itself is not force-disabled (a sibling key in a
	// multi-key channel may still be valid).
	UpstreamErrorClassAuthFatal
)

// accountFatalCodes are normalized structured provider codes that mean the
// upstream account is out of credit or disabled. Production sources include
// DashScope/Bailian ("Arrearage") and TokenRhythm ("INSUFFICIENT_BALANCE").
var accountFatalCodes = []string{
	"arrearage",
	"arrears",
	"accountarrears",
	"accountoverdue",
	"accountdisabled",
	"accountnotingoodstanding",
	"insufficientbalance",
	"insufficientquota",
	"quotaexceeded",
	"billingnotenabled",
	"paymentrequired",
	"nobalance",
	"outofcredit",
}

// authFatalCodes are normalized structured provider codes that mean the
// credential used for this request is invalid or has no permission.
var authFatalCodes = []string{
	"invalidapikey",
	"invalidkey",
	"invalidtoken",
	"authenticationerror",
	"authenticationfailed",
	"unauthorized",
	"accessdenied",
	"modelaccessdenied",
	"permissiondenied",
	"apikeynotfound",
}

// ClassifyUpstreamErrorCode normalizes a provider code and returns its class.
// Comparison is exact (after lowercasing and stripping non-alphanumerics) to
// avoid false positives from substring matches.
func ClassifyUpstreamErrorCode(code string) UpstreamErrorClass {
	normalized := normalizeUpstreamErrorCode(code)
	if normalized == "" {
		return UpstreamErrorClassUnknown
	}
	for _, fatal := range accountFatalCodes {
		if normalized == fatal {
			return UpstreamErrorClassAccountFatal
		}
	}
	for _, fatal := range authFatalCodes {
		if normalized == fatal {
			return UpstreamErrorClassAuthFatal
		}
	}
	return UpstreamErrorClassUnknown
}

func normalizeUpstreamErrorCode(code string) string {
	var builder strings.Builder
	builder.Grow(len(code))
	for _, r := range strings.ToLower(code) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// classifyUpstreamError returns the class of a structured detail, preferring
// the code over the type (the type often duplicates the code or is empty).
func classifyUpstreamError(err *types.NewAPIError) UpstreamErrorClass {
	if err == nil {
		return UpstreamErrorClassUnknown
	}
	detail := err.GetUpstreamErrorDetail()
	if detail == nil {
		return UpstreamErrorClassUnknown
	}
	if class := ClassifyUpstreamErrorCode(detail.Code); class != UpstreamErrorClassUnknown {
		return class
	}
	return ClassifyUpstreamErrorCode(detail.Type)
}

// IsAccountFatalError reports whether the error carries a structured upstream
// code indicating the account is out of credit or disabled. Used to
// auto-disable the channel regardless of the message-text keyword path.
func IsAccountFatalError(err *types.NewAPIError) bool {
	return classifyUpstreamError(err) == UpstreamErrorClassAccountFatal
}

// IsNonRetryableUpstreamError reports whether the error carries a structured
// upstream code that cannot be resolved by retrying (account-fatal or
// auth-fatal). Retrying the same request wastes latency; this is the
// structured-code complement to the status-code retry matrix.
func IsNonRetryableUpstreamError(err *types.NewAPIError) bool {
	switch classifyUpstreamError(err) {
	case UpstreamErrorClassAccountFatal, UpstreamErrorClassAuthFatal:
		return true
	default:
		return false
	}
}
