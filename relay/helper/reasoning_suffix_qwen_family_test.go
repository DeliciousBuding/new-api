package helper

import (
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Production shape behind the qwen3.8-max incident: clients request
// qwen3.8-max and the channel maps it to a dated upstream id. The origin name
// must survive as a real model ID. When it was parsed as base "qwen3.8" plus
// effort "max", an explicit reasoning_effort 400'd with "reasoning settings
// conflict" and the mapped upstream id was reported as the conflicting model.
func TestApplyReasoningModelSuffixKeepsMappedQwenMaxModelId(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{Model: "qwen3.8-max", ReasoningEffort: "high"}
	info := &relaycommon.RelayInfo{
		OriginModelName: "qwen3.8-max",
		Request:         req,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "qwen3.8-max-0902",
			IsModelMapped:     true,
		},
	}

	require.NoError(t, ApplyReasoningModelSuffix(info))
	assert.Equal(t, "qwen3.8-max-0902", info.UpstreamModelName)
	assert.Equal(t, "qwen3.8-max-0902", req.Model)
	assert.Nil(t, info.ReasoningConversion)
}

// Without an explicit effort the same request must not gain one: a suffix-derived
// effort=max would be injected into an upstream call that never asked for it.
func TestApplyReasoningModelSuffixDoesNotInventEffortForQwenMax(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{Model: "qwen-vl-max"}
	info := &relaycommon.RelayInfo{
		OriginModelName: "qwen-vl-max",
		Request:         req,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "qwen-vl-max",
		},
	}

	require.NoError(t, ApplyReasoningModelSuffix(info))
	assert.Equal(t, "qwen-vl-max", info.UpstreamModelName)
	assert.Nil(t, info.ReasoningConversion)
}

// The family rule is narrow on purpose: a real synthetic alias still trims to
// its base model plus the requested effort.
func TestApplyReasoningModelSuffixStillTrimsNonFamilyAlias(t *testing.T) {
	info := &relaycommon.RelayInfo{
		OriginModelName: "grok-4.20-multi-agent-high",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "grok-4.20-multi-agent-high",
		},
	}

	require.NoError(t, ApplyReasoningModelSuffix(info))
	assert.Equal(t, "grok-4.20-multi-agent", info.UpstreamModelName)
	require.NotNil(t, info.ReasoningConversion)
	assert.Equal(t, "high", info.ReasoningConversion.Effort)
}
