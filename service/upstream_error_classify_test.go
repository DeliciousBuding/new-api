package service

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
)

func TestClassifyUpstreamErrorCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		code     string
		expected UpstreamErrorClass
	}{
		{name: "DashScope arrearage", code: "Arrearage", expected: UpstreamErrorClassAccountFatal},
		{name: "TokenRhythm insufficient balance", code: "INSUFFICIENT_BALANCE", expected: UpstreamErrorClassAccountFatal},
		{name: "insufficient quota", code: "insufficient_quota", expected: UpstreamErrorClassAccountFatal},
		{name: "quota exceeded", code: "QuotaExceeded", expected: UpstreamErrorClassAccountFatal},
		{name: "invalid api key", code: "InvalidApiKey", expected: UpstreamErrorClassAuthFatal},
		{name: "authentication error", code: "authentication_error", expected: UpstreamErrorClassAuthFatal},
		{name: "unauthorized", code: "Unauthorized", expected: UpstreamErrorClassAuthFatal},
		{name: "bailian model access denied", code: "Model.AccessDenied", expected: UpstreamErrorClassAuthFatal},
		{name: "parameter error is unknown", code: "InvalidParameter", expected: UpstreamErrorClassUnknown},
		{name: "content inspection is unknown", code: "data_inspection_failed", expected: UpstreamErrorClassUnknown},
		{name: "empty code is unknown", code: "", expected: UpstreamErrorClassUnknown},
		{name: "punctuation and case are normalized", code: " Insufficient-Balance ", expected: UpstreamErrorClassAccountFatal},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, ClassifyUpstreamErrorCode(tc.code))
		})
	}
}

func TestIsAccountFatalError(t *testing.T) {
	t.Parallel()

	accountErr := &types.NewAPIError{}
	accountErr.SetUpstreamErrorDetail(&types.UpstreamErrorDetail{Code: "Arrearage", Message: "denied"})
	assert.True(t, IsAccountFatalError(accountErr))
	assert.True(t, IsNonRetryableUpstreamError(accountErr))

	authErr := &types.NewAPIError{}
	authErr.SetUpstreamErrorDetail(&types.UpstreamErrorDetail{Code: "InvalidApiKey"})
	assert.False(t, IsAccountFatalError(authErr))
	assert.True(t, IsNonRetryableUpstreamError(authErr))

	paramErr := &types.NewAPIError{}
	paramErr.SetUpstreamErrorDetail(&types.UpstreamErrorDetail{Code: "InvalidParameter"})
	assert.False(t, IsAccountFatalError(paramErr))
	assert.False(t, IsNonRetryableUpstreamError(paramErr))

	// No detail, or nil error, never classifies as fatal.
	assert.False(t, IsAccountFatalError(&types.NewAPIError{}))
	assert.False(t, IsAccountFatalError(nil))
	assert.False(t, IsNonRetryableUpstreamError(nil))
}

func TestClassifyUpstreamErrorPrefersCodeOverType(t *testing.T) {
	t.Parallel()

	// code is fatal even when the type is unclassified.
	err := &types.NewAPIError{}
	err.SetUpstreamErrorDetail(&types.UpstreamErrorDetail{Code: "Arrearage", Type: "whatever"})
	assert.True(t, IsAccountFatalError(err))

	// type is used as a fallback when code is empty.
	err = &types.NewAPIError{}
	err.SetUpstreamErrorDetail(&types.UpstreamErrorDetail{Type: "InvalidApiKey"})
	assert.True(t, IsNonRetryableUpstreamError(err))
	assert.False(t, IsAccountFatalError(err))
}
