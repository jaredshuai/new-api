package relay

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	appconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	codexchannel "github.com/QuantumNous/new-api/relay/channel/codex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func isCodexImageChatCompletionRequest(info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) bool {
	if info == nil || request == nil {
		return false
	}
	if info.RelayMode != relayconstant.RelayModeChatCompletions || info.ApiType != appconstant.APITypeCodex {
		return false
	}
	model := strings.TrimSpace(request.Model)
	if model == "" && info.ChannelMeta != nil {
		model = strings.TrimSpace(info.UpstreamModelName)
	}
	return strings.EqualFold(model, codexchannel.CodexImageModel)
}

func chatCompletionsViaCodexImageResponses(c *gin.Context, info *relaycommon.RelayInfo, adaptor channel.Adaptor, request *dto.GeneralOpenAIRequest) (*dto.Usage, *types.NewAPIError) {
	chatRequest, newAPIError := prepareCodexImageChatRequest(info, request)
	if newAPIError != nil {
		return nil, newAPIError
	}
	if chatRequest.N != nil && *chatRequest.N > 1 {
		return nil, types.NewErrorWithStatusCode(fmt.Errorf("n>1 is not supported for codex gpt-image-2 chat compatibility"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if err := validateSimpleCodexImageChatRequest(chatRequest); err != nil {
		return nil, err
	}

	prompt := codexImagePromptFromChatRequest(chatRequest)
	if prompt == "" {
		return nil, types.NewErrorWithStatusCode(fmt.Errorf("user text prompt is required for codex gpt-image-2 chat compatibility"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	responsesReq, err := codexchannel.BuildCodexImageGenerationResponsesRequest(c, info, prompt)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	codexchannel.EnsureResponsesImageGenerationTool(info)
	info.AppendRequestConversion(types.RelayFormatOpenAIResponses)
	relaycommon.AppendRequestConversionFromRequest(info, responsesReq)

	jsonData, err := common.Marshal(responsesReq)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}

	savedRelayMode := info.RelayMode
	savedRequestURLPath := info.RequestURLPath
	savedIsStream := info.IsStream
	downstreamStream := chatRequest.IsStream(c)
	defer func() {
		info.RelayMode = savedRelayMode
		info.RequestURLPath = savedRequestURLPath
		info.IsStream = savedIsStream
	}()

	info.RelayMode = relayconstant.RelayModeResponses
	info.RequestURLPath = "/v1/responses"
	info.IsStream = downstreamStream

	resp, err := adaptor.DoRequest(c, info, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	if resp == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	httpResp := resp.(*http.Response)
	statusCodeMappingStr := c.GetString("status_code_mapping")
	if httpResp.StatusCode != http.StatusOK {
		newAPIError = codexchannel.RelayErrorHandler(c.Request.Context(), httpResp)
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return nil, newAPIError
	}

	model := strings.TrimSpace(info.OriginModelName)
	if model == "" {
		model = codexchannel.CodexImageModel
	}
	if downstreamStream {
		usage, newAPIError := writeCodexImageChatStream(c, info, httpResp, model)
		if newAPIError != nil {
			service.ResetStatusCode(newAPIError, statusCodeMappingStr)
			return nil, newAPIError
		}
		return usage, nil
	}

	usage, newAPIError := writeCodexImageChatResponse(c, info, httpResp, model)
	if newAPIError != nil {
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return nil, newAPIError
	}
	return usage, nil
}

func prepareCodexImageChatRequest(info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, *types.NewAPIError) {
	chatJSON, err := common.Marshal(request)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}

	chatJSON, err = relaycommon.RemoveDisabledFields(chatJSON, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}

	if len(info.ParamOverride) > 0 {
		chatJSON, err = relaycommon.ApplyParamOverrideWithRelayInfo(chatJSON, info)
		if err != nil {
			return nil, newAPIErrorFromParamOverride(err)
		}
	}

	var overridden dto.GeneralOpenAIRequest
	if err := common.Unmarshal(chatJSON, &overridden); err != nil {
		return nil, types.NewError(err, types.ErrorCodeChannelParamOverrideInvalid, types.ErrOptionWithSkipRetry())
	}
	return &overridden, nil
}

func codexImagePromptFromChatRequest(request *dto.GeneralOpenAIRequest) string {
	if request == nil {
		return ""
	}
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(request.Messages[i].Role), "user") {
			continue
		}
		if text := textFromChatMessage(request.Messages[i]); text != "" {
			return text
		}
	}
	return ""
}

func validateSimpleCodexImageChatRequest(request *dto.GeneralOpenAIRequest) *types.NewAPIError {
	if request == nil {
		return types.NewErrorWithStatusCode(fmt.Errorf("request is nil"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if len(request.Tools) > 0 || request.ToolChoice != nil || len(request.Functions) > 0 || len(request.FunctionCall) > 0 {
		return unsupportedCodexImageChatRequest("tools and tool_choice are not supported")
	}

	userMessages := 0
	for _, message := range request.Messages {
		role := strings.TrimSpace(message.Role)
		if role == "" {
			return unsupportedCodexImageChatRequest("message role is required")
		}
		if len(message.ToolCalls) > 0 || strings.TrimSpace(message.ToolCallId) != "" {
			return unsupportedCodexImageChatRequest("tool messages are not supported")
		}
		if !strings.EqualFold(role, "user") {
			return unsupportedCodexImageChatRequest("only a single user text message is supported")
		}
		userMessages++
		if userMessages > 1 {
			return unsupportedCodexImageChatRequest("multi-turn chat history is not supported")
		}
		if !isTextOnlyChatMessage(message) {
			return unsupportedCodexImageChatRequest("only text message content is supported")
		}
	}
	if userMessages == 0 {
		return unsupportedCodexImageChatRequest("user text message is required")
	}
	return nil
}

func unsupportedCodexImageChatRequest(reason string) *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		fmt.Errorf("codex gpt-image-2 chat compatibility only supports a single user text prompt: %s", reason),
		types.ErrorCodeInvalidRequest,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
	)
}

func isTextOnlyChatMessage(message dto.Message) bool {
	if message.Content == nil {
		return false
	}
	if message.IsStringContent() {
		return strings.TrimSpace(message.StringContent()) != ""
	}
	parts := message.ParseContent()
	if len(parts) == 0 {
		return false
	}
	hasText := false
	for _, part := range parts {
		if part.Type != dto.ContentTypeText {
			return false
		}
		if strings.TrimSpace(part.Text) != "" {
			hasText = true
		}
	}
	return hasText
}

func textFromChatMessage(message dto.Message) string {
	if message.Content == nil {
		return ""
	}
	if message.IsStringContent() {
		return strings.TrimSpace(message.StringContent())
	}
	parts := message.ParseContent()
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == dto.ContentTypeText && strings.TrimSpace(part.Text) != "" {
			texts = append(texts, strings.TrimSpace(part.Text))
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

func writeCodexImageChatResponse(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response, model string) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	markdown, createdAt, usage, newAPIError := codexchannel.ImageMarkdownFromResponseBody(c, info, body)
	if newAPIError != nil {
		return nil, newAPIError
	}
	usage = ensureCodexImageChatUsage(c, info, model, markdown, usage)

	chatResponse := dto.OpenAITextResponse{
		Id:      helper.GetResponseID(c),
		Object:  "chat.completion",
		Created: createdAt,
		Model:   model,
		Choices: []dto.OpenAITextResponseChoice{
			{
				Index: 0,
				Message: dto.Message{
					Role:    "assistant",
					Content: markdown,
				},
				FinishReason: "stop",
			},
		},
		Usage: *usage,
	}
	responseBody, err := common.Marshal(chatResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Transfer-Encoding")
	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func writeCodexImageChatStream(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response, model string) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	markdown, createdAt, usage, newAPIError := codexchannel.ImageMarkdownFromResponseBody(c, info, body)
	if newAPIError != nil {
		return nil, newAPIError
	}
	usage = ensureCodexImageChatUsage(c, info, model, markdown, usage)
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	helper.SetEventStreamHeaders(c)
	responseID := helper.GetResponseID(c)
	if err := helper.ObjectData(c, helper.GenerateStartEmptyResponse(responseID, createdAt, model, nil)); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if markdown != "" {
		chunk := &dto.ChatCompletionsStreamResponse{
			Id:      responseID,
			Object:  "chat.completion.chunk",
			Created: createdAt,
			Model:   model,
			Choices: []dto.ChatCompletionsStreamResponseChoice{
				{
					Index: 0,
					Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
						Content: &markdown,
					},
				},
			},
		}
		if err := helper.ObjectData(c, chunk); err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
	}
	if err := helper.ObjectData(c, helper.GenerateStopResponse(responseID, createdAt, model, "stop")); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if info.ShouldIncludeUsage {
		if err := helper.ObjectData(c, helper.GenerateFinalUsageResponse(responseID, createdAt, model, *usage)); err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
	}
	helper.Done(c)
	return usage, nil
}

func ensureCodexImageChatUsage(c *gin.Context, info *relaycommon.RelayInfo, model string, markdown string, usage *dto.Usage) *dto.Usage {
	if usage != nil && usage.TotalTokens != 0 {
		return usage
	}
	promptTokens := 0
	if info != nil {
		promptTokens = info.GetEstimatePromptTokens()
	}
	return service.ResponseText2Usage(c, markdown, model, promptTokens)
}
