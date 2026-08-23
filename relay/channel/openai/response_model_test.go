package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
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

// TestOpenaiHandlerResponseModelRewriting covers the non-stream path:
// origin mode rewrites the top-level model field, the default keeps the
// upstream name, and origin + usage repair compose in one rewrite branch.
func TestOpenaiHandlerResponseModelRewriting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	// usage 全 0 会触发 usage 补全分支，与 model 改写走同一 bodyMap rewrite。
	bodyWithUsageRepair := `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`

	tests := []struct {
		name      string
		setting   dto.ChannelSettings
		body      string
		wantModel string
		absent    string
	}{
		{
			name:      "origin rewrites response model",
			setting:   dto.ChannelSettings{ResponseModel: dto.ResponseModelOrigin},
			body:      body,
			wantModel: `"model":"origin-model"`,
			absent:    "upstream-model",
		},
		{
			name:      "default keeps upstream model",
			setting:   dto.ChannelSettings{},
			body:      body,
			wantModel: `"model":"upstream-model"`,
		},
		{
			name:      "origin composes with usage repair",
			setting:   dto.ChannelSettings{ResponseModel: dto.ResponseModelOrigin},
			body:      bodyWithUsageRepair,
			wantModel: `"model":"origin-model"`,
			absent:    "upstream-model",
		},
		{
			// 默认零行为回归：上游回显名与 UpstreamModelName 不一致时，
			// 默认模式必须原样透传上游回显（不归一化）。
			name:      "default passes through divergent upstream echo",
			setting:   dto.ChannelSettings{},
			body:      `{"id":"chatcmpl-3","object":"chat.completion","created":1,"model":"snapshot-alias","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			wantModel: `"model":"snapshot-alias"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

			info := &relaycommon.RelayInfo{
				OriginModelName: "origin-model",
				RelayFormat:     types.RelayFormatOpenAI,
				ChannelMeta: &relaycommon.ChannelMeta{
					UpstreamModelName: "upstream-model",
					ChannelSetting:    tt.setting,
				},
			}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}

			_, err := OpenaiHandler(ctx, info, resp)
			// OpenaiHandler 返回具体指针类型，nil 时是 typed nil，
			// 须用 require.Nil 直接判指针（require.NoError 会误报）。
			require.Nil(t, err)

			assert.Contains(t, w.Body.String(), tt.wantModel)
			if tt.absent != "" {
				assert.NotContains(t, w.Body.String(), tt.absent)
			}
		})
	}
}
