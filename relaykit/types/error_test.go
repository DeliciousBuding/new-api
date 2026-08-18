package types

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamErrorDetailStringFormatsDiagnostics(t *testing.T) {
	testCases := []struct {
		name     string
		detail   *UpstreamErrorDetail
		expected string
	}{
		{
			name:     "nil detail renders empty",
			detail:   nil,
			expected: "",
		},
		{
			name: "code and type deduplicated when equal",
			detail: &UpstreamErrorDetail{
				Code:      "Arrearage",
				Type:      "Arrearage",
				RequestID: "req-1",
				Message:   "Access denied",
			},
			expected: "code=Arrearage, request_id=req-1, message=Access denied",
		},
		{
			name: "distinct type is kept",
			detail: &UpstreamErrorDetail{
				Message: "Overloaded",
				Type:    "overloaded_error",
			},
			expected: "type=overloaded_error, message=Overloaded",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.detail.String())
		})
	}
}

func TestNewAPIErrorUpstreamDetailRoundTrip(t *testing.T) {
	newAPIError := NewError(errors.New("boom"), ErrorCodeBadResponseStatusCode)
	require.Nil(t, newAPIError.GetUpstreamErrorDetail())

	detail := &UpstreamErrorDetail{Code: "Arrearage", Message: "Access denied"}
	newAPIError.SetUpstreamErrorDetail(detail)
	newAPIError.SetUpstreamErrorDetail(nil)

	got := newAPIError.GetUpstreamErrorDetail()
	require.NotNil(t, got)
	assert.Equal(t, "Arrearage", got.Code)
	assert.Equal(t, "Access denied", got.Message)
	// Attaching diagnostics must never change the client-visible message.
	assert.Equal(t, "boom", newAPIError.Error())
}
