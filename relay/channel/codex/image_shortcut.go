package codex

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func buildCodexImageShortcutResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest, isCompact bool) (dto.OpenAIResponsesRequest, bool, error) {
	if isCompact || !strings.EqualFold(strings.TrimSpace(request.Model), CodexImageModel) {
		return dto.OpenAIResponsesRequest{}, false, nil
	}

	ok, err := isSimpleCodexImageShortcutRequest(c, request)
	if err != nil || !ok {
		return dto.OpenAIResponsesRequest{}, false, err
	}

	prompt, ok, err := imagePromptFromResponsesInput(request.Input)
	if err != nil || !ok {
		return dto.OpenAIResponsesRequest{}, ok, err
	}

	imageRequest, err := BuildCodexImageGenerationResponsesRequest(c, info, prompt)
	if err != nil {
		return dto.OpenAIResponsesRequest{}, true, err
	}
	EnsureResponsesImageGenerationTool(info)
	return imageRequest, true, nil
}

func isSimpleCodexImageShortcutRequest(c *gin.Context, request dto.OpenAIResponsesRequest) (bool, error) {
	if !rawMessageUnset(request.Tools) || !rawMessageUnset(request.ToolChoice) {
		return false, nil
	}
	if !rawMessageUnset(request.Include) ||
		!rawMessageUnset(request.Conversation) ||
		!rawMessageUnset(request.ContextManagement) ||
		!rawMessageUnset(request.Instructions) ||
		!rawMessageUnset(request.Metadata) ||
		!rawMessageUnset(request.ParallelToolCalls) ||
		!rawMessageUnset(request.Store) ||
		!rawMessageUnset(request.PromptCacheKey) ||
		!rawMessageUnset(request.PromptCacheRetention) ||
		!rawMessageUnset(request.SafetyIdentifier) ||
		!rawMessageUnset(request.Text) ||
		!rawMessageUnset(request.Truncation) ||
		!rawMessageUnset(request.User) ||
		!rawMessageUnset(request.Prompt) ||
		!rawMessageUnset(request.EnableThinking) ||
		!rawMessageUnset(request.Preset) ||
		request.MaxOutputTokens != nil ||
		request.TopLogProbs != nil ||
		request.PreviousResponseID != "" ||
		request.Reasoning != nil ||
		request.ServiceTier != "" ||
		request.StreamOptions != nil ||
		request.Temperature != nil ||
		request.TopP != nil ||
		request.MaxToolCalls != nil {
		return false, nil
	}

	raw, ok, err := rawResponsesRequestBody(c)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	for key := range gjson.ParseBytes(raw).Map() {
		switch key {
		case "model", "input", "stream":
		default:
			return false, nil
		}
	}
	if input := gjson.GetBytes(raw, "input"); input.Exists() && input.Type != gjson.String {
		return false, nil
	}
	return true, nil
}

func rawMessageUnset(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0
}

func rawResponsesRequestBody(c *gin.Context) ([]byte, bool, error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, false, nil
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, false, err
	}
	raw, err := storage.Bytes()
	if err != nil {
		return nil, false, err
	}
	if len(raw) == 0 || common.GetJsonType(raw) != "object" {
		return nil, false, nil
	}
	return raw, true, nil
}

func imagePromptFromResponsesInput(raw json.RawMessage) (string, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false, nil
	}

	switch common.GetJsonType(raw) {
	case "string":
		var prompt string
		if err := common.Unmarshal(raw, &prompt); err != nil {
			return "", false, err
		}
		prompt = strings.TrimSpace(prompt)
		return prompt, prompt != "", nil
	default:
		return "", false, nil
	}
}
