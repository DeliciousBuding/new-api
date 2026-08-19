package types

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
