package openai

import (
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSendStreamDataResponseModelRewriting covers the channel-level
// response_model origin mode: stream chunks carrying a "model" field are
// rewritten to the downstream request name; everything else passes through.
func TestSendStreamDataResponseModelRewriting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		setting    dto.ChannelSettings
		chunk      string
		wantModel  string
		absent     string
	}{
		{
			name:      "origin rewrites chunk model",
			setting:   dto.ChannelSettings{ResponseModel: dto.ResponseModelOrigin},
			chunk:     `{"id":"chatcmpl-1","model":"upstream-model","choices":[]}`,
			wantModel: `"model":"origin-model"`,
			absent:    "upstream-model",
		},
		{
			name:      "default keeps upstream model",
			setting:   dto.ChannelSettings{},
			chunk:     `{"id":"chatcmpl-1","model":"upstream-model","choices":[]}`,
			wantModel: `"model":"upstream-model"`,
		},
		{
			name:      "chunk without model passes through",
			setting:   dto.ChannelSettings{ResponseModel: dto.ResponseModelOrigin},
			chunk:     `{"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`,
			wantModel: `"content":"hi"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

			info := &relaycommon.RelayInfo{
				OriginModelName: "origin-model",
				ChannelMeta: &relaycommon.ChannelMeta{
					UpstreamModelName: "upstream-model",
					ChannelSetting:    tt.setting,
				},
			}

			err := sendStreamData(ctx, info, tt.chunk, false, false)
			require.NoError(t, err)

			assert.Contains(t, w.Body.String(), tt.wantModel)
			if tt.absent != "" {
				assert.NotContains(t, w.Body.String(), tt.absent)
			}
		})
	}
}
