package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	// 响应模型名回显策略：origin 模式下把顶层 model 字段改写为下游请求名。
	// 默认模式不触碰 body（严格零行为变化）。
	if info.ResponseModelOriginEnabled() &&
		info.OriginModelName != responsesResponse.Model {
		var bodyMap map[string]interface{}
		err = common.Unmarshal(responseBody, &bodyMap)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		responsesResponse.Model = info.OriginModelName
		bodyMap["model"] = info.OriginModelName
		responseBody, _ = common.Marshal(bodyMap)
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := relayconvert.NormalizeResponsesUsage(responsesResponse.Usage)
	// Count actual tool invocations from Output (not tool declarations).
	for _, output := range responsesResponse.Output {
		switch output.Type {
		case dto.BuildInCallWebSearchCall:
			info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
		case dto.BuildInCallFileSearchCall:
			info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
		case dto.BuildInCallFunctionCall:
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
		}
	}

	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	if !relaycommon.IsNonBillableResponsesStatus(responsesResponse.Status) {
		for i := range responsesResponse.Output {
			idx := i
			imageCounter.Observe(&responsesResponse.Output[i], &idx)
		}
	}
	imageCounter.Commit(info)

	return usage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var usage = &dto.Usage{}
	var responseTextBuilder strings.Builder
	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	imageCommitted := false
	// streamErr captures an in-stream error event (an "error", "response.error"
	// or "response.failed" frame inside an HTTP 200 SSE stream). Once set, later
	// chunks are dropped and the stream stops, mirroring chat_via_responses.go.
	var streamErr *types.NewAPIError
	// terminalSeen records whether the upstream emitted a terminal event before
	// the stream closed. A well-formed Responses stream always ends with one.
	terminalSeen := false
	// outputSeen records whether the upstream produced any output (text deltas or
	// a completed output item). Together with terminalSeen it separates a stream
	// that died before producing anything from one that was merely truncated
	// after delivering content.
	outputSeen := false

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			sr.Error(err)
			return
		}
		// In-stream error frames: some upstreams (notably Bailian/DashScope) answer
		// a streaming request with HTTP 200 and end the SSE stream with an error
		// event instead of a non-2xx status. Return it as an error so the relay loop
		// runs the normal channel-error path (error log, cross-channel retry,
		// auto-ban, affinity rebind) instead of recording a quota=0 success consume
		// log. The frame is not forwarded to the client; mirrors
		// chat_via_responses.go.
		if streamErr = service.NewResponsesStreamEventError(&streamResponse, data); streamErr != nil {
			// A failed response is not billable: discard pending image generation
			// counts exactly like the terminal branch below does.
			if !imageCommitted {
				imageCounter.Reset()
				imageCounter.Commit(info)
				imageCommitted = true
			}
			sr.Stop(streamErr)
			return
		}
		// 响应模型名回显策略：origin 模式下改写 response.created/completed
		// 等事件携带的 response.model；默认模式原样透传。
		if info.ResponseModelOriginEnabled() &&
			streamResponse.Response != nil &&
			streamResponse.Response.Model != "" &&
			streamResponse.Response.Model != info.OriginModelName {
			streamResponse.Response.Model = info.OriginModelName
			if rewritten, err := common.Marshal(streamResponse); err == nil {
				data = string(rewritten)
			}
		}
		sendResponsesStreamData(c, streamResponse, data)
		switch streamResponse.Type {
		case "response.completed", "response.done":
			terminalSeen = true
			if streamResponse.Response != nil {
				if streamResponse.Response.Usage != nil {
					incomingUsage := relayconvert.NormalizeResponsesUsage(streamResponse.Response.Usage)
					usage = dto.MergeUsageNonZero(usage, incomingUsage)
				}
				if !imageCommitted {
					if relaycommon.IsNonBillableResponsesStatus(streamResponse.Response.Status) {
						imageCounter.Reset()
						imageCounter.Commit(info)
						imageCommitted = true
					} else {
						for i := range streamResponse.Response.Output {
							idx := i
							imageCounter.Observe(&streamResponse.Response.Output[i], &idx)
						}
						imageCounter.Commit(info)
						imageCommitted = true
					}
				}
			} else if !imageCommitted {
				imageCounter.Commit(info)
				imageCommitted = true
			}
		case "response.incomplete", "response.cancelled", "response.canceled":
			terminalSeen = true
			if !imageCommitted {
				imageCounter.Reset()
				imageCounter.Commit(info)
				imageCommitted = true
			}
		case "response.output_text.delta":
			// 处理输出文本
			outputSeen = true
			responseTextBuilder.WriteString(streamResponse.Delta)
		case dto.ResponsesOutputTypeItemDone:
			outputSeen = true
			if streamResponse.Item != nil {
				switch streamResponse.Item.Type {
				case dto.BuildInCallWebSearchCall:
					info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
				case dto.BuildInCallFileSearchCall:
					info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
				case dto.BuildInCallFunctionCall:
					info.CountBillableToolCall(dto.BuildInCallFunctionCall, streamResponse.Item.Name)
				case dto.ResponsesOutputTypeImageGenerationCall:
					if !imageCommitted {
						imageCounter.Observe(streamResponse.Item, streamResponse.OutputIndex)
					}
				}
			}
		}
	})

	if streamErr != nil {
		return nil, streamErr
	}

	// The upstream closed an HTTP 200 stream without a terminal event (e.g.
	// "stream closed before response.completed") and without producing any output
	// or usage. Report it as an upstream failure so it never becomes a quota=0
	// success consume log. Anything the client may already have received (output
	// items, text deltas, usage) keeps the legacy estimate-and-bill path instead,
	// because turning a delivered response into an error would retry work the
	// client already got. A client disconnect is not an upstream failure, and an
	// explicit [DONE] is treated as a complete stream. The guard never keys on
	// end_reason alone: a healthy upstream also ends with eof.
	if !terminalSeen && !outputSeen && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		reason := relaycommon.StreamEndReasonEOF
		if info.StreamStatus != nil && info.StreamStatus.EndReason != "" {
			reason = info.StreamStatus.EndReason
		}
		if reason != relaycommon.StreamEndReasonClientGone && reason != relaycommon.StreamEndReasonDone {
			return nil, types.NewError(fmt.Errorf("responses stream ended without a terminal event (reason=%s)", reason), types.ErrorCodeBadResponse)
		}
	}

	if usage.CompletionTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			usage.CompletionTokens = completionTokens
		}
	}

	if usage.PromptTokens == 0 && usage.CompletionTokens != 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if usage.BillingUsage != nil {
		usage.BillingUsage = dto.CloneBillingUsageWithEstimatedCompletion(usage.BillingUsage, usage.CompletionTokens)
	}

	return usage, nil
}
