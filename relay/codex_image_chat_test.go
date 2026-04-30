package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	appconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	codexchannel "github.com/QuantumNous/new-api/relay/channel/codex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-gonic/gin"
)

func TestCodexImagePromptFromChatRequestUsesUserText(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: codexchannel.CodexImageModel,
		Messages: []dto.Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "text", "text": "画一张中文海报"},
					map[string]any{"type": "text", "text": "标题是「春日计划」"},
				},
			},
		},
	}

	got := codexImagePromptFromChatRequest(req)
	want := "画一张中文海报\n标题是「春日计划」"
	if got != want {
		t.Fatalf("unexpected prompt: got %q want %q", got, want)
	}
}

func TestCodexImagePromptFromChatRequestIncludesTextContext(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: codexchannel.CodexImageModel,
		Messages: []dto.Message{
			{Role: "system", Content: "你是海报设计师"},
			{Role: "developer", Content: "优先使用清晰中文排版"},
			{Role: "user", Content: "画一张夜市读书会海报"},
		},
	}

	got := codexImagePromptFromChatRequest(req)
	want := "Instructions:\n你是海报设计师\n\n优先使用清晰中文排版\n\nUser request:\n画一张夜市读书会海报"
	if got != want {
		t.Fatalf("unexpected prompt: got %q want %q", got, want)
	}
}

func TestIsCodexImageChatCompletionRequestUsesMappedModel(t *testing.T) {
	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeChatCompletions,
		OriginModelName: codexchannel.CodexImageModel,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiType:           appconstant.APITypeCodex,
			UpstreamModelName: "gpt-5.4",
		},
	}
	req := &dto.GeneralOpenAIRequest{Model: "gpt-5.4"}

	if isCodexImageChatCompletionRequest(info, req) {
		t.Fatalf("remapped non-image upstream model should not enter codex image chat shim")
	}
}

func TestValidateSimpleCodexImageChatRequestAllowsTextContext(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: codexchannel.CodexImageModel,
		Messages: []dto.Message{
			{Role: "system", Content: "你是设计师"},
			{Role: "developer", Content: []any{
				map[string]any{"type": "text", "text": "只输出中文海报"},
			}},
			{Role: "user", Content: "画图"},
		},
	}

	if err := validateSimpleCodexImageChatRequest(req); err != nil {
		t.Fatalf("expected text context to be accepted, got %v", err)
	}
}

func TestValidateSimpleCodexImageChatRequestRejectsComplexInputs(t *testing.T) {
	tests := []struct {
		name string
		req  *dto.GeneralOpenAIRequest
	}{
		{
			name: "tools",
			req: &dto.GeneralOpenAIRequest{
				Model: codexchannel.CodexImageModel,
				Messages: []dto.Message{
					{Role: "user", Content: "画图"},
				},
				Tools: []dto.ToolCallRequest{{Type: "function"}},
			},
		},
		{
			name: "image_url",
			req: &dto.GeneralOpenAIRequest{
				Model: codexchannel.CodexImageModel,
				Messages: []dto.Message{
					{
						Role: "user",
						Content: []any{
							map[string]any{"type": "text", "text": "参考这张图"},
							map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
						},
					},
				},
			},
		},
		{
			name: "assistant_history",
			req: &dto.GeneralOpenAIRequest{
				Model: codexchannel.CodexImageModel,
				Messages: []dto.Message{
					{Role: "assistant", Content: "之前已经生成过主题"},
					{Role: "user", Content: "画图"},
				},
			},
		},
		{
			name: "context_after_user",
			req: &dto.GeneralOpenAIRequest{
				Model: codexchannel.CodexImageModel,
				Messages: []dto.Message{
					{Role: "user", Content: "画图"},
					{Role: "system", Content: "你是设计师"},
				},
			},
		},
		{
			name: "multi_turn",
			req: &dto.GeneralOpenAIRequest{
				Model: codexchannel.CodexImageModel,
				Messages: []dto.Message{
					{Role: "user", Content: "先想一个主题"},
					{Role: "user", Content: "画出来"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateSimpleCodexImageChatRequest(tt.req); err == nil {
				t.Fatalf("expected complex input to be rejected")
			}
		})
	}
}

func TestWriteCodexImageChatResponseReturnsMarkdownDataURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: codexchannel.CodexImageModel,
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{},
		},
	}
	body := `data: {"type":"response.completed","response":{"created_at":123,"output":[{"type":"image_generation_call","result":"aGVsbG8=","output_format":"png","quality":"low","size":"1024x1024"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	usage, err := writeCodexImageChatResponse(c, info, resp, codexchannel.CodexImageModel)
	if err != nil {
		t.Fatalf("writeCodexImageChatResponse returned error: %v", err)
	}
	if usage.TotalTokens != 3 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
	if !c.GetBool("image_generation_call") {
		t.Fatalf("expected image_generation_call marker")
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(w.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("chat response unmarshal failed: %v; body=%s", err, w.Body.String())
	}
	if chatResp.Object != "chat.completion" || chatResp.Model != codexchannel.CodexImageModel {
		t.Fatalf("unexpected chat response: %#v", chatResp)
	}
	if len(chatResp.Choices) != 1 || !strings.Contains(chatResp.Choices[0].Message.StringContent(), "data:image/png;base64,aGVsbG8=") {
		t.Fatalf("chat response did not include markdown image: %s", w.Body.String())
	}
	if contentType := w.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("expected application/json content type, got %q", contentType)
	}
}

func TestWriteCodexImageChatStreamReturnsSingleMarkdownDelta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName:    codexchannel.CodexImageModel,
		ShouldIncludeUsage: true,
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{}},
	}
	body := `data: {"type":"response.completed","response":{"created_at":123,"output":[{"type":"image_generation_call","result":"aGVsbG8=","output_format":"png"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	usage, err := writeCodexImageChatStream(c, info, resp, codexchannel.CodexImageModel)
	if err != nil {
		t.Fatalf("writeCodexImageChatStream returned error: %v", err)
	}
	if usage.TotalTokens != 3 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
	streamBody := w.Body.String()
	if !strings.Contains(streamBody, `"object":"chat.completion.chunk"`) ||
		!strings.Contains(streamBody, "data:image/png;base64,aGVsbG8=") ||
		!strings.Contains(streamBody, "[DONE]") {
		t.Fatalf("unexpected stream body: %s", streamBody)
	}
	if contentType := w.Header().Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("expected text/event-stream content type, got %q", contentType)
	}
}

func TestWriteCodexImageChatStreamDoesNotEmitBeforeInvalidUpstreamBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{OriginModelName: codexchannel.CodexImageModel}
	body := `data: {"type":"response.completed","response":{"created_at":123,"output":[]}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	_, err := writeCodexImageChatStream(c, info, resp, codexchannel.CodexImageModel)
	if err == nil {
		t.Fatalf("expected invalid upstream body error")
	}
	if w.Body.Len() != 0 {
		t.Fatalf("stream should not emit partial success before validation, got %s", w.Body.String())
	}
	if contentType := w.Header().Get("Content-Type"); strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("event-stream header should not be committed before validation, got %q", contentType)
	}
}
